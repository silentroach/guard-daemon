package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"
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

func newProductionDependencies(observer observability.Observer, mode config.Mode) daemonDependencies {
	dialer := rpc.EthClientDialer{}
	dependencies := daemonDependencies{
		serviceClock: clock.Real{},
		observer:     observer,
		dial: func(ctx context.Context, endpoint string, generation uint64) (generationClient, error) {
			return dialer.DialContext(ctx, endpoint, generation)
		},
		loadManifest: loadConfiguredManifest,
		openStore: func(runtimeConfig config.Runtime, _ config.Network, network domain.Network) (store.HandoffStore, error) {
			path := filepath.Join(runtimeConfig.Watch.StateDirectory, "watcher-"+strconv.FormatInt(int64(network.ChainID), 10)+".db")
			return store.Open(path, store.OpenOptions{
				Network:             network.ChainID,
				Source:              runtimeConfig.SourceAddress,
				Sponsor:             runtimeConfig.SponsorAddress,
				Destination:         runtimeConfig.Destination,
				Rescuer:             network.Rescuer,
				PolicyFingerprint:   watcher.PolicyFingerprint(network, runtimeConfig.Watch.LookbackBlocks),
				MaxPending:          maxPendingCandidates,
				MaxDiscoveredTokens: maxDiscoveredTokens,
				Clock:               clock.Real{},
			})
		},
		openQuorum: openRuntimeQuorum,
		openBudget: func(runtimeConfig config.Runtime, prepared []preparedNetwork) (budget.Ledger, error) {
			policy, err := runtimeBudgetPolicy(runtimeConfig)
			if err != nil {
				return nil, err
			}
			path, err := store.CanonicalBudgetPath(runtimeConfig.SponsorAddress)
			if err != nil {
				return nil, err
			}
			return budget.Open(path, budget.OpenOptions{
				Policy: policy, PolicyFingerprint: budgetBindingFingerprint(runtimeConfig, prepared), Now: time.Now,
			})
		},
		openAdmission: func(runtimeConfig config.Runtime, admissionConfig rescue.AdmissionConfig) (*rescue.AdmissionController, error) {
			path, err := store.CanonicalAdmissionPath(runtimeConfig.SponsorAddress)
			if err != nil {
				return nil, err
			}
			return rescue.OpenAdmissionController(path, admissionConfig, clock.Real{})
		},
		alertStatePath: func(runtimeConfig config.Runtime) (string, error) {
			return store.CanonicalAlertPath(runtimeConfig.SponsorAddress)
		},
		lstatRestoreMarker: os.Lstat,
	}
	if mode.IsLive() {
		dependencies.dialSubmission = dialSubmissionClient
		dependencies.attestNetwork = attestConfiguredNetwork
		dependencies.newSigners = newPrivateKeySigners
		dependencies.acquireFence = store.AcquireProcessFence
		dependencies.acquireBudgetFence = store.AcquireBudgetFence
	}
	return dependencies
}

func runtimeBudgetPolicy(runtimeConfig config.Runtime) (budget.Policy, error) {
	global, err := budgetLimits(runtimeConfig.Policy.MaxTransactionCostWei, runtimeConfig.Policy.HourlyBudgetWei, runtimeConfig.Policy.DailyBudgetWei, runtimeConfig.Policy.CumulativeBudgetWei)
	if err != nil {
		return budget.Policy{}, err
	}
	policy := budget.Policy{Global: global, Networks: make([]budget.NetworkPolicy, 0, len(runtimeConfig.Networks))}
	for _, network := range runtimeConfig.Networks {
		economic := network.EconomicPolicy
		limits, err := budgetLimits(economic.MaxTransactionCostWei, economic.HourlyBudgetWei, economic.DailyBudgetWei, economic.CumulativeBudgetWei)
		if err != nil {
			return budget.Policy{}, err
		}
		overhead, overheadOverflow := uint256.FromBig(economic.TransactionOverheadWei)
		reserve, reserveOverflow := uint256.FromBig(economic.SponsorMinimumBalanceWei)
		if overheadOverflow || reserveOverflow || reserve.IsZero() {
			return budget.Policy{}, errors.New("некорректная budget policy сети")
		}
		policy.Networks = append(policy.Networks, budget.NetworkPolicy{
			Network: network.ChainID, Sponsor: runtimeConfig.SponsorAddress, Limits: limits,
			TransactionOverhead: *overhead, EmergencySponsorReserve: *reserve,
		})
	}
	return policy, nil
}

func budgetLimits(perTransaction, perHour, perDay, cumulative *big.Int) (budget.Limits, error) {
	values := []*big.Int{perTransaction, perHour, perDay, cumulative}
	converted := make([]*uint256.Int, len(values))
	for index, value := range values {
		if value == nil || value.Sign() <= 0 {
			return budget.Limits{}, errors.New("некорректные global budget limits")
		}
		var overflow bool
		converted[index], overflow = uint256.FromBig(value)
		if overflow {
			return budget.Limits{}, errors.New("budget limit не помещается в uint256")
		}
	}
	return budget.Limits{PerTransaction: *converted[0], PerHour: *converted[1], PerDay: *converted[2], Cumulative: *converted[3]}, nil
}

func budgetBindingFingerprint(runtimeConfig config.Runtime, prepared []preparedNetwork) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte("guard-daemon/budget-binding/v1\x00"))
	_, _ = hash.Write(runtimeConfig.SourceAddress[:])
	_, _ = hash.Write(runtimeConfig.SponsorAddress[:])
	_, _ = hash.Write(runtimeConfig.Destination[:])
	for _, network := range prepared {
		var chain [8]byte
		binary.BigEndian.PutUint64(chain[:], uint64(network.configured.ChainID))
		_, _ = hash.Write(chain[:])
		_, _ = hash.Write(network.manifest.Address[:])
		if network.configured.AllowUnknownTokens {
			_, _ = hash.Write([]byte{1})
		} else {
			_, _ = hash.Write([]byte{0})
		}
		for _, token := range network.configured.TrustedTokens {
			_, _ = hash.Write(token[:])
		}
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func rescuePolicy(network config.Network) (rescue.FeePolicy, uint256.Int, map[common.Address]rescue.TrustedTokenValuePolicy, error) {
	economic := network.EconomicPolicy
	values := []*big.Int{
		economic.MaxFeePerGasWei,
		economic.MaxPriorityFeePerGasWei,
		economic.TransactionOverheadWei,
		economic.UnknownTokenMaxTransactionCostWei,
		economic.NativeMinimumNetValueWei,
		economic.MaxTransactionCostWei,
	}
	converted := make([]*uint256.Int, len(values))
	for index, value := range values {
		if value == nil || value.Sign() < 0 {
			return rescue.FeePolicy{}, uint256.Int{}, nil, errors.New("некорректная rescue policy сети")
		}
		var overflow bool
		converted[index], overflow = uint256.FromBig(value)
		if overflow {
			return rescue.FeePolicy{}, uint256.Int{}, nil, errors.New("rescue policy не помещается в uint256")
		}
	}
	policy := rescue.FeePolicy{
		Network:                 network.ChainID,
		MaxFeePerGas:            *converted[0],
		MaxPriorityFeePerGas:    *converted[1],
		TokenGasLimit:           economic.TokenGasLimit,
		NativeGasLimit:          economic.NativeGasLimit,
		Overhead:                *converted[2],
		UnknownTokenCostCap:     *converted[3],
		TransactionCostCap:      *converted[5],
		UnboundedAdditionalFees: economic.UnboundedAdditionalFees,
	}
	tokenValues := make(map[common.Address]rescue.TrustedTokenValuePolicy, len(economic.TokenValueRules))
	for _, rule := range economic.TokenValueRules {
		minimum, minimumOverflow := uint256.FromBig(rule.MinimumBalance)
		maximum, maximumOverflow := uint256.FromBig(rule.MaxTransactionCostWei)
		if minimumOverflow || maximumOverflow || minimum.IsZero() || maximum.IsZero() {
			return rescue.FeePolicy{}, uint256.Int{}, nil, errors.New("некорректное правило ценности trusted token")
		}
		tokenValues[rule.Address] = rescue.TrustedTokenValuePolicy{MinimumBalance: *minimum, MaximumCost: *maximum}
	}
	return policy, *converted[4], tokenValues, nil
}

func openRuntimeQuorum(ctx context.Context, network config.Network, timeout time.Duration) (runtimeQuorum, error) {
	endpoints := make([]rpc.ProviderEndpoint, 0, len(network.ReadProviders))
	for _, provider := range network.ReadProviders {
		endpoints = append(endpoints, rpc.ProviderEndpoint{
			Identity: rpc.ProviderIdentity{
				ID: provider.ID, Fingerprint: provider.EndpointFingerprint, TrustDomain: provider.TrustDomain,
			},
			Endpoint: provider.HTTPURL,
		})
	}
	return rpc.DialQuorum(ctx, endpoints, timeout)
}

func loadConfiguredManifest(runtimeConfig config.Runtime, network config.Network) (contracts.DeploymentManifest, error) {
	manifestFile, err := os.Open(network.ManifestPath)
	if err != nil {
		return contracts.DeploymentManifest{}, errors.New("не удалось открыть deployment manifest")
	}
	defer manifestFile.Close()

	artifactFile, err := os.Open(runtimeConfig.Artifact.Path)
	if err != nil {
		return contracts.DeploymentManifest{}, errors.New("не удалось открыть canonical artifact")
	}
	defer artifactFile.Close()

	return contracts.LoadTrustedManifest(manifestFile, artifactFile, configuredManifestExpectations(runtimeConfig, network))
}

func configuredManifestExpectations(runtimeConfig config.Runtime, network config.Network) contracts.ManifestExpectations {
	return contracts.ManifestExpectations{
		ChainID:        strconv.FormatInt(int64(network.ChainID), 10),
		ContractRole:   "rescuer",
		Destination:    runtimeConfig.Destination,
		Sponsor:        runtimeConfig.SponsorAddress,
		ArtifactSHA256: runtimeConfig.Artifact.SHA256,
		SourceProvenance: contracts.SourceProvenance{
			Kind:  network.ManifestSourceKind,
			Value: network.ManifestSourceValue,
		},
		CompilerVersion: runtimeConfig.Artifact.CompilerVersion,
	}
}

func attestConfiguredNetwork(ctx context.Context, network config.Network, manifest contracts.DeploymentManifest, readTimeout time.Duration) error {
	return attestConfiguredNetworkWith(
		ctx,
		network,
		manifest,
		readTimeout,
		func(dialContext context.Context, endpoint string) (attestationClient, error) {
			return ethclient.DialContext(dialContext, endpoint)
		},
		contracts.AttestDeployment,
	)
}

func attestConfiguredNetworkWith(
	ctx context.Context,
	network config.Network,
	manifest contracts.DeploymentManifest,
	readTimeout time.Duration,
	dial func(context.Context, string) (attestationClient, error),
	attest func(context.Context, contracts.DeploymentManifest, []contracts.ReadProvider, time.Duration) error,
) error {
	if dial == nil || attest == nil {
		return errors.New("не заданы зависимости аттестации")
	}
	providers := make([]contracts.ReadProvider, 0, len(network.ReadProviders))
	clients := make([]attestationClient, 0, len(network.ReadProviders))
	defer func() {
		for _, client := range clients {
			client.Close()
		}
	}()

	for _, configuredProvider := range network.ReadProviders {
		dialContext, cancel := context.WithTimeout(ctx, readTimeout)
		client, err := dial(dialContext, configuredProvider.HTTPURL)
		cancel()
		if err != nil || client == nil {
			return domain.NewError("daemon.attestation_dial", domain.ErrorRPCTransient, errorDialFailed, true, false, err)
		}
		clients = append(clients, client)
		providers = append(providers, contracts.ReadProvider{
			ID:                  configuredProvider.ID,
			EndpointFingerprint: configuredProvider.EndpointFingerprint,
			TrustDomain:         configuredProvider.TrustDomain,
			Reader:              client,
		})
	}
	return attest(ctx, manifest, providers, readTimeout)
}

func newPrivateKeySigners(secrets config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
	sourceKey, sponsorKey, ok := secrets.PrivateKeys()
	if !ok {
		return nil, nil, errors.New("live private keys не заданы")
	}
	authorizer, err := rescue.NewPrivateKeyAuthorizationSigner(sourceKey)
	if err != nil {
		return nil, nil, err
	}
	transactioner, err := rescue.NewPrivateKeyTransactionSigner(sponsorKey)
	if err != nil {
		return nil, nil, err
	}
	return authorizer, transactioner, nil
}

func dialSubmissionClient(ctx context.Context, endpoint string) (submissionClient, error) {
	client, err := ethclient.DialContext(ctx, endpoint)
	if err != nil {
		return nil, domain.NewError("daemon.broadcast_dial", domain.ErrorRPCTransient, errorDialFailed, true, false, err)
	}
	return client, nil
}

func newProcessLeaseOwner() (string, error) {
	var opaque [32]byte
	if _, err := rand.Read(opaque[:]); err != nil {
		return "", errors.New("не удалось создать идентификатор владельца lease")
	}
	return hex.EncodeToString(opaque[:]), nil
}

func newDaemon(ctx context.Context, runtimeConfig config.Runtime, dependencies daemonDependencies) (_ *daemon, resultErr error) {
	if ctx == nil {
		return nil, processError("daemon.context", domain.ErrorConfiguration, errorStartupInvalid, false, nil)
	}
	if (!runtimeConfig.Mode.IsDryRun() && !runtimeConfig.Mode.IsLive()) || runtimeConfig.SourceAddress == (common.Address{}) || runtimeConfig.SponsorAddress == (common.Address{}) || runtimeConfig.Destination == (common.Address{}) || runtimeConfig.SourceAddress == runtimeConfig.SponsorAddress || runtimeConfig.SourceAddress == runtimeConfig.Destination || runtimeConfig.SponsorAddress == runtimeConfig.Destination || runtimeConfig.ReadTimeout <= 0 || runtimeConfig.Watch.StateDirectory == "" || strings.IndexByte(runtimeConfig.Watch.StateDirectory, 0) >= 0 || filepath.Clean(runtimeConfig.Watch.StateDirectory) != runtimeConfig.Watch.StateDirectory || runtimeConfig.Watch.LookbackBlocks == 0 || runtimeConfig.Watch.LookbackBlocks > maxWatcherLookback {
		return nil, processError("daemon.config", domain.ErrorConfiguration, errorStartupInvalid, false, nil)
	}
	dependencies, err := dependencies.withDefaults(runtimeConfig.Mode)
	if err != nil {
		return nil, processError("daemon.dependencies", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	if err := rejectRestoredSigningState(runtimeConfig, dependencies.lstatRestoreMarker); err != nil {
		return nil, processError("daemon.restore_marker", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	if _, err := runtimeBudgetPolicy(runtimeConfig); err != nil {
		return nil, processError("daemon.budget_policy", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}

	prepared := make([]preparedNetwork, 0, len(runtimeConfig.Networks))
	for _, configuredNetwork := range runtimeConfig.Networks {
		manifest, err := dependencies.loadManifest(runtimeConfig, configuredNetwork)
		if err != nil || manifest.Address == (common.Address{}) {
			return nil, processError("daemon.manifest", domain.ErrorConfiguration, errorStartupInvalid, false, err)
		}
		prepared = append(prepared, preparedNetwork{configured: configuredNetwork, manifest: manifest})
	}
	if len(prepared) == 0 {
		return nil, processError("daemon.networks", domain.ErrorConfiguration, errorStartupInvalid, false, nil)
	}
	chainIDs := make([]domain.NetworkID, 0, len(prepared))
	for _, network := range prepared {
		chainIDs = append(chainIDs, network.configured.ChainID)
	}
	metrics, err := observability.NewMetrics(chainIDs)
	if err != nil {
		return nil, processError("daemon.metrics", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	health, err := observability.NewHealth(chainIDs)
	if err != nil {
		return nil, processError("daemon.health", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	alertPath, err := dependencies.alertStatePath(runtimeConfig)
	if err != nil {
		return nil, processError("daemon.alerts", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	alerts, err := observability.NewAlertManager(observability.AlertManagerConfig{
		Cooldown:  runtimeConfig.Policy.AlertCooldown,
		Capacity:  len(chainIDs) * 8,
		StatePath: alertPath,
	}, dependencies.serviceClock, structuredAlertSink{observer: dependencies.observer})
	if err != nil {
		return nil, processError("daemon.alerts", domain.ErrorConfiguration, errorStartupInvalid, false, err)
	}
	gate := observability.NewPaidActionGate()
	if runtimeConfig.Policy.EmergencyStop {
		gate.Stop()
		health.Stop()
		for _, chainID := range chainIDs {
			if _, alertErr := alerts.Raise(chainID, observability.AlertPaidActionsStopped); alertErr != nil {
				return nil, processError("daemon.alert", domain.ErrorInternal, errorStartupInvalid, false, alertErr)
			}
		}
	} else {
		for _, chainID := range chainIDs {
			if _, alertErr := alerts.Resolve(chainID, observability.AlertPaidActionsStopped); alertErr != nil {
				return nil, processError("daemon.alert", domain.ErrorInternal, errorStartupInvalid, false, alertErr)
			}
		}
	}

	if runtimeConfig.Mode.IsLive() {
		for _, network := range prepared {
			if err := dependencies.attestNetwork(ctx, network.configured, network.manifest, runtimeConfig.ReadTimeout); err != nil {
				return nil, processError("daemon.attestation", domain.ErrorConfiguration, errorStartupInvalid, false, err)
			}
		}
	}

	startupComplete := false
	var budgetLedger budget.Ledger
	var budgetFence store.ProcessFence
	var admission *rescue.AdmissionController
	defer func() {
		if startupComplete {
			return
		}
		var cleanupError error
		for _, network := range prepared {
			if network.handoff != nil && network.lease != (store.Lease{}) {
				cleanupContext, cancel := context.WithTimeout(context.Background(), leaseReleaseTimeout)
				if err := network.handoff.Release(cleanupContext, network.lease); err != nil {
					cleanupError = errors.Join(cleanupError, processError("daemon.lease_release", domain.ErrorInternal, errorLeaseReleaseFailed, false, err))
				}
				cancel()
			}
		}
		for _, network := range prepared {
			if network.fence != nil {
				if err := network.fence.Release(); err != nil {
					cleanupError = errors.Join(cleanupError, processError("daemon.fence_release", domain.ErrorInternal, errorFenceReleaseFailed, false, err))
				}
			}
		}
		for _, network := range prepared {
			if network.handoff != nil {
				if err := network.handoff.Close(); err != nil {
					cleanupError = errors.Join(cleanupError, processError("daemon.store.close", domain.ErrorInternal, errorStoreFailed, false, err))
				}
			}
		}
		if budgetLedger != nil {
			if err := budgetLedger.Close(); err != nil {
				cleanupError = errors.Join(cleanupError, processError("daemon.budget.close", domain.ErrorInternal, errorBudgetFailed, false, err))
			}
		}
		if admission != nil {
			if err := admission.Close(); err != nil {
				cleanupError = errors.Join(cleanupError, processError("daemon.admission.close", domain.ErrorInternal, errorBudgetFailed, false, err))
			}
		}
		if budgetFence != nil {
			if err := budgetFence.Release(); err != nil {
				cleanupError = errors.Join(cleanupError, processError("daemon.budget_fence_release", domain.ErrorInternal, errorFenceReleaseFailed, false, err))
			}
		}
		resultErr = errors.Join(resultErr, cleanupError)
	}()
	if runtimeConfig.Mode.IsLive() {
		budgetFence, err = dependencies.acquireBudgetFence(runtimeConfig.SponsorAddress)
		if err != nil || budgetFence == nil {
			return nil, processError("daemon.budget_fence_acquire", domain.ErrorInternal, errorFenceAcquireFailed, false, err)
		}
		for index := range prepared {
			network := prepared[index].configured.Domain(prepared[index].manifest.Address)
			key := store.LeaseKey{Network: network.ChainID, Sponsor: runtimeConfig.SponsorAddress}
			fence, acquireErr := dependencies.acquireFence(key)
			if fence != nil {
				prepared[index].fence = fence
			}
			if acquireErr != nil || fence == nil {
				return nil, processError("daemon.fence_acquire", domain.ErrorInternal, errorFenceAcquireFailed, false, acquireErr)
			}
		}
	}
	if runtimeConfig.Mode.IsLive() {
		admissionConfig := rescue.AdmissionConfig{
			Window: runtimeConfig.Policy.AbuseWindow, RateWindow: time.Minute, RateLimit: runtimeConfig.Policy.RateLimitPerMinute,
			MaxAttemptsPerToken:       runtimeConfig.Policy.MaxAttemptsPerTokenWindow,
			MaxAttemptsPerSourceEvent: runtimeConfig.Policy.MaxAttemptsPerSourceEvent,
			MaxNewUnknownTokens:       runtimeConfig.Policy.MaxNewUnknownTokensPerWindow,
			Capacity:                  int(maxPendingCandidates) * len(prepared),
		}
		admission, err = dependencies.openAdmission(runtimeConfig, admissionConfig)
		if err != nil || admission == nil {
			return nil, processError("daemon.admission", domain.ErrorInternal, errorBudgetFailed, false, err)
		}
	}
	for index := range prepared {
		network := prepared[index].configured.Domain(prepared[index].manifest.Address)
		handoff, openErr := dependencies.openStore(runtimeConfig, prepared[index].configured, network)
		if openErr != nil || handoff == nil {
			return nil, processError("daemon.store", domain.ErrorInternal, errorStoreFailed, false, openErr)
		}
		prepared[index].handoff = handoff
	}
	if runtimeConfig.Mode.IsLive() {
		openedBudget, openErr := dependencies.openBudget(runtimeConfig, prepared)
		if openErr != nil || openedBudget == nil {
			return nil, processError("daemon.budget", domain.ErrorInternal, errorBudgetFailed, false, openErr)
		}
		budgetLedger = openedBudget
		snapshot, snapshotErr := budgetLedger.Snapshot(ctx)
		if snapshotErr != nil {
			return nil, processError("daemon.budget_snapshot", domain.ErrorInternal, errorBudgetFailed, false, snapshotErr)
		}
		metrics.SetGlobalBudget(snapshot.Global.Spent.Cumulative, snapshot.Global.Reserved.Cumulative, snapshot.Global.Remaining.Cumulative)
		for chainID, network := range snapshot.Networks {
			if metricsErr := metrics.SetBudget(chainID, network.Spent.Cumulative, network.Reserved.Cumulative, network.Remaining.Cumulative); metricsErr != nil {
				return nil, processError("daemon.budget_metrics", domain.ErrorInternal, errorBudgetFailed, false, metricsErr)
			}
			blocked := snapshot.Global.Blocked() || network.Blocked()
			if healthErr := health.SetCondition(chainID, observability.ConditionBudgetBlocked, blocked); healthErr != nil {
				return nil, processError("daemon.budget_health", domain.ErrorInternal, errorBudgetFailed, false, healthErr)
			}
			if blocked {
				if _, alertErr := alerts.Raise(chainID, observability.AlertBudgetBlocked); alertErr != nil {
					return nil, processError("daemon.budget_alert", domain.ErrorInternal, errorBudgetFailed, false, alertErr)
				}
			} else if _, alertErr := alerts.Resolve(chainID, observability.AlertBudgetBlocked); alertErr != nil {
				return nil, processError("daemon.budget_alert", domain.ErrorInternal, errorBudgetFailed, false, alertErr)
			}
		}
	}
	if runtimeConfig.Mode.IsLive() {
		owner, ownerErr := newProcessLeaseOwner()
		if ownerErr != nil {
			return nil, processError("daemon.lease_owner", domain.ErrorInternal, errorLeaseAcquireFailed, false, ownerErr)
		}
		for index := range prepared {
			network := prepared[index].configured.Domain(prepared[index].manifest.Address)
			key := store.LeaseKey{
				Network: network.ChainID,
				Sponsor: runtimeConfig.SponsorAddress,
			}
			lease, acquireErr := prepared[index].handoff.Acquire(ctx, key, owner, processLeaseTTL)
			if lease != (store.Lease{}) {
				prepared[index].lease = lease
			}
			if acquireErr != nil || lease.Key != key || lease.Owner != owner || !lease.ExpiresAt.After(dependencies.serviceClock.Now()) {
				return nil, processError("daemon.lease_acquire", domain.ErrorInternal, errorLeaseAcquireFailed, false, acquireErr)
			}
		}
	}

	var authorizer rescue.AuthorizationSigner
	var transactioner rescue.TransactionSigner
	var stoppedBroadcaster rpc.Broadcaster
	if runtimeConfig.Mode.IsLive() && !runtimeConfig.Policy.EmergencyStop {
		authorizer, transactioner, err = dependencies.newSigners(runtimeConfig.LiveSecrets)
		if err != nil || authorizer == nil || transactioner == nil {
			return nil, processError("daemon.signers", domain.ErrorConfiguration, errorStartupInvalid, false, err)
		}
		if authorizer.Address() != runtimeConfig.SourceAddress || transactioner.Address() != runtimeConfig.SponsorAddress {
			return nil, processError("daemon.signer_address", domain.ErrorConfiguration, errorStartupInvalid, false, nil)
		}
	} else if runtimeConfig.Mode.IsLive() {
		authorizer, transactioner, stoppedBroadcaster, _ = dryrun.NewGuards(runtimeConfig.SourceAddress, runtimeConfig.SponsorAddress)
	}

	codec, err := contracts.NewERC20Codec()
	if err != nil {
		return nil, processError("daemon.erc20_codec", domain.ErrorInternal, errorStartupInvalid, false, err)
	}

	process := &daemon{
		dependencies: dependencies, networks: make([]*networkProcess, 0, len(prepared)),
		budget: budgetLedger, budgetFence: budgetFence, metrics: metrics, health: health, alerts: alerts, gate: gate,
		stateDirectory: runtimeConfig.Watch.StateDirectory, admission: admission,
	}
	for _, preparedNetwork := range prepared {
		network := preparedNetwork.configured.Domain(preparedNetwork.manifest.Address)
		var coordinator *rescue.Coordinator
		if runtimeConfig.Mode.IsLive() {
			feePolicy, nativeMinimum, tokenValues, policyErr := rescuePolicy(preparedNetwork.configured)
			if policyErr != nil {
				return nil, processError("daemon.rescue_policy", domain.ErrorConfiguration, errorStartupInvalid, false, policyErr)
			}
			coordinator, err = rescue.NewCoordinator(rescue.Config{
				Network:            network,
				Source:             runtimeConfig.SourceAddress,
				Sponsor:            runtimeConfig.SponsorAddress,
				Destination:        runtimeConfig.Destination,
				StartupTimeout:     runtimeConfig.ReadTimeout,
				FeeReadTimeout:     runtimeConfig.ReadTimeout,
				State:              preparedNetwork.handoff,
				LeaseManager:       preparedNetwork.handoff,
				Lease:              preparedNetwork.lease,
				ProcessFence:       preparedNetwork.fence,
				LeaseTTL:           processLeaseTTL,
				MaxAttempts:        rescueMaxAttempts,
				RetryDelay:         candidateRetryDelay,
				ReceiptTimeout:     rescueReceiptTimeout,
				Budget:             budgetLedger,
				Gate:               gate,
				Admission:          admission,
				FeePolicy:          feePolicy,
				NativeMinimum:      nativeMinimum,
				TrustedTokens:      append([]common.Address(nil), preparedNetwork.configured.TrustedTokens...),
				TrustedTokenValues: tokenValues,
				Metrics:            metrics,
				Alerts:             alerts,
				Health:             health,
			}, authorizer, transactioner, dependencies.serviceClock, dependencies.observer)
			if err != nil {
				return nil, processError("daemon.coordinator", domain.ErrorConfiguration, errorStartupInvalid, false, err)
			}
		}

		networkState := &networkProcess{
			configured:       preparedNetwork.configured,
			network:          network,
			mode:             runtimeConfig.Mode,
			source:           runtimeConfig.SourceAddress,
			sponsor:          runtimeConfig.SponsorAddress,
			destination:      runtimeConfig.Destination,
			readTimeout:      runtimeConfig.ReadTimeout,
			watchLookback:    runtimeConfig.Watch.LookbackBlocks,
			coordinator:      coordinator,
			handoff:          preparedNetwork.handoff,
			fence:            preparedNetwork.fence,
			codec:            codec,
			allowedTokens:    make(map[common.Address]struct{}, len(network.Tokens)),
			emergencyStopped: runtimeConfig.Policy.EmergencyStop,
			dryBroadcaster:   stoppedBroadcaster,
			metrics:          metrics,
			health:           health,
			alerts:           alerts,
		}
		for _, token := range network.Tokens {
			networkState.allowedTokens[token.Address] = struct{}{}
		}
		if network.AllowUnknownTokens {
			dependencies.observer.Record(observability.Event{
				Level: observability.LevelWarning, Code: eventUnknownTokenOptIn, ChainID: network.ChainID, NetworkName: network.Name,
			})
		}
		if runtimeConfig.Mode.IsDryRun() {
			networkState.dryAuthorizer, networkState.dryTransactioner, networkState.dryBroadcaster, networkState.dryAttempts = dryrun.NewGuards(runtimeConfig.SourceAddress, runtimeConfig.SponsorAddress)
		}
		process.networks = append(process.networks, networkState)
	}
	startupComplete = true
	return process, nil
}

func rejectRestoredSigningState(runtimeConfig config.Runtime, lstat func(string) (os.FileInfo, error)) error {
	if !runtimeConfig.Mode.IsLive() || runtimeConfig.Policy.EmergencyStop {
		return nil
	}
	_, err := lstat(restoreMarkerPath)
	if err == nil {
		return errors.New("состояние было восстановлено из резервной копии")
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.New("не удалось безопасно проверить marker восстановленного состояния")
}

func (dependencies daemonDependencies) withDefaults(mode config.Mode) (daemonDependencies, error) {
	if dependencies.lstatRestoreMarker == nil {
		dependencies.lstatRestoreMarker = os.Lstat
	}
	if dependencies.serviceClock == nil || dependencies.observer == nil || dependencies.dial == nil || dependencies.loadManifest == nil || dependencies.openStore == nil || dependencies.openQuorum == nil || dependencies.alertStatePath == nil {
		return daemonDependencies{}, errors.New("не заданы обязательные зависимости процесса")
	}
	if mode.IsLive() && (dependencies.dialSubmission == nil || dependencies.attestNetwork == nil || dependencies.newSigners == nil || dependencies.acquireFence == nil || dependencies.acquireBudgetFence == nil || dependencies.openBudget == nil || dependencies.openAdmission == nil) {
		return daemonDependencies{}, errors.New("не заданы обязательные live-зависимости процесса")
	}
	if dependencies.newWatcher == nil {
		dependencies.newWatcher = func(dependencies watcher.Dependencies) (generationRunner, error) {
			return watcher.NewService(dependencies)
		}
	}
	return dependencies, nil
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
					result.err = errors.New("network worker неожиданно остановлен")
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
			result.err = errors.New("поддержание lease неожиданно остановлено")
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

func (network *networkProcess) supervise(ctx context.Context, dependencies daemonDependencies) error {
	for generation := uint64(1); ; generation++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := network.runGeneration(ctx, generation, dependencies)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		dependencies.observer.Record(observability.Event{
			Level:       observability.LevelWarning,
			Code:        eventGenerationError,
			ChainID:     network.network.ChainID,
			NetworkName: network.network.Name,
			ErrorCode:   publicErrorCode(err),
		})
		if network.metrics != nil {
			_ = network.metrics.RecordReconnect(network.network.ChainID)
			_ = network.metrics.RecordRPCError(network.network.ChainID)
		}
		if network.health != nil {
			_ = network.health.SetCondition(network.network.ChainID, observability.ConditionRPCDegraded, true)
		}
		if network.alerts != nil {
			if _, alertErr := network.alerts.Raise(network.network.ChainID, observability.AlertRPCDegraded); alertErr != nil {
				return alertErr
			}
		}
		if err := dependencies.serviceClock.Sleep(ctx, reconnectDelay); err != nil {
			return err
		}
	}
}

func (network *networkProcess) runGeneration(ctx context.Context, generation uint64, dependencies daemonDependencies) error {
	generationContext, cancelGeneration := context.WithCancel(ctx)
	defer cancelGeneration()

	client, err := network.dialClient(generationContext, generation, dependencies)
	if err != nil {
		return err
	}
	defer client.Close()
	if client.Generation() != generation {
		return processError("daemon.client_generation", domain.ErrorInternal, errorClientInvalid, false, nil)
	}
	reader, err := rpc.NewDeadlineReader(client.Reader(), network.readTimeout)
	if err != nil {
		return processError("daemon.reader", domain.ErrorInternal, errorClientInvalid, false, err)
	}
	logSubscriber, err := rpc.NewDeadlineLogSubscriber(client.LogSubscriber(), network.readTimeout)
	if err != nil {
		return processError("daemon.logs", domain.ErrorInternal, errorClientInvalid, false, err)
	}
	headSubscriber, err := rpc.NewDeadlineHeadSubscriber(client.HeadSubscriber(), network.readTimeout)
	if err != nil {
		return processError("daemon.heads", domain.ErrorInternal, errorClientInvalid, false, err)
	}
	boundedClient := &generationClientView{
		generationClient: client,
		reader:           reader,
		logs:             logSubscriber,
		heads:            headSubscriber,
	}
	quorum, err := dependencies.openQuorum(generationContext, network.configured, network.readTimeout)
	if err != nil || quorum == nil {
		return processError("daemon.quorum", domain.ErrorRPCTransient, errorQuorumFailed, true, err)
	}
	defer quorum.Close()

	session, err := network.openSession(generationContext, boundedClient, quorum, dependencies)
	if err != nil {
		return processError("daemon.session", domain.ErrorRPCTransient, errorSessionFailed, true, err)
	}
	defer session.Close()
	if network.alerts != nil {
		if _, alertErr := network.alerts.Resolve(network.network.ChainID, observability.AlertRPCDegraded); alertErr != nil {
			return processError("daemon.rpc_alert", domain.ErrorInternal, errorSessionFailed, true, alertErr)
		}
	}
	if network.health != nil {
		_ = network.health.SetCondition(network.network.ChainID, observability.ConditionRPCDegraded, false)
	}
	if err := updateQueueDepth(generationContext, network.network.ChainID, network.handoff, network.metrics); err != nil {
		return err
	}
	watcherService, err := dependencies.newWatcher(watcher.Dependencies{
		Contracts:      reader,
		Finalized:      quorum,
		LogSubscriber:  logSubscriber,
		HeadSubscriber: headSubscriber,
		Codec:          network.codec,
		Clock:          dependencies.serviceClock,
		Observer:       dependencies.observer,
		Store:          network.handoff,
		Source:         network.source,
		Network:        network.network,
		Generation:     generation,
		LookbackBlocks: network.watchLookback,
		ReadTimeout:    network.readTimeout,
	})
	if err != nil {
		return processError("daemon.watcher", domain.ErrorInternal, errorWatcherFailed, false, err)
	}

	workerCount := 2
	reconciliation := reconciliationWorker(network.mode, session)
	if reconciliation != nil {
		workerCount++
	}
	results := make(chan error, workerCount)
	go func() {
		results <- watcherService.Run(generationContext)
	}()
	go func() {
		results <- consumeCandidates(generationContext, network.network.ChainID, network.handoff, network.handoff, session, dependencies.serviceClock, network.metrics)
	}()
	if reconciliation != nil {
		go func() {
			results <- reconciliation.RunReconciliation(generationContext)
		}()
	}

	first := <-results
	cancelGeneration()
	workerErrors := []error{first}
	for range workerCount - 1 {
		workerErrors = append(workerErrors, <-results)
	}
	return generationError(workerErrors...)
}

func reconciliationWorker(mode config.Mode, session candidateSession) reconciliationSession {
	if !mode.IsLive() {
		return nil
	}
	reconciliation, _ := session.(reconciliationSession)
	return reconciliation
}

func (network *networkProcess) openSession(ctx context.Context, client generationClient, quorum runtimeQuorum, dependencies daemonDependencies) (candidateSession, error) {
	if network.mode.IsDryRun() {
		session, err := dryrun.NewSession(ctx, client.Generation(), client.Reader(), dryrun.Config{
			Network:     network.network.ChainID,
			Source:      network.source,
			Sponsor:     network.sponsor,
			Destination: network.destination,
			Rescuer:     network.network.Rescuer,
			ReadTimeout: network.readTimeout,
		})
		if err != nil {
			return nil, err
		}
		return network.bindTokenPolicy(session), nil
	}
	if dependencies.newSession != nil {
		if err := network.validateProcessFence(); err != nil {
			return nil, err
		}
		session, err := dependencies.newSession(ctx, network, client, quorum)
		if err != nil {
			return nil, err
		}
		if session == nil {
			return nil, errors.New("session factory вернула пустой результат")
		}
		return network.bindTokenPolicy(session), nil
	}
	if network.emergencyStopped {
		session, err := network.coordinator.NewSession(ctx, client.Generation(), client.Reader(), quorum, network.dryBroadcaster)
		if err != nil {
			return nil, err
		}
		return network.bindTokenPolicy(&guardedCandidateSession{Session: session}), nil
	}

	dialContext, cancel := context.WithTimeout(ctx, network.readTimeout)
	submission, err := dependencies.dialSubmission(dialContext, network.configured.BroadcastHTTP)
	cancel()
	if err != nil || submission == nil {
		if submission != nil {
			submission.Close()
		}
		return nil, err
	}
	boundedBroadcaster, err := rpc.NewDeadlineBroadcaster(submission, network.readTimeout)
	if err != nil {
		submission.Close()
		return nil, err
	}
	if err := network.validateProcessFence(); err != nil {
		submission.Close()
		return nil, err
	}
	session, err := network.coordinator.NewSession(ctx, client.Generation(), client.Reader(), quorum, boundedBroadcaster)
	if err != nil {
		submission.Close()
		return nil, err
	}
	return network.bindTokenPolicy(&liveCandidateSession{Session: session, submission: submission}), nil
}

func (network *networkProcess) validateProcessFence() error {
	if network.fence == nil {
		return processError("daemon.fence_validate", domain.ErrorInternal, errorFenceValidateFailed, false, store.ErrFenceLost)
	}
	if err := network.fence.Validate(); err != nil {
		return processError("daemon.fence_validate", domain.ErrorInternal, errorFenceValidateFailed, false, err)
	}
	return nil
}

func (network *networkProcess) bindTokenPolicy(session candidateSession) candidateSession {
	policySession := &tokenPolicySession{
		candidateSession: session,
		allowed:          network.allowedTokens,
		allowUnknown:     network.network.AllowUnknownTokens,
	}
	if reconciliation, ok := session.(reconciliationSession); ok {
		return &reconcilingTokenPolicySession{tokenPolicySession: policySession, reconciliationSession: reconciliation}
	}
	return policySession
}

func (network *networkProcess) dialClient(ctx context.Context, generation uint64, dependencies daemonDependencies) (generationClient, error) {
	endpoints := make([]string, 0, len(network.configured.ReadProviders)*2)
	for _, provider := range network.configured.ReadProviders {
		endpoints = append(endpoints, provider.WSURL)
	}
	webSocketCount := len(endpoints)
	for _, provider := range network.configured.ReadProviders {
		endpoints = append(endpoints, provider.HTTPURL)
	}
	if len(endpoints) == 0 {
		return nil, processError("daemon.rpc_dial", domain.ErrorConfiguration, errorDialFailed, false, nil)
	}
	start := int((generation - 1) % uint64(len(endpoints)))
	var lastError error
	httpFallbackRecorded := false
	for attempt := range endpoints {
		index := (start + attempt) % len(endpoints)
		endpoint := endpoints[index]
		if index >= webSocketCount && !httpFallbackRecorded {
			dependencies.observer.Record(observability.Event{
				Level:       observability.LevelWarning,
				Code:        eventHTTPFallback,
				ChainID:     network.network.ChainID,
				NetworkName: network.network.Name,
				ErrorCode:   publicErrorCode(lastError),
			})
			httpFallbackRecorded = true
		}
		dialContext, cancelDial := context.WithTimeout(ctx, network.readTimeout)
		client, err := dependencies.dial(dialContext, endpoint, generation)
		cancelDial()
		if err == nil && client != nil {
			return client, nil
		}
		if client != nil {
			client.Close()
		}
		lastError = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, processError("daemon.rpc_dial", domain.ErrorRPCTransient, errorDialFailed, true, lastError)
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
			// The stable queued ID remains unchanged while replay is bound to the
			// current immutable RPC generation.
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
