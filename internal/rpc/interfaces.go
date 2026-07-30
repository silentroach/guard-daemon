package rpc

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type ChainReader interface {
	ChainID(context.Context) (*big.Int, error)
	BlockNumber(context.Context) (uint64, error)
	HeaderByNumber(context.Context, *big.Int) (*types.Header, error)
}

type StateReader interface {
	BalanceAt(context.Context, common.Address, *big.Int) (*big.Int, error)
	CodeAt(context.Context, common.Address, *big.Int) ([]byte, error)
	NonceAt(context.Context, common.Address, *big.Int) (uint64, error)
	PendingNonceAt(context.Context, common.Address) (uint64, error)
}

type ContractCaller interface {
	CallContract(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error)
}

type FeeReader interface {
	SuggestGasPrice(context.Context) (*big.Int, error)
	SuggestGasTipCap(context.Context) (*big.Int, error)
	EstimateGas(context.Context, ethereum.CallMsg) (uint64, error)
}

type ReceiptReader interface {
	TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error)
}

type LogReader interface {
	FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error)
}

type Reader interface {
	ChainReader
	StateReader
	ContractCaller
	FeeReader
	ReceiptReader
	LogReader
}

type LogSubscriber interface {
	SubscribeFilterLogs(context.Context, ethereum.FilterQuery, chan<- types.Log) (ethereum.Subscription, error)
}

type HeadSubscriber interface {
	SubscribeNewHead(context.Context, chan<- *types.Header) (ethereum.Subscription, error)
}

type Broadcaster interface {
	SendTransaction(context.Context, *types.Transaction) error
}

type Closer interface {
	Close()
}
