package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"guard-daemon/internal/clock"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"

	"github.com/holiman/uint256"
)

func TestDiagnosticsExposeStateAndBoundedMetricsWithoutSensitiveFields(t *testing.T) {
	chainID := domain.NetworkID(31337)
	health, err := observability.NewHealth([]domain.NetworkID{chainID})
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := observability.NewMetrics([]domain.NetworkID{chainID})
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := observability.NewAlertManager(observability.AlertManagerConfig{Cooldown: time.Minute, Capacity: 8}, clock.Real{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	health.Stop()
	metrics.SetGlobalBudget(*uint256.NewInt(3), *uint256.NewInt(2), *uint256.NewInt(5))
	if err := metrics.SetBudget(chainID, *uint256.NewInt(1), *uint256.NewInt(2), *uint256.NewInt(3)); err != nil {
		t.Fatal(err)
	}
	_, _ = alerts.Raise(chainID, observability.AlertPaidActionsStopped)

	path := filepath.Join(shortTestStateDirectory(t), "diagnostics.sock")
	diagnostics, err := newDiagnosticsServer(path, health, metrics, alerts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = diagnostics.Close() })
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("diagnostics socket mode = %v", info.Mode().Perm())
	}

	healthResponse := httptest.NewRecorder()
	diagnostics.handleHealth(healthResponse, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if healthResponse.Code != http.StatusServiceUnavailable || !strings.Contains(healthResponse.Body.String(), `"status":"stopped"`) {
		t.Fatalf("health response = %d %s", healthResponse.Code, healthResponse.Body.String())
	}
	metricsResponse := httptest.NewRecorder()
	diagnostics.handleMetrics(metricsResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := metricsResponse.Body.String()
	for _, required := range []string{`"31337"`, `"spent_wei":"3"`, `"active_alerts":1`} {
		if !strings.Contains(body, required) {
			t.Fatalf("metrics response does not contain %s: %s", required, body)
		}
	}
	for _, forbidden := range []string{"rpc_url", "private_key", "signature", "raw_transaction", "token_address"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metrics response contains forbidden field %q: %s", forbidden, body)
		}
	}
}

func TestDiagnosticsRejectUnsafeExistingPath(t *testing.T) {
	chainID := domain.NetworkID(31337)
	health, _ := observability.NewHealth([]domain.NetworkID{chainID})
	metrics, _ := observability.NewMetrics([]domain.NetworkID{chainID})
	alerts, _ := observability.NewAlertManager(observability.AlertManagerConfig{Cooldown: time.Minute, Capacity: 8}, clock.Real{}, nil)
	path := filepath.Join(shortTestStateDirectory(t), "diagnostics.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if diagnostics, err := newDiagnosticsServer(path, health, metrics, alerts); err == nil || diagnostics != nil {
		t.Fatalf("unsafe diagnostics path = (%v, %v)", diagnostics, err)
	}
}
