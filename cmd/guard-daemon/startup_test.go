package main

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"guard-daemon/internal/clock"
	"guard-daemon/internal/config"
	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rescue"
	"guard-daemon/internal/rescue/dryrun"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestDryRunStartupHasNoProductionSigningOrBroadcastGraph(t *testing.T) {
	production := newProductionDependencies(observability.Discard{}, config.ModeDryRun)
	if production.newSigners != nil || production.dialSubmission != nil || production.attestNetwork != nil || production.acquireFence != nil {
		t.Fatal("dry-run production dependencies содержат live capability")
	}

	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("dry-network", 401)})
	runtimeConfig.Mode = config.ModeDryRun
	var networkCalls atomic.Int32
	dependencies := daemonDependencies{
		serviceClock: clock.Real{},
		observer:     observability.Discard{},
		dial: func(context.Context, string, uint64) (generationClient, error) {
			networkCalls.Add(1)
			return nil, errors.New("неожиданный network access")
		},
		loadManifest: func(config.Runtime, config.Network) (contracts.DeploymentManifest, error) {
			return contracts.DeploymentManifest{Address: testProcessAddress(4)}, nil
		},
	}
	dependencies = completeTestRuntimeDependencies(dependencies)
	var fenceAcquisitions atomic.Int32
	dependencies.acquireFence = func(store.LeaseKey) (store.ProcessFence, error) {
		fenceAcquisitions.Add(1)
		return &testProcessFence{}, nil
	}

	process, err := newDaemon(context.Background(), runtimeConfig, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if networkCalls.Load() != 0 {
		t.Fatalf("network calls during dry startup = %d, нужно 0", networkCalls.Load())
	}
	if len(process.networks) != 1 {
		t.Fatalf("network process count = %d, нужно 1", len(process.networks))
	}
	network := process.networks[0]
	if network.coordinator != nil || network.dryAuthorizer == nil || network.dryTransactioner == nil || network.dryBroadcaster == nil || network.dryAttempts == nil {
		t.Fatal("dry-run graph содержит coordinator или не содержит fail-closed guards")
	}
	handoff := network.handoff.(*legacyMemoryHandoff)
	handoff.mu.Lock()
	leaseCount := len(handoff.leases)
	handoff.mu.Unlock()
	if leaseCount != 0 {
		t.Fatalf("dry-run startup получил leases: %d", leaseCount)
	}
	if fenceAcquisitions.Load() != 0 || network.fence != nil {
		t.Fatalf("dry-run startup получил process fence: acquisitions=%d fence=%v", fenceAcquisitions.Load(), network.fence)
	}
	if network.dryAttempts.AuthorizationSignatures() != 0 || network.dryAttempts.TransactionSignatures() != 0 || network.dryAttempts.Broadcasts() != 0 {
		t.Fatal("dry-run startup вызвал signing или broadcast guard")
	}
}

func TestLiveProductionDependenciesUseStoreProcessFence(t *testing.T) {
	production := newProductionDependencies(observability.Discard{}, config.ModeLive)
	if production.acquireFence == nil || reflect.ValueOf(production.acquireFence).Pointer() != reflect.ValueOf(store.AcquireProcessFence).Pointer() {
		t.Fatal("live production dependencies do not use store.AcquireProcessFence")
	}
}

func TestDaemonPassesAcquiredFenceToCoordinator(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("coordinator-fence", 403)})
	dependencies := startupDependencies(t)
	var validations atomic.Int32
	fence := &testProcessFence{onValidate: func() { validations.Add(1) }}
	dependencies.acquireFence = func(store.LeaseKey) (store.ProcessFence, error) { return fence, nil }
	process, err := newDaemon(context.Background(), runtimeConfig, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if process.networks[0].coordinator == nil || validations.Load() != 1 {
		t.Fatalf("coordinator process fence validations = %d", validations.Load())
	}
	if err := process.shutdown(nil); err != nil {
		t.Fatal(err)
	}
}

func TestEmergencyStopBuildsReadOnlyLiveGraphWithoutPrivateSigners(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("stopped-network", 404)})
	runtimeConfig.Policy.EmergencyStop = true
	dependencies := startupDependencies(t)
	var signerCalls atomic.Int32
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		signerCalls.Add(1)
		return nil, nil, errors.New("private signer не должен создаваться")
	}
	process, err := newDaemon(context.Background(), runtimeConfig, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if signerCalls.Load() != 0 || !process.gate.Stopped() || process.health.Snapshot().State != observability.HealthStopped || process.networks[0].coordinator == nil {
		t.Fatalf("stopped graph: signerCalls=%d gate=%t health=%s coordinator=%v", signerCalls.Load(), process.gate.Stopped(), process.health.Snapshot().State, process.networks[0].coordinator)
	}
	if err := process.shutdown(nil); err != nil {
		t.Fatal(err)
	}
}

func TestProductionOpenStoreBindsConfiguredRoles(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("bound-store", 402)})
	runtimeConfig.Watch.StateDirectory = t.TempDir()
	configured := runtimeConfig.Networks[0]
	network := configured.Domain(testProcessAddress(4))
	production := newProductionDependencies(observability.Discard{}, config.ModeDryRun)
	handoff, err := production.openStore(runtimeConfig, configured, network)
	if err != nil {
		t.Fatal(err)
	}
	if err := handoff.Close(); err != nil {
		t.Fatal(err)
	}

	network.Rescuer = testProcessAddress(5)
	if reopened, err := production.openStore(runtimeConfig, configured, network); err == nil {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("openStore() accepted a different configured rescuer: store=%v error=%v", reopened, err)
	}
}

func TestLiveStartupAttestsEveryNetworkBeforeConstructingSigners(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{
		testNetwork("first-network", 501),
		testNetwork("second-network", 502),
	})
	var events []string
	dependencies := startupDependencies(t)
	dependencies.loadManifest = func(_ config.Runtime, network config.Network) (contracts.DeploymentManifest, error) {
		events = append(events, "manifest:"+network.Name)
		return contracts.DeploymentManifest{Address: testProcessAddress(byte(network.ChainID))}, nil
	}
	dependencies.attestNetwork = func(_ context.Context, network config.Network, _ contracts.DeploymentManifest, timeout time.Duration) error {
		if timeout != runtimeConfig.ReadTimeout {
			t.Fatalf("attestation timeout = %s, нужен %s", timeout, runtimeConfig.ReadTimeout)
		}
		events = append(events, "attest:"+network.Name)
		return nil
	}
	dependencies.openStore = func(_ config.Runtime, configured config.Network, network domain.Network) (store.HandoffStore, error) {
		events = append(events, "store:"+configured.Name)
		return &startupLifecycleHandoff{
			legacyMemoryHandoff: newLegacyMemoryHandoff(network.ChainID, dependencies.serviceClock),
			events:              &events,
			name:                configured.Name,
		}, nil
	}
	dependencies.acquireFence = func(key store.LeaseKey) (store.ProcessFence, error) {
		for _, network := range runtimeConfig.Networks {
			if network.ChainID == key.Network {
				events = append(events, "fence:"+network.Name)
				return &testProcessFence{}, nil
			}
		}
		return nil, errors.New("unknown test network")
	}
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		events = append(events, "signers")
		return testPrivateKeySigners(t)
	}

	if _, err := newDaemon(context.Background(), runtimeConfig, dependencies); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"manifest:first-network",
		"manifest:second-network",
		"attest:first-network",
		"attest:second-network",
		"fence:first-network",
		"fence:second-network",
		"store:first-network",
		"store:second-network",
		"lease:first-network",
		"lease:second-network",
		"signers",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("startup order = %v, нужен %v", events, want)
	}
}

func TestProcessFenceContentionBlocksStoreAndSignerConstruction(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("fence-contended-network", 649)})
	dependencies := startupDependencies(t)
	var storeOpens atomic.Int32
	var signerConstructions atomic.Int32
	dependencies.acquireFence = func(store.LeaseKey) (store.ProcessFence, error) {
		return nil, store.ErrFenceHeld
	}
	dependencies.openStore = func(config.Runtime, config.Network, domain.Network) (store.HandoffStore, error) {
		storeOpens.Add(1)
		return newLegacyMemoryHandoff(runtimeConfig.Networks[0].ChainID, dependencies.serviceClock), nil
	}
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		signerConstructions.Add(1)
		return testPrivateKeySigners(t)
	}

	_, err := newDaemon(context.Background(), runtimeConfig, dependencies)
	if publicErrorCode(err) != errorFenceAcquireFailed || !errors.Is(err, store.ErrFenceHeld) {
		t.Fatalf("fence contention error = %v", err)
	}
	if storeOpens.Load() != 0 || signerConstructions.Load() != 0 {
		t.Fatalf("fence contention side effects: store opens=%d signer constructions=%d", storeOpens.Load(), signerConstructions.Load())
	}
}

func TestStateFailureBlocksSignerConstruction(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("state-network", 650)})
	dependencies := startupDependencies(t)
	var signerConstructions atomic.Int32
	dependencies.openStore = func(config.Runtime, config.Network, domain.Network) (store.HandoffStore, error) {
		return nil, errors.New("private state path detail")
	}
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		signerConstructions.Add(1)
		return testPrivateKeySigners(t)
	}

	err := func() error {
		_, err := newDaemon(context.Background(), runtimeConfig, dependencies)
		return err
	}()
	if publicErrorCode(err) != errorStoreFailed || signerConstructions.Load() != 0 {
		t.Fatalf("state failure: error=%v signer constructions=%d", err, signerConstructions.Load())
	}
}

func TestLeaseContentionBlocksSignerConstruction(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("contended-network", 651)})
	dependencies := startupDependencies(t)
	handoff := newLeaseLifecycleHandoff(runtimeConfig.Networks[0].ChainID, dependencies.serviceClock)
	key := store.LeaseKey{Network: runtimeConfig.Networks[0].ChainID, Sponsor: runtimeConfig.SponsorAddress}
	if _, err := handoff.legacyMemoryHandoff.Acquire(context.Background(), key, "другой-тестовый-процесс", processLeaseTTL); err != nil {
		t.Fatal(err)
	}
	dependencies.openStore = func(config.Runtime, config.Network, domain.Network) (store.HandoffStore, error) {
		return handoff, nil
	}
	var signerConstructions atomic.Int32
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		signerConstructions.Add(1)
		return testPrivateKeySigners(t)
	}

	_, err := newDaemon(context.Background(), runtimeConfig, dependencies)
	if publicErrorCode(err) != errorLeaseAcquireFailed {
		t.Fatalf("contention error = %v", err)
	}
	if signerConstructions.Load() != 0 {
		t.Fatalf("signer constructions = %d, нужен 0", signerConstructions.Load())
	}
	if got := handoff.snapshot(); !reflect.DeepEqual(got, []string{"acquire", "close"}) {
		t.Fatalf("contention lifecycle = %v", got)
	}
}

func TestStartupFailureReleasesAcquiredLeaseBeforeClosingStore(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("rollback-network", 652)})
	dependencies := startupDependencies(t)
	handoff := newLeaseLifecycleHandoff(runtimeConfig.Networks[0].ChainID, dependencies.serviceClock)
	dependencies.openStore = func(config.Runtime, config.Network, domain.Network) (store.HandoffStore, error) {
		return handoff, nil
	}
	dependencies.acquireFence = func(store.LeaseKey) (store.ProcessFence, error) {
		handoff.record("fence-acquire")
		return &testProcessFence{onRelease: func() { handoff.record("fence-release") }}, nil
	}
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		return nil, nil, errors.New("тестовая ошибка создания signer")
	}

	if _, err := newDaemon(context.Background(), runtimeConfig, dependencies); err == nil {
		t.Fatal("newDaemon() не вернул ошибку signer")
	}
	if got := handoff.snapshot(); !reflect.DeepEqual(got, []string{"fence-acquire", "acquire", "release", "fence-release", "close"}) {
		t.Fatalf("startup rollback lifecycle = %v", got)
	}
	if err := handoff.lastReleaseError(); err != nil {
		t.Fatalf("startup rollback release error = %v", err)
	}
}

func TestStartupSurfacesRedactedFenceCleanupFailure(t *testing.T) {
	const privateDetail = "private-fence-cleanup-detail"
	cleanupFailure := errors.New(privateDetail)
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("cleanup-failure", 653)})
	dependencies := startupDependencies(t)
	dependencies.acquireFence = func(store.LeaseKey) (store.ProcessFence, error) {
		return &testProcessFence{releaseError: cleanupFailure}, nil
	}
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		return nil, nil, errors.New("force startup rollback")
	}

	_, err := newDaemon(context.Background(), runtimeConfig, dependencies)
	if !errors.Is(err, cleanupFailure) {
		t.Fatalf("startup error does not contain fence cleanup failure: %v", err)
	}
	if strings.Contains(err.Error(), privateDetail) {
		t.Fatalf("startup error exposed cleanup detail: %v", err)
	}
}

func TestAttestationFailureBlocksSignerConstruction(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{
		testNetwork("first-network", 601),
		testNetwork("second-network", 602),
	})
	dependencies := startupDependencies(t)
	var attestations atomic.Int32
	var signerConstructions atomic.Int32
	dependencies.attestNetwork = func(context.Context, config.Network, contracts.DeploymentManifest, time.Duration) error {
		if attestations.Add(1) == 2 {
			return errors.New("test-only quorum disagreement")
		}
		return nil
	}
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		signerConstructions.Add(1)
		return testPrivateKeySigners(t)
	}

	if _, err := newDaemon(context.Background(), runtimeConfig, dependencies); err == nil {
		t.Fatal("newDaemon() не отклонил расхождение quorum")
	}
	if attestations.Load() != 2 || signerConstructions.Load() != 0 {
		t.Fatalf("attestations=%d signer constructions=%d", attestations.Load(), signerConstructions.Load())
	}
}

func TestSignerAddressMismatchFailsBeforeSigningOrBroadcast(t *testing.T) {
	tests := []struct {
		name          string
		authorizer    commonAddressSelector
		transactioner commonAddressSelector
	}{
		{name: "source", authorizer: wrongAddress, transactioner: sponsorAddress},
		{name: "sponsor", authorizer: sourceAddress, transactioner: wrongAddress},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtimeConfig := testRuntime(t, []domain.Network{testNetwork("mismatch-network", 701)})
			dependencies := startupDependencies(t)
			var broadcastDials atomic.Int32
			dependencies.dialSubmission = func(context.Context, string) (submissionClient, error) {
				broadcastDials.Add(1)
				return &stubSubmissionClient{}, nil
			}
			var attempts *dryrun.Attempts
			dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
				authorizer, transactioner, _, guardAttempts := dryrun.NewGuards(
					test.authorizer(runtimeConfig),
					test.transactioner(runtimeConfig),
				)
				attempts = guardAttempts
				return authorizer, transactioner, nil
			}

			if _, err := newDaemon(context.Background(), runtimeConfig, dependencies); err == nil {
				t.Fatal("newDaemon() не отклонил адрес signer")
			}
			if attempts == nil || attempts.AuthorizationSignatures() != 0 || attempts.TransactionSignatures() != 0 || attempts.Broadcasts() != 0 || broadcastDials.Load() != 0 {
				t.Fatalf("mismatch вызвал side effect: attempts=%v broadcast dials=%d", attempts, broadcastDials.Load())
			}
		})
	}
}

func TestLiveSessionUsesSeparateConfiguredBroadcastRPC(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("separate-broadcast", 801)})
	dependencies := startupDependencies(t)
	var attempts *dryrun.Attempts
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		authorizer, transactioner, _, guardAttempts := dryrun.NewGuards(runtimeConfig.SourceAddress, runtimeConfig.SponsorAddress)
		attempts = guardAttempts
		return authorizer, transactioner, nil
	}
	submission := &countingSubmissionClient{}
	var dialedEndpoint string
	dependencies.dialSubmission = func(_ context.Context, endpoint string) (submissionClient, error) {
		dialedEndpoint = endpoint
		return submission, nil
	}
	process, err := newDaemon(context.Background(), runtimeConfig, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	network := process.networks[0]
	reader := &startupGenerationReader{
		chainID:     big.NewInt(int64(network.network.ChainID)),
		destination: runtimeConfig.Destination,
		delegatedTo: network.network.Rescuer,
	}
	client := &readGenerationClient{generation: 1, reader: reader}
	quorum := &startupRuntimeQuorum{destination: runtimeConfig.Destination}
	session, err := network.openSession(context.Background(), client, quorum, process.dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if dialedEndpoint != network.configured.BroadcastHTTP {
		t.Fatalf("broadcast endpoint = %q, нужен configured override", dialedEndpoint)
	}
	if attempts == nil || attempts.AuthorizationSignatures() != 0 || attempts.TransactionSignatures() != 0 || submission.broadcasts.Load() != 0 {
		t.Fatal("создание live session вызвало подпись или отправку при активной делегации")
	}
	session.Close()
	if submission.closed.Load() != 1 {
		t.Fatalf("broadcast client close count = %d, нужен 1", submission.closed.Load())
	}
}

func TestConfiguredAttestationUsesBothHTTPOverridesAndProviderIdentity(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("attested-network", 901)})
	network := runtimeConfig.Networks[0]
	clients := []*fakeAttestationClient{{}, {}}
	var endpoints []string
	dial := func(ctx context.Context, endpoint string) (attestationClient, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("attestation dial не получил deadline")
		}
		endpoints = append(endpoints, endpoint)
		return clients[len(endpoints)-1], nil
	}
	attestCalls := 0
	err := attestConfiguredNetworkWith(
		context.Background(),
		network,
		contracts.DeploymentManifest{Address: testProcessAddress(4)},
		runtimeConfig.ReadTimeout,
		dial,
		func(_ context.Context, _ contracts.DeploymentManifest, providers []contracts.ReadProvider, timeout time.Duration) error {
			attestCalls++
			if timeout != runtimeConfig.ReadTimeout || len(providers) != 2 {
				t.Fatalf("attestation input: timeout=%s providers=%d", timeout, len(providers))
			}
			for index, provider := range providers {
				configured := network.ReadProviders[index]
				if provider.ID != configured.ID || provider.EndpointFingerprint != configured.EndpointFingerprint || provider.TrustDomain != configured.TrustDomain || provider.Reader != clients[index] {
					t.Fatalf("provider %d не совпал с config override", index)
				}
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	wantEndpoints := []string{network.ReadProviders[0].HTTPURL, network.ReadProviders[1].HTTPURL}
	if !reflect.DeepEqual(endpoints, wantEndpoints) || attestCalls != 1 {
		t.Fatalf("attestation endpoints=%v calls=%d", endpoints, attestCalls)
	}
	for index, client := range clients {
		if client.closed.Load() != 1 {
			t.Fatalf("attestation client %d close count = %d", index, client.closed.Load())
		}
	}
}

func TestDryRunSessionPlansWithoutCallingGuards(t *testing.T) {
	runtimeConfig := testRuntime(t, []domain.Network{testNetwork("dry-plan", 902)})
	runtimeConfig.Mode = config.ModeDryRun
	allowedToken := testProcessAddress(9)
	runtimeConfig.Networks[0].Tokens = []domain.Token{{Address: allowedToken}}
	dependencies := daemonDependencies{
		serviceClock: clock.Real{},
		observer:     observability.Discard{},
		dial: func(context.Context, string, uint64) (generationClient, error) {
			return nil, errors.New("unexpected watcher dial")
		},
		loadManifest: func(config.Runtime, config.Network) (contracts.DeploymentManifest, error) {
			return contracts.DeploymentManifest{Address: testProcessAddress(4)}, nil
		},
		newSession: func(context.Context, *networkProcess, generationClient, runtimeQuorum) (candidateSession, error) {
			t.Fatal("dry run вызвал injected session factory")
			return nil, nil
		},
	}
	dependencies = completeTestRuntimeDependencies(dependencies)
	process, err := newDaemon(context.Background(), runtimeConfig, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	network := process.networks[0]
	reader := &startupGenerationReader{chainID: big.NewInt(int64(network.network.ChainID))}
	session, err := network.openSession(context.Background(), &readGenerationClient{generation: 3, reader: reader}, nil, process.dependencies)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	candidate := domain.RescueCandidate{
		Network: network.network.ChainID,
		Source:  runtimeConfig.SourceAddress,
		Kind:    domain.CandidateToken,
		Token:   domain.Token{Address: allowedToken},
	}
	if err := session.Handle(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if len(reader.estimates) != 1 {
		t.Fatalf("EstimateGas calls = %d, нужен 1", len(reader.estimates))
	}
	if network.dryAttempts.AuthorizationSignatures() != 0 || network.dryAttempts.TransactionSignatures() != 0 || network.dryAttempts.Broadcasts() != 0 {
		t.Fatal("обычный dry-run путь вызвал signing или broadcast guard")
	}
}

func TestSessionTokenPolicyRejectsInjectedCandidateBeforeHandler(t *testing.T) {
	allowed := testProcessAddress(10)
	network := &networkProcess{
		network:       domain.Network{AllowUnknownTokens: false},
		allowedTokens: map[common.Address]struct{}{allowed: {}},
	}
	base := &countingCandidateSession{generation: 1}
	session := network.bindTokenPolicy(base)
	err := session.Handle(context.Background(), domain.RescueCandidate{
		Kind:  domain.CandidateToken,
		Token: domain.Token{Address: testProcessAddress(11)},
	})
	if publicErrorCode(err) != errorTokenForbidden || base.handles.Load() != 0 {
		t.Fatalf("forbidden candidate: error=%v handler calls=%d", err, base.handles.Load())
	}

	network.network.AllowUnknownTokens = true
	base = &countingCandidateSession{generation: 1}
	session = network.bindTokenPolicy(base)
	if err := session.Handle(context.Background(), domain.RescueCandidate{Kind: domain.CandidateToken, Token: domain.Token{Address: testProcessAddress(11)}}); err != nil {
		t.Fatal(err)
	}
	if base.handles.Load() != 1 {
		t.Fatalf("all-token handler calls = %d, нужен 1", base.handles.Load())
	}
}

type commonAddressSelector func(config.Runtime) common.Address

func sourceAddress(runtimeConfig config.Runtime) common.Address  { return runtimeConfig.SourceAddress }
func sponsorAddress(runtimeConfig config.Runtime) common.Address { return runtimeConfig.SponsorAddress }
func wrongAddress(config.Runtime) common.Address                 { return testProcessAddress(0xfe) }

func startupDependencies(t *testing.T) daemonDependencies {
	t.Helper()
	dependencies := daemonDependencies{
		serviceClock: clock.Real{},
		observer:     observability.Discard{},
		dial: func(context.Context, string, uint64) (generationClient, error) {
			return nil, errors.New("неожиданный watcher dial")
		},
		loadManifest: func(config.Runtime, config.Network) (contracts.DeploymentManifest, error) {
			return contracts.DeploymentManifest{Address: testProcessAddress(4)}, nil
		},
		attestNetwork: func(context.Context, config.Network, contracts.DeploymentManifest, time.Duration) error {
			return nil
		},
		dialSubmission: func(context.Context, string) (submissionClient, error) {
			return &stubSubmissionClient{}, nil
		},
	}
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		return testPrivateKeySigners(t)
	}
	return completeTestRuntimeDependencies(dependencies, t)
}

func testPrivateKeySigners(t *testing.T) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
	t.Helper()
	authorizer, err := rescue.NewPrivateKeyAuthorizationSigner(deterministicKey(t, 1))
	if err != nil {
		return nil, nil, err
	}
	transactioner, err := rescue.NewPrivateKeyTransactionSigner(deterministicKey(t, 2))
	return authorizer, transactioner, err
}

type countingSubmissionClient struct {
	broadcasts atomic.Int32
	closed     atomic.Int32
}

func (client *countingSubmissionClient) SendTransaction(context.Context, *types.Transaction) error {
	client.broadcasts.Add(1)
	return nil
}

func (client *countingSubmissionClient) Close() { client.closed.Add(1) }

type countingCandidateSession struct {
	generation uint64
	handles    atomic.Int32
}

func (session *countingCandidateSession) Generation() uint64 { return session.generation }
func (session *countingCandidateSession) Handle(context.Context, domain.RescueCandidate) error {
	session.handles.Add(1)
	return nil
}
func (*countingCandidateSession) Close() {}

type startupRuntimeQuorum struct {
	inertRuntimeQuorum
	destination common.Address
}

func (*startupRuntimeQuorum) Finalized(context.Context) (rpc.BlockRef, error) {
	return rpc.BlockRef{Number: 1, Hash: common.Hash{31: 1}}, nil
}

func (quorum *startupRuntimeQuorum) CallContract(context.Context, rpc.BlockRef, ethereum.CallMsg) ([]byte, error) {
	result := make([]byte, 32)
	copy(result[12:], quorum.destination[:])
	return result, nil
}

type startupLifecycleHandoff struct {
	*legacyMemoryHandoff
	events *[]string
	name   string
}

func (handoff *startupLifecycleHandoff) Acquire(ctx context.Context, key store.LeaseKey, owner string, ttl time.Duration) (store.Lease, error) {
	*handoff.events = append(*handoff.events, "lease:"+handoff.name)
	return handoff.legacyMemoryHandoff.Acquire(ctx, key, owner, ttl)
}

type leaseLifecycleHandoff struct {
	*legacyMemoryHandoff
	mu             sync.Mutex
	events         []string
	releaseResults []error
	releaseError   error
}

func newLeaseLifecycleHandoff(network domain.NetworkID, serviceClock clock.Clock) *leaseLifecycleHandoff {
	return &leaseLifecycleHandoff{legacyMemoryHandoff: newLegacyMemoryHandoff(network, serviceClock)}
}

func (handoff *leaseLifecycleHandoff) Acquire(ctx context.Context, key store.LeaseKey, owner string, ttl time.Duration) (store.Lease, error) {
	handoff.record("acquire")
	return handoff.legacyMemoryHandoff.Acquire(ctx, key, owner, ttl)
}

func (handoff *leaseLifecycleHandoff) Release(ctx context.Context, lease store.Lease) error {
	handoff.record("release")
	err := handoff.releaseError
	if err == nil {
		err = handoff.legacyMemoryHandoff.Release(ctx, lease)
	}
	handoff.mu.Lock()
	handoff.releaseResults = append(handoff.releaseResults, err)
	handoff.mu.Unlock()
	return err
}

func (handoff *leaseLifecycleHandoff) Close() error {
	handoff.record("close")
	return handoff.legacyMemoryHandoff.Close()
}

func (handoff *leaseLifecycleHandoff) record(event string) {
	handoff.mu.Lock()
	handoff.events = append(handoff.events, event)
	handoff.mu.Unlock()
}

func (handoff *leaseLifecycleHandoff) snapshot() []string {
	handoff.mu.Lock()
	defer handoff.mu.Unlock()
	return append([]string(nil), handoff.events...)
}

func (handoff *leaseLifecycleHandoff) lastReleaseError() error {
	handoff.mu.Lock()
	defer handoff.mu.Unlock()
	if len(handoff.releaseResults) == 0 {
		return errors.New("release не вызывался")
	}
	return handoff.releaseResults[len(handoff.releaseResults)-1]
}

func (handoff *leaseLifecycleHandoff) currentLease(key store.LeaseKey) store.Lease {
	handoff.legacyMemoryHandoff.mu.Lock()
	defer handoff.legacyMemoryHandoff.mu.Unlock()
	return handoff.legacyMemoryHandoff.leases[key].lease
}

type readGenerationClient struct {
	generation uint64
	reader     rpc.Reader
}

func (client *readGenerationClient) Generation() uint64          { return client.generation }
func (client *readGenerationClient) Reader() rpc.Reader          { return client.reader }
func (*readGenerationClient) LogSubscriber() rpc.LogSubscriber   { return nil }
func (*readGenerationClient) HeadSubscriber() rpc.HeadSubscriber { return nil }
func (*readGenerationClient) Close()                             {}

type startupGenerationReader struct {
	chainID     *big.Int
	destination common.Address
	delegatedTo common.Address
	estimates   []ethereum.CallMsg
}

func (reader *startupGenerationReader) ChainID(context.Context) (*big.Int, error) {
	return new(big.Int).Set(reader.chainID), nil
}

func (*startupGenerationReader) BlockNumber(context.Context) (uint64, error) {
	return 0, errors.New("unexpected call")
}

func (*startupGenerationReader) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return nil, errors.New("unexpected call")
}

func (*startupGenerationReader) BalanceAt(context.Context, common.Address, *big.Int) (*big.Int, error) {
	return nil, errors.New("unexpected call")
}

func (reader *startupGenerationReader) CodeAt(context.Context, common.Address, *big.Int) ([]byte, error) {
	return append([]byte{0xef, 0x01, 0x00}, reader.delegatedTo.Bytes()...), nil
}

func (*startupGenerationReader) NonceAt(context.Context, common.Address, *big.Int) (uint64, error) {
	return 0, errors.New("unexpected call")
}

func (*startupGenerationReader) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	return 0, errors.New("unexpected call")
}

func (reader *startupGenerationReader) CallContract(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error) {
	result := make([]byte, 32)
	copy(result[12:], reader.destination[:])
	return result, nil
}

func (*startupGenerationReader) SuggestGasPrice(context.Context) (*big.Int, error) {
	return nil, errors.New("unexpected call")
}

func (*startupGenerationReader) SuggestGasTipCap(context.Context) (*big.Int, error) {
	return nil, errors.New("unexpected call")
}

func (reader *startupGenerationReader) EstimateGas(_ context.Context, call ethereum.CallMsg) (uint64, error) {
	reader.estimates = append(reader.estimates, call)
	return 100_000, nil
}

func (*startupGenerationReader) TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error) {
	return nil, errors.New("unexpected call")
}

func (*startupGenerationReader) FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
	return nil, errors.New("unexpected call")
}

var _ rpc.Reader = (*startupGenerationReader)(nil)

type fakeAttestationClient struct {
	closed atomic.Int32
}

func (*fakeAttestationClient) ChainID(context.Context) (*big.Int, error) {
	return nil, errors.New("unexpected call")
}

func (*fakeAttestationClient) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return nil, errors.New("unexpected call")
}

func (*fakeAttestationClient) TransactionByHash(context.Context, common.Hash) (*types.Transaction, bool, error) {
	return nil, false, errors.New("unexpected call")
}

func (*fakeAttestationClient) TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error) {
	return nil, errors.New("unexpected call")
}

func (*fakeAttestationClient) CodeAtHash(context.Context, common.Address, common.Hash) ([]byte, error) {
	return nil, errors.New("unexpected call")
}

func (*fakeAttestationClient) CallContractAtHash(context.Context, ethereum.CallMsg, common.Hash) ([]byte, error) {
	return nil, errors.New("unexpected call")
}

func (client *fakeAttestationClient) Close() { client.closed.Add(1) }

var _ attestationClient = (*fakeAttestationClient)(nil)
