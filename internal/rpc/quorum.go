package rpc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
)

const (
	maxLogsPerResponse = 20_000
	maxTopicsPerLog    = 4
	maxLogDataBytes    = 1 << 20
)

var (
	ErrInvalidProviders   = errors.New("invalid RPC quorum providers")
	ErrInvalidTimeout     = errors.New("invalid RPC timeout")
	ErrQuorumUnavailable  = errors.New("RPC quorum unavailable")
	ErrQuorumMismatch     = errors.New("RPC quorum responses do not match")
	ErrMalformedResponse  = errors.New("malformed RPC response")
	ErrInvalidBlockRef    = errors.New("invalid block reference")
	ErrInvalidFilterQuery = errors.New("invalid finalized log query")
	ErrUnfinalizedReceipt = errors.New("transaction receipt is not finalized")
	ErrQuorumDial         = errors.New("failed to connect to RPC quorum")
)

type BlockRef struct {
	Number     uint64
	Hash       common.Hash
	ParentHash common.Hash
	Timestamp  uint64
}

type ProviderIdentity struct {
	ID          string
	Fingerprint string
	TrustDomain string
}

type Provider struct {
	Identity ProviderIdentity
	Reader   HistoricalReader
	Close    func()
}

type ProviderEndpoint struct {
	Identity ProviderIdentity
	Endpoint string
}

type QuorumReader struct {
	providers []Provider
	timeout   time.Duration
	closeOnce sync.Once
}

func NewQuorumReader(providers []Provider, timeout time.Duration) (*QuorumReader, error) {
	if timeout <= 0 {
		return nil, ErrInvalidTimeout
	}
	if err := validateProviders(providers); err != nil {
		return nil, err
	}
	return &QuorumReader{
		providers: append([]Provider(nil), providers...),
		timeout:   timeout,
	}, nil
}

func validateProviders(providers []Provider) error {
	if len(providers) < 2 {
		return ErrInvalidProviders
	}

	identities := make([]ProviderIdentity, len(providers))
	readers := make(map[readerIdentity]struct{}, len(providers))
	for index, provider := range providers {
		identities[index] = provider.Identity
		readerID, ok := readerInstanceID(provider.Reader)
		if !ok || provider.Close == nil {
			return ErrInvalidProviders
		}
		if _, exists := readers[readerID]; exists {
			return ErrInvalidProviders
		}
		readers[readerID] = struct{}{}
	}
	return validateProviderIdentities(identities)
}

func validateProviderIdentities(identities []ProviderIdentity) error {
	if len(identities) < 2 {
		return ErrInvalidProviders
	}
	ids := make(map[string]struct{}, len(identities))
	fingerprints := make(map[string]struct{}, len(identities))
	trustDomains := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		id := normalizedIdentity(identity.ID)
		fingerprint := normalizedIdentity(identity.Fingerprint)
		trustDomain := normalizedIdentity(identity.TrustDomain)
		if id == "" || fingerprint == "" || trustDomain == "" {
			return ErrInvalidProviders
		}
		if _, exists := ids[id]; exists {
			return ErrInvalidProviders
		}
		if _, exists := fingerprints[fingerprint]; exists {
			return ErrInvalidProviders
		}
		if _, exists := trustDomains[trustDomain]; exists {
			return ErrInvalidProviders
		}
		ids[id] = struct{}{}
		fingerprints[fingerprint] = struct{}{}
		trustDomains[trustDomain] = struct{}{}
	}
	return nil
}

func normalizedIdentity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

type readerIdentity struct {
	typeOf  reflect.Type
	pointer uintptr
}

func readerInstanceID(reader HistoricalReader) (readerIdentity, bool) {
	if reader == nil {
		return readerIdentity{}, false
	}
	value := reflect.ValueOf(reader)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return readerIdentity{}, false
	}
	return readerIdentity{typeOf: value.Type(), pointer: value.Pointer()}, true
}

func (reader *QuorumReader) Close() {
	reader.closeOnce.Do(func() {
		for _, provider := range reader.providers {
			provider.Close()
		}
	})
}

func (reader *QuorumReader) Finalized(ctx context.Context) (BlockRef, error) {
	headers, err := collectProviders(reader, ctx, func(callCtx context.Context, provider HistoricalReader) (*types.Header, error) {
		return provider.HeaderByNumber(callCtx, big.NewInt(int64(gethrpc.FinalizedBlockNumber)))
	})
	if err != nil {
		return BlockRef{}, err
	}

	minimum := ^uint64(0)
	for _, header := range headers {
		ref, refErr := blockRefFromHeader(header, nil)
		if refErr != nil {
			return BlockRef{}, refErr
		}
		if ref.Number < minimum {
			minimum = ref.Number
		}
	}
	return reader.Header(ctx, minimum)
}

func (reader *QuorumReader) Header(ctx context.Context, number uint64) (BlockRef, error) {
	headers, err := collectProviders(reader, ctx, func(callCtx context.Context, provider HistoricalReader) (*types.Header, error) {
		return provider.HeaderByNumber(callCtx, new(big.Int).SetUint64(number))
	})
	if err != nil {
		return BlockRef{}, err
	}

	var agreed BlockRef
	for index, header := range headers {
		ref, refErr := blockRefFromHeader(header, &number)
		if refErr != nil {
			return BlockRef{}, refErr
		}
		if index == 0 {
			agreed = ref
			continue
		}
		if ref != agreed {
			return BlockRef{}, ErrQuorumMismatch
		}
	}
	return agreed, nil
}

func blockRefFromHeader(header *types.Header, expected *uint64) (BlockRef, error) {
	if header == nil || header.Number == nil || !header.Number.IsUint64() || header.Difficulty == nil ||
		header.Difficulty.Sign() < 0 || header.Extra == nil || header.GasUsed > header.GasLimit ||
		(header.BaseFee != nil && header.BaseFee.Sign() < 0) || header.SanityCheck() != nil {
		return BlockRef{}, ErrMalformedResponse
	}
	number := header.Number.Uint64()
	if expected != nil && number != *expected {
		return BlockRef{}, ErrMalformedResponse
	}
	return BlockRef{Number: number, Hash: header.Hash(), ParentHash: header.ParentHash, Timestamp: header.Time}, nil
}

func (reader *QuorumReader) FilterLogs(ctx context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	anchor, err := explicitFilterAnchor(query)
	if err != nil {
		return nil, err
	}

	responses, err := collectProviders(reader, ctx, func(callCtx context.Context, provider HistoricalReader) ([]types.Log, error) {
		return provider.FilterLogs(callCtx, cloneFilterQuery(query))
	})
	if err != nil {
		return nil, err
	}

	var agreed []types.Log
	var agreedEncoding [][]byte
	for index, response := range responses {
		normalized, encoding, normalizeErr := normalizeLogs(response, anchor)
		if normalizeErr != nil {
			return nil, normalizeErr
		}
		if index == 0 {
			agreed = normalized
			agreedEncoding = encoding
			continue
		}
		if !equalEncoding(agreedEncoding, encoding) {
			return nil, ErrQuorumMismatch
		}
	}
	return agreed, nil
}

type logFilterAnchor struct {
	from      uint64
	to        uint64
	blockHash *common.Hash
}

func explicitFilterAnchor(query ethereum.FilterQuery) (logFilterAnchor, error) {
	if query.BlockHash != nil {
		if query.FromBlock != nil || query.ToBlock != nil || *query.BlockHash == (common.Hash{}) {
			return logFilterAnchor{}, ErrInvalidFilterQuery
		}
		hash := *query.BlockHash
		return logFilterAnchor{blockHash: &hash}, nil
	}
	if query.FromBlock == nil || query.ToBlock == nil || !query.FromBlock.IsUint64() || !query.ToBlock.IsUint64() {
		return logFilterAnchor{}, ErrInvalidFilterQuery
	}
	from := query.FromBlock.Uint64()
	to := query.ToBlock.Uint64()
	if from > to {
		return logFilterAnchor{}, ErrInvalidFilterQuery
	}
	return logFilterAnchor{from: from, to: to}, nil
}

func normalizeLogs(logs []types.Log, anchor logFilterAnchor) ([]types.Log, [][]byte, error) {
	if logs == nil || len(logs) > maxLogsPerResponse {
		return nil, nil, ErrMalformedResponse
	}
	normalized := make([]types.Log, 0, len(logs))
	seen := make(map[logIdentity][]byte, len(logs))
	for _, log := range logs {
		if err := validateLog(log, anchor); err != nil {
			return nil, nil, err
		}
		cloned := cloneLog(log)
		identity := logIdentity{blockHash: cloned.BlockHash, transactionHash: cloned.TxHash, logIndex: cloned.Index}
		encoded := encodeLog(cloned)
		if previous, duplicate := seen[identity]; duplicate {
			if !bytes.Equal(previous, encoded) {
				return nil, nil, ErrMalformedResponse
			}
			continue
		}
		seen[identity] = encoded
		normalized = append(normalized, cloned)
	}
	sort.Slice(normalized, func(left, right int) bool {
		if normalized[left].BlockNumber != normalized[right].BlockNumber {
			return normalized[left].BlockNumber < normalized[right].BlockNumber
		}
		if normalized[left].TxIndex != normalized[right].TxIndex {
			return normalized[left].TxIndex < normalized[right].TxIndex
		}
		if normalized[left].Index != normalized[right].Index {
			return normalized[left].Index < normalized[right].Index
		}
		return bytes.Compare(encodeLog(normalized[left]), encodeLog(normalized[right])) < 0
	})
	encoding := make([][]byte, len(normalized))
	for index := range normalized {
		encoding[index] = encodeLog(normalized[index])
	}
	return normalized, encoding, nil
}

type logIdentity struct {
	blockHash       common.Hash
	transactionHash common.Hash
	logIndex        uint
}

func validateLog(log types.Log, anchor logFilterAnchor) error {
	if log.Removed || log.BlockHash == (common.Hash{}) ||
		log.TxHash == (common.Hash{}) || log.Topics == nil || len(log.Topics) > maxTopicsPerLog ||
		log.Data == nil || len(log.Data) > maxLogDataBytes {
		return ErrMalformedResponse
	}
	if anchor.blockHash != nil {
		if log.BlockHash != *anchor.blockHash {
			return ErrMalformedResponse
		}
	} else if log.BlockNumber < anchor.from || log.BlockNumber > anchor.to {
		return ErrMalformedResponse
	}
	return nil
}

func cloneLog(log types.Log) types.Log {
	log.Topics = append([]common.Hash(nil), log.Topics...)
	if log.Topics == nil {
		log.Topics = []common.Hash{}
	}
	log.Data = append([]byte(nil), log.Data...)
	if log.Data == nil {
		log.Data = []byte{}
	}
	return log
}

func encodeLog(log types.Log) []byte {
	encoded := make([]byte, 0, 20+len(log.Topics)*common.HashLength+len(log.Data)+128)
	encoded = append(encoded, log.Address[:]...)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(log.Topics)))
	for _, topic := range log.Topics {
		encoded = append(encoded, topic[:]...)
	}
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(log.Data)))
	encoded = append(encoded, log.Data...)
	encoded = binary.BigEndian.AppendUint64(encoded, log.BlockNumber)
	encoded = append(encoded, log.TxHash[:]...)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(log.TxIndex))
	encoded = append(encoded, log.BlockHash[:]...)
	encoded = binary.BigEndian.AppendUint64(encoded, log.BlockTimestamp)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(log.Index))
	return append(encoded, 0)
}

func equalEncoding(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !bytes.Equal(left[index], right[index]) {
			return false
		}
	}
	return true
}

func (reader *QuorumReader) BalanceAt(ctx context.Context, block BlockRef, account common.Address) (*big.Int, error) {
	if err := reader.requireCanonicalBlock(ctx, block); err != nil {
		return nil, err
	}
	balances, err := collectProviders(reader, ctx, func(callCtx context.Context, provider HistoricalReader) (*big.Int, error) {
		return provider.BalanceAtHash(callCtx, account, block.Hash)
	})
	if err != nil {
		return nil, err
	}
	var agreed *big.Int
	for index, balance := range balances {
		if balance == nil || balance.Sign() < 0 {
			return nil, ErrMalformedResponse
		}
		if index == 0 {
			agreed = new(big.Int).Set(balance)
			continue
		}
		if agreed.Cmp(balance) != 0 {
			return nil, ErrQuorumMismatch
		}
	}
	return agreed, nil
}

func (reader *QuorumReader) CodeAt(ctx context.Context, block BlockRef, account common.Address) ([]byte, error) {
	if err := reader.requireCanonicalBlock(ctx, block); err != nil {
		return nil, err
	}
	responses, err := collectProviders(reader, ctx, func(callCtx context.Context, provider HistoricalReader) ([]byte, error) {
		return provider.CodeAtHash(callCtx, account, block.Hash)
	})
	return unanimousBytes(responses, err)
}

func (reader *QuorumReader) CallContract(ctx context.Context, block BlockRef, call ethereum.CallMsg) ([]byte, error) {
	if err := reader.requireCanonicalBlock(ctx, block); err != nil {
		return nil, err
	}
	responses, err := collectProviders(reader, ctx, func(callCtx context.Context, provider HistoricalReader) ([]byte, error) {
		return provider.CallContractAtHash(callCtx, cloneCallMsg(call), block.Hash)
	})
	return unanimousBytes(responses, err)
}

func unanimousBytes(responses [][]byte, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	var agreed []byte
	for index, response := range responses {
		if response == nil {
			return nil, ErrMalformedResponse
		}
		if index == 0 {
			agreed = append([]byte{}, response...)
			continue
		}
		if !bytes.Equal(agreed, response) {
			return nil, ErrQuorumMismatch
		}
	}
	return agreed, nil
}

func (reader *QuorumReader) requireCanonicalBlock(ctx context.Context, block BlockRef) error {
	if block.Hash == (common.Hash{}) {
		return ErrInvalidBlockRef
	}
	canonical, err := reader.Header(ctx, block.Number)
	if err != nil {
		return err
	}
	if canonical != block {
		return ErrInvalidBlockRef
	}
	return nil
}

func (reader *QuorumReader) Receipt(ctx context.Context, transaction common.Hash) (*types.Receipt, error) {
	if transaction == (common.Hash{}) {
		return nil, ErrMalformedResponse
	}
	finalized, err := reader.Finalized(ctx)
	if err != nil {
		return nil, err
	}
	receipts, err := collectProviders(reader, ctx, func(callCtx context.Context, provider HistoricalReader) (*types.Receipt, error) {
		return provider.TransactionReceipt(callCtx, transaction)
	})
	if err != nil {
		return nil, err
	}

	var agreed *types.Receipt
	var agreedJSON []byte
	for index, receipt := range receipts {
		if err := validateReceipt(receipt, transaction); err != nil {
			return nil, err
		}
		encoded, marshalErr := json.Marshal(receipt)
		if marshalErr != nil {
			return nil, ErrMalformedResponse
		}
		if index == 0 {
			agreed = receipt
			agreedJSON = encoded
			continue
		}
		if !bytes.Equal(agreedJSON, encoded) {
			return nil, ErrQuorumMismatch
		}
	}

	number := agreed.BlockNumber.Uint64()
	if number > finalized.Number {
		return nil, ErrUnfinalizedReceipt
	}
	canonical, err := reader.Header(ctx, number)
	if err != nil {
		return nil, err
	}
	if agreed.BlockHash != canonical.Hash {
		return nil, ErrQuorumMismatch
	}
	return agreed, nil
}

func (reader *QuorumReader) TransactionReceipt(ctx context.Context, transaction common.Hash) (*types.Receipt, error) {
	return reader.Receipt(ctx, transaction)
}

func validateReceipt(receipt *types.Receipt, transaction common.Hash) error {
	if receipt == nil || receipt.TxHash != transaction || receipt.BlockHash == (common.Hash{}) ||
		receipt.BlockNumber == nil || !receipt.BlockNumber.IsUint64() || receipt.Logs == nil ||
		len(receipt.Logs) > maxLogsPerResponse ||
		receipt.Status > types.ReceiptStatusSuccessful || (len(receipt.PostState) != 0 && len(receipt.PostState) != common.HashLength) ||
		(len(receipt.PostState) != 0 && receipt.Status != types.ReceiptStatusFailed) ||
		receipt.CumulativeGasUsed < receipt.GasUsed ||
		(receipt.EffectiveGasPrice != nil && receipt.EffectiveGasPrice.Sign() < 0) ||
		(receipt.BlobGasPrice != nil && receipt.BlobGasPrice.Sign() < 0) {
		return ErrMalformedResponse
	}
	number := receipt.BlockNumber.Uint64()
	for _, log := range receipt.Logs {
		if log == nil || log.BlockNumber != number || log.BlockHash != receipt.BlockHash || log.TxHash != transaction ||
			log.TxIndex != receipt.TransactionIndex {
			return ErrMalformedResponse
		}
		if err := validateLog(*log, logFilterAnchor{from: number, to: number}); err != nil {
			return err
		}
	}
	if receipt.Bloom != types.CreateBloom(receipt) {
		return ErrMalformedResponse
	}
	return nil
}

func cloneFilterQuery(query ethereum.FilterQuery) ethereum.FilterQuery {
	cloned := query
	cloned.BlockHash = cloneHash(query.BlockHash)
	cloned.FromBlock = cloneBig(query.FromBlock)
	cloned.ToBlock = cloneBig(query.ToBlock)
	cloned.Addresses = append([]common.Address(nil), query.Addresses...)
	if query.Topics != nil {
		cloned.Topics = make([][]common.Hash, len(query.Topics))
		for index := range query.Topics {
			cloned.Topics[index] = append([]common.Hash(nil), query.Topics[index]...)
		}
	}
	return cloned
}

func cloneCallMsg(call ethereum.CallMsg) ethereum.CallMsg {
	cloned := call
	if call.To != nil {
		to := *call.To
		cloned.To = &to
	}
	cloned.GasPrice = cloneBig(call.GasPrice)
	cloned.GasFeeCap = cloneBig(call.GasFeeCap)
	cloned.GasTipCap = cloneBig(call.GasTipCap)
	cloned.Value = cloneBig(call.Value)
	cloned.BlobGasFeeCap = cloneBig(call.BlobGasFeeCap)
	cloned.Data = append([]byte(nil), call.Data...)
	cloned.BlobHashes = append([]common.Hash(nil), call.BlobHashes...)
	cloned.AuthorizationList = append([]types.SetCodeAuthorization(nil), call.AuthorizationList...)
	if call.AccessList != nil {
		cloned.AccessList = make(types.AccessList, len(call.AccessList))
		for index, tuple := range call.AccessList {
			cloned.AccessList[index] = tuple
			cloned.AccessList[index].StorageKeys = append([]common.Hash(nil), tuple.StorageKeys...)
		}
	}
	return cloned
}

func cloneBig(value *big.Int) *big.Int {
	if value == nil {
		return nil
	}
	return new(big.Int).Set(value)
}

func cloneHash(value *common.Hash) *common.Hash {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type providerResult[T any] struct {
	index      int
	value      T
	err        error
	contextErr error
}

func collectProviders[T any](
	reader *QuorumReader,
	ctx context.Context,
	call func(context.Context, HistoricalReader) (T, error),
) ([]T, error) {
	callGroupCtx, cancelGroup := context.WithCancel(ctx)
	defer cancelGroup()
	phaseTimer := time.NewTimer(reader.timeout)
	defer phaseTimer.Stop()

	results := make(chan providerResult[T], len(reader.providers))
	for index, provider := range reader.providers {
		go func() {
			callCtx, cancel := context.WithTimeout(callGroupCtx, reader.timeout)
			value, err := call(callCtx, provider.Reader)
			contextErr := callCtx.Err()
			cancel()
			results <- providerResult[T]{index: index, value: value, err: err, contextErr: contextErr}
		}()
	}

	values := make([]T, len(reader.providers))
	for range reader.providers {
		select {
		case result := <-results:
			values[result.index] = result.value
			if result.err != nil || result.contextErr != nil {
				cancelGroup()
				return nil, redactedProviderError(ctx, result.err, result.contextErr)
			}
		case <-ctx.Done():
			cancelGroup()
			return nil, redactedProviderError(ctx, nil, ctx.Err())
		case <-phaseTimer.C:
			cancelGroup()
			return nil, redactedProviderError(ctx, nil, context.DeadlineExceeded)
		}
	}
	return values, nil
}

func redactedProviderError(parent context.Context, backendErr, callContextErr error) error {
	if parent.Err() != nil {
		return fmt.Errorf("%w: %w", ErrQuorumUnavailable, parent.Err())
	}
	if errors.Is(callContextErr, context.DeadlineExceeded) || errors.Is(backendErr, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrQuorumUnavailable, context.DeadlineExceeded)
	}
	if errors.Is(callContextErr, context.Canceled) || errors.Is(backendErr, context.Canceled) {
		return fmt.Errorf("%w: %w", ErrQuorumUnavailable, context.Canceled)
	}
	return ErrQuorumUnavailable
}

var _ FinalizedReader = (*QuorumReader)(nil)
var _ Closer = (*QuorumReader)(nil)
