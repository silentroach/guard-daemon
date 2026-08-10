package rpc

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type deadlineProbeReader struct {
	mu       sync.Mutex
	contexts []context.Context
}

type deadlineProbeBroadcaster struct {
	ctx   context.Context
	block bool
}

func (broadcaster *deadlineProbeBroadcaster) SendTransaction(ctx context.Context, _ *types.Transaction) error {
	broadcaster.ctx = ctx
	if broadcaster.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func TestDeadlineBroadcasterCancelsCallContextAfterReturn(t *testing.T) {
	backend := &deadlineProbeBroadcaster{}
	broadcaster, err := NewDeadlineBroadcaster(backend, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parent := context.Background()
	if err := broadcaster.SendTransaction(parent, types.NewTx(&types.LegacyTx{})); err != nil {
		t.Fatal(err)
	}
	if backend.ctx == nil {
		t.Fatal("SendTransaction() did not propagate context")
	}
	if _, ok := backend.ctx.Deadline(); !ok {
		t.Fatal("SendTransaction() context has no deadline")
	}
	if !errors.Is(backend.ctx.Err(), context.Canceled) {
		t.Fatalf("SendTransaction() context error: %v", backend.ctx.Err())
	}
	if parent.Err() != nil {
		t.Fatalf("parent context canceled: %v", parent.Err())
	}
}

func TestDeadlineBroadcasterCancelsHungSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broadcaster, err := NewDeadlineBroadcaster(&deadlineProbeBroadcaster{block: true}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		err = broadcaster.SendTransaction(context.Background(), types.NewTx(&types.LegacyTx{}))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SendTransaction() returned an error: %v", err)
		}
	})
}

func (probe *deadlineProbeReader) record(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("missing deadline")
	}
	probe.mu.Lock()
	probe.contexts = append(probe.contexts, ctx)
	probe.mu.Unlock()
	return nil
}

func (probe *deadlineProbeReader) ChainID(ctx context.Context) (*big.Int, error) {
	return big.NewInt(1), probe.record(ctx)
}

func (probe *deadlineProbeReader) BlockNumber(ctx context.Context) (uint64, error) {
	return 1, probe.record(ctx)
}

func (probe *deadlineProbeReader) HeaderByNumber(ctx context.Context, _ *big.Int) (*types.Header, error) {
	return &types.Header{}, probe.record(ctx)
}

func (probe *deadlineProbeReader) BalanceAt(ctx context.Context, _ common.Address, _ *big.Int) (*big.Int, error) {
	return big.NewInt(1), probe.record(ctx)
}

func (probe *deadlineProbeReader) CodeAt(ctx context.Context, _ common.Address, _ *big.Int) ([]byte, error) {
	return []byte{}, probe.record(ctx)
}

func (probe *deadlineProbeReader) NonceAt(ctx context.Context, _ common.Address, _ *big.Int) (uint64, error) {
	return 1, probe.record(ctx)
}

func (probe *deadlineProbeReader) PendingNonceAt(ctx context.Context, _ common.Address) (uint64, error) {
	return 1, probe.record(ctx)
}

func (probe *deadlineProbeReader) CallContract(ctx context.Context, _ ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	return []byte{}, probe.record(ctx)
}

func (probe *deadlineProbeReader) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return big.NewInt(1), probe.record(ctx)
}

func (probe *deadlineProbeReader) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	return big.NewInt(1), probe.record(ctx)
}

func (probe *deadlineProbeReader) EstimateGas(ctx context.Context, _ ethereum.CallMsg) (uint64, error) {
	return 1, probe.record(ctx)
}

func (probe *deadlineProbeReader) TransactionReceipt(ctx context.Context, _ common.Hash) (*types.Receipt, error) {
	return &types.Receipt{}, probe.record(ctx)
}

func (probe *deadlineProbeReader) FilterLogs(ctx context.Context, _ ethereum.FilterQuery) ([]types.Log, error) {
	return []types.Log{}, probe.record(ctx)
}

func TestDeadlineReaderGivesEveryUnaryCallItsOwnCanceledDeadline(t *testing.T) {
	probe := &deadlineProbeReader{}
	reader, err := NewDeadlineReader(probe, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	number := big.NewInt(1)
	address := common.Address{1}
	hash := common.Hash{1}

	_, err = reader.ChainID(ctx)
	requireNoError(t, err)
	_, err = reader.BlockNumber(ctx)
	requireNoError(t, err)
	_, err = reader.HeaderByNumber(ctx, number)
	requireNoError(t, err)
	_, err = reader.BalanceAt(ctx, address, number)
	requireNoError(t, err)
	_, err = reader.CodeAt(ctx, address, number)
	requireNoError(t, err)
	_, err = reader.NonceAt(ctx, address, number)
	requireNoError(t, err)
	_, err = reader.PendingNonceAt(ctx, address)
	requireNoError(t, err)
	_, err = reader.CallContract(ctx, ethereum.CallMsg{}, number)
	requireNoError(t, err)
	_, err = reader.SuggestGasPrice(ctx)
	requireNoError(t, err)
	_, err = reader.SuggestGasTipCap(ctx)
	requireNoError(t, err)
	_, err = reader.EstimateGas(ctx, ethereum.CallMsg{})
	requireNoError(t, err)
	_, err = reader.TransactionReceipt(ctx, hash)
	requireNoError(t, err)
	_, err = reader.FilterLogs(ctx, ethereum.FilterQuery{})
	requireNoError(t, err)

	probe.mu.Lock()
	contexts := append([]context.Context(nil), probe.contexts...)
	probe.mu.Unlock()
	if len(contexts) != 13 {
		t.Fatalf("recorded contexts = %d, want 13", len(contexts))
	}
	unique := make(map[context.Context]struct{}, len(contexts))
	for _, callCtx := range contexts {
		unique[callCtx] = struct{}{}
		if !errors.Is(callCtx.Err(), context.Canceled) {
			t.Fatalf("call context error = %v, want cancellation after return", callCtx.Err())
		}
	}
	if len(unique) != len(contexts) {
		t.Fatalf("unary calls shared deadline contexts: %d unique out of %d", len(unique), len(contexts))
	}
	if ctx.Err() != nil {
		t.Fatalf("parent context canceled: %v", ctx.Err())
	}
}

type blockingDeadlineReader struct{ *deadlineProbeReader }

func (reader *blockingDeadlineReader) HeaderByNumber(ctx context.Context, _ *big.Int) (*types.Header, error) {
	if err := reader.record(ctx); err != nil {
		return nil, err
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestDeadlineReaderCancelsHungUnaryCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, err := NewDeadlineReader(&blockingDeadlineReader{deadlineProbeReader: &deadlineProbeReader{}}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_, err = reader.HeaderByNumber(context.Background(), big.NewInt(1))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("HeaderByNumber() returned an error: %v", err)
		}
	})
}

type testSubscription struct {
	err  chan error
	once sync.Once
}

func newTestSubscription() *testSubscription {
	return &testSubscription{err: make(chan error)}
}

func (subscription *testSubscription) Err() <-chan error { return subscription.err }

func (subscription *testSubscription) Unsubscribe() {
	subscription.once.Do(func() { close(subscription.err) })
}

type setupLogSubscriber struct {
	ctx   context.Context
	block bool
}

func (subscriber *setupLogSubscriber) SubscribeFilterLogs(
	ctx context.Context,
	_ ethereum.FilterQuery,
	_ chan<- types.Log,
) (ethereum.Subscription, error) {
	subscriber.ctx = ctx
	if subscriber.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return newTestSubscription(), nil
}

type setupHeadSubscriber struct {
	ctx   context.Context
	block bool
}

func (subscriber *setupHeadSubscriber) SubscribeNewHead(
	ctx context.Context,
	_ chan<- *types.Header,
) (ethereum.Subscription, error) {
	subscriber.ctx = ctx
	if subscriber.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return newTestSubscription(), nil
}

func TestDeadlineSubscribersLimitOnlySetup(t *testing.T) {
	parent := context.Background()
	logsBackend := &setupLogSubscriber{}
	logs, err := NewDeadlineLogSubscriber(logsBackend, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	logSubscription, err := logs.SubscribeFilterLogs(parent, ethereum.FilterQuery{}, make(chan types.Log))
	if err != nil {
		t.Fatal(err)
	}
	assertSetupContextCanceled(t, logsBackend.ctx)
	assertSubscriptionActive(t, logSubscription)

	headsBackend := &setupHeadSubscriber{}
	heads, err := NewDeadlineHeadSubscriber(headsBackend, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	headSubscription, err := heads.SubscribeNewHead(parent, make(chan *types.Header))
	if err != nil {
		t.Fatal(err)
	}
	assertSetupContextCanceled(t, headsBackend.ctx)
	assertSubscriptionActive(t, headSubscription)

	logSubscription.Unsubscribe()
	headSubscription.Unsubscribe()
}

func TestDeadlineSubscribersCancelHungSetup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs, err := NewDeadlineLogSubscriber(&setupLogSubscriber{block: true}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := logs.SubscribeFilterLogs(context.Background(), ethereum.FilterQuery{}, make(chan types.Log)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SubscribeFilterLogs() returned an error: %v", err)
		}

		heads, err := NewDeadlineHeadSubscriber(&setupHeadSubscriber{block: true}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := heads.SubscribeNewHead(context.Background(), make(chan *types.Header)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SubscribeNewHead() returned an error: %v", err)
		}
	})
}

func TestDeadlineConstructorsRejectInvalidConfiguration(t *testing.T) {
	if _, err := NewDeadlineBroadcaster(nil, time.Second); !errors.Is(err, ErrInvalidDeadlineWrapper) {
		t.Fatalf("NewDeadlineBroadcaster(nil) returned an error: %v", err)
	}
	var nilBroadcaster *deadlineProbeBroadcaster
	if _, err := NewDeadlineBroadcaster(nilBroadcaster, time.Second); !errors.Is(err, ErrInvalidDeadlineWrapper) {
		t.Fatalf("NewDeadlineBroadcaster(typed nil) returned an error: %v", err)
	}
	if _, err := NewDeadlineBroadcaster(&deadlineProbeBroadcaster{}, 0); !errors.Is(err, ErrInvalidDeadlineWrapper) {
		t.Fatalf("NewDeadlineBroadcaster(timeout=0) returned an error: %v", err)
	}
	if _, err := NewDeadlineReader(nil, time.Second); !errors.Is(err, ErrInvalidDeadlineWrapper) {
		t.Fatalf("NewDeadlineReader(nil) returned an error: %v", err)
	}
	if _, err := NewDeadlineReader(&deadlineProbeReader{}, 0); !errors.Is(err, ErrInvalidDeadlineWrapper) {
		t.Fatalf("NewDeadlineReader(timeout=0) returned an error: %v", err)
	}
	if _, err := NewDeadlineLogSubscriber(nil, time.Second); !errors.Is(err, ErrInvalidDeadlineWrapper) {
		t.Fatalf("NewDeadlineLogSubscriber(nil) returned an error: %v", err)
	}
	if _, err := NewDeadlineHeadSubscriber(nil, time.Second); !errors.Is(err, ErrInvalidDeadlineWrapper) {
		t.Fatalf("NewDeadlineHeadSubscriber(nil) returned an error: %v", err)
	}
}

func TestGenerationClientCloseIsConcurrentAndIdempotent(t *testing.T) {
	var closes atomic.Int32
	client, err := NewGenerationClient(1, ClientParts{
		Reader: &deadlineProbeReader{},
		Logs:   &setupLogSubscriber{},
		Heads:  &setupHeadSubscriber{},
		Closer: closeFunc(func() { closes.Add(1) }),
	})
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			client.Close()
		}()
	}
	wait.Wait()
	if closes.Load() != 1 {
		t.Fatalf("Close() call count = %d, want 1", closes.Load())
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertSetupContextCanceled(t *testing.T, ctx context.Context) {
	t.Helper()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("setup context has no deadline")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("setup context error: %v", ctx.Err())
	}
}

func assertSubscriptionActive(t *testing.T, subscription ethereum.Subscription) {
	t.Helper()
	select {
	case err, ok := <-subscription.Err():
		t.Fatalf("subscription stopped after setup cancellation: error=%v open=%t", err, ok)
	default:
	}
}
