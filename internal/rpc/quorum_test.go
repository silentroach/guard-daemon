package rpc

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var errUnexpectedHistoricalCall = errors.New("unexpected historical call")

type historicalFake struct {
	header  func(context.Context, *big.Int) (*types.Header, error)
	balance func(context.Context, common.Address, common.Hash) (*big.Int, error)
	code    func(context.Context, common.Address, common.Hash) ([]byte, error)
	call    func(context.Context, ethereum.CallMsg, common.Hash) ([]byte, error)
	receipt func(context.Context, common.Hash) (*types.Receipt, error)
	logs    func(context.Context, ethereum.FilterQuery) ([]types.Log, error)
}

func (fake *historicalFake) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	if fake.header == nil {
		return nil, errUnexpectedHistoricalCall
	}
	return fake.header(ctx, number)
}

func (fake *historicalFake) BalanceAtHash(ctx context.Context, account common.Address, hash common.Hash) (*big.Int, error) {
	if fake.balance == nil {
		return nil, errUnexpectedHistoricalCall
	}
	return fake.balance(ctx, account, hash)
}

func (fake *historicalFake) CodeAtHash(ctx context.Context, account common.Address, hash common.Hash) ([]byte, error) {
	if fake.code == nil {
		return nil, errUnexpectedHistoricalCall
	}
	return fake.code(ctx, account, hash)
}

func (fake *historicalFake) CallContractAtHash(ctx context.Context, call ethereum.CallMsg, hash common.Hash) ([]byte, error) {
	if fake.call == nil {
		return nil, errUnexpectedHistoricalCall
	}
	return fake.call(ctx, call, hash)
}

func (fake *historicalFake) TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	if fake.receipt == nil {
		return nil, errUnexpectedHistoricalCall
	}
	return fake.receipt(ctx, hash)
}

func (fake *historicalFake) FilterLogs(ctx context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	if fake.logs == nil {
		return nil, errUnexpectedHistoricalCall
	}
	return fake.logs(ctx, query)
}

func TestNewQuorumReaderValidatesProviderIndependence(t *testing.T) {
	readers := []*historicalFake{{}, {}}
	valid := testProviders(readers...)
	if _, err := NewQuorumReader(valid, time.Second); err != nil {
		t.Fatalf("NewQuorumReader(valid) error = %v", err)
	}

	tests := map[string]func([]Provider) []Provider{
		"minimum": func(providers []Provider) []Provider { return providers[:1] },
		"id": func(providers []Provider) []Provider {
			providers[1].Identity.ID = providers[0].Identity.ID
			return providers
		},
		"fingerprint": func(providers []Provider) []Provider {
			providers[1].Identity.Fingerprint = strings.ToUpper(providers[0].Identity.Fingerprint)
			return providers
		},
		"trust domain": func(providers []Provider) []Provider {
			providers[1].Identity.TrustDomain = providers[0].Identity.TrustDomain
			return providers
		},
		"reader instance": func(providers []Provider) []Provider {
			providers[1].Reader = providers[0].Reader
			return providers
		},
		"missing close": func(providers []Provider) []Provider {
			providers[1].Close = nil
			return providers
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			providers := append([]Provider(nil), valid...)
			_, err := NewQuorumReader(mutate(providers), time.Second)
			if !errors.Is(err, ErrInvalidProviders) {
				t.Fatalf("NewQuorumReader() error = %v, want %v", err, ErrInvalidProviders)
			}
		})
	}
	if _, err := NewQuorumReader(valid, 0); !errors.Is(err, ErrInvalidTimeout) {
		t.Fatalf("NewQuorumReader(timeout=0) error = %v", err)
	}
}

func TestQuorumFinalizedUsesLowestCommonCanonicalBlock(t *testing.T) {
	commonHeader := testHeader(10, 1)
	reader := mustQuorum(t,
		&historicalFake{header: finalizedHeaderPlan(10, map[uint64]*types.Header{10: commonHeader})},
		&historicalFake{header: finalizedHeaderPlan(12, map[uint64]*types.Header{10: commonHeader, 12: testHeader(12, 2)})},
	)

	ref, err := reader.Finalized(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := refForHeader(commonHeader)
	if ref != want {
		t.Fatalf("Finalized() = %#v, want %#v", ref, want)
	}
}

func TestQuorumFinalizedAndHeaderRejectByzantineOrMalformedResponse(t *testing.T) {
	t.Run("exact finalized hash disagreement", func(t *testing.T) {
		reader := mustQuorum(t,
			&historicalFake{header: finalizedHeaderPlan(10, map[uint64]*types.Header{10: testHeader(10, 1)})},
			&historicalFake{header: finalizedHeaderPlan(12, map[uint64]*types.Header{10: testHeader(10, 2), 12: testHeader(12, 3)})},
		)
		if _, err := reader.Finalized(context.Background()); !errors.Is(err, ErrQuorumMismatch) {
			t.Fatalf("Finalized() error = %v", err)
		}
	})

	t.Run("header parent disagreement", func(t *testing.T) {
		first := testHeader(7, 1)
		second := types.CopyHeader(first)
		second.ParentHash = common.BigToHash(big.NewInt(99))
		reader := mustQuorum(t,
			&historicalFake{header: exactHeaderPlan(first)},
			&historicalFake{header: exactHeaderPlan(second)},
		)
		if _, err := reader.Header(context.Background(), 7); !errors.Is(err, ErrQuorumMismatch) {
			t.Fatalf("Header() error = %v", err)
		}
	})

	t.Run("nil header", func(t *testing.T) {
		reader := mustQuorum(t,
			&historicalFake{header: func(context.Context, *big.Int) (*types.Header, error) { return nil, nil }},
			&historicalFake{header: exactHeaderPlan(testHeader(7, 1))},
		)
		if _, err := reader.Header(context.Background(), 7); !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("Header() error = %v", err)
		}
	})

	t.Run("wrong number", func(t *testing.T) {
		reader := mustQuorum(t,
			&historicalFake{header: exactHeaderPlan(testHeader(8, 1))},
			&historicalFake{header: exactHeaderPlan(testHeader(8, 1))},
		)
		if _, err := reader.Header(context.Background(), 7); !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("Header() error = %v", err)
		}
	})
}

func TestQuorumHeaderDeadlineAndRedaction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const secret = "https://credential.invalid/raw-backend-cause"
		reader := mustQuorumWithTimeout(t, time.Second,
			&historicalFake{header: func(ctx context.Context, _ *big.Int) (*types.Header, error) {
				<-ctx.Done()
				return nil, fmt.Errorf("%s: %w", secret, ctx.Err())
			}},
			&historicalFake{header: exactHeaderPlan(testHeader(3, 1))},
		)
		_, err := reader.Header(context.Background(), 3)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Header() error = %v, want deadline", err)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "raw-backend") {
			t.Fatalf("Header() exposed backend error: %q", err)
		}
	})
}

func TestQuorumHeaderReturnsWhenBackendIgnoresCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		finished := make(chan struct{})
		reader := mustQuorumWithTimeout(t, time.Second,
			&historicalFake{header: func(context.Context, *big.Int) (*types.Header, error) {
				defer close(finished)
				<-release
				return nil, errors.New("released")
			}},
			&historicalFake{header: exactHeaderPlan(testHeader(3, 1))},
		)

		if _, err := reader.Header(context.Background(), 3); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Header() error = %v, want coordinator deadline", err)
		}
		close(release)
		synctest.Wait()
		<-finished
	})
}

func TestQuorumHeaderPropagatesParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 2)
	hung := func(ctx context.Context, _ *big.Int) (*types.Header, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	reader := mustQuorumWithTimeout(t, time.Hour,
		&historicalFake{header: hung},
		&historicalFake{header: hung},
	)
	go func() {
		<-started
		<-started
		cancel()
	}()

	if _, err := reader.Header(ctx, 3); !errors.Is(err, context.Canceled) {
		t.Fatalf("Header() error = %v, want parent cancellation", err)
	}
}

func TestQuorumFilterLogsNormalizesOrderAndCoalescesDuplicates(t *testing.T) {
	first := testLog(5, 0)
	second := testLog(5, 1)
	reader := mustQuorum(t,
		&historicalFake{logs: fixedLogs([]types.Log{second, first, first})},
		&historicalFake{logs: fixedLogs([]types.Log{first, second, first})},
	)

	logs, err := reader.FilterLogs(context.Background(), explicitQuery(5, 5))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 || logs[0].Index != 0 || logs[1].Index != 1 {
		t.Fatalf("FilterLogs() did not deterministically coalesce duplicates: %#v", logs)
	}
	conflict := cloneLog(first)
	conflict.Data = []byte{0xff}
	reader = mustQuorum(t,
		&historicalFake{logs: fixedLogs([]types.Log{first, conflict})},
		&historicalFake{logs: fixedLogs([]types.Log{first, conflict})},
	)
	if _, err := reader.FilterLogs(context.Background(), explicitQuery(5, 5)); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("conflicting duplicate error = %v", err)
	}
}

func TestQuorumFilterLogsPinsAgreedBlockHash(t *testing.T) {
	t.Parallel()

	blockHash := common.Hash{31: 0x55}
	logEntry := testLog(5, 1)
	logEntry.BlockHash = blockHash
	first := &historicalFake{logs: fixedLogs([]types.Log{logEntry})}
	second := &historicalFake{logs: fixedLogs([]types.Log{logEntry})}
	reader := mustQuorum(t, first, second)
	got, err := reader.FilterLogs(context.Background(), ethereum.FilterQuery{BlockHash: &blockHash})
	if err != nil || len(got) != 1 || got[0].BlockHash != blockHash {
		t.Fatalf("hash-pinned FilterLogs() = (%v, %v)", got, err)
	}

	wrong := cloneLog(logEntry)
	wrong.BlockHash = common.Hash{31: 0x56}
	second.logs = fixedLogs([]types.Log{wrong})
	if _, err := reader.FilterLogs(context.Background(), ethereum.FilterQuery{BlockHash: &blockHash}); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("wrong-hash FilterLogs() error = %v", err)
	}
	emptyLogs := func(context.Context, ethereum.FilterQuery) ([]types.Log, error) { return []types.Log{}, nil }
	first.logs = emptyLogs
	second.logs = emptyLogs
	if logs, err := reader.FilterLogs(context.Background(), ethereum.FilterQuery{BlockHash: &blockHash}); err != nil || len(logs) != 0 {
		t.Fatalf("empty hash-pinned FilterLogs() = (%v, %v)", logs, err)
	}
}

func TestQuorumFilterLogsRejectsDisagreementAndMalformedResponses(t *testing.T) {
	valid := testLog(5, 0)
	different := cloneLog(valid)
	different.Data = []byte{9}
	tests := map[string]struct {
		query  ethereum.FilterQuery
		second []types.Log
		want   error
	}{
		"semantic disagreement": {explicitQuery(5, 5), []types.Log{different}, ErrQuorumMismatch},
		"nil response":          {explicitQuery(5, 5), nil, ErrMalformedResponse},
		"removed": func() struct {
			query  ethereum.FilterQuery
			second []types.Log
			want   error
		} {
			removed := cloneLog(valid)
			removed.Removed = true
			return struct {
				query  ethereum.FilterQuery
				second []types.Log
				want   error
			}{explicitQuery(5, 5), []types.Log{removed}, ErrMalformedResponse}
		}(),
		"out of range": {explicitQuery(6, 7), []types.Log{valid}, ErrMalformedResponse},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			reader := mustQuorum(t,
				&historicalFake{logs: fixedLogs([]types.Log{valid})},
				&historicalFake{logs: fixedLogs(test.second)},
			)
			if _, err := reader.FilterLogs(context.Background(), test.query); !errors.Is(err, test.want) {
				t.Fatalf("FilterLogs() error = %v, want %v", err, test.want)
			}
		})
	}

	invalidQueries := []ethereum.FilterQuery{
		{},
		{BlockHash: pointerToHash(common.Hash{})},
		{FromBlock: big.NewInt(-1), ToBlock: big.NewInt(1)},
		{FromBlock: big.NewInt(2), ToBlock: big.NewInt(1)},
		{FromBlock: big.NewInt(1), ToBlock: big.NewInt(1), BlockHash: pointerToHash(common.Hash{1})},
	}
	for _, query := range invalidQueries {
		reader := mustQuorum(t, &historicalFake{}, &historicalFake{})
		if _, err := reader.FilterLogs(context.Background(), query); !errors.Is(err, ErrInvalidFilterQuery) {
			t.Fatalf("FilterLogs(invalid) error = %v", err)
		}
	}
}

func TestQuorumFilterLogsRejectsApplicationLimitViolations(t *testing.T) {
	valid := testLog(5, 0)
	tests := map[string][]types.Log{
		"слишком много logs": make([]types.Log, maxLogsPerResponse+1),
		"слишком много topics": func() []types.Log {
			entry := cloneLog(valid)
			entry.Topics = make([]common.Hash, maxTopicsPerLog+1)
			return []types.Log{entry}
		}(),
		"слишком большие data": func() []types.Log {
			entry := cloneLog(valid)
			entry.Data = make([]byte, maxLogDataBytes+1)
			return []types.Log{entry}
		}(),
	}
	for name, logs := range tests {
		t.Run(name, func(t *testing.T) {
			reader := mustQuorum(t,
				&historicalFake{logs: fixedLogs(logs)},
				&historicalFake{logs: fixedLogs(logs)},
			)
			if _, err := reader.FilterLogs(context.Background(), explicitQuery(5, 5)); !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("FilterLogs() error = %v, want %v", err, ErrMalformedResponse)
			}
		})
	}
}

func TestQuorumCriticalStateUsesCanonicalHashAndStrictEquality(t *testing.T) {
	header := testHeader(8, 1)
	block := refForHeader(header)
	address := common.Address{1}
	checkHash := func(hash common.Hash) error {
		if hash != block.Hash {
			return errors.New("wrong hash")
		}
		return nil
	}
	first := &historicalFake{
		header: exactHeaderPlan(header),
		balance: func(_ context.Context, _ common.Address, hash common.Hash) (*big.Int, error) {
			return big.NewInt(4), checkHash(hash)
		},
		code: func(_ context.Context, _ common.Address, hash common.Hash) ([]byte, error) {
			return []byte{1, 2}, checkHash(hash)
		},
		call: func(_ context.Context, _ ethereum.CallMsg, hash common.Hash) ([]byte, error) {
			return []byte{3, 4}, checkHash(hash)
		},
	}
	second := *first
	reader := mustQuorum(t, first, &second)

	balance, err := reader.BalanceAt(context.Background(), block, address)
	if err != nil || balance.Cmp(big.NewInt(4)) != 0 {
		t.Fatalf("BalanceAt() = %v, %v", balance, err)
	}
	code, err := reader.CodeAt(context.Background(), block, address)
	if err != nil || !stringBytesEqual(code, []byte{1, 2}) {
		t.Fatalf("CodeAt() = %x, %v", code, err)
	}
	output, err := reader.CallContract(context.Background(), block, ethereum.CallMsg{To: &address})
	if err != nil || !stringBytesEqual(output, []byte{3, 4}) {
		t.Fatalf("CallContract() = %x, %v", output, err)
	}
}

func TestQuorumCriticalStateRejectsByzantineAndNilResponses(t *testing.T) {
	header := testHeader(8, 1)
	block := refForHeader(header)
	base := func() *historicalFake {
		return &historicalFake{
			header:  exactHeaderPlan(header),
			balance: func(context.Context, common.Address, common.Hash) (*big.Int, error) { return big.NewInt(1), nil },
			code:    func(context.Context, common.Address, common.Hash) ([]byte, error) { return []byte{1}, nil },
			call:    func(context.Context, ethereum.CallMsg, common.Hash) ([]byte, error) { return []byte{1}, nil },
		}
	}

	t.Run("balance disagreement", func(t *testing.T) {
		first, second := base(), base()
		second.balance = func(context.Context, common.Address, common.Hash) (*big.Int, error) { return big.NewInt(2), nil }
		reader := mustQuorum(t, first, second)
		if _, err := reader.BalanceAt(context.Background(), block, common.Address{}); !errors.Is(err, ErrQuorumMismatch) {
			t.Fatalf("BalanceAt() error = %v", err)
		}
	})
	t.Run("code disagreement", func(t *testing.T) {
		first, second := base(), base()
		second.code = func(context.Context, common.Address, common.Hash) ([]byte, error) { return []byte{2}, nil }
		reader := mustQuorum(t, first, second)
		if _, err := reader.CodeAt(context.Background(), block, common.Address{}); !errors.Is(err, ErrQuorumMismatch) {
			t.Fatalf("CodeAt() error = %v", err)
		}
	})
	t.Run("call disagreement", func(t *testing.T) {
		first, second := base(), base()
		second.call = func(context.Context, ethereum.CallMsg, common.Hash) ([]byte, error) { return []byte{2}, nil }
		reader := mustQuorum(t, first, second)
		if _, err := reader.CallContract(context.Background(), block, ethereum.CallMsg{}); !errors.Is(err, ErrQuorumMismatch) {
			t.Fatalf("CallContract() error = %v", err)
		}
	})
	t.Run("nil values", func(t *testing.T) {
		first, second := base(), base()
		second.balance = func(context.Context, common.Address, common.Hash) (*big.Int, error) { return nil, nil }
		reader := mustQuorum(t, first, second)
		if _, err := reader.BalanceAt(context.Background(), block, common.Address{}); !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("BalanceAt() error = %v", err)
		}
		second = base()
		second.code = func(context.Context, common.Address, common.Hash) ([]byte, error) { return nil, nil }
		reader = mustQuorum(t, base(), second)
		if _, err := reader.CodeAt(context.Background(), block, common.Address{}); !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("CodeAt() error = %v", err)
		}
	})
	t.Run("noncanonical block ref", func(t *testing.T) {
		invalid := block
		invalid.ParentHash = common.Hash{9}
		reader := mustQuorum(t, base(), base())
		if _, err := reader.CodeAt(context.Background(), invalid, common.Address{}); !errors.Is(err, ErrInvalidBlockRef) {
			t.Fatalf("CodeAt() error = %v", err)
		}
	})
}

func TestQuorumReceiptRequiresEqualCanonicalFinalizedReceipt(t *testing.T) {
	txHash := common.Hash{7}
	finalizedHeader := testHeader(10, 1)
	receiptHeader := testHeader(8, 2)
	receipt := testReceipt(txHash, receiptHeader, types.ReceiptStatusSuccessful)
	first := receiptFake(finalizedHeader, receiptHeader, receipt)
	second := receiptFake(finalizedHeader, receiptHeader, copyReceipt(receipt))
	reader := mustQuorum(t, first, second)

	got, err := reader.Receipt(context.Background(), txHash)
	if err != nil || got.Status != types.ReceiptStatusSuccessful || got.BlockHash != receiptHeader.Hash() {
		t.Fatalf("Receipt() = %#v, %v", got, err)
	}
}

func TestQuorumReceiptRejectsDisagreementMalformedAndUnfinalized(t *testing.T) {
	txHash := common.Hash{7}
	finalizedHeader := testHeader(10, 1)
	receiptHeader := testHeader(8, 2)
	valid := testReceipt(txHash, receiptHeader, types.ReceiptStatusSuccessful)

	t.Run("status disagreement", func(t *testing.T) {
		failed := copyReceipt(valid)
		failed.Status = types.ReceiptStatusFailed
		reader := mustQuorum(t,
			receiptFake(finalizedHeader, receiptHeader, valid),
			receiptFake(finalizedHeader, receiptHeader, failed),
		)
		if _, err := reader.Receipt(context.Background(), txHash); !errors.Is(err, ErrQuorumMismatch) {
			t.Fatalf("Receipt() error = %v", err)
		}
	})

	t.Run("malformed status", func(t *testing.T) {
		malformed := copyReceipt(valid)
		malformed.Status = 2
		reader := mustQuorum(t,
			receiptFake(finalizedHeader, receiptHeader, malformed),
			receiptFake(finalizedHeader, receiptHeader, malformed),
		)
		if _, err := reader.Receipt(context.Background(), txHash); !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("Receipt() error = %v", err)
		}
	})

	t.Run("nil receipt", func(t *testing.T) {
		first := receiptFake(finalizedHeader, receiptHeader, valid)
		second := receiptFake(finalizedHeader, receiptHeader, nil)
		reader := mustQuorum(t, first, second)
		if _, err := reader.Receipt(context.Background(), txHash); !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("Receipt() error = %v", err)
		}
	})

	t.Run("noncanonical block", func(t *testing.T) {
		wrong := copyReceipt(valid)
		wrong.BlockHash = common.Hash{9}
		reader := mustQuorum(t,
			receiptFake(finalizedHeader, receiptHeader, wrong),
			receiptFake(finalizedHeader, receiptHeader, wrong),
		)
		if _, err := reader.Receipt(context.Background(), txHash); !errors.Is(err, ErrQuorumMismatch) {
			t.Fatalf("Receipt() error = %v", err)
		}
	})

	t.Run("above finalized", func(t *testing.T) {
		futureHeader := testHeader(11, 3)
		future := testReceipt(txHash, futureHeader, types.ReceiptStatusSuccessful)
		reader := mustQuorum(t,
			receiptFake(finalizedHeader, futureHeader, future),
			receiptFake(finalizedHeader, futureHeader, future),
		)
		if _, err := reader.Receipt(context.Background(), txHash); !errors.Is(err, ErrUnfinalizedReceipt) {
			t.Fatalf("Receipt() error = %v", err)
		}
	})
}

func TestDialQuorumCleansUpPartialFailureAndRedacts(t *testing.T) {
	endpoints := []ProviderEndpoint{
		{Identity: testIdentity(0), Endpoint: "https://first.invalid/private"},
		{Identity: testIdentity(1), Endpoint: "https://second.invalid/credential"},
	}
	var closed atomic.Int32
	dials := 0
	_, err := dialQuorum(context.Background(), endpoints, time.Second, func(_ context.Context, endpoint string) (HistoricalReader, func(), error) {
		dials++
		if dials == 2 {
			return nil, nil, fmt.Errorf("raw backend error at %s", endpoint)
		}
		return &historicalFake{}, func() { closed.Add(1) }, nil
	})
	if !errors.Is(err, ErrQuorumDial) {
		t.Fatalf("dialQuorum() error = %v", err)
	}
	if closed.Load() != 1 {
		t.Fatalf("partial dial close count = %d, want 1", closed.Load())
	}
	for _, endpoint := range endpoints {
		if strings.Contains(err.Error(), endpoint.Endpoint) {
			t.Fatalf("dialQuorum() exposed endpoint: %q", err)
		}
	}
	if strings.Contains(err.Error(), "raw backend") {
		t.Fatalf("dialQuorum() exposed backend error: %q", err)
	}
}

func TestQuorumReaderCloseIsConcurrentAndIdempotent(t *testing.T) {
	var first, second atomic.Int32
	providers := testProviders(&historicalFake{}, &historicalFake{})
	providers[0].Close = func() { first.Add(1) }
	providers[1].Close = func() { second.Add(1) }
	reader, err := NewQuorumReader(providers, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			reader.Close()
		}()
	}
	wait.Wait()
	if first.Load() != 1 || second.Load() != 1 {
		t.Fatalf("close counts = %d, %d", first.Load(), second.Load())
	}
}

func mustQuorum(t *testing.T, readers ...*historicalFake) *QuorumReader {
	t.Helper()
	return mustQuorumWithTimeout(t, time.Second, readers...)
}

func mustQuorumWithTimeout(t *testing.T, timeout time.Duration, readers ...*historicalFake) *QuorumReader {
	t.Helper()
	reader, err := NewQuorumReader(testProviders(readers...), timeout)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func testProviders(readers ...*historicalFake) []Provider {
	providers := make([]Provider, len(readers))
	for index, reader := range readers {
		providers[index] = Provider{Identity: testIdentity(index), Reader: reader, Close: func() {}}
	}
	return providers
}

func testIdentity(index int) ProviderIdentity {
	return ProviderIdentity{
		ID:          fmt.Sprintf("provider-%d", index),
		Fingerprint: fmt.Sprintf("fingerprint-%d", index),
		TrustDomain: fmt.Sprintf("trust-%d.invalid", index),
	}
}

func testHeader(number uint64, marker byte) *types.Header {
	return &types.Header{
		ParentHash: common.BigToHash(big.NewInt(int64(marker))),
		Number:     new(big.Int).SetUint64(number),
		Difficulty: big.NewInt(1),
		GasLimit:   30_000_000,
		Time:       uint64(marker),
		Extra:      []byte{marker},
	}
}

func refForHeader(header *types.Header) BlockRef {
	return BlockRef{Number: header.Number.Uint64(), Hash: header.Hash(), ParentHash: header.ParentHash}
}

func finalizedHeaderPlan(finalized uint64, exact map[uint64]*types.Header) func(context.Context, *big.Int) (*types.Header, error) {
	return func(_ context.Context, number *big.Int) (*types.Header, error) {
		if number.Sign() < 0 {
			return testHeader(finalized, byte(finalized)), nil
		}
		header := exact[number.Uint64()]
		if header == nil {
			return nil, errors.New("missing test header")
		}
		return types.CopyHeader(header), nil
	}
}

func exactHeaderPlan(header *types.Header) func(context.Context, *big.Int) (*types.Header, error) {
	return func(_ context.Context, _ *big.Int) (*types.Header, error) {
		return types.CopyHeader(header), nil
	}
}

func testLog(block uint64, index uint) types.Log {
	return types.Log{
		Address:        common.Address{1},
		Topics:         []common.Hash{},
		Data:           []byte{},
		BlockNumber:    block,
		TxHash:         common.BigToHash(new(big.Int).SetUint64(uint64(index + 1))),
		TxIndex:        index,
		BlockHash:      common.BigToHash(new(big.Int).SetUint64(block + 100)),
		BlockTimestamp: block * 10,
		Index:          index,
	}
}

func fixedLogs(logs []types.Log) func(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
	return func(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
		if logs == nil {
			return nil, nil
		}
		return append([]types.Log(nil), logs...), nil
	}
}

func explicitQuery(from, to uint64) ethereum.FilterQuery {
	return ethereum.FilterQuery{FromBlock: new(big.Int).SetUint64(from), ToBlock: new(big.Int).SetUint64(to)}
}

func pointerToHash(hash common.Hash) *common.Hash {
	return &hash
}

func testReceipt(transaction common.Hash, header *types.Header, status uint64) *types.Receipt {
	return &types.Receipt{
		Status:            status,
		CumulativeGasUsed: 1,
		Logs:              []*types.Log{},
		TxHash:            transaction,
		GasUsed:           1,
		BlockHash:         header.Hash(),
		BlockNumber:       new(big.Int).Set(header.Number),
	}
}

func copyReceipt(receipt *types.Receipt) *types.Receipt {
	if receipt == nil {
		return nil
	}
	cloned := *receipt
	cloned.PostState = append([]byte(nil), receipt.PostState...)
	cloned.BlockNumber = cloneBig(receipt.BlockNumber)
	cloned.Logs = append([]*types.Log{}, receipt.Logs...)
	return &cloned
}

func receiptFake(finalized, included *types.Header, receipt *types.Receipt) *historicalFake {
	return &historicalFake{
		header: func(_ context.Context, number *big.Int) (*types.Header, error) {
			if number.Sign() < 0 {
				return types.CopyHeader(finalized), nil
			}
			switch number.Uint64() {
			case finalized.Number.Uint64():
				return types.CopyHeader(finalized), nil
			case included.Number.Uint64():
				return types.CopyHeader(included), nil
			default:
				return nil, errors.New("missing test header")
			}
		},
		receipt: func(context.Context, common.Hash) (*types.Receipt, error) {
			return copyReceipt(receipt), nil
		},
	}
}

func stringBytesEqual(left, right []byte) bool {
	return string(left) == string(right)
}
