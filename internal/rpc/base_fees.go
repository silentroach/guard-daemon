package rpc

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

var baseGasOracle = common.HexToAddress("0x420000000000000000000000000000000000000F")

type pinnedFeeCaller interface {
	CallContract(context.Context, BlockRef, ethereum.CallMsg) ([]byte, error)
}

// EstimateBaseAdditionalFee reserves for one type-4 authorization, its signatures
// and calldata. The oracle quote is an estimate at this block, not a protocol cap.
func EstimateBaseAdditionalFee(ctx context.Context, reader pinnedFeeCaller, block BlockRef, calldataBytes int, gas uint64) (*big.Int, error) {
	if ctx == nil || reader == nil || calldataBytes < 0 || calldataBytes > 64<<10 || gas == 0 {
		return nil, ErrMalformedResponse
	}
	l1, err := baseOracleFee(ctx, reader, block, []byte{0xf1, 0xc7, 0xa5, 0x8b}, uint64(calldataBytes)+1024)
	if err != nil {
		return nil, err
	}
	operator, err := baseOracleFee(ctx, reader, block, []byte{0x27, 0x5a, 0xed, 0xd2}, gas)
	if err != nil {
		return nil, err
	}
	return addBaseFees(l1, operator)
}

func baseOracleFee(ctx context.Context, reader pinnedFeeCaller, block BlockRef, selector []byte, value uint64) (*big.Int, error) {
	data := make([]byte, 36)
	copy(data, selector)
	new(big.Int).SetUint64(value).FillBytes(data[4:])
	result, err := reader.CallContract(ctx, block, ethereum.CallMsg{To: &baseGasOracle, Gas: 200_000, Data: data})
	if err != nil {
		return nil, err
	}
	if len(result) != 32 {
		return nil, ErrMalformedResponse
	}
	return new(big.Int).SetBytes(result), nil
}

func addBaseFees(left, right *big.Int) (*big.Int, error) {
	result := new(big.Int).Add(left, right)
	if result.Sign() < 0 || result.BitLen() > 256 {
		return nil, ErrMalformedResponse
	}
	return result, nil
}

type baseReceiptFee struct {
	TransactionHash common.Hash  `json:"transactionHash"`
	BlockHash       common.Hash  `json:"blockHash"`
	L1Fee           *hexutil.Big `json:"l1Fee"`
}

type baseFeeReader interface {
	baseReceiptFee(context.Context, common.Hash) (*baseReceiptFee, error)
}

type baseHistoricalReader struct {
	HistoricalReader
	client *ethclient.Client
}

func (reader *baseHistoricalReader) baseReceiptFee(ctx context.Context, hash common.Hash) (*baseReceiptFee, error) {
	var result *baseReceiptFee
	if err := reader.client.Client().CallContext(ctx, &result, "eth_getTransactionReceipt", hash); err != nil {
		return nil, ErrQuorumUnavailable
	}
	return result, nil
}

// BaseAdditionalFee requires agreement on the receipt's L1 fee. The operator fee
// is evaluated at the receipt block by the chain's oracle, including its active
// upgrade formula (rather than hardcoding the pre-/post-Jovian multiplier).
func (reader *QuorumReader) BaseAdditionalFee(ctx context.Context, receipt *types.Receipt) (*big.Int, error) {
	if receipt == nil || receipt.BlockNumber == nil || !receipt.BlockNumber.IsUint64() || receipt.TxHash == (common.Hash{}) {
		return nil, ErrMalformedResponse
	}
	block, err := reader.Header(ctx, receipt.BlockNumber.Uint64())
	if err != nil || block.Hash != receipt.BlockHash {
		return nil, ErrQuorumMismatch
	}
	fees, err := collectProviders(reader, ctx, func(ctx context.Context, provider HistoricalReader) (*baseReceiptFee, error) {
		backend, ok := provider.(baseFeeReader)
		if !ok {
			return nil, ErrMalformedResponse
		}
		return backend.baseReceiptFee(ctx, receipt.TxHash)
	})
	if err != nil {
		return nil, err
	}
	var agreed *big.Int
	for _, fee := range fees {
		if fee == nil || fee.TransactionHash != receipt.TxHash || fee.BlockHash != receipt.BlockHash || fee.L1Fee == nil {
			return nil, ErrMalformedResponse
		}
		value := (*big.Int)(fee.L1Fee)
		if value.Sign() < 0 || value.BitLen() > 256 {
			return nil, ErrMalformedResponse
		}
		if agreed != nil && agreed.Cmp(value) != 0 {
			return nil, ErrQuorumMismatch
		}
		agreed = new(big.Int).Set(value)
	}
	operator, err := baseOracleFee(ctx, reader, block, []byte{0x27, 0x5a, 0xed, 0xd2}, receipt.GasUsed)
	if err != nil {
		return nil, err
	}
	return addBaseFees(agreed, operator)
}
