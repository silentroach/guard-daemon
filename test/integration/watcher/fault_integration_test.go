package watcher_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"guard-daemon/internal/clock"
	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"
	"guard-daemon/internal/watcher"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	quorumTimeout = 250 * time.Millisecond
	testTimeout   = 5 * time.Second
)

var errUnusedRPCMethod = errors.New("unexpected RPC method in watcher integration test")

func TestWatcherQuorumFaultRecoveryAndReorg(t *testing.T) {
	ctx := context.Background()
	source := testAddress(0x11)
	token := domain.Token{Address: testAddress(0x12), Symbol: "TEST", Decimals: 18}
	network := domain.Network{Name: "local-fixture", ChainID: 31337, Tokens: []domain.Token{token}}
	codec := testCodec(t)

	chain := newHistoricalChain()
	block10 := testHeader(10, testHash(0x09), 0x10)
	log10 := transferLog(codec, source, token.Address, block10, 0)
	chain.setBlock(block10, []types.Log{log10, log10})
	providers := []*historicalProvider{{chain: chain}, {chain: chain}}
	quorum := testQuorum(t, providers...)
	defer quorum.Close()

	path := filepath.Join(t.TempDir(), "watcher.db")
	options := testStoreOptions(network, source, 16)
	handoff := openStore(t, path, options)
	defer handoff.Close()

	providers[0].hangFinalized.Store(true)
	timedOut := newService(t, network, source, codec, quorum, handoff, newControlledClock(), newSetupFailureSubscriptions())
	if err := timedOut.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("watcher quorum timeout error = %v, want deadline exceeded", err)
	}
	assertStoreState(t, handoff, network.ChainID, store.Checkpoint{}, nil)
	providers[0].hangFinalized.Store(false)

	staleBlock10 := testHeader(10, testHash(0x08), 0xa0)
	providers[1].setOverride(10, staleBlock10)
	disagreed := newService(t, network, source, codec, quorum, handoff, newControlledClock(), newSetupFailureSubscriptions())
	if err := disagreed.Run(ctx); !errors.Is(err, rpc.ErrQuorumMismatch) {
		t.Fatalf("watcher accepted Byzantine finalized branch: %v", err)
	}
	assertStoreState(t, handoff, network.ChainID, store.Checkpoint{}, nil)
	providers[1].setOverride(10, nil)

	observer := newEventRecorder()
	pollClock := newControlledClock()
	fallback := newSetupFailureSubscriptions()
	service := newServiceWithObserver(t, network, source, codec, quorum, handoff, pollClock, fallback, observer)
	runCtx, cancel := context.WithCancel(ctx)
	runResult := make(chan error, 1)
	go func() { runResult <- service.Run(runCtx) }()

	scanTicker := waitTicker(t, pollClock, 3*time.Second)
	waitSignal(t, fallback.logSetup)
	block11 := testHeader(11, block10.Hash(), 0x11)
	log11 := transferLog(codec, source, token.Address, block11, 1)
	chain.setBlock(block11, []types.Log{log11})
	scanTicker.tick()
	wantLog11 := domain.NewLogCandidate(network.ChainID, source, token, block11.Hash(), log11.TxHash, 11, 1)
	waitEvent(t, observer, func(event observability.Event) bool { return event.Candidate == wantLog11.ID })
	cancel()
	if err := waitResult(t, runResult); !errors.Is(err, context.Canceled) {
		t.Fatalf("polling watcher shutdown error: %v", err)
	}

	wantCanonical := []domain.RescueCandidate{
		domain.NewBlockCandidate(network.ChainID, domain.CandidateNative, source, block10.Hash(), 10),
		domain.NewLogCandidate(network.ChainID, source, token, block10.Hash(), log10.TxHash, 10, 0),
		domain.NewBlockCandidate(network.ChainID, domain.CandidateNative, source, block11.Hash(), 11),
		wantLog11,
	}
	assertStoreState(t, handoff, network.ChainID, checkpoint(network.ChainID, block11), wantCanonical)

	liveClock := newControlledClock()
	liveSubscriptions := newControlledSubscriptions()
	liveObserver := newEventRecorder()
	live := newServiceWithObserver(t, network, source, codec, quorum, handoff, liveClock, liveSubscriptions, liveObserver)
	liveCtx, stopLive := context.WithCancel(ctx)
	liveResult := make(chan error, 1)
	go func() { liveResult <- live.Run(liveCtx) }()
	logSink := waitResult(t, liveSubscriptions.logsRegistered)
	waitSignal(t, liveSubscriptions.headRegistered)

	orphanHeader := testHeader(12, block11.Hash(), 0xee)
	orphanLog := transferLog(codec, source, token.Address, orphanHeader, 2)
	orphanCandidate := domain.NewLogCandidate(network.ChainID, source, token, orphanHeader.Hash(), orphanLog.TxHash, 12, 2)
	logSink <- orphanLog
	waitEventCode(t, liveObserver, orphanCandidate.ID, "watcher_candidate_inserted")
	removed := orphanLog
	removed.Removed = true
	logSink <- removed
	waitEventCode(t, liveObserver, orphanCandidate.ID, "watcher_candidate_pending")
	logSink <- types.Log{}
	waitEventCode(t, liveObserver, domain.CandidateID{}, "watcher_log_rejected")

	// После удаления предварительного наблюдения ту же подсказку можно добавить заново.
	logSink <- orphanLog
	waitEventCode(t, liveObserver, orphanCandidate.ID, "watcher_candidate_inserted")
	logSink <- removed
	waitEventCode(t, liveObserver, orphanCandidate.ID, "watcher_candidate_pending")
	logSink <- types.Log{}
	waitEventCode(t, liveObserver, domain.CandidateID{}, "watcher_log_rejected")
	stopLive()
	if err := waitResult(t, liveResult); !errors.Is(err, context.Canceled) {
		t.Fatalf("subscribing watcher shutdown error: %v", err)
	}
	assertStoreState(t, handoff, network.ChainID, checkpoint(network.ChainID, block11), wantCanonical)

	providers[1].setOverride(11, testHeader(11, block10.Hash(), 0xb1))
	byzantineAfterCommit := newService(t, network, source, codec, quorum, handoff, newControlledClock(), newSetupFailureSubscriptions())
	if err := byzantineAfterCommit.Run(ctx); !errors.Is(err, rpc.ErrQuorumMismatch) {
		t.Fatalf("persisted cursor accepted Byzantine branch: %v", err)
	}
	providers[1].setOverride(11, nil)
	assertStoreState(t, handoff, network.ChainID, checkpoint(network.ChainID, block11), wantCanonical)

	reorganized11 := testHeader(11, block10.Hash(), 0xc1)
	chain.setBlock(reorganized11, []types.Log{})
	reorg := newService(t, network, source, codec, quorum, handoff, newControlledClock(), newSetupFailureSubscriptions())
	err := reorg.Run(ctx)
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) || classified.Class != domain.ErrorRPCInvalidResponse || classified.Code != "watcher_finalized_reorg" {
		t.Fatalf("agreed finalized reorganization error: %v", err)
	}
	assertStoreState(t, handoff, network.ChainID, checkpoint(network.ChainID, block11), wantCanonical)
}

func TestWatcherCrashConsistentQueueCheckpointAndIncidentRestore(t *testing.T) {
	ctx := context.Background()
	source := testAddress(0x21)
	token := domain.Token{Address: testAddress(0x22), Symbol: "TEST", Decimals: 6}
	network := domain.Network{Name: "restart-fixture", ChainID: 31337, Tokens: []domain.Token{token}}
	codec := testCodec(t)
	chain := newHistoricalChain()
	block := testHeader(20, testHash(0x19), 0x20)
	logEntry := transferLog(codec, source, token.Address, block, 3)
	chain.setBlock(block, []types.Log{logEntry})
	quorum := testQuorum(t, &historicalProvider{chain: chain}, &historicalProvider{chain: chain})
	defer quorum.Close()

	path := filepath.Join(t.TempDir(), "restart.db")
	options := testStoreOptions(network, source, 1)
	handoff := openStore(t, path, options)
	observer := newEventRecorder()
	service := newServiceWithObserver(t, network, source, codec, quorum, handoff, newControlledClock(), newSetupFailureSubscriptions(), observer)
	runCtx, cancel := context.WithCancel(ctx)
	runResult := make(chan error, 1)
	go func() { runResult <- service.Run(runCtx) }()

	nativeCandidate := domain.NewBlockCandidate(network.ChainID, domain.CandidateNative, source, block.Hash(), 20)
	tokenCandidate := domain.NewLogCandidate(network.ChainID, source, token, block.Hash(), logEntry.TxHash, 20, 3)
	waitEvent(t, observer, func(event observability.Event) bool { return event.Candidate == nativeCandidate.ID })
	waitEvent(t, observer, func(event observability.Event) bool { return event.Candidate == tokenCandidate.ID })
	cancel()
	if err := waitResult(t, runResult); !errors.Is(err, context.Canceled) {
		t.Fatalf("watcher shutdown error: %v", err)
	}
	wantCandidates := []domain.RescueCandidate{nativeCandidate, tokenCandidate}
	assertStoreState(t, handoff, network.ChainID, checkpoint(network.ChainID, block), wantCandidates)
	assertConfirmedCheckpoint(t, handoff, network.ChainID, store.Checkpoint{})

	first, err := handoff.Next(ctx, network.ChainID)
	if err != nil {
		t.Fatal(err)
	}
	firstHandoffIncident := handoffIncident(first, 1)
	if err := handoff.PutIncident(ctx, firstHandoffIncident); err != nil {
		t.Fatal(err)
	}
	firstRescueIncident := rescueIncident(first, 1)
	if _, err := handoff.PutRescueIncident(ctx, firstRescueIncident); err != nil {
		t.Fatal(err)
	}
	if err := handoff.Close(); err != nil {
		t.Fatal(err)
	}

	handoff = openStore(t, path, options)
	assertStoreState(t, handoff, network.ChainID, checkpoint(network.ChainID, block), wantCandidates)
	assertConfirmedCheckpoint(t, handoff, network.ChainID, store.Checkpoint{})
	replayedFirst, err := handoff.Next(ctx, network.ChainID)
	if err != nil || replayedFirst.ID != first.ID {
		t.Fatalf("ready candidate after restart = %s, %v; want %s", replayedFirst.ID, err, first.ID)
	}
	assertHandoffIncident(t, handoff, firstHandoffIncident)
	assertRescueIncident(t, handoff, firstRescueIncident)

	if err := handoff.Ack(ctx, first.ID, firstHandoffIncident.ID); err != nil {
		t.Fatal(err)
	}
	assertConfirmedCheckpoint(t, handoff, network.ChainID, store.Checkpoint{})
	second, err := handoff.Next(ctx, network.ChainID)
	if err != nil || second.ID == first.ID || !containsCandidate(wantCandidates, second.ID) {
		t.Fatalf("prepared candidate after releasing saturated slot = %s, %v", second.ID, err)
	}
	secondHandoffIncident := handoffIncident(second, 2)
	if err := handoff.PutIncident(ctx, secondHandoffIncident); err != nil {
		t.Fatal(err)
	}
	secondRescueIncident := rescueIncident(second, 2)
	if _, err := handoff.PutRescueIncident(ctx, secondRescueIncident); err != nil {
		t.Fatal(err)
	}
	if err := handoff.Ack(ctx, second.ID, secondHandoffIncident.ID); err != nil {
		t.Fatal(err)
	}
	assertConfirmedCheckpoint(t, handoff, network.ChainID, checkpoint(network.ChainID, block))
	if err := handoff.Close(); err != nil {
		t.Fatal(err)
	}

	handoff = openStore(t, path, options)
	defer handoff.Close()
	assertStoreState(t, handoff, network.ChainID, checkpoint(network.ChainID, block), nil)
	assertConfirmedCheckpoint(t, handoff, network.ChainID, checkpoint(network.ChainID, block))
	assertHandoffIncident(t, handoff, firstHandoffIncident)
	assertHandoffIncident(t, handoff, secondHandoffIncident)
	assertRescueIncident(t, handoff, firstRescueIncident)
	assertRescueIncident(t, handoff, secondRescueIncident)
}

type historicalChain struct {
	mu       sync.RWMutex
	latest   uint64
	headers  map[uint64]*types.Header
	logsByID map[common.Hash][]types.Log
}

func newHistoricalChain() *historicalChain {
	return &historicalChain{headers: make(map[uint64]*types.Header), logsByID: make(map[common.Hash][]types.Log)}
}

func (chain *historicalChain) setBlock(header *types.Header, logs []types.Log) {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	chain.headers[header.Number.Uint64()] = types.CopyHeader(header)
	chain.logsByID[header.Hash()] = cloneLogs(logs)
	if header.Number.Uint64() >= chain.latest {
		chain.latest = header.Number.Uint64()
	}
}

func (chain *historicalChain) header(number *big.Int) (*types.Header, error) {
	chain.mu.RLock()
	defer chain.mu.RUnlock()
	wanted := chain.latest
	if number.Sign() >= 0 {
		wanted = number.Uint64()
	}
	header := chain.headers[wanted]
	if header == nil {
		return nil, errors.New("fixture header unavailable")
	}
	return types.CopyHeader(header), nil
}

func (chain *historicalChain) logs(query ethereum.FilterQuery) ([]types.Log, error) {
	chain.mu.RLock()
	defer chain.mu.RUnlock()
	if query.BlockHash == nil {
		return nil, errors.New("fixture requires a hash-pinned log query")
	}
	logs, ok := chain.logsByID[*query.BlockHash]
	if !ok {
		return nil, errors.New("fixture block unavailable")
	}
	return cloneLogs(logs), nil
}

type historicalProvider struct {
	chain         *historicalChain
	hangFinalized atomic.Bool
	mu            sync.RWMutex
	overrides     map[uint64]*types.Header
}

func (provider *historicalProvider) setOverride(number uint64, header *types.Header) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.overrides == nil {
		provider.overrides = make(map[uint64]*types.Header)
	}
	if header == nil {
		delete(provider.overrides, number)
		return
	}
	provider.overrides[number] = types.CopyHeader(header)
}

func (provider *historicalProvider) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	if number.Sign() < 0 && provider.hangFinalized.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	header, err := provider.chain.header(number)
	if err != nil {
		return nil, err
	}
	provider.mu.RLock()
	override := provider.overrides[header.Number.Uint64()]
	provider.mu.RUnlock()
	if override != nil {
		return types.CopyHeader(override), nil
	}
	return header, nil
}

func (*historicalProvider) BalanceAtHash(context.Context, common.Address, common.Hash) (*big.Int, error) {
	return nil, errUnusedRPCMethod
}

func (*historicalProvider) CodeAtHash(context.Context, common.Address, common.Hash) ([]byte, error) {
	return nil, errUnusedRPCMethod
}

func (*historicalProvider) CallContractAtHash(context.Context, ethereum.CallMsg, common.Hash) ([]byte, error) {
	return nil, errUnusedRPCMethod
}

func (*historicalProvider) TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error) {
	return nil, errUnusedRPCMethod
}

func (provider *historicalProvider) FilterLogs(_ context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	return provider.chain.logs(query)
}

type controlledClock struct {
	now     time.Time
	created chan *controlledTicker
}

func newControlledClock() *controlledClock {
	return &controlledClock{
		now:     time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC),
		created: make(chan *controlledTicker, 8),
	}
}

func (testClock *controlledClock) Now() time.Time { return testClock.now }

func (*controlledClock) Sleep(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

func (testClock *controlledClock) NewTicker(duration time.Duration) clock.Ticker {
	ticker := &controlledTicker{duration: duration, ticks: make(chan time.Time, 1)}
	testClock.created <- ticker
	return ticker
}

func (*controlledClock) NewTimer(time.Duration) clock.Timer {
	return &controlledTimer{ticks: make(chan time.Time)}
}

type controlledTicker struct {
	duration time.Duration
	ticks    chan time.Time
	stopped  atomic.Bool
}

func (ticker *controlledTicker) C() <-chan time.Time { return ticker.ticks }
func (ticker *controlledTicker) Stop()               { ticker.stopped.Store(true) }
func (ticker *controlledTicker) tick()               { ticker.ticks <- time.Time{} }

type controlledTimer struct {
	ticks chan time.Time
}

func (timer *controlledTimer) C() <-chan time.Time { return timer.ticks }
func (*controlledTimer) Stop()                     {}

type setupFailureSubscriptions struct {
	logSetup chan struct{}
	once     sync.Once
}

func newSetupFailureSubscriptions() *setupFailureSubscriptions {
	return &setupFailureSubscriptions{logSetup: make(chan struct{})}
}

func (subscriptions *setupFailureSubscriptions) SubscribeFilterLogs(context.Context, ethereum.FilterQuery, chan<- types.Log) (ethereum.Subscription, error) {
	subscriptions.once.Do(func() { close(subscriptions.logSetup) })
	return nil, errors.New("deterministic subscription disconnect")
}

func (*setupFailureSubscriptions) SubscribeNewHead(context.Context, chan<- *types.Header) (ethereum.Subscription, error) {
	return nil, errors.New("unexpected head subscription")
}

type controlledSubscriptions struct {
	logsRegistered chan chan<- types.Log
	headRegistered chan struct{}
	logSub         *testSubscription
	headSub        *testSubscription
	headOnce       sync.Once
}

func newControlledSubscriptions() *controlledSubscriptions {
	return &controlledSubscriptions{
		logsRegistered: make(chan chan<- types.Log, 1),
		headRegistered: make(chan struct{}),
		logSub:         newTestSubscription(),
		headSub:        newTestSubscription(),
	}
}

func (subscriptions *controlledSubscriptions) SubscribeFilterLogs(_ context.Context, _ ethereum.FilterQuery, logs chan<- types.Log) (ethereum.Subscription, error) {
	subscriptions.logsRegistered <- logs
	return subscriptions.logSub, nil
}

func (subscriptions *controlledSubscriptions) SubscribeNewHead(context.Context, chan<- *types.Header) (ethereum.Subscription, error) {
	subscriptions.headOnce.Do(func() { close(subscriptions.headRegistered) })
	return subscriptions.headSub, nil
}

type testSubscription struct {
	errors chan error
	once   sync.Once
}

func newTestSubscription() *testSubscription {
	return &testSubscription{errors: make(chan error)}
}

func (subscription *testSubscription) Err() <-chan error { return subscription.errors }
func (subscription *testSubscription) Unsubscribe() {
	subscription.once.Do(func() { close(subscription.errors) })
}

type eventRecorder struct {
	events chan observability.Event
}

func newEventRecorder() *eventRecorder {
	return &eventRecorder{events: make(chan observability.Event, 64)}
}

func (recorder *eventRecorder) Record(event observability.Event) { recorder.events <- event }

type noContractCalls struct{}

func (noContractCalls) CallContract(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error) {
	return nil, errUnusedRPCMethod
}

func testQuorum(t *testing.T, readers ...*historicalProvider) *rpc.QuorumReader {
	t.Helper()
	providers := make([]rpc.Provider, len(readers))
	for index, reader := range readers {
		providers[index] = rpc.Provider{
			Identity: rpc.ProviderIdentity{
				ID:          fmt.Sprintf("fixture-provider-%d", index),
				Fingerprint: fmt.Sprintf("fixture-fingerprint-%d", index),
				TrustDomain: fmt.Sprintf("fixture-%d.invalid", index),
			},
			Reader: reader,
			Close:  func() {},
		}
	}
	quorum, err := rpc.NewQuorumReader(providers, quorumTimeout)
	if err != nil {
		t.Fatal(err)
	}
	return quorum
}

func newService(
	t *testing.T,
	network domain.Network,
	source common.Address,
	codec *contracts.ERC20Codec,
	finalized rpc.FinalizedReader,
	handoff *store.BoltStore,
	serviceClock clock.Clock,
	subscriptions interface {
		rpc.LogSubscriber
		rpc.HeadSubscriber
	},
) *watcher.Service {
	t.Helper()
	return newServiceWithObserver(t, network, source, codec, finalized, handoff, serviceClock, subscriptions, observability.Discard{})
}

func newServiceWithObserver(
	t *testing.T,
	network domain.Network,
	source common.Address,
	codec *contracts.ERC20Codec,
	finalized rpc.FinalizedReader,
	handoff *store.BoltStore,
	serviceClock clock.Clock,
	subscriptions interface {
		rpc.LogSubscriber
		rpc.HeadSubscriber
	},
	observer observability.Observer,
) *watcher.Service {
	t.Helper()
	logs, err := rpc.NewDeadlineLogSubscriber(subscriptions, quorumTimeout)
	if err != nil {
		t.Fatal(err)
	}
	heads, err := rpc.NewDeadlineHeadSubscriber(subscriptions, quorumTimeout)
	if err != nil {
		t.Fatal(err)
	}
	service, err := watcher.NewService(watcher.Dependencies{
		Contracts: noContractCalls{}, Finalized: finalized, LogSubscriber: logs, HeadSubscriber: heads,
		Codec: codec, Clock: serviceClock, Observer: observer, Store: handoff,
		Source: source, Network: network, Generation: 1, LookbackBlocks: 1, ReadTimeout: quorumTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testStoreOptions(network domain.Network, source common.Address, maxPending uint32) store.OpenOptions {
	return store.OpenOptions{
		Network:             network.ChainID,
		Source:              source,
		Sponsor:             testAddress(0xf1),
		Destination:         testAddress(0xf2),
		Rescuer:             testAddress(0xf3),
		PolicyFingerprint:   watcher.PolicyFingerprint(network, 1),
		MaxPending:          maxPending,
		MaxDiscoveredTokens: 16,
	}
}

func openStore(t *testing.T, path string, options store.OpenOptions) *store.BoltStore {
	t.Helper()
	handoff, err := store.Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	return handoff
}

func assertStoreState(t *testing.T, handoff *store.BoltStore, network domain.NetworkID, wantCursor store.Checkpoint, want []domain.RescueCandidate) {
	t.Helper()
	cursor, found, err := handoff.LoadScanCursor(context.Background(), network)
	wantCursorFound := wantCursor != (store.Checkpoint{})
	if err != nil || found != wantCursorFound || cursor != wantCursor {
		t.Fatalf("scan cursor = (%v, %v, %v), want (%v, %v, nil)", cursor, found, err, wantCursor, wantCursorFound)
	}
	replayed, err := handoff.Replay(context.Background(), network)
	if err != nil {
		t.Fatal(err)
	}
	gotCandidates := make(map[domain.CandidateID]domain.RescueCandidate, len(replayed))
	for _, candidate := range replayed {
		if _, duplicate := gotCandidates[candidate.ID]; duplicate {
			t.Fatalf("duplicate replayed candidate %s", candidate.ID)
		}
		gotCandidates[candidate.ID] = candidate
	}
	wantCandidates := make(map[domain.CandidateID]domain.RescueCandidate, len(want))
	for _, candidate := range want {
		wantCandidates[candidate.ID] = candidate
	}
	if !reflect.DeepEqual(gotCandidates, wantCandidates) {
		t.Fatalf("replayed candidates = %v, want %v", gotCandidates, wantCandidates)
	}
}

func assertConfirmedCheckpoint(t *testing.T, handoff *store.BoltStore, network domain.NetworkID, want store.Checkpoint) {
	t.Helper()
	got, found, err := handoff.LoadCheckpoint(context.Background(), network)
	wantFound := want != (store.Checkpoint{})
	if err != nil || found != wantFound || got != want {
		t.Fatalf("committed checkpoint = (%v, %v, %v), want (%v, %v, nil)", got, found, err, want, wantFound)
	}
}

func assertHandoffIncident(t *testing.T, handoff *store.BoltStore, want store.Incident) {
	t.Helper()
	got, found, err := handoff.IncidentByCandidate(context.Background(), want.Candidate)
	if err != nil || !found || got != want {
		t.Fatalf("handoff incident = (%v, %v, %v), want %v", got, found, err, want)
	}
}

func assertRescueIncident(t *testing.T, handoff *store.BoltStore, want store.RescueIncident) {
	t.Helper()
	got, found, err := handoff.RescueIncident(context.Background(), want.ID)
	if err != nil || !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("rescue incident = (%v, %v, %v), want %v", got, found, err, want)
	}
}

func handoffIncident(candidate domain.RescueCandidate, offset int) store.Incident {
	return store.Incident{
		ID:        domain.NewIncidentID(candidate.ID),
		Candidate: candidate.ID,
		Network:   candidate.Network,
		CreatedAt: time.Date(2026, time.August, 3, 13, 0, offset, 0, time.UTC),
	}
}

func rescueIncident(candidate domain.RescueCandidate, offset int) store.RescueIncident {
	asset := common.Address{}
	trusted := true
	if candidate.Kind == domain.CandidateToken {
		asset = candidate.Token.Address
		trusted = false
	}
	created := time.Date(2026, time.August, 3, 14, 0, offset, 0, time.UTC)
	return store.RescueIncident{
		ID:         domain.NewAssetIncidentID(candidate.ID, candidate.Kind, asset),
		Parent:     domain.NewIncidentID(candidate.ID),
		Candidate:  candidate.ID,
		Network:    candidate.Network,
		Kind:       candidate.Kind,
		Asset:      asset,
		Generation: 1,
		Trusted:    trusted,
		Policy: store.RescuePolicySnapshot{
			MaxAttempts: 3, RetryDelay: time.Second, FinalityTimeout: time.Minute,
		},
		Status:    store.RescuePending,
		CreatedAt: created,
		UpdatedAt: created,
	}
}

func checkpoint(network domain.NetworkID, header *types.Header) store.Checkpoint {
	return store.Checkpoint{Network: network, BlockNumber: header.Number.Uint64(), BlockHash: header.Hash()}
}

func containsCandidate(candidates []domain.RescueCandidate, id domain.CandidateID) bool {
	for _, candidate := range candidates {
		if candidate.ID == id {
			return true
		}
	}
	return false
}

func testHeader(number uint64, parent common.Hash, marker byte) *types.Header {
	return &types.Header{
		ParentHash: parent,
		Number:     new(big.Int).SetUint64(number),
		Difficulty: big.NewInt(1),
		GasLimit:   30_000_000,
		Time:       number * 10,
		Extra:      []byte{marker},
	}
}

func transferLog(codec *contracts.ERC20Codec, source, token common.Address, header *types.Header, index uint) types.Log {
	return types.Log{
		Address: token,
		Topics: []common.Hash{
			codec.TransferTopic(),
			testHash(byte(0x80 + index)),
			common.BytesToHash(source.Bytes()),
		},
		Data:           make([]byte, common.HashLength),
		BlockNumber:    header.Number.Uint64(),
		TxHash:         testHash(byte(header.Number.Uint64() + uint64(index) + 1)),
		BlockHash:      header.Hash(),
		BlockTimestamp: header.Time,
		Index:          index,
	}
}

func cloneLogs(logs []types.Log) []types.Log {
	cloned := make([]types.Log, len(logs))
	for index, logEntry := range logs {
		cloned[index] = logEntry
		cloned[index].Topics = append([]common.Hash(nil), logEntry.Topics...)
		cloned[index].Data = append([]byte(nil), logEntry.Data...)
	}
	return cloned
}

func testCodec(t *testing.T) *contracts.ERC20Codec {
	t.Helper()
	codec, err := contracts.NewERC20Codec()
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func waitTicker(t *testing.T, testClock *controlledClock, duration time.Duration) *controlledTicker {
	t.Helper()
	deadline := time.NewTimer(testTimeout)
	defer deadline.Stop()
	for {
		select {
		case ticker := <-testClock.created:
			if ticker.duration == duration {
				return ticker
			}
		case <-deadline.C:
			t.Fatalf("timer %s was not created", duration)
		}
	}
}

func waitEvent(t *testing.T, recorder *eventRecorder, matches func(observability.Event) bool) observability.Event {
	t.Helper()
	deadline := time.NewTimer(testTimeout)
	defer deadline.Stop()
	for {
		select {
		case event := <-recorder.events:
			if matches(event) {
				return event
			}
		case <-deadline.C:
			t.Fatal("expected watcher event was not recorded")
		}
	}
}

func waitEventCode(t *testing.T, recorder *eventRecorder, candidate domain.CandidateID, code observability.EventCode) {
	t.Helper()
	waitEvent(t, recorder, func(event observability.Event) bool {
		return event.Candidate == candidate && event.Code == code
	})
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testTimeout):
		t.Fatal("expected test signal not received")
	}
}

func waitResult[T any](t *testing.T, result <-chan T) T {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for test result")
		var zero T
		return zero
	}
}

func testAddress(marker byte) common.Address {
	var address common.Address
	address[len(address)-1] = marker
	return address
}

func testHash(marker byte) common.Hash {
	var hash common.Hash
	hash[len(hash)-1] = marker
	return hash
}
