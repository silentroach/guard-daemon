package rpc

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var ErrInvalidDeadlineWrapper = errors.New("некорректная RPC deadline-обёртка")

type DeadlineReader struct {
	reader  Reader
	timeout time.Duration
}

type DeadlineBroadcaster struct {
	broadcaster Broadcaster
	timeout     time.Duration
}

func NewDeadlineBroadcaster(broadcaster Broadcaster, timeout time.Duration) (*DeadlineBroadcaster, error) {
	if nilCapability(broadcaster) || timeout <= 0 {
		return nil, ErrInvalidDeadlineWrapper
	}
	return &DeadlineBroadcaster{broadcaster: broadcaster, timeout: timeout}, nil
}

func (broadcaster *DeadlineBroadcaster) SendTransaction(ctx context.Context, transaction *types.Transaction) error {
	callCtx, cancel := context.WithTimeout(ctx, broadcaster.timeout)
	defer cancel()
	return broadcaster.broadcaster.SendTransaction(callCtx, transaction)
}

func NewDeadlineReader(reader Reader, timeout time.Duration) (*DeadlineReader, error) {
	if nilCapability(reader) || timeout <= 0 {
		return nil, ErrInvalidDeadlineWrapper
	}
	return &DeadlineReader{reader: reader, timeout: timeout}, nil
}

func (reader *DeadlineReader) ChainID(ctx context.Context) (*big.Int, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.ChainID(callCtx)
}

func (reader *DeadlineReader) BlockNumber(ctx context.Context) (uint64, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.BlockNumber(callCtx)
}

func (reader *DeadlineReader) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.HeaderByNumber(callCtx, number)
}

func (reader *DeadlineReader) BalanceAt(ctx context.Context, account common.Address, number *big.Int) (*big.Int, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.BalanceAt(callCtx, account, number)
}

func (reader *DeadlineReader) CodeAt(ctx context.Context, account common.Address, number *big.Int) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.CodeAt(callCtx, account, number)
}

func (reader *DeadlineReader) NonceAt(ctx context.Context, account common.Address, number *big.Int) (uint64, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.NonceAt(callCtx, account, number)
}

func (reader *DeadlineReader) PendingNonceAt(ctx context.Context, account common.Address) (uint64, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.PendingNonceAt(callCtx, account)
}

func (reader *DeadlineReader) CallContract(ctx context.Context, call ethereum.CallMsg, number *big.Int) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.CallContract(callCtx, call, number)
}

func (reader *DeadlineReader) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.SuggestGasPrice(callCtx)
}

func (reader *DeadlineReader) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.SuggestGasTipCap(callCtx)
}

func (reader *DeadlineReader) EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.EstimateGas(callCtx, call)
}

func (reader *DeadlineReader) TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.TransactionReceipt(callCtx, hash)
}

func (reader *DeadlineReader) FilterLogs(ctx context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	callCtx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	return reader.reader.FilterLogs(callCtx, query)
}

type DeadlineLogSubscriber struct {
	subscriber LogSubscriber
	timeout    time.Duration
}

func NewDeadlineLogSubscriber(subscriber LogSubscriber, timeout time.Duration) (*DeadlineLogSubscriber, error) {
	if nilCapability(subscriber) || timeout <= 0 {
		return nil, ErrInvalidDeadlineWrapper
	}
	return &DeadlineLogSubscriber{subscriber: subscriber, timeout: timeout}, nil
}

func (subscriber *DeadlineLogSubscriber) SubscribeFilterLogs(
	ctx context.Context,
	query ethereum.FilterQuery,
	logs chan<- types.Log,
) (ethereum.Subscription, error) {
	setupCtx, cancel := context.WithTimeout(ctx, subscriber.timeout)
	defer cancel()
	return subscriber.subscriber.SubscribeFilterLogs(setupCtx, query, logs)
}

type DeadlineHeadSubscriber struct {
	subscriber HeadSubscriber
	timeout    time.Duration
}

func NewDeadlineHeadSubscriber(subscriber HeadSubscriber, timeout time.Duration) (*DeadlineHeadSubscriber, error) {
	if nilCapability(subscriber) || timeout <= 0 {
		return nil, ErrInvalidDeadlineWrapper
	}
	return &DeadlineHeadSubscriber{subscriber: subscriber, timeout: timeout}, nil
}

func nilCapability(capability any) bool {
	if capability == nil {
		return true
	}
	value := reflect.ValueOf(capability)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (subscriber *DeadlineHeadSubscriber) SubscribeNewHead(
	ctx context.Context,
	heads chan<- *types.Header,
) (ethereum.Subscription, error) {
	setupCtx, cancel := context.WithTimeout(ctx, subscriber.timeout)
	defer cancel()
	return subscriber.subscriber.SubscribeNewHead(setupCtx, heads)
}

var _ Reader = (*DeadlineReader)(nil)
var _ Broadcaster = (*DeadlineBroadcaster)(nil)
var _ LogSubscriber = (*DeadlineLogSubscriber)(nil)
var _ HeadSubscriber = (*DeadlineHeadSubscriber)(nil)
