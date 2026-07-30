package watcher

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"guard-daemon/internal/clock"
	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestServiceBuildsTransferQueryAndProducesSubscriptionCandidates(t *testing.T) {
	codec := newTestCodec(t)
	reader := &fakeReader{}
	logSource := newFakeLogSource(nil)
	headSource := newFakeHeadSource(nil)
	testClock := newFakeClock()
	queue := newFakeQueue()
	source := testAddress(0x11)
	token := domain.Token{Address: testAddress(0x22), Symbol: "LOCAL", Decimals: 6}
	network := domain.Network{Name: "test-network", ChainID: 31337, Tokens: []domain.Token{token}}
	service := newTestService(t, reader, logSource, headSource, testClock, queue, codec, source, network, 7)

	ctx, cancel := context.WithCancel(context.Background())
	done, runErr := runTestService(service, ctx)
	defer func() {
		cancel()
		<-done
	}()

	query := receive(t, logSource.queries)
	assertTransferQuery(t, query, codec.TransferTopic(), source)
	registration := receive(t, testClock.tickers)
	if registration.duration != periodicInterval {
		t.Fatalf("ticker duration = %s, want %s", registration.duration, periodicInterval)
	}

	logEntry := matchingLog(codec, source, token.Address, 41, 3)
	logSource.send(logEntry)
	got := receive(t, queue.candidates)
	want := domain.NewLogCandidate(network.ChainID, source, token, logEntry.BlockHash, logEntry.TxHash, logEntry.BlockNumber, logEntry.Index)
	if got != want {
		t.Fatalf("log candidate = %#v, want %#v", got, want)
	}

	header := &types.Header{Number: big.NewInt(42), ParentHash: testHash(0x31), Time: 1}
	headSource.send(header)
	got = receive(t, queue.candidates)
	want = domain.NewBlockCandidate(network.ChainID, domain.CandidateNative, source, header.Hash(), 42)
	if got != want {
		t.Fatalf("head candidate = %#v, want %#v", got, want)
	}

	registration.ticker.tick()
	got = receive(t, queue.candidates)
	want = domain.NewPeriodicCandidate(network.ChainID, source, 7, 0)
	if got != want {
		t.Fatalf("periodic candidate = %#v, want %#v", got, want)
	}

	cancel()
	<-done
	if !errors.Is(*runErr, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", *runErr)
	}
	if !logSource.subscription.unsubscribed.Load() || !headSource.subscription.unsubscribed.Load() || !registration.ticker.stopped.Load() {
		t.Fatal("Run() did not release subscriptions and ticker")
	}
}

func TestPollingStartsAtCurrentBlockAndReadsNextRange(t *testing.T) {
	codec := newTestCodec(t)
	reader := newPollingReader()
	logSource := newFakeLogSource(errors.New("subscription unavailable"))
	headSource := newFakeHeadSource(nil)
	testClock := newFakeClock()
	queue := newFakeQueue()
	source := testAddress(0x41)
	token := domain.Token{Address: testAddress(0x42), Symbol: "TEST", Decimals: 18}
	network := domain.Network{Name: "test-network", ChainID: 31337, Tokens: []domain.Token{token}}
	service := newTestService(t, reader, logSource, headSource, testClock, queue, codec, source, network, 2)

	ctx, cancel := context.WithCancel(context.Background())
	done, runErr := runTestService(service, ctx)
	defer func() {
		cancel()
		<-done
	}()

	<-logSource.queries
	registration := receive(t, testClock.tickers)
	if registration.duration != pollInterval {
		t.Fatalf("ticker duration = %s, want %s", registration.duration, pollInterval)
	}

	reader.blockNumbers <- 100
	registration.ticker.tick()
	<-reader.blockCalls
	select {
	case query := <-reader.filterQueries:
		t.Fatalf("initial block unexpectedly filtered with query %#v", query)
	default:
	}

	reader.blockNumbers <- 102
	registration.ticker.tick()
	query := receive(t, reader.filterQueries)
	if query.FromBlock == nil || query.FromBlock.Uint64() != 101 || query.ToBlock == nil || query.ToBlock.Uint64() != 102 {
		t.Fatalf("poll range = %v..%v, want 101..102", query.FromBlock, query.ToBlock)
	}
	assertTransferQuery(t, query, codec.TransferTopic(), source)

	headerCandidate := receive(t, queue.candidates)
	if headerCandidate.Kind != domain.CandidateNative || headerCandidate.BlockNumber != 102 {
		t.Fatalf("poll head candidate = %#v", headerCandidate)
	}
	logEntry := matchingLog(codec, source, token.Address, 101, 1)
	reader.filterResults <- []types.Log{logEntry}
	logCandidate := receive(t, queue.candidates)
	want := domain.NewLogCandidate(network.ChainID, source, token, logEntry.BlockHash, logEntry.TxHash, logEntry.BlockNumber, logEntry.Index)
	if logCandidate != want {
		t.Fatalf("poll log candidate = %#v, want %#v", logCandidate, want)
	}

	cancel()
	<-done
	if !errors.Is(*runErr, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", *runErr)
	}
}

func TestUnknownMetadataIsSanitizedAndNotCached(t *testing.T) {
	codec := newTestCodec(t)
	symbolData := encodeABIValue(t, "string", "  LONG\nTOKEN\x1bVALUE-EXTRA  ")
	decimalsData := make([]byte, 32)
	decimalsData[len(decimalsData)-1] = 6
	reader := &fakeReader{call: func(_ context.Context, message ethereum.CallMsg, _ *big.Int) ([]byte, error) {
		if reflect.DeepEqual(message.Data, mustPack(t, codec.PackSymbol)) {
			return symbolData, nil
		}
		return decimalsData, nil
	}}
	service := newTestService(
		t,
		reader,
		newFakeLogSource(nil),
		newFakeHeadSource(nil),
		newFakeClock(),
		newFakeQueue(),
		codec,
		testAddress(0x51),
		domain.Network{Name: "test-network", ChainID: 31337},
		1,
	)
	unknown := testAddress(0x52)

	first := service.resolveToken(context.Background(), unknown)
	second := service.resolveToken(context.Background(), unknown)
	want := domain.Token{Address: unknown, Symbol: "LONGTOKENVALUE-E...", Decimals: 6}
	if first != want || second != want {
		t.Fatalf("resolved tokens = %#v, %#v, want %#v", first, second, want)
	}
	if reader.callCount.Load() != 4 {
		t.Fatalf("metadata call count = %d, want 4 (unknown metadata must not be cached)", reader.callCount.Load())
	}

	failingReader := &fakeReader{call: func(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error) {
		return nil, errors.New("metadata unavailable")
	}}
	fallbackService := newTestService(
		t,
		failingReader,
		newFakeLogSource(nil),
		newFakeHeadSource(nil),
		newFakeClock(),
		newFakeQueue(),
		codec,
		testAddress(0x61),
		domain.Network{Name: "test-network", ChainID: 31337},
		1,
	)
	fallback := fallbackService.resolveToken(context.Background(), unknown)
	if fallback.Symbol != addressFallback(unknown) || fallback.Decimals != 18 {
		t.Fatalf("metadata fallback = %#v", fallback)
	}
}

func TestCancellationStopsPollingWithoutAnotherTick(t *testing.T) {
	service := newTestService(
		t,
		&fakeReader{},
		newFakeLogSource(errors.New("subscription unavailable")),
		newFakeHeadSource(nil),
		newFakeClock(),
		newFakeQueue(),
		newTestCodec(t),
		testAddress(0x71),
		domain.Network{Name: "test-network", ChainID: 31337},
		1,
	)

	ctx, cancel := context.WithCancel(context.Background())
	done, runErr := runTestService(service, ctx)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}
	if !errors.Is(*runErr, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", *runErr)
	}
}

func TestActiveLogSubscriptionFailureEndsGeneration(t *testing.T) {
	codec := newTestCodec(t)
	logSource := newFakeLogSource(nil)
	testClock := newFakeClock()
	service := newTestService(
		t,
		&fakeReader{},
		logSource,
		newFakeHeadSource(nil),
		testClock,
		newFakeQueue(),
		codec,
		testAddress(0x72),
		domain.Network{Name: "test-network", ChainID: 31337},
		1,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done, runErr := runTestService(service, ctx)
	receive(t, logSource.queries)
	receive(t, testClock.tickers)
	logSource.subscription.errors <- errors.New("test disconnect")
	receive(t, done)

	var classified *domain.ClassifiedError
	if !errors.As(*runErr, &classified) || classified.Code != errorLogSubscription {
		t.Fatalf("Run() error = %v, want classified subscription failure", *runErr)
	}
}

type fakeReader struct {
	call      func(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error)
	callCount atomic.Int32
}

func (reader *fakeReader) CallContract(ctx context.Context, message ethereum.CallMsg, block *big.Int) ([]byte, error) {
	reader.callCount.Add(1)
	if reader.call == nil {
		return nil, errors.New("unexpected contract call")
	}
	return reader.call(ctx, message, block)
}

func (*fakeReader) FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
	return nil, errors.New("unexpected log read")
}

func (*fakeReader) BlockNumber(context.Context) (uint64, error) {
	return 0, errors.New("unexpected block read")
}

func (*fakeReader) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	return nil, errors.New("unexpected header read")
}

type pollingReader struct {
	blockNumbers  chan uint64
	blockCalls    chan struct{}
	filterQueries chan ethereum.FilterQuery
	filterResults chan []types.Log
}

func newPollingReader() *pollingReader {
	return &pollingReader{
		blockNumbers:  make(chan uint64, 2),
		blockCalls:    make(chan struct{}, 2),
		filterQueries: make(chan ethereum.FilterQuery, 1),
		filterResults: make(chan []types.Log, 1),
	}
}

func (*pollingReader) CallContract(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error) {
	return nil, errors.New("unexpected contract call")
}

func (reader *pollingReader) BlockNumber(ctx context.Context) (uint64, error) {
	select {
	case block := <-reader.blockNumbers:
		reader.blockCalls <- struct{}{}
		return block, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (*pollingReader) HeaderByNumber(_ context.Context, number *big.Int) (*types.Header, error) {
	return &types.Header{Number: new(big.Int).Set(number), ParentHash: testHash(0x81), Time: 1}, nil
}

func (reader *pollingReader) FilterLogs(ctx context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	reader.filterQueries <- query
	select {
	case logs := <-reader.filterResults:
		return logs, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type fakeQueue struct {
	candidates chan domain.RescueCandidate
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{candidates: make(chan domain.RescueCandidate, 16)}
}

func (queue *fakeQueue) Put(ctx context.Context, candidate domain.RescueCandidate) (store.PutResult, error) {
	select {
	case queue.candidates <- candidate:
		return store.PutInserted, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

type fakeSubscription struct {
	errors       chan error
	unsubscribed atomic.Bool
	once         sync.Once
}

func newFakeSubscription() *fakeSubscription {
	return &fakeSubscription{errors: make(chan error)}
}

func (subscription *fakeSubscription) Err() <-chan error { return subscription.errors }

func (subscription *fakeSubscription) Unsubscribe() {
	subscription.once.Do(func() {
		subscription.unsubscribed.Store(true)
		close(subscription.errors)
	})
}

type fakeLogSource struct {
	queries      chan ethereum.FilterQuery
	subscribeErr error
	subscription *fakeSubscription
	logs         chan<- types.Log
}

func newFakeLogSource(subscribeErr error) *fakeLogSource {
	return &fakeLogSource{
		queries:      make(chan ethereum.FilterQuery, 1),
		subscribeErr: subscribeErr,
		subscription: newFakeSubscription(),
	}
}

func (source *fakeLogSource) SubscribeFilterLogs(_ context.Context, query ethereum.FilterQuery, logs chan<- types.Log) (ethereum.Subscription, error) {
	source.logs = logs
	source.queries <- query
	if source.subscribeErr != nil {
		return nil, source.subscribeErr
	}
	return source.subscription, nil
}

func (source *fakeLogSource) send(logEntry types.Log) { source.logs <- logEntry }

type fakeHeadSource struct {
	subscribeErr error
	subscription *fakeSubscription
	heads        chan<- *types.Header
}

func newFakeHeadSource(subscribeErr error) *fakeHeadSource {
	return &fakeHeadSource{subscribeErr: subscribeErr, subscription: newFakeSubscription()}
}

func (source *fakeHeadSource) SubscribeNewHead(_ context.Context, heads chan<- *types.Header) (ethereum.Subscription, error) {
	source.heads = heads
	if source.subscribeErr != nil {
		return nil, source.subscribeErr
	}
	return source.subscription, nil
}

func (source *fakeHeadSource) send(header *types.Header) { source.heads <- header }

type tickerRegistration struct {
	duration time.Duration
	ticker   *fakeTicker
}

type fakeClock struct {
	tickers chan tickerRegistration
}

func newFakeClock() *fakeClock {
	return &fakeClock{tickers: make(chan tickerRegistration, 4)}
}

func (*fakeClock) Now() time.Time { return time.Unix(0, 0) }

func (*fakeClock) Sleep(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

func (testClock *fakeClock) NewTicker(duration time.Duration) clock.Ticker {
	ticker := &fakeTicker{ticks: make(chan time.Time, 8)}
	testClock.tickers <- tickerRegistration{duration: duration, ticker: ticker}
	return ticker
}

func (*fakeClock) NewTimer(time.Duration) clock.Timer {
	return &fakeTicker{ticks: make(chan time.Time, 1)}
}

type fakeTicker struct {
	ticks   chan time.Time
	stopped atomic.Bool
}

func (ticker *fakeTicker) C() <-chan time.Time { return ticker.ticks }
func (ticker *fakeTicker) Stop()               { ticker.stopped.Store(true) }
func (ticker *fakeTicker) tick()               { ticker.ticks <- time.Unix(0, 0) }

func newTestService(
	t *testing.T,
	reader interface {
		rpcReader
		BlockReader
	},
	logSource *fakeLogSource,
	headSource *fakeHeadSource,
	testClock *fakeClock,
	queue *fakeQueue,
	codec *contracts.ERC20Codec,
	source common.Address,
	network domain.Network,
	generation uint64,
) *Service {
	t.Helper()
	service, err := NewService(Dependencies{
		Contracts:      reader,
		Logs:           reader,
		Blocks:         reader,
		LogSubscriber:  logSource,
		HeadSubscriber: headSource,
		Codec:          codec,
		Clock:          testClock,
		Observer:       observability.Discard{},
		Queue:          queue,
		Source:         source,
		Network:        network,
		Generation:     generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type rpcReader interface {
	CallContract(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error)
	FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error)
}

func runTestService(service *Service, ctx context.Context) (<-chan struct{}, *error) {
	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = service.Run(ctx)
		close(done)
	}()
	return done, &runErr
}

func assertTransferQuery(t *testing.T, query ethereum.FilterQuery, transferTopic common.Hash, source common.Address) {
	t.Helper()
	wantTopics := [][]common.Hash{{transferTopic}, nil, {common.BytesToHash(source.Bytes())}}
	if len(query.Addresses) != 0 || !reflect.DeepEqual(query.Topics, wantTopics) {
		t.Fatalf("transfer query = %#v, want no addresses and topics %#v", query, wantTopics)
	}
}

func matchingLog(codec *contracts.ERC20Codec, source, token common.Address, blockNumber uint64, index uint) types.Log {
	return types.Log{
		Address:     token,
		Topics:      []common.Hash{codec.TransferTopic(), testHash(0x91), common.BytesToHash(source.Bytes())},
		BlockNumber: blockNumber,
		BlockHash:   testHash(byte(blockNumber)),
		TxHash:      testHash(byte(blockNumber + 1)),
		Index:       index,
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

func mustPack(t *testing.T, pack func() ([]byte, error)) []byte {
	t.Helper()
	data, err := pack()
	if err != nil {
		t.Fatal(err)
	}
	return data
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

func testAddress(lastByte byte) common.Address {
	var address common.Address
	address[len(address)-1] = lastByte
	return address
}

func testHash(lastByte byte) common.Hash {
	var hash common.Hash
	hash[len(hash)-1] = lastByte
	return hash
}
