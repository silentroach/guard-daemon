package rpc

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

type receiptFeeFake struct {
	*historicalFake
	fee *baseReceiptFee
}

func (fake *receiptFeeFake) baseReceiptFee(context.Context, common.Hash) (*baseReceiptFee, error) {
	return fake.fee, nil
}

func TestBaseFeesRequireMatchingReceiptAndPinnedOracle(t *testing.T) {
	header := testHeader(10, 1)
	hash := common.Hash{1}
	for _, scenario := range []struct {
		name       string
		fee        *big.Int
		wrongBlock bool
		want       error
	}{
		{name: "agreement", fee: big.NewInt(7)},
		{name: "fee disagreement", fee: big.NewInt(8), want: ErrQuorumMismatch},
		{name: "missing fee", want: ErrMalformedResponse},
		{name: "wrong receipt block", fee: big.NewInt(7), wrongBlock: true, want: ErrMalformedResponse},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			providers := make([]Provider, 2)
			for index := range providers {
				fee := big.NewInt(7)
				blockHash := header.Hash()
				if index == 1 {
					fee = scenario.fee
					if scenario.wrongBlock {
						blockHash = common.Hash{9}
					}
				}
				backend := &receiptFeeFake{
					fee: &baseReceiptFee{TransactionHash: hash, BlockHash: blockHash, L1Fee: (*hexutil.Big)(fee)},
					historicalFake: &historicalFake{
						header: exactHeaderPlan(header),
						call: func(_ context.Context, call ethereum.CallMsg, at common.Hash) ([]byte, error) {
							if at != header.Hash() || call.To == nil || *call.To != baseGasOracle || len(call.Data) != 36 {
								return nil, ErrInvalidBlockRef
							}
							return common.LeftPadBytes(big.NewInt(3).Bytes(), 32), nil
						},
					},
				}
				providers[index] = Provider{Identity: testIdentity(index), Reader: backend, Close: func() {}}
			}
			quorum, err := NewQuorumReader(providers, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			fee, err := quorum.BaseAdditionalFee(context.Background(), &types.Receipt{TxHash: hash, BlockHash: header.Hash(), BlockNumber: header.Number, GasUsed: 100})
			if !errors.Is(err, scenario.want) {
				t.Fatalf("additional fee error: %v", err)
			}
			if err == nil && fee.Cmp(big.NewInt(10)) != 0 {
				t.Fatalf("L1 + operator fee = %s", fee)
			}
		})
	}
}

func TestBaseFeeEstimateIncludesCalldataAndOperator(t *testing.T) {
	header := testHeader(10, 1)
	backend := func() *historicalFake {
		return &historicalFake{header: exactHeaderPlan(header), call: func(_ context.Context, call ethereum.CallMsg, _ common.Hash) ([]byte, error) {
			if len(call.Data) != 36 {
				return nil, ErrMalformedResponse
			}
			value := new(big.Int).SetBytes(call.Data[4:])
			switch hexutil.Encode(call.Data[:4]) {
			case "0xf1c7a58b":
				if value.Uint64() != 1124 {
					return nil, ErrMalformedResponse
				}
				return common.LeftPadBytes(big.NewInt(7).Bytes(), 32), nil
			case "0x275aedd2":
				if value.Uint64() != 300_000 {
					return nil, ErrMalformedResponse
				}
				return make([]byte, 32), nil
			default:
				return nil, ErrMalformedResponse
			}
		}}
	}
	quorum := mustQuorum(t, backend(), backend())
	fee, err := EstimateBaseAdditionalFee(context.Background(), quorum, refForHeader(header), 100, 300_000)
	if err != nil || fee.Cmp(big.NewInt(7)) != 0 {
		t.Fatalf("estimate = %v, %v", fee, err)
	}
}
