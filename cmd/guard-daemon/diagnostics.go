package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"guard-daemon/internal/observability"
)

type diagnosticsServer struct {
	path     string
	listener net.Listener
	server   *http.Server
	health   *observability.Health
	metrics  *observability.Metrics
	alerts   *observability.AlertManager
	close    sync.Once
}

func newDiagnosticsServer(path string, health *observability.Health, metrics *observability.Metrics, alerts *observability.AlertManager) (*diagnosticsServer, error) {
	if path == "" || health == nil || metrics == nil || alerts == nil {
		return nil, errors.New("diagnostics dependencies are missing")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("diagnostics path is occupied by an unsafe file")
		}
		if err := os.Remove(path); err != nil {
			return nil, errors.New("failed to remove stale diagnostics socket")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("failed to inspect diagnostics socket")
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, errors.New("failed to open diagnostics socket")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, errors.New("failed to restrict access to diagnostics socket")
	}
	diagnostics := &diagnosticsServer{path: path, listener: listener, health: health, metrics: metrics, alerts: alerts}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", diagnostics.handleHealth)
	mux.HandleFunc("GET /metrics", diagnostics.handleMetrics)
	diagnostics.server = &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	return diagnostics, nil
}

func (diagnostics *diagnosticsServer) Run(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = diagnostics.server.Shutdown(context.Background())
		case <-done:
		}
	}()
	err := diagnostics.server.Serve(diagnostics.listener)
	close(done)
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (diagnostics *diagnosticsServer) Close() error {
	var result error
	diagnostics.close.Do(func() {
		result = diagnostics.server.Close()
		if errors.Is(result, http.ErrServerClosed) {
			result = nil
		}
		if err := os.Remove(diagnostics.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, errors.New("failed to remove diagnostics socket"))
		}
	})
	return result
}

func (diagnostics *diagnosticsServer) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	snapshot := diagnostics.health.Snapshot()
	writer.Header().Set("Content-Type", "application/json")
	if snapshot.State != observability.HealthHealthyIdle {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}
	writeJSON(writer, healthResponse(snapshot))
}

func (diagnostics *diagnosticsServer) handleMetrics(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, metricsResponse(diagnostics.metrics.Snapshot(), diagnostics.alerts.Snapshot()))
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(true)
	_ = encoder.Encode(value)
}

type diagnosticsHealth struct {
	Status   string                            `json:"status"`
	Networks map[string]diagnosticsChainHealth `json:"networks"`
}

type diagnosticsChainHealth struct {
	Status          string `json:"status"`
	RPCDegraded     bool   `json:"rpc_degraded"`
	BudgetBlocked   bool   `json:"budget_blocked"`
	AmbiguousRescue bool   `json:"ambiguous_rescue"`
	Stopped         bool   `json:"stopped"`
}

func healthResponse(snapshot observability.HealthSnapshot) diagnosticsHealth {
	result := diagnosticsHealth{Status: snapshot.State.String(), Networks: make(map[string]diagnosticsChainHealth, len(snapshot.Chains))}
	for chainID, chain := range snapshot.Chains {
		result.Networks[strconv.FormatInt(int64(chainID), 10)] = diagnosticsChainHealth{
			Status: chain.State.String(), RPCDegraded: chain.Conditions.RPCDegraded,
			BudgetBlocked: chain.Conditions.BudgetBlocked, AmbiguousRescue: chain.Conditions.AmbiguousRescue,
			Stopped: chain.Conditions.Stopped,
		}
	}
	return result
}

type diagnosticsMetrics struct {
	GlobalBudget diagnosticsBudget                  `json:"global_budget"`
	Networks     map[string]diagnosticsChainMetrics `json:"networks"`
	ActiveAlerts int                                `json:"active_alerts"`
	Alerts       []diagnosticsAlert                 `json:"alerts"`
}

type diagnosticsAlert struct {
	ChainID string `json:"chain_id"`
	Code    string `json:"code"`
}

type diagnosticsBudget struct {
	Spent     string `json:"spent_wei"`
	Reserved  string `json:"reserved_wei"`
	Remaining string `json:"remaining_wei"`
}

type diagnosticsChainMetrics struct {
	QueueDepth         uint64            `json:"queue_depth"`
	Candidates         uint64            `json:"candidates"`
	Attempts           uint64            `json:"attempts"`
	Budget             diagnosticsBudget `json:"budget"`
	RPCErrors          uint64            `json:"rpc_errors"`
	Reconnects         uint64            `json:"reconnects"`
	LostRaces          uint64            `json:"lost_races"`
	ActiveAmbiguous    uint64            `json:"active_ambiguous"`
	TotalAmbiguous     uint64            `json:"total_ambiguous"`
	DelegationState    uint8             `json:"delegation_state"`
	LastReconciliation string            `json:"last_successful_reconciliation,omitempty"`
}

func metricsResponse(snapshot observability.MetricsSnapshot, alerts []observability.AlertStatus) diagnosticsMetrics {
	result := diagnosticsMetrics{
		GlobalBudget: diagnosticBudget(snapshot.GlobalBudget),
		Networks:     make(map[string]diagnosticsChainMetrics, len(snapshot.Chains)),
	}
	for _, alert := range alerts {
		if alert.Active {
			result.ActiveAlerts++
			result.Alerts = append(result.Alerts, diagnosticsAlert{ChainID: strconv.FormatInt(int64(alert.ChainID), 10), Code: alert.Code.String()})
		}
	}
	for chainID, chain := range snapshot.Chains {
		last := ""
		if !chain.LastSuccessfulReconciliation.IsZero() {
			last = chain.LastSuccessfulReconciliation.UTC().Format(time.RFC3339Nano)
		}
		result.Networks[strconv.FormatInt(int64(chainID), 10)] = diagnosticsChainMetrics{
			QueueDepth: chain.QueueDepth, Candidates: chain.Candidates, Attempts: chain.Attempts,
			Budget:    diagnosticsBudget{Spent: chain.SpentBudget.String(), Reserved: chain.ReservedBudget.String(), Remaining: chain.RemainingBudget.String()},
			RPCErrors: chain.RPCErrors, Reconnects: chain.Reconnects, LostRaces: chain.LostRaces,
			ActiveAmbiguous: chain.ActiveAmbiguous, TotalAmbiguous: chain.TotalAmbiguous,
			DelegationState: uint8(chain.Delegation), LastReconciliation: last,
		}
	}
	return result
}

func diagnosticBudget(budget observability.BudgetMetrics) diagnosticsBudget {
	return diagnosticsBudget{Spent: budget.Spent.String(), Reserved: budget.Reserved.String(), Remaining: budget.Remaining.String()}
}
