package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
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
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/watcher"

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
	clients := []*stubGenerationClient{{generation: 1}, {generation: 2}}
	dependencies := daemonDependencies{
		serviceClock: testClock,
		observer:     observability.Discard{},
		dial: func(_ context.Context, _ string, generation uint64) (generationClient, error) {
			if generation == 2 && clients[0].closed.Load() != 1 {
				t.Error("second generation dialed before first client closed")
			}
			return clients[generation-1], nil
		},
		newSession: func(_ context.Context, network *networkProcess, client generationClient) (candidateSession, error) {
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

type stubGenerationClient struct {
	generation uint64
	closed     atomic.Int32
}

func (client *stubGenerationClient) Generation() uint64          { return client.generation }
func (*stubGenerationClient) Reader() rpc.Reader                 { return nil }
func (*stubGenerationClient) LogSubscriber() rpc.LogSubscriber   { return nil }
func (*stubGenerationClient) HeadSubscriber() rpc.HeadSubscriber { return nil }
func (client *stubGenerationClient) Close()                      { client.closed.Add(1) }

type idleHandler struct {
	generation uint64
}

func (handler *idleHandler) Generation() uint64 { return handler.generation }
func (*idleHandler) Handle(context.Context, domain.RescueCandidate) error {
	return nil
}

func (*idleHandler) Close() {}

func stubSessionFactory(started chan<- *rescue.Coordinator) func(context.Context, *networkProcess, generationClient) (candidateSession, error) {
	return func(_ context.Context, network *networkProcess, client generationClient) (candidateSession, error) {
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

func (*supervisorClock) NewTicker(time.Duration) clock.Ticker { return nil }
func (*supervisorClock) NewTimer(duration time.Duration) clock.Timer {
	return clock.Real{}.NewTimer(duration)
}

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
	return dependencies
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
