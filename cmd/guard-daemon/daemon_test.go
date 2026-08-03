package main

import (
	"context"
	"crypto/ecdsa"
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
	"guard-daemon/internal/watcher"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestDaemonStartsExactlyOneLoopPerConfiguredNetworkAndStopsWorkers(t *testing.T) {
	networks := []domain.Network{
		testNetwork("network-one", 101),
		testNetwork("network-two", 102),
		testNetwork("network-three", 103),
	}
	testClock := newSupervisorClock()
	started := make(chan domain.NetworkID, len(networks))
	var active atomic.Int32
	var mu sync.Mutex
	dialCount := make(map[string]int)
	var clients []*stubGenerationClient
	dependencies := daemonDependencies{
		serviceClock: testClock,
		observer:     observability.Discard{},
		dial: func(_ context.Context, endpoint string, generation uint64) (generationClient, error) {
			client := &stubGenerationClient{generation: generation}
			mu.Lock()
			dialCount[endpoint]++
			clients = append(clients, client)
			mu.Unlock()
			return client, nil
		},
		newSession: stubSessionFactory(nil),
		newWatcher: func(dependencies watcher.Dependencies) (generationRunner, error) {
			return runnerFunc(func(ctx context.Context) error {
				active.Add(1)
				defer active.Add(-1)
				started <- dependencies.Network.ChainID
				<-ctx.Done()
				return ctx.Err()
			}), nil
		},
	}
	dependencies = completeTestDependencies(t, dependencies)
	process, err := newDaemon(context.Background(), testRuntime(t, networks), dependencies)
	if err != nil {
		t.Fatalf("newDaemon() error = %v", err)
	}
	if len(process.networks) != len(networks) {
		t.Fatalf("network process count = %d, want %d", len(process.networks), len(networks))
	}
	for index, network := range process.networks {
		if network.coordinator == nil || network.handoff == nil {
			t.Fatalf("network %d is missing long-lived state", index)
		}
		for previous := 0; previous < index; previous++ {
			if network.coordinator == process.networks[previous].coordinator || network.handoff == process.networks[previous].handoff {
				t.Fatal("configured networks share coordinator or handoff state")
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(process, ctx)
	seen := make(map[domain.NetworkID]int)
	for range networks {
		seen[receiveWithin(t, started)]++
	}
	for _, network := range networks {
		if seen[network.ChainID] != 1 {
			t.Fatalf("network %d worker starts = %d, want 1", network.ChainID, seen[network.ChainID])
		}
	}

	cancel()
	if err := receiveWithin(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if active.Load() != 0 {
		t.Fatalf("active generation workers = %d, want 0", active.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(clients) != len(networks) {
		t.Fatalf("dial count = %d, want %d", len(clients), len(networks))
	}
	for _, network := range networks {
		if dialCount[network.WSURL] != 1 || dialCount[network.HTTPURL] != 0 {
			t.Fatalf("network %d dial counts = ws:%d http:%d", network.ChainID, dialCount[network.WSURL], dialCount[network.HTTPURL])
		}
	}
	for _, client := range clients {
		if client.closed.Load() != 1 {
			t.Fatalf("client close count = %d, want 1", client.closed.Load())
		}
	}
}

func TestGenerationFallsBackFromWSToHTTP(t *testing.T) {
	network := testNetwork("fallback-network", 201)
	testClock := newSupervisorClock()
	started := make(chan struct{}, 1)
	client := &stubGenerationClient{generation: 1}
	var mu sync.Mutex
	var calls []string
	dependencies := daemonDependencies{
		serviceClock: testClock,
		observer:     observability.Discard{},
		dial: func(_ context.Context, endpoint string, generation uint64) (generationClient, error) {
			mu.Lock()
			calls = append(calls, endpoint)
			mu.Unlock()
			if strings.Contains(endpoint, "-ws") {
				return nil, errors.New("non-public backend detail")
			}
			if generation != 1 {
				t.Errorf("dial generation = %d, want 1", generation)
			}
			return client, nil
		},
		newSession: stubSessionFactory(nil),
		newWatcher: func(watcher.Dependencies) (generationRunner, error) {
			return runnerFunc(func(ctx context.Context) error {
				started <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}), nil
		},
	}
	dependencies = completeTestDependencies(t, dependencies)
	process, err := newDaemon(context.Background(), testRuntime(t, []domain.Network{network}), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(process, ctx)
	receiveWithin(t, started)
	cancel()
	receiveWithin(t, done)

	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(calls, []string{network.WSURL, network.WSURL + "-two", network.HTTPURL}) {
		t.Fatalf("dial order = %v", calls)
	}
	if client.closed.Load() != 1 {
		t.Fatalf("HTTP client close count = %d", client.closed.Load())
	}
}

func TestReconnectReusesCoordinatorAndInjectedDelay(t *testing.T) {
	network := testNetwork("reconnect-network", 301)
	testClock := newSupervisorClock()
	coordinators := make(chan *rescue.Coordinator, 2)
	secondStarted := make(chan struct{}, 1)
	dialed := make(chan string, 2)
	clients := []*stubGenerationClient{{generation: 1}, {generation: 2}}
	dependencies := daemonDependencies{
		serviceClock: testClock,
		observer:     observability.Discard{},
		dial: func(_ context.Context, endpoint string, generation uint64) (generationClient, error) {
			if generation == 2 && clients[0].closed.Load() != 1 {
				t.Error("second generation dialed before first client closed")
			}
			dialed <- endpoint
			return clients[generation-1], nil
		},
		newSession: func(_ context.Context, network *networkProcess, client generationClient, _ runtimeQuorum) (candidateSession, error) {
			coordinators <- network.coordinator
			return &idleHandler{generation: client.Generation()}, nil
		},
		newWatcher: func(dependencies watcher.Dependencies) (generationRunner, error) {
			if dependencies.Generation == 1 {
				return runnerFunc(func(context.Context) error { return errors.New("generation ended") }), nil
			}
			return runnerFunc(func(ctx context.Context) error {
				secondStarted <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}), nil
		},
	}
	dependencies = completeTestDependencies(t, dependencies)
	process, err := newDaemon(context.Background(), testRuntime(t, []domain.Network{network}), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(process, ctx)
	firstCoordinator := receiveWithin(t, coordinators)
	if delay := receiveWithin(t, testClock.sleeps); delay != reconnectDelay {
		t.Fatalf("reconnect delay = %s, want %s", delay, reconnectDelay)
	}
	testClock.allowSleep <- struct{}{}
	secondCoordinator := receiveWithin(t, coordinators)
	receiveWithin(t, secondStarted)
	if first, second := receiveWithin(t, dialed), receiveWithin(t, dialed); first != network.WSURL || second != network.WSURL+"-two" {
		t.Fatalf("primary rotation = %q, %q", first, second)
	}
	if firstCoordinator != secondCoordinator {
		t.Fatal("coordinator was recreated across reconnect generations")
	}

	cancel()
	receiveWithin(t, done)
	for index, client := range clients {
		if client.closed.Load() != 1 {
			t.Fatalf("client %d close count = %d", index, client.closed.Load())
		}
	}
}

func TestGenerationUsesDeadlineQuorumAndClosesRuntimeState(t *testing.T) {
	network := testNetwork("bounded-network", 351)
	testClock := newSupervisorClock()
	started := make(chan struct{}, 1)
	client := &stubGenerationClient{generation: 1}
	quorum := &countingRuntimeQuorum{}
	handoff := &countingHandoff{legacyMemoryHandoff: newLegacyMemoryHandoff(network.ChainID, testClock)}
	dependencies := daemonDependencies{
		serviceClock: testClock,
		observer:     observability.Discard{},
		dial: func(ctx context.Context, _ string, _ uint64) (generationClient, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("generation dial не получил deadline")
			}
			return client, nil
		},
		openStore: func(config.Runtime, config.Network, domain.Network) (store.HandoffStore, error) {
			return handoff, nil
		},
		openQuorum: func(_ context.Context, configured config.Network, timeout time.Duration) (runtimeQuorum, error) {
			if timeout != time.Second || len(configured.ReadProviders) != 2 {
				t.Fatalf("runtime quorum input: timeout=%s providers=%d", timeout, len(configured.ReadProviders))
			}
			return quorum, nil
		},
		newSession: func(_ context.Context, _ *networkProcess, client generationClient, got runtimeQuorum) (candidateSession, error) {
			if got != quorum {
				t.Fatal("rescue session не получил runtime quorum поколения")
			}
			return &idleHandler{generation: client.Generation()}, nil
		},
		newWatcher: func(watcher.Dependencies) (generationRunner, error) {
			return runnerFunc(func(ctx context.Context) error {
				started <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}), nil
		},
	}
	dependencies = completeTestDependencies(t, dependencies)
	process, err := newDaemon(context.Background(), testRuntime(t, []domain.Network{network}), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(process, ctx)
	receiveWithin(t, started)
	cancel()
	if err := receiveWithin(t, done); err != nil {
		t.Fatal(err)
	}
	if quorum.closed.Load() != 1 || handoff.closed.Load() != 1 || client.closed.Load() != 1 {
		t.Fatalf("close counts: quorum=%d state=%d client=%d", quorum.closed.Load(), handoff.closed.Load(), client.closed.Load())
	}
}

func TestDaemonGracefulShutdownReleasesLeaseBeforeStoreClose(t *testing.T) {
	network := testNetwork("graceful-lease", 361)
	testClock := newSupervisorClock()
	started := make(chan struct{}, 1)
	handoff := newLeaseLifecycleHandoff(network.ChainID, testClock)
	dependencies := daemonDependencies{
		serviceClock: testClock,
		observer:     observability.Discard{},
		dial: func(context.Context, string, uint64) (generationClient, error) {
			return &stubGenerationClient{generation: 1}, nil
		},
		openStore: func(config.Runtime, config.Network, domain.Network) (store.HandoffStore, error) {
			return handoff, nil
		},
		newSession: stubSessionFactory(nil),
		newWatcher: func(watcher.Dependencies) (generationRunner, error) {
			return runnerFunc(func(ctx context.Context) error {
				started <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}), nil
		},
	}
	dependencies = completeTestDependencies(t, dependencies)
	dependencies.acquireFence = func(store.LeaseKey) (store.ProcessFence, error) {
		handoff.record("fence-acquire")
		return &testProcessFence{onRelease: func() { handoff.record("fence-release") }}, nil
	}
	process, err := newDaemon(context.Background(), testRuntime(t, []domain.Network{network}), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(process, ctx)
	receiveWithin(t, started)
	cancel()
	if err := receiveWithin(t, done); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := handoff.snapshot(); !reflect.DeepEqual(got, []string{"fence-acquire", "acquire", "release", "fence-release", "close"}) {
		t.Fatalf("shutdown lifecycle = %v", got)
	}
	if err := handoff.lastReleaseError(); err != nil {
		t.Fatalf("graceful release error = %v", err)
	}
}

func TestDaemonSurfacesGracefulLeaseReleaseFailure(t *testing.T) {
	network := testNetwork("release-failure", 363)
	testClock := newSupervisorClock()
	started := make(chan struct{}, 1)
	handoff := newLeaseLifecycleHandoff(network.ChainID, testClock)
	handoff.releaseError = errors.New("тестовая ошибка release")
	dependencies := daemonDependencies{
		serviceClock: testClock,
		observer:     observability.Discard{},
		dial: func(context.Context, string, uint64) (generationClient, error) {
			return &stubGenerationClient{generation: 1}, nil
		},
		openStore: func(config.Runtime, config.Network, domain.Network) (store.HandoffStore, error) {
			return handoff, nil
		},
		newSession: stubSessionFactory(nil),
		newWatcher: func(watcher.Dependencies) (generationRunner, error) {
			return runnerFunc(func(ctx context.Context) error {
				started <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}), nil
		},
	}
	dependencies = completeTestDependencies(t, dependencies)
	process, err := newDaemon(context.Background(), testRuntime(t, []domain.Network{network}), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(process, ctx)
	receiveWithin(t, started)
	cancel()
	if err := receiveWithin(t, done); publicErrorCode(err) != errorLeaseReleaseFailed {
		t.Fatalf("Run() release error = %v", err)
	}
	if got := handoff.snapshot(); !reflect.DeepEqual(got, []string{"acquire", "release", "close"}) {
		t.Fatalf("failed release lifecycle = %v", got)
	}
}

func TestDaemonSurfacesRedactedFenceReleaseFailure(t *testing.T) {
	network := testNetwork("fence-release-failure", 364)
	testClock := newSupervisorClock()
	started := make(chan struct{}, 1)
	handoff := newLeaseLifecycleHandoff(network.ChainID, testClock)
	fence := &testProcessFence{releaseError: store.ErrFenceLost}
	dependencies := daemonDependencies{
		serviceClock: testClock,
		observer:     observability.Discard{},
		dial: func(context.Context, string, uint64) (generationClient, error) {
			return &stubGenerationClient{generation: 1}, nil
		},
		openStore: func(config.Runtime, config.Network, domain.Network) (store.HandoffStore, error) {
			return handoff, nil
		},
		newSession: stubSessionFactory(nil),
		newWatcher: func(watcher.Dependencies) (generationRunner, error) {
			return runnerFunc(func(ctx context.Context) error {
				started <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}), nil
		},
	}
	dependencies = completeTestDependencies(t, dependencies)
	dependencies.acquireFence = func(store.LeaseKey) (store.ProcessFence, error) { return fence, nil }
	process, err := newDaemon(context.Background(), testRuntime(t, []domain.Network{network}), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runDaemon(process, ctx)
	receiveWithin(t, started)
	cancel()
	runError := receiveWithin(t, done)
	if publicErrorCode(runError) != errorFenceReleaseFailed || !errors.Is(runError, store.ErrFenceLost) {
		t.Fatalf("Run() fence release error = %v", runError)
	}
	if strings.Contains(runError.Error(), "fence-release-failure") {
		t.Fatalf("fence release error exposed network identity: %v", runError)
	}
}

func TestLeaseMaintenanceLossCancelsWorkersAndBlocksSigning(t *testing.T) {
	network := testNetwork("lost-lease", 362)
	testClock := newSupervisorClock()
	started := make(chan struct{}, 1)
	stopped := make(chan struct{})
	handoff := newLeaseLifecycleHandoff(network.ChainID, testClock)
	fence := &testProcessFence{}
	dependencies := daemonDependencies{
		serviceClock: testClock,
		observer:     observability.Discard{},
		dial: func(context.Context, string, uint64) (generationClient, error) {
			return &stubGenerationClient{generation: 1}, nil
		},
		openStore: func(config.Runtime, config.Network, domain.Network) (store.HandoffStore, error) {
			return handoff, nil
		},
		newSession: stubSessionFactory(nil),
		newWatcher: func(watcher.Dependencies) (generationRunner, error) {
			return runnerFunc(func(ctx context.Context) error {
				started <- struct{}{}
				<-ctx.Done()
				close(stopped)
				return ctx.Err()
			}), nil
		},
	}
	dependencies = completeTestDependencies(t, dependencies)
	dependencies.acquireFence = func(store.LeaseKey) (store.ProcessFence, error) { return fence, nil }
	runtimeConfig := testRuntime(t, []domain.Network{network})
	var attempts *dryrun.Attempts
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		authorizer, transactioner, _, guardAttempts := dryrun.NewGuards(runtimeConfig.SourceAddress, runtimeConfig.SponsorAddress)
		attempts = guardAttempts
		return authorizer, transactioner, nil
	}
	process, err := newDaemon(context.Background(), runtimeConfig, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	done := runDaemon(process, context.Background())
	receiveWithin(t, started)
	lease := handoff.currentLease(store.LeaseKey{Network: network.ChainID, Sponsor: runtimeConfig.SponsorAddress})
	if err := handoff.legacyMemoryHandoff.Release(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	runError := receiveWithin(t, done)
	if publicErrorCode(runError) != errorLeaseMaintainFailed || !rescue.IsLeaseLost(runError) {
		t.Fatalf("Run() lease loss error = %v", runError)
	}
	receiveWithin(t, stopped)
	if attempts == nil || attempts.AuthorizationSignatures() != 0 || attempts.TransactionSignatures() != 0 || attempts.Broadcasts() != 0 {
		t.Fatal("lease loss допустил signing или broadcast")
	}
	_, err = process.networks[0].coordinator.NewSession(
		context.Background(),
		2,
		&startupGenerationReader{chainID: big.NewInt(int64(network.ChainID))},
		&startupRuntimeQuorum{destination: runtimeConfig.Destination},
		&stubSubmissionClient{},
	)
	if !rescue.IsLeaseLost(err) {
		t.Fatalf("NewSession() after lease loss error = %v", err)
	}
	if !fence.isReleased() {
		t.Fatal("lease-loss shutdown did not release process fence")
	}
}

func TestLiveSessionValidatesProcessFenceBeforeFactory(t *testing.T) {
	var events []string
	fence := &testProcessFence{onValidate: func() { events = append(events, "validate") }}
	network := &networkProcess{
		mode:          config.ModeLive,
		fence:         fence,
		allowedTokens: make(map[common.Address]struct{}),
	}
	dependencies := daemonDependencies{
		newSession: func(context.Context, *networkProcess, generationClient, runtimeQuorum) (candidateSession, error) {
			events = append(events, "session")
			return &idleHandler{generation: 1}, nil
		},
	}
	session, err := network.openSession(context.Background(), &stubGenerationClient{generation: 1}, inertRuntimeQuorum{}, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	session.Close()
	if !reflect.DeepEqual(events, []string{"validate", "session"}) {
		t.Fatalf("live session fence order = %v", events)
	}

	fence.validateError = store.ErrFenceLost
	if _, err := network.openSession(context.Background(), &stubGenerationClient{generation: 2}, inertRuntimeQuorum{}, dependencies); !errors.Is(err, store.ErrFenceLost) {
		t.Fatalf("openSession() after fence loss = %v", err)
	}
	if !reflect.DeepEqual(events, []string{"validate", "session", "validate"}) {
		t.Fatalf("session factory ran after fence loss: %v", events)
	}
}

func TestGenerationRunsLiveReconciliationWorker(t *testing.T) {
	network := testNetwork("reconciliation-worker", 365)
	testClock := newSupervisorClock()
	reconciliationError := errors.New("reconciliation state failed")
	reconciliationStarted := make(chan struct{}, 1)
	watcherStopped := make(chan struct{})
	client := &stubGenerationClient{generation: 1}
	dependencies := daemonDependencies{
		serviceClock: testClock,
		observer:     observability.Discard{},
		dial: func(context.Context, string, uint64) (generationClient, error) {
			return client, nil
		},
		newSession: func(_ context.Context, _ *networkProcess, client generationClient, _ runtimeQuorum) (candidateSession, error) {
			return &reconcilingHandler{
				idleHandler: &idleHandler{generation: client.Generation()},
				run: func(context.Context) error {
					reconciliationStarted <- struct{}{}
					return reconciliationError
				},
			}, nil
		},
		newWatcher: func(watcher.Dependencies) (generationRunner, error) {
			return runnerFunc(func(ctx context.Context) error {
				<-ctx.Done()
				close(watcherStopped)
				return ctx.Err()
			}), nil
		},
	}
	dependencies = completeTestDependencies(t, dependencies)
	process, err := newDaemon(context.Background(), testRuntime(t, []domain.Network{network}), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	defer process.shutdown(nil)

	err = process.networks[0].runGeneration(context.Background(), 1, dependencies)
	if !errors.Is(err, reconciliationError) {
		t.Fatalf("runGeneration() reconciliation error = %v", err)
	}
	receiveWithin(t, reconciliationStarted)
	receiveWithin(t, watcherStopped)
	if client.closed.Load() != 1 {
		t.Fatalf("client close count = %d, want 1", client.closed.Load())
	}
}

func TestReconciliationWorkerIsLiveOnlyAndSurvivesTokenPolicy(t *testing.T) {
	handler := &reconcilingHandler{idleHandler: &idleHandler{generation: 1}}
	network := &networkProcess{allowedTokens: make(map[common.Address]struct{})}
	session := network.bindTokenPolicy(handler)
	if reconciliationWorker(config.ModeLive, session) == nil {
		t.Fatal("live token policy wrapper hid reconciliation capability")
	}
	if reconciliationWorker(config.ModeDryRun, session) != nil {
		t.Fatal("dry-run enabled reconciliation worker")
	}
}

type stubGenerationClient struct {
	generation uint64
	closed     atomic.Int32
}

func (client *stubGenerationClient) Generation() uint64 { return client.generation }
func (*stubGenerationClient) Reader() rpc.Reader {
	return &startupGenerationReader{chainID: big.NewInt(1)}
}
func (*stubGenerationClient) LogSubscriber() rpc.LogSubscriber   { return inertSubscriber{} }
func (*stubGenerationClient) HeadSubscriber() rpc.HeadSubscriber { return inertSubscriber{} }
func (client *stubGenerationClient) Close()                      { client.closed.Add(1) }

type inertSubscriber struct{}

func (inertSubscriber) SubscribeFilterLogs(context.Context, ethereum.FilterQuery, chan<- types.Log) (ethereum.Subscription, error) {
	return nil, errors.New("test subscription is not configured")
}
func (inertSubscriber) SubscribeNewHead(context.Context, chan<- *types.Header) (ethereum.Subscription, error) {
	return nil, errors.New("test subscription is not configured")
}

type idleHandler struct {
	generation uint64
}

func (handler *idleHandler) Generation() uint64 { return handler.generation }
func (*idleHandler) Handle(context.Context, domain.RescueCandidate) error {
	return nil
}

func (*idleHandler) Close() {}

type reconcilingHandler struct {
	*idleHandler
	run func(context.Context) error
}

func (handler *reconcilingHandler) RunReconciliation(ctx context.Context) error {
	if handler.run == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	return handler.run(ctx)
}

func stubSessionFactory(started chan<- *rescue.Coordinator) func(context.Context, *networkProcess, generationClient, runtimeQuorum) (candidateSession, error) {
	return func(_ context.Context, network *networkProcess, client generationClient, _ runtimeQuorum) (candidateSession, error) {
		if started != nil {
			started <- network.coordinator
		}
		return &idleHandler{generation: client.Generation()}, nil
	}
}

type runnerFunc func(context.Context) error

func (run runnerFunc) Run(ctx context.Context) error { return run(ctx) }

type supervisorClock struct {
	sleeps     chan time.Duration
	allowSleep chan struct{}
}

func newSupervisorClock() *supervisorClock {
	return &supervisorClock{sleeps: make(chan time.Duration, 8), allowSleep: make(chan struct{}, 8)}
}

func (*supervisorClock) Now() time.Time { return time.Unix(0, 0) }

func (serviceClock *supervisorClock) Sleep(ctx context.Context, duration time.Duration) error {
	select {
	case serviceClock.sleeps <- duration:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-serviceClock.allowSleep:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (*supervisorClock) NewTicker(time.Duration) clock.Ticker {
	return &inertClockTicker{ticks: make(chan time.Time)}
}
func (*supervisorClock) NewTimer(duration time.Duration) clock.Timer {
	return clock.Real{}.NewTimer(duration)
}

type inertClockTicker struct {
	ticks chan time.Time
}

func (ticker *inertClockTicker) C() <-chan time.Time { return ticker.ticks }
func (*inertClockTicker) Stop()                      {}

func testRuntime(t *testing.T, networks []domain.Network) config.Runtime {
	t.Helper()
	sourceKey := deterministicKey(t, 1)
	sponsorKey := deterministicKey(t, 2)
	configured := make([]config.Network, 0, len(networks))
	for _, network := range networks {
		configured = append(configured, config.Network{
			Name:    network.Name,
			ChainID: network.ChainID,
			ReadProviders: []config.ReadProvider{
				{ID: network.Name + "-read-1", HTTPURL: network.HTTPURL, WSURL: network.WSURL, TrustDomain: network.Name + "-one", EndpointFingerprint: network.Name + "-one"},
				{ID: network.Name + "-read-2", HTTPURL: network.HTTPURL + "-two", WSURL: network.WSURL + "-two", TrustDomain: network.Name + "-two", EndpointFingerprint: network.Name + "-two"},
			},
			BroadcastHTTP: network.HTTPURL + "-broadcast",
			ManifestPath:  network.Name + "-manifest",
			Tokens:        append([]domain.Token(nil), network.Tokens...),
		})
	}
	return config.Runtime{
		Mode:           config.ModeLive,
		SourceAddress:  crypto.PubkeyToAddress(sourceKey.PublicKey),
		SponsorAddress: crypto.PubkeyToAddress(sponsorKey.PublicKey),
		Destination:    testProcessAddress(3),
		Networks:       configured,
		ReadTimeout:    time.Second,
		Watch:          config.WatchPolicy{StateDirectory: "test-state", LookbackBlocks: 64},
	}
}

func completeTestDependencies(t *testing.T, dependencies daemonDependencies) daemonDependencies {
	t.Helper()
	dependencies.loadManifest = func(config.Runtime, config.Network) (contracts.DeploymentManifest, error) {
		return contracts.DeploymentManifest{Address: testProcessAddress(4)}, nil
	}
	dependencies.attestNetwork = func(context.Context, config.Network, contracts.DeploymentManifest, time.Duration) error {
		return nil
	}
	dependencies.newSigners = func(config.LiveSecrets) (rescue.AuthorizationSigner, rescue.TransactionSigner, error) {
		authorizer, err := rescue.NewPrivateKeyAuthorizationSigner(deterministicKey(t, 1))
		if err != nil {
			return nil, nil, err
		}
		transactioner, err := rescue.NewPrivateKeyTransactionSigner(deterministicKey(t, 2))
		return authorizer, transactioner, err
	}
	dependencies.dialSubmission = func(context.Context, string) (submissionClient, error) {
		return &stubSubmissionClient{}, nil
	}
	return completeTestRuntimeDependencies(dependencies)
}

func completeTestRuntimeDependencies(dependencies daemonDependencies) daemonDependencies {
	if dependencies.openStore == nil {
		dependencies.openStore = func(_ config.Runtime, _ config.Network, network domain.Network) (store.HandoffStore, error) {
			return newLegacyMemoryHandoff(network.ChainID, dependencies.serviceClock), nil
		}
	}
	if dependencies.openQuorum == nil {
		dependencies.openQuorum = func(context.Context, config.Network, time.Duration) (runtimeQuorum, error) {
			return inertRuntimeQuorum{}, nil
		}
	}
	if dependencies.acquireFence == nil {
		dependencies.acquireFence = func(store.LeaseKey) (store.ProcessFence, error) {
			return &testProcessFence{}, nil
		}
	}
	return dependencies
}

type inertRuntimeQuorum struct{}

func (inertRuntimeQuorum) Finalized(context.Context) (rpc.BlockRef, error) {
	return rpc.BlockRef{}, errors.New("test quorum read is not configured")
}
func (inertRuntimeQuorum) Header(context.Context, uint64) (rpc.BlockRef, error) {
	return rpc.BlockRef{}, errors.New("test quorum read is not configured")
}
func (inertRuntimeQuorum) FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
	return nil, errors.New("test quorum read is not configured")
}
func (inertRuntimeQuorum) BalanceAt(context.Context, rpc.BlockRef, common.Address) (*big.Int, error) {
	return nil, errors.New("test quorum read is not configured")
}
func (inertRuntimeQuorum) CodeAt(context.Context, rpc.BlockRef, common.Address) ([]byte, error) {
	return nil, errors.New("test quorum read is not configured")
}
func (inertRuntimeQuorum) CallContract(context.Context, rpc.BlockRef, ethereum.CallMsg) ([]byte, error) {
	return nil, errors.New("test quorum read is not configured")
}
func (inertRuntimeQuorum) Receipt(context.Context, common.Hash) (*types.Receipt, error) {
	return nil, errors.New("test quorum read is not configured")
}
func (inertRuntimeQuorum) Close() {}

type countingRuntimeQuorum struct {
	inertRuntimeQuorum
	closed atomic.Int32
}

func (quorum *countingRuntimeQuorum) Close() { quorum.closed.Add(1) }

type countingHandoff struct {
	*legacyMemoryHandoff
	closed atomic.Int32
}

type testProcessFence struct {
	mu            sync.Mutex
	released      bool
	validateError error
	releaseError  error
	onValidate    func()
	onRelease     func()
}

func (fence *testProcessFence) Validate() error {
	fence.mu.Lock()
	defer fence.mu.Unlock()
	if fence.onValidate != nil {
		fence.onValidate()
	}
	if fence.released {
		return store.ErrFenceLost
	}
	return fence.validateError
}

func (fence *testProcessFence) Release() error {
	fence.mu.Lock()
	defer fence.mu.Unlock()
	if fence.released {
		return nil
	}
	fence.released = true
	if fence.onRelease != nil {
		fence.onRelease()
	}
	return fence.releaseError
}

func (fence *testProcessFence) isReleased() bool {
	fence.mu.Lock()
	defer fence.mu.Unlock()
	return fence.released
}

func (handoff *countingHandoff) Close() error {
	handoff.closed.Add(1)
	return nil
}

type stubSubmissionClient struct{}

func (*stubSubmissionClient) SendTransaction(context.Context, *types.Transaction) error { return nil }
func (*stubSubmissionClient) Close()                                                    {}

func deterministicKey(t *testing.T, value byte) *ecdsa.PrivateKey {
	t.Helper()
	material := make([]byte, 32)
	material[len(material)-1] = value
	key, err := crypto.ToECDSA(material)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testNetwork(name string, chainID domain.NetworkID) domain.Network {
	return domain.Network{
		Name:    name,
		ChainID: chainID,
		WSURL:   name + "-ws",
		HTTPURL: name + "-http",
	}
}

func testProcessAddress(value byte) common.Address {
	var address common.Address
	address[len(address)-1] = value
	return address
}

func runDaemon(process *daemon, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- process.Run(ctx)
	}()
	return done
}

func receiveWithin[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for test event")
		var zero T
		return zero
	}
}
