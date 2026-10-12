package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"guard-daemon/internal/budget"
	"guard-daemon/internal/clock"
	"guard-daemon/internal/config"
	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rescue"
	"guard-daemon/internal/rescue/dryrun"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"
	"guard-daemon/internal/watcher"

	"github.com/ethereum/go-ethereum/common"
)

const (
	reconnectDelay       = 5 * time.Second
	candidateRetryDelay  = 5 * time.Second
	maxPendingCandidates = uint32(1024)
	maxDiscoveredTokens  = uint32(1024)
	maxWatcherLookback   = uint64(10_000)
	processLeaseTTL      = 30 * time.Second
	leaseReleaseTimeout  = 5 * time.Second
	rescueReceiptTimeout = 60 * time.Second
	rescueMaxAttempts    = uint32(3)
	restoreMarkerPath    = "/var/lib/guard-daemon-operator/live-disabled-after-restore"
)

const (
	errorStartupInvalid      domain.ErrorCode = "daemon_startup_invalid"
	errorDialFailed          domain.ErrorCode = "daemon_rpc_dial_failed"
	errorClientInvalid       domain.ErrorCode = "daemon_rpc_client_invalid"
	errorSessionFailed       domain.ErrorCode = "daemon_session_failed"
	errorWatcherFailed       domain.ErrorCode = "daemon_watcher_failed"
	errorWorkerStopped       domain.ErrorCode = "daemon_worker_stopped"
	errorQueueNextFailed     domain.ErrorCode = "daemon_queue_next_failed"
	errorIncidentPutFailed   domain.ErrorCode = "daemon_incident_put_failed"
	errorCandidateNackFailed domain.ErrorCode = "daemon_candidate_nack_failed"
	errorCandidateAckFailed  domain.ErrorCode = "daemon_candidate_ack_failed"
	errorTokenForbidden      domain.ErrorCode = "daemon_token_forbidden"
	errorStoreFailed         domain.ErrorCode = "daemon_store_failed"
	errorQuorumFailed        domain.ErrorCode = "daemon_runtime_quorum_failed"
	errorLeaseAcquireFailed  domain.ErrorCode = "daemon_lease_acquire_failed"
	errorLeaseMaintainFailed domain.ErrorCode = "daemon_lease_maintain_failed"
	errorLeaseReleaseFailed  domain.ErrorCode = "daemon_lease_release_failed"
	errorFenceAcquireFailed  domain.ErrorCode = "daemon_fence_acquire_failed"
	errorFenceValidateFailed domain.ErrorCode = "daemon_fence_validate_failed"
	errorFenceReleaseFailed  domain.ErrorCode = "daemon_fence_release_failed"
	errorBudgetFailed        domain.ErrorCode = "daemon_budget_failed"
)

const (
	eventHTTPFallback      observability.EventCode = "daemon_rpc_http_fallback"
	eventGenerationError   observability.EventCode = "daemon_generation_failed"
	eventUnknownTokenOptIn observability.EventCode = "daemon_unknown_token_opt_in"
)

type generationClient interface {
	Generation() uint64
	Reader() rpc.Reader
	LogSubscriber() rpc.LogSubscriber
	HeadSubscriber() rpc.HeadSubscriber
	Close()
}

type structuredAlertSink struct {
	observer observability.Observer
}

func (sink structuredAlertSink) Notify(alert observability.Alert) error {
	structured, ok := sink.observer.(observability.StructuredObserver)
	if !ok {
		return observability.ErrAlertDelivery
	}
	level := observability.LevelWarning
	result := observability.ResultAccepted
	if alert.State == observability.AlertResolved {
		level = observability.LevelInfo
		result = observability.ResultConfirmed
	}
	return structured.Write(observability.SafeEvent{
		Level: level, Code: observability.LogAlertState, ChainID: alert.ChainID,
		Result: result, Alert: alert.Code,
	})
}

type submissionClient interface {
	rpc.Broadcaster
	Close()
}

type attestationClient interface {
	contracts.AttestationReader
	Close()
}

type generationRunner interface {
	Run(context.Context) error
}

type runtimeQuorum interface {
	rpc.FinalizedReader
	rescue.FinalityReader
	Close()
}

type candidateHandler interface {
	Generation() uint64
	Handle(context.Context, domain.RescueCandidate) error
}

type candidateSession interface {
	candidateHandler
	Close()
}

type reconciliationSession interface {
	RunReconciliation(context.Context) error
}

type preparedNetwork struct {
	configured config.Network
	manifest   contracts.DeploymentManifest
	handoff    store.HandoffStore
	lease      store.Lease
	fence      store.ProcessFence
}

type daemonDependencies struct {
	serviceClock       clock.Clock
	observer           observability.Observer
	dial               func(context.Context, string, uint64) (generationClient, error)
	dialSubmission     func(context.Context, string) (submissionClient, error)
	loadManifest       func(config.Runtime, config.Network) (contracts.DeploymentManifest, error)
	attestNetwork      func(context.Context, config.Network, contracts.DeploymentManifest, time.Duration) error
	newSigners         func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error)
	newSession         func(context.Context, *networkProcess, generationClient, runtimeQuorum) (candidateSession, error)
	newWatcher         func(watcher.Dependencies) (generationRunner, error)
	openStore          func(config.Runtime, config.Network, domain.Network) (store.HandoffStore, error)
	openQuorum         func(context.Context, config.Network, time.Duration) (runtimeQuorum, error)
	acquireFence       func(store.LeaseKey) (store.ProcessFence, error)
	acquireBudgetFence func(common.Address) (store.ProcessFence, error)
	openBudget         func(config.Runtime, []preparedNetwork) (budget.Ledger, error)
	openAdmission      func(config.Runtime, rescue.AdmissionConfig) (*rescue.AdmissionController, error)
	alertStatePath     func(config.Runtime) (string, error)
	lstatRestoreMarker func(string) (os.FileInfo, error)
}

type daemon struct {
	dependencies   daemonDependencies
	networks       []*networkProcess
	budget         budget.Ledger
	budgetFence    store.ProcessFence
	metrics        *observability.Metrics
	health         *observability.Health
	alerts         *observability.AlertManager
	gate           *observability.PaidActionGate
	stateDirectory string
	admission      *rescue.AdmissionController
}

type networkProcess struct {
	configured       config.Network
	network          domain.Network
	mode             config.Mode
	source           common.Address
	sponsor          common.Address
	destination      common.Address
	readTimeout      time.Duration
	watchLookback    uint64
	coordinator      *rescue.Coordinator
	dryAuthorizer    rescue.AuthorizationSigner
	dryTransactioner rescue.TransactionSigner
	dryBroadcaster   rpc.Broadcaster
	dryAttempts      *dryrun.Attempts
	handoff          store.HandoffStore
	fence            store.ProcessFence
	codec            *contracts.ERC20Codec
	allowedTokens    map[common.Address]struct{}
	emergencyStopped bool
	metrics          *observability.Metrics
	health           *observability.Health
	alerts           *observability.AlertManager
}

type generationClientView struct {
	generationClient
	reader rpc.Reader
	logs   rpc.LogSubscriber
	heads  rpc.HeadSubscriber
}

func (client *generationClientView) Reader() rpc.Reader                 { return client.reader }
func (client *generationClientView) LogSubscriber() rpc.LogSubscriber   { return client.logs }
func (client *generationClientView) HeadSubscriber() rpc.HeadSubscriber { return client.heads }

type liveCandidateSession struct {
	*rescue.Session
	submission submissionClient
}

type guardedCandidateSession struct {
	*rescue.Session
}

func (*guardedCandidateSession) Close() {}

type tokenPolicySession struct {
	candidateSession
	allowed      map[common.Address]struct{}
	allowUnknown bool
}

type reconcilingTokenPolicySession struct {
	*tokenPolicySession
	reconciliationSession
}

func (session *tokenPolicySession) Handle(ctx context.Context, candidate domain.RescueCandidate) error {
	if candidate.Kind == domain.CandidateToken {
		if candidate.Token.Address == (common.Address{}) {
			return processError("daemon.token_policy", domain.ErrorConfiguration, errorTokenForbidden, false, nil)
		}
		if _, allowed := session.allowed[candidate.Token.Address]; !session.allowUnknown && !allowed {
			return processError("daemon.token_policy", domain.ErrorConfiguration, errorTokenForbidden, false, nil)
		}
	}
	return session.candidateSession.Handle(ctx, candidate)
}

func (session *liveCandidateSession) Close() {
	session.submission.Close()
}

func (process *daemon) Run(ctx context.Context) error {
	if ctx == nil {
		runError := processError("daemon.run", domain.ErrorConfiguration, errorStartupInvalid, false, nil)
		return errors.Join(runError, process.shutdown(nil))
	}

	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	diagnostics, err := newDiagnosticsServer(filepath.Join(process.stateDirectory, "diagnostics.sock"), process.health, process.metrics, process.alerts)
	if err != nil {
		return errors.Join(processError("daemon.diagnostics", domain.ErrorInternal, errorStartupInvalid, false, err), process.shutdown(nil))
	}
	defer diagnostics.Close()

	type workerResult struct {
		network     *networkProcess
		lease       bool
		diagnostics bool
		err         error
	}
	liveNetworks := 0
	for _, network := range process.networks {
		if network.coordinator != nil {
			liveNetworks++
		}
	}
	results := make(chan workerResult, len(process.networks)+liveNetworks+1)
	go func() {
		results <- workerResult{diagnostics: true, err: diagnostics.Run(runContext)}
	}()
	for _, network := range process.networks {
		go func() {
			results <- workerResult{network: network, err: network.supervise(runContext, process.dependencies)}
		}()
		if network.coordinator != nil {
			go func() {
				results <- workerResult{network: network, lease: true, err: network.coordinator.MaintainLease(runContext)}
			}()
		}
	}

	var runError error
	lostLeases := make(map[*networkProcess]struct{})
	for range len(process.networks) + liveNetworks + 1 {
		result := <-results
		if result.diagnostics {
			if runContext.Err() == nil && runError == nil {
				runError = processError("daemon.diagnostics", domain.ErrorInternal, errorWorkerStopped, false, result.err)
				cancel()
			}
			continue
		}
		if result.lease && rescue.IsLeaseLost(result.err) {
			lostLeases[result.network] = struct{}{}
		}
		if !result.lease {
			if runContext.Err() == nil && runError == nil {
				if result.err == nil {
					result.err = errors.New("network worker stopped unexpectedly")
				}
				runError = processError("daemon.network", domain.ErrorInternal, errorWorkerStopped, false, result.err)
				cancel()
			}
			continue
		}
		if runContext.Err() != nil || runError != nil {
			continue
		}
		if result.err == nil {
			result.err = errors.New("lease maintenance stopped unexpectedly")
		}
		runError = processError("daemon.lease_maintain", domain.ErrorInternal, errorLeaseMaintainFailed, false, result.err)
		cancel()
	}
	return errors.Join(runError, process.shutdown(lostLeases))
}

func (process *daemon) shutdown(lostLeases map[*networkProcess]struct{}) error {
	var shutdownError error
	for _, network := range process.networks {
		if network.coordinator == nil {
			continue
		}
		releaseContext, cancel := context.WithTimeout(context.Background(), leaseReleaseTimeout)
		err := network.coordinator.ReleaseLease(releaseContext)
		cancel()
		_, alreadyLost := lostLeases[network]
		if err != nil && !alreadyLost {
			shutdownError = errors.Join(shutdownError, processError("daemon.lease_release", domain.ErrorInternal, errorLeaseReleaseFailed, false, err))
		}
	}
	for _, network := range process.networks {
		if network.fence == nil {
			continue
		}
		if err := network.fence.Release(); err != nil {
			shutdownError = errors.Join(shutdownError, processError("daemon.fence_release", domain.ErrorInternal, errorFenceReleaseFailed, false, err))
		}
	}
	if err := process.closeStores(); err != nil {
		shutdownError = errors.Join(shutdownError, processError("daemon.store.close", domain.ErrorInternal, errorStoreFailed, false, err))
	}
	if process.budget != nil {
		if err := process.budget.Close(); err != nil {
			shutdownError = errors.Join(shutdownError, processError("daemon.budget.close", domain.ErrorInternal, errorBudgetFailed, false, err))
		}
	}
	if process.admission != nil {
		if err := process.admission.Close(); err != nil {
			shutdownError = errors.Join(shutdownError, processError("daemon.admission.close", domain.ErrorInternal, errorBudgetFailed, false, err))
		}
	}
	if process.budgetFence != nil {
		if err := process.budgetFence.Release(); err != nil {
			shutdownError = errors.Join(shutdownError, processError("daemon.budget_fence_release", domain.ErrorInternal, errorFenceReleaseFailed, false, err))
		}
	}
	return shutdownError
}

func (process *daemon) closeStores() error {
	var firstError error
	for _, network := range process.networks {
		if err := network.handoff.Close(); err != nil && firstError == nil {
			firstError = err
		}
	}
	return firstError
}

func consumeCandidates(ctx context.Context, network domain.NetworkID, queue store.CandidateQueue, incidents store.IncidentStore, session candidateHandler, serviceClock clock.Clock, metrics *observability.Metrics) error {
	for {
		candidate, err := queue.Next(ctx, network)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return processError("daemon.queue_next", domain.ErrorInternal, errorQueueNextFailed, true, err)
		}
		if metrics != nil {
			_ = metrics.RecordCandidate(network)
		}

		incidentID := domain.NewIncidentID(candidate.ID)
		if err := incidents.PutIncident(ctx, store.Incident{
			ID:        incidentID,
			Candidate: candidate.ID,
			Network:   candidate.Network,
			CreatedAt: serviceClock.Now(),
		}); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return processError("daemon.incident_put", domain.ErrorInternal, errorIncidentPutFailed, true, err)
		}

		handlingCandidate := candidate
		if handlingCandidate.Generation != 0 && handlingCandidate.Generation != session.Generation() {
			// Неизменный ID кандидата в очереди сохраняется, а при повторной обработке
			// кандидат привязывается к текущему поколению RPC.
			handlingCandidate.Generation = session.Generation()
		}
		handleError := session.Handle(ctx, handlingCandidate)
		if err := ctx.Err(); err != nil {
			return err
		}
		if shouldReplay(handleError) {
			retryAt := serviceClock.Now().Add(candidateRetryDelay)
			var scheduled interface{ NextRetryAt() time.Time }
			if errors.As(handleError, &scheduled) && scheduled.NextRetryAt().After(retryAt) {
				retryAt = scheduled.NextRetryAt()
			}
			if err := queue.Nack(ctx, candidate.ID, incidentID, retryAt); err != nil {
				return processError("daemon.candidate_nack", domain.ErrorInternal, errorCandidateNackFailed, true, err)
			}
			if err := updateQueueDepth(ctx, network, queue, metrics); err != nil {
				return err
			}
			continue
		}
		if err := queue.Ack(ctx, candidate.ID, incidentID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return processError("daemon.candidate_ack", domain.ErrorInternal, errorCandidateAckFailed, true, err)
		}
		if err := updateQueueDepth(ctx, network, queue, metrics); err != nil {
			return err
		}
	}
}

func updateQueueDepth(ctx context.Context, network domain.NetworkID, queue store.CandidateQueue, metrics *observability.Metrics) error {
	if metrics == nil {
		return nil
	}
	candidates, err := queue.Replay(ctx, network)
	if err != nil {
		return processError("daemon.queue_depth", domain.ErrorInternal, errorQueueNextFailed, true, err)
	}
	if err := metrics.SetQueueDepth(network, uint64(len(candidates))); err != nil {
		return processError("daemon.queue_metrics", domain.ErrorInternal, errorQueueNextFailed, true, err)
	}
	return nil
}

func shouldReplay(err error) bool {
	if err == nil {
		return false
	}
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) {
		return true
	}
	return classified.Retryable || classified.Ambiguous
}

func generationError(workerErrors ...error) error {
	for _, err := range workerErrors {
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	for _, err := range workerErrors {
		if err != nil {
			return err
		}
	}
	return processError("daemon.worker", domain.ErrorInternal, errorWorkerStopped, true, nil)
}

func publicErrorCode(err error) domain.ErrorCode {
	var classified *domain.ClassifiedError
	if errors.As(err, &classified) && classified.Code != "" {
		return classified.Code
	}
	return errorWorkerStopped
}

func processError(operation string, class domain.ErrorClass, code domain.ErrorCode, retryable bool, cause error) error {
	return domain.NewError(operation, class, code, retryable, false, cause)
}
