package rpc

import (
	"context"
	"errors"
	"math/big"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestMinedReceiptDoesNotSettleAnUnfinalizedTransaction(t *testing.T) {
	finalized, included := testHeader(5, 1), testHeader(10, 2)
	hash := common.Hash{1}
	receipt := testReceipt(hash, included, types.ReceiptStatusSuccessful)
	reader := mustQuorum(t, receiptFake(finalized, included, receipt), receiptFake(finalized, included, receipt))
	if _, err := reader.MinedReceipt(context.Background(), hash); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Receipt(context.Background(), hash); !errors.Is(err, ErrUnfinalizedReceipt) {
		t.Fatalf("unfinalized receipt was accepted as final: %v", err)
	}
}

func TestPinnedPreflightReusesOneAgreedHeader(t *testing.T) {
	var headers atomic.Int32
	header := testHeader(10, 1)
	backend := func() *historicalFake {
		return &historicalFake{
			header: func(context.Context, *big.Int) (*types.Header, error) {
				headers.Add(1)
				return types.CopyHeader(header), nil
			},
			balance: func(context.Context, common.Address, common.Hash) (*big.Int, error) { return big.NewInt(7), nil },
			call:    func(context.Context, ethereum.CallMsg, common.Hash) ([]byte, error) { return []byte{}, nil },
		}
	}
	reader := mustQuorum(t, backend(), backend())
	block, err := reader.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pinned := reader.Pin(block)
	for range 3 {
		if _, err := pinned.BalanceAt(context.Background(), block, common.Address{1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pinned.CallContract(context.Background(), block, ethereum.CallMsg{}); err != nil {
		t.Fatal(err)
	}
	if headers.Load() != 2 {
		t.Fatalf("preflight repeated header reads: %d", headers.Load())
	}
	if _, err := reader.Header(context.Background(), block.Number); err != nil {
		t.Fatal(err)
	}
	if headers.Load() != 4 {
		t.Fatalf("final canonical check count: %d", headers.Load())
	}
}
