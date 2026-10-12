package watcher

import (
	"context"
	"errors"
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

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestScanUsesBoundedLookbackAndRetriesFailedBlock(t *testing.T) {
	source := testAddress(0x11)
	token := domain.Token{Address: testAddress(0x12), Symbol: "LOCAL", Decimals: 6}
	network := domain.Network{Name: "local", ChainID: 31337, Tokens: []domain.Token{token}}
	finalized := newFakeFinalized()
	finalized.addBlock(99, testHash(0x63), testHash(0x62))
	finalized.addBlock(100, testHash(0x64), testHash(0x63))
	finalized.addBlock(101, testHash(0x65), testHash(0x64))
	logEntry := matchingLog(newTestCodec(t), source, token.Address, 100, testHash(0x64), 1)
	finalized.setLogs(100, []types.Log{logEntry, logEntry})

	path := filepath.Join(t.TempDir(), "watcher.db")
	handoff := openWatchStore(t, path, source, network, 2)
	service := newTestService(t, finalized, handoff, source, network, 2, &fakeContractCaller{}, newFakeLogSubscriber(), newFakeHeadSubscriber())
	if err := service.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCursor(t, handoff, network.ChainID, 101, testHash(0x65))
	if !finalized.usedHashPinnedQuery(100) || !finalized.usedHashPinnedQuery(101) {
		t.Fatal("scanner requested logs by number instead of the agreed block hash")
	}
	assertCandidateIDs(t, handoff, network.ChainID,
		domain.NewBlockCandidate(network.ChainID, domain.CandidateNative, source, testHash(0x64), 100).ID,
		domain.NewLogCandidate(network.ChainID, source, token, testHash(0x64), logEntry.TxHash, 100, 1).ID,
		domain.NewBlockCandidate(network.ChainID, domain.CandidateNative, source, testHash(0x65), 101).ID,
	)

	if err := handoff.Close(); err != nil {
		t.Fatal(err)
	}
	handoff = openWatchStore(t, path, source, network, 2)
	defer handoff.Close()
	service = newTestService(t, finalized, handoff, source, network, 2, &fakeContractCaller{}, newFakeLogSubscriber(), newFakeHeadSubscriber())
	finalized.addBlock(102, testHash(0x66), testHash(0x65))
	finalized.setFilterError(102, errors.New("private backend detail"))
	if err := service.scan(context.Background()); err == nil {
		t.Fatal("FilterLogs error did not stop scanning")
	}
	assertCursor(t, handoff, network.ChainID, 101, testHash(0x65))

	finalized.setFilterError(102, nil)
	if err := service.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCursor(t, handoff, network.ChainID, 102, testHash(0x66))
	if finalized.filterCalls(102) != 2 {
		t.Fatalf("FilterLogs calls for uncommitted block = %d, want 2", finalized.filterCalls(102))
	}
}

func TestReadyRunsOnlyAfterInitialScan(t *testing.T) {
	source := testAddress(0x19)
	network := domain.Network{Name: "local", ChainID: 31337, AllowUnknownTokens: true}
	finalized := newFakeFinalized()
	finalized.addBlock(7, testHash(0x07), testHash(0x06))
	handoff := openWatchStore(t, filepath.Join(t.TempDir(), "watcher.db"), source, network, 1)
	defer handoff.Close()
	service := newTestService(t, finalized, handoff, source, network, 1, &fakeContractCaller{}, newFakeLogSubscriber(), newFakeHeadSubscriber())
	ready := make(chan struct{})
	service.ready = func() error {
		cursor, found, err := handoff.LoadScanCursor(context.Background(), network.ChainID)
		if err != nil || !found || cursor.BlockNumber != 7 || cursor.BlockHash != testHash(0x07) {
			return errors.New("readiness signal invoked before durable initial scan")
		}
		close(ready)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- service.Run(ctx) }()
	receive(t, ready)
	cancel()
	if err := receive(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after cancellation = %v", err)
	}
}

func TestSubscriptionDisconnectBackfillsGapWithoutDuplicates(t *testing.T) {
	source := testAddress(0x21)
	token := domain.Token{Address: testAddress(0x22), Symbol: "LOCAL", Decimals: 18}
	network := domain.Network{Name: "local", ChainID: 31337, Tokens: []domain.Token{token}}
	codec := newTestCodec(t)
	finalized := newFakeFinalized()
	finalized.addBlock(10, testHash(0x0a), testHash(0x09))
	firstLog := matchingLog(codec, source, token.Address, 10, testHash(0x0a), 1)
	finalized.setLogs(10, []types.Log{firstLog})
	handoff := openWatchStore(t, filepath.Join(t.TempDir(), "watcher.db"), source, network, 1)
	defer handoff.Close()
	logs := newFakeLogSubscriber()
	heads := newFakeHeadSubscriber()
	service := newTestService(t, finalized, handoff, source, network, 1, &fakeContractCaller{}, logs, heads)

	runResult := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { runResult <- service.Run(ctx) }()
	receive(t, logs.registered)

	// Событие подписки для кандидата, уже подтверждённого канонической цепочкой,
	// безопасно объединяется с существующей записью.
	logs.send(firstLog)
	gapLog := matchingLog(codec, source, token.Address, 11, testHash(0x0b), 2)
	finalized.addBlockWithLogs(11, testHash(0x0b), testHash(0x0a), []types.Log{gapLog})
	logs.subscription.fail(errors.New("disconnect"))
	if err := receive(t, runResult); err == nil {
		t.Fatal("disconnect did not end the generation")
	}

	service = newTestService(t, finalized, handoff, source, network, 1, &fakeContractCaller{}, newFakeLogSubscriber(), newFakeHeadSubscriber())
	if err := service.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCursor(t, handoff, network.ChainID, 11, testHash(0x0b))
	assertCandidateIDs(t, handoff, network.ChainID,
		domain.NewBlockCandidate(network.ChainID, domain.CandidateNative, source, testHash(0x0a), 10).ID,
		domain.NewLogCandidate(network.ChainID, source, token, testHash(0x0a), firstLog.TxHash, 10, 1).ID,
		domain.NewBlockCandidate(network.ChainID, domain.CandidateNative, source, testHash(0x0b), 11).ID,
		domain.NewLogCandidate(network.ChainID, source, token, testHash(0x0b), gapLog.TxHash, 11, 2).ID,
	)
	if !logs.subscription.unsubscribed.Load() || !heads.subscription.unsubscribed.Load() {
		t.Fatal("generation change did not release subscriptions")
	}
}

func TestProvisionalRemovedAndOrphanedLogsNeverBecomeReady(t *testing.T) {
	source := testAddress(0x31)
	network := domain.Network{Name: "local", ChainID: 31337, AllowUnknownTokens: true}
	codec := newTestCodec(t)
	finalized := newFakeFinalized()
	canonicalHash := testHash(0x32)
	finalized.addBlock(20, canonicalHash, testHash(0x1f))
	handoff := openWatchStore(t, filepath.Join(t.TempDir(), "watcher.db"), source, network, 1)
	defer handoff.Close()
	service := newTestService(t, finalized, handoff, source, network, 1, &fakeContractCaller{}, newFakeLogSubscriber(), newFakeHeadSubscriber())

	orphan := matchingLog(codec, source, testAddress(0x33), 20, testHash(0xee), 1)
	if err := service.observeLog(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}
	removed := matchingLog(codec, source, testAddress(0x34), 20, testHash(0xef), 2)
	removed.Removed = true
	if err := service.observeLog(context.Background(), removed); err != nil {
		t.Fatal(err)
	}
	assertCandidateIDs(t, handoff, network.ChainID)

	if err := service.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCandidateIDs(t, handoff, network.ChainID,
		domain.NewBlockCandidate(network.ChainID, domain.CandidateNative, source, canonicalHash, 20).ID,
	)
}

func TestCanonicalScanPromotesProvisionalCandidate(t *testing.T) {
	source := testAddress(0x41)
	token := domain.Token{Address: testAddress(0x42), Symbol: "TOK", Decimals: 18}
	network := domain.Network{Name: "local", ChainID: 31337, Tokens: []domain.Token{token}}
	codec := newTestCodec(t)
	finalized := newFakeFinalized()
	finalized.addBlock(30, testHash(0x30), testHash(0x2f))
	logEntry := matchingLog(codec, source, token.Address, 30, testHash(0x30), 3)
	finalized.setLogs(30, []types.Log{logEntry})
	handoff := openWatchStore(t, filepath.Join(t.TempDir(), "watcher.db"), source, network, 1)
	defer handoff.Close()
	service := newTestService(t, finalized, handoff, source, network, 1, &fakeContractCaller{}, newFakeLogSubscriber(), newFakeHeadSubscriber())
	if err := service.observeLog(context.Background(), logEntry); err != nil {
		t.Fatal(err)
	}
	assertCandidateIDs(t, handoff, network.ChainID)
	if err := service.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertCandidateIDs(t, handoff, network.ChainID,
		domain.NewBlockCandidate(network.ChainID, domain.CandidateNative, source, testHash(0x30), 30).ID,
		domain.NewLogCandidate(network.ChainID, source, token, testHash(0x30), logEntry.TxHash, 30, 3).ID,
	)
}

func TestLogCandidateRequiresStrictTransferShape(t *testing.T) {
	source := testAddress(0x43)
	codec := newTestCodec(t)
	service := &Service{
		networkID:    31337,
		source:       source,
		codec:        codec,
		allowUnknown: true,
		knownTokens:  make(map[common.Address]domain.Token),
	}
	valid := matchingLog(codec, source, testAddress(0x44), 1, testHash(1), 1)
	tests := map[string]types.Log{
		"extra topic": func() types.Log {
			entry := valid
			entry.Topics = append(append([]common.Hash(nil), valid.Topics...), testHash(4))
			return entry
		}(),
		"short data": func() types.Log {
			entry := valid
			entry.Data = make([]byte, common.HashLength-1)
			return entry
		}(),
	}
	for name, logEntry := range tests {
		t.Run(name, func(t *testing.T) {
			if _, accepted := service.logCandidate(context.Background(), logEntry, false); accepted {
				t.Fatal("non-strict Transfer log accepted")
			}
		})
	}
}

func TestUnknownMetadataHasDeadlineSizeAndCacheBounds(t *testing.T) {
	source := testAddress(0x51)
	network := domain.Network{Name: "local", ChainID: 31337, AllowUnknownTokens: true}
	handoff := openWatchStore(t, filepath.Join(t.TempDir(), "watcher.db"), source, network, 1)
	defer handoff.Close()
	caller := &fakeContractCaller{call: func(ctx context.Context, _ ethereum.CallMsg, _ *big.Int) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("metadata call did not receive a deadline")
		}
		return make([]byte, metadataReturnLimit+1), nil
	}}
	service := newTestService(t, newFakeFinalized(), handoff, source, network, 1, caller, newFakeLogSubscriber(), newFakeHeadSubscriber())
	unknown := testAddress(0x52)
	first := service.resolveToken(context.Background(), unknown)
	second := service.resolveToken(context.Background(), unknown)
	if first != second || first.Symbol != addressFallback(unknown) || caller.calls.Load() != 2 {
		t.Fatalf("bounded fallback metadata and cache = (%#v, %#v, calls=%d)", first, second, caller.calls.Load())
	}

	encodedSymbol := encodeABIValue(t, "string", "TOKEN")
	encodedDecimals := make([]byte, 32)
	encodedDecimals[31] = 6
	caller.call = func(_ context.Context, message ethereum.CallMsg, _ *big.Int) ([]byte, error) {
		if len(message.Data) != 0 && message.Data[len(message.Data)-1] == service.codecSymbolSelectorLastByte(t) {
			return encodedSymbol, nil
		}
		return encodedDecimals, nil
	}
	for index := 0; index < metadataCacheLimit+20; index++ {
		address := common.BigToAddress(big.NewInt(int64(1000 + index)))
		service.resolveToken(context.Background(), address)
	}
	if len(service.metadata) != metadataCacheLimit || len(service.metadataOrder) != metadataCacheLimit {
		t.Fatalf("metadata cache size = %d/%d, want %d", len(service.metadata), len(service.metadataOrder), metadataCacheLimit)
	}
}

func TestReconciliationRestoresDiscoveredUnknownToken(t *testing.T) {
	source := testAddress(0x61)
	network := domain.Network{Name: "local", ChainID: 31337, AllowUnknownTokens: true}
	handoff := openWatchStore(t, filepath.Join(t.TempDir(), "watcher.db"), source, network, 1)
	defer handoff.Close()
	finalized := newFakeFinalized()
	finalized.addBlock(1, testHash(0x01), testHash(0x02))
	service := newTestService(t, finalized, handoff, source, network, 1, &fakeContractCaller{}, newFakeLogSubscriber(), newFakeHeadSubscriber())
	logEntry := matchingLog(newTestCodec(t), source, testAddress(0x62), 1, testHash(0x01), 1)
	finalized.setLogs(1, []types.Log{logEntry})
	if err := service.observeLog(context.Background(), logEntry); err != nil {
		t.Fatal(err)
	}
	if discovered, err := handoff.DiscoveredTokens(context.Background(), network.ChainID); err != nil || len(discovered) != 0 {
		t.Fatalf("preliminary discovery = (%v, %v), want empty confirmed registry", discovered, err)
	}
	if err := service.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	candidates, err := handoff.Replay(context.Background(), network.ChainID)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Kind != domain.CandidatePeriodic {
		t.Fatalf("reconciliation candidates before canonical commit = %v", candidates)
	}
	if err := service.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if discovered, err := handoff.DiscoveredTokens(context.Background(), network.ChainID); err != nil || !reflect.DeepEqual(discovered, []common.Address{logEntry.Address}) {
		t.Fatalf("confirmed discovery = (%v, %v)", discovered, err)
	}
}

func TestObservationSaturationDoesNotBlockCanonicalScanner(t *testing.T) {
	source := testAddress(0x63)
	network := domain.Network{Name: "local", ChainID: 31337, AllowUnknownTokens: true}
	handoff, err := store.Open(filepath.Join(t.TempDir(), "watcher.db"), store.OpenOptions{
		Network: network.ChainID, Source: source, Sponsor: testAddress(0xfa), Destination: testAddress(0xfb), Rescuer: testAddress(0xfc), PolicyFingerprint: PolicyFingerprint(network, 1),
		MaxPending: 1, MaxDiscoveredTokens: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handoff.Close()
	finalized := newFakeFinalized()
	finalized.addBlock(5, testHash(0x65), testHash(0x64))
	service := newTestService(t, finalized, handoff, source, network, 1, &fakeContractCaller{}, newFakeLogSubscriber(), newFakeHeadSubscriber())
	codec := newTestCodec(t)
	first := matchingLog(codec, source, testAddress(0x66), 5, testHash(0xee), 1)
	second := matchingLog(codec, source, testAddress(0x67), 5, testHash(0xef), 2)
	if err := service.observeLog(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := service.observeLog(context.Background(), second); err != nil {
		t.Fatalf("saturated untrusted hint blocked watcher: %v", err)
	}
	if err := service.scan(context.Background()); err != nil {
		t.Fatalf("canonical scanner did not recover after hint saturation: %v", err)
	}
	assertCursor(t, handoff, network.ChainID, 5, testHash(0x65))
}

func TestPolicyFingerprintIsDeterministicAndPolicyBound(t *testing.T) {
	first := domain.Token{Address: testAddress(1)}
	second := domain.Token{Address: testAddress(2)}
	network := domain.Network{ChainID: 31337, Tokens: []domain.Token{first, second}}
	fingerprint := PolicyFingerprint(network, 64)
	network.Tokens = []domain.Token{second, first}
	if fingerprint != PolicyFingerprint(network, 64) {
		t.Fatal("token configuration order changed fingerprint")
	}
	if fingerprint == PolicyFingerprint(network, 65) {
		t.Fatal("lookback depth is not bound to fingerprint")
	}
	network.AllowUnknownTokens = true
	if fingerprint == PolicyFingerprint(network, 64) {
		t.Fatal("token mode is not bound to fingerprint")
	}
}

func (service *Service) codecSymbolSelectorLastByte(t *testing.T) byte {
	t.Helper()
	data, err := service.codec.PackSymbol()
	if err != nil || len(data) == 0 {
		t.Fatal(err)
	}
	return data[len(data)-1]
}

type fakeFinalized struct {
	mu          sync.Mutex
	latest      uint64
	blocks      map[uint64]rpc.BlockRef
	logs        map[uint64][]types.Log
	filterError map[uint64]error
	filterCount map[uint64]int
	hashPinned  map[uint64]bool
}

func newFakeFinalized() *fakeFinalized {
	return &fakeFinalized{
		blocks:      make(map[uint64]rpc.BlockRef),
		logs:        make(map[uint64][]types.Log),
		filterError: make(map[uint64]error),
		filterCount: make(map[uint64]int),
		hashPinned:  make(map[uint64]bool),
	}
}

func (reader *fakeFinalized) addBlock(number uint64, hash, parent common.Hash) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.blocks[number] = rpc.BlockRef{Number: number, Hash: hash, ParentHash: parent}
	if number > reader.latest || len(reader.blocks) == 1 {
		reader.latest = number
	}
}

func (reader *fakeFinalized) addBlockWithLogs(number uint64, hash, parent common.Hash, logs []types.Log) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.blocks[number] = rpc.BlockRef{Number: number, Hash: hash, ParentHash: parent}
	reader.logs[number] = append([]types.Log(nil), logs...)
	if number > reader.latest || len(reader.blocks) == 1 {
		reader.latest = number
	}
}

func (reader *fakeFinalized) setFilterError(number uint64, err error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.filterError[number] = err
}

func (reader *fakeFinalized) setLogs(number uint64, logs []types.Log) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.logs[number] = append([]types.Log(nil), logs...)
}

func (reader *fakeFinalized) filterCalls(number uint64) int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.filterCount[number]
}

func (reader *fakeFinalized) usedHashPinnedQuery(number uint64) bool {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.hashPinned[number]
}

func (reader *fakeFinalized) Finalized(context.Context) (rpc.BlockRef, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	block, ok := reader.blocks[reader.latest]
	if !ok {
		return rpc.BlockRef{}, errors.New("finalized unavailable")
	}
	return block, nil
}

func (reader *fakeFinalized) Header(_ context.Context, number uint64) (rpc.BlockRef, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	block, ok := reader.blocks[number]
	if !ok {
		return rpc.BlockRef{}, errors.New("header unavailable")
	}
	return block, nil
}

func (reader *fakeFinalized) FilterLogs(_ context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	var number uint64
	if query.BlockHash != nil && query.FromBlock == nil && query.ToBlock == nil {
		found := false
		for candidateNumber, block := range reader.blocks {
			if block.Hash == *query.BlockHash {
				number = candidateNumber
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("unknown block hash")
		}
		reader.hashPinned[number] = true
	} else {
		if query.FromBlock == nil || query.ToBlock == nil || query.FromBlock.Cmp(query.ToBlock) != 0 || !query.FromBlock.IsUint64() {
			return nil, errors.New("invalid range")
		}
		number = query.FromBlock.Uint64()
	}
	reader.filterCount[number]++
	if err := reader.filterError[number]; err != nil {
		return nil, err
	}
	return append([]types.Log{}, reader.logs[number]...), nil
}

type fakeContractCaller struct {
	call  func(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error)
	calls atomic.Int32
}

func (caller *fakeContractCaller) CallContract(ctx context.Context, message ethereum.CallMsg, number *big.Int) ([]byte, error) {
	caller.calls.Add(1)
	if caller.call == nil {
		return nil, errors.New("metadata unavailable")
	}
	return caller.call(ctx, message, number)
}

type fakeSubscription struct {
	errors       chan error
	unsubscribed atomic.Bool
	once         sync.Once
}

func newFakeSubscription() *fakeSubscription {
	return &fakeSubscription{errors: make(chan error, 1)}
}

func (subscription *fakeSubscription) Err() <-chan error { return subscription.errors }
func (subscription *fakeSubscription) Unsubscribe() {
	subscription.once.Do(func() {
		subscription.unsubscribed.Store(true)
		close(subscription.errors)
	})
}
func (subscription *fakeSubscription) fail(err error) { subscription.errors <- err }

type fakeLogSubscriber struct {
	registered   chan ethereum.FilterQuery
	subscription *fakeSubscription
	logs         chan<- types.Log
}

func newFakeLogSubscriber() *fakeLogSubscriber {
	return &fakeLogSubscriber{registered: make(chan ethereum.FilterQuery, 1), subscription: newFakeSubscription()}
}

func (source *fakeLogSubscriber) SubscribeFilterLogs(_ context.Context, query ethereum.FilterQuery, logs chan<- types.Log) (ethereum.Subscription, error) {
	source.logs = logs
	source.registered <- query
	return source.subscription, nil
}
func (source *fakeLogSubscriber) send(logEntry types.Log) { source.logs <- logEntry }

type fakeHeadSubscriber struct {
	subscription *fakeSubscription
}

func newFakeHeadSubscriber() *fakeHeadSubscriber {
	return &fakeHeadSubscriber{subscription: newFakeSubscription()}
}
func (source *fakeHeadSubscriber) SubscribeNewHead(context.Context, chan<- *types.Header) (ethereum.Subscription, error) {
	return source.subscription, nil
}

func newTestService(
	t *testing.T,
	finalized rpc.FinalizedReader,
	handoff WatchStore,
	source common.Address,
	network domain.Network,
	lookback uint64,
	caller rpc.ContractCaller,
	logs rpc.LogSubscriber,
	heads rpc.HeadSubscriber,
) *Service {
	t.Helper()
	service, err := NewService(Dependencies{
		Contracts: caller, Finalized: finalized, LogSubscriber: logs, HeadSubscriber: heads,
		Codec: newTestCodec(t), Clock: clock.Real{}, Observer: observability.Discard{}, Store: handoff,
		Source: source, Network: network, Generation: 1, LookbackBlocks: lookback, ReadTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func openWatchStore(t *testing.T, path string, source common.Address, network domain.Network, lookback uint64) *store.BoltStore {
	t.Helper()
	handoff, err := store.Open(path, store.OpenOptions{
		Network: network.ChainID, Source: source, Sponsor: testAddress(0xfa), Destination: testAddress(0xfb), Rescuer: testAddress(0xfc), PolicyFingerprint: PolicyFingerprint(network, lookback),
		MaxPending: 64, MaxDiscoveredTokens: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handoff
}

func assertCursor(t *testing.T, handoff *store.BoltStore, network domain.NetworkID, number uint64, hash common.Hash) {
	t.Helper()
	cursor, found, err := handoff.LoadScanCursor(context.Background(), network)
	if err != nil || !found || cursor.BlockNumber != number || cursor.BlockHash != hash {
		t.Fatalf("scan cursor = (%v, %v, %v), want block %d %s", cursor, found, err, number, hash)
	}
}

func assertCandidateIDs(t *testing.T, handoff *store.BoltStore, network domain.NetworkID, want ...domain.CandidateID) {
	t.Helper()
	candidates, err := handoff.Replay(context.Background(), network)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[domain.CandidateID]int, len(candidates))
	for _, candidate := range candidates {
		got[candidate.ID]++
	}
	wanted := make(map[domain.CandidateID]int, len(want))
	for _, id := range want {
		wanted[id]++
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("candidate IDs = %v, want %v", got, wanted)
	}
}

func matchingLog(codec *contracts.ERC20Codec, source, token common.Address, number uint64, blockHash common.Hash, index uint) types.Log {
	return types.Log{
		Address: token,
		Topics:  []common.Hash{codec.TransferTopic(), testHash(0x91), common.BytesToHash(source.Bytes())},
		Data:    make([]byte, 32), BlockNumber: number, BlockHash: blockHash,
		TxHash: testHash(byte(number + 1)), Index: index,
	}
}

func newTestCodec(t *testing.T) *contracts.ERC20Codec {
	t.Helper()
	codec, err := contracts.NewERC20Codec()
	if err != nil {
		t.Fatal(err)
	}
	return codec
}

func encodeABIValue(t *testing.T, kind string, value any) []byte {
	t.Helper()
	typeDefinition, err := abi.NewType(kind, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := (abi.Arguments{{Type: typeDefinition}}).Pack(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func receive[T any](t *testing.T, channel <-chan T) T {
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

func testAddress(value byte) common.Address {
	var address common.Address
	address[len(address)-1] = value
	return address
}

func testHash(value byte) common.Hash {
	var hash common.Hash
	hash[len(hash)-1] = value
	return hash
}
