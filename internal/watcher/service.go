package watcher

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"time"

	"guard-daemon/internal/clock"
	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	pollInterval      = 3 * time.Second
	periodicInterval  = 12 * time.Second
	pollPeriodicEvery = 10

	eventCandidateInserted     observability.EventCode = "watcher_candidate_inserted"
	eventCandidatePending      observability.EventCode = "watcher_candidate_pending"
	eventCandidateAcknowledged observability.EventCode = "watcher_candidate_acknowledged"
	eventSubscriptionFallback  observability.EventCode = "watcher_subscription_fallback"
	eventHeadUnavailable       observability.EventCode = "watcher_head_unavailable"
	eventReadFailed            observability.EventCode = "watcher_read_failed"
	eventMetadataFallback      observability.EventCode = "watcher_metadata_fallback"
	eventLogRejected           observability.EventCode = "watcher_log_rejected"
	eventCandidatePutFailed    observability.EventCode = "watcher_candidate_put_failed"

	errorLogSubscription  domain.ErrorCode = "watcher_log_subscription_failed"
	errorHeadSubscription domain.ErrorCode = "watcher_head_subscription_failed"
	errorBlockRead        domain.ErrorCode = "watcher_block_read_failed"
	errorHeaderRead       domain.ErrorCode = "watcher_header_read_failed"
	errorLogRead          domain.ErrorCode = "watcher_log_read_failed"
	errorInvalidHead      domain.ErrorCode = "watcher_invalid_head"
	errorInvalidLog       domain.ErrorCode = "watcher_invalid_log"
	errorMetadata         domain.ErrorCode = "watcher_metadata_unavailable"
	errorCandidatePut     domain.ErrorCode = "watcher_candidate_put_failed"
	errorCandidateResult  domain.ErrorCode = "watcher_candidate_result_invalid"
	errorTickerClosed     domain.ErrorCode = "watcher_ticker_closed"
)

var (
	ErrInvalidDependencies = errors.New("invalid watcher dependencies")
	errPollingFallback     = errors.New("watcher polling fallback")
)

// BlockReader is the block-specific subset needed by the watcher.
type BlockReader interface {
	BlockNumber(context.Context) (uint64, error)
	HeaderByNumber(context.Context, *big.Int) (*types.Header, error)
}

// CandidateQueue is the producer-only subset of store.CandidateQueue.
type CandidateQueue interface {
	Put(context.Context, domain.RescueCandidate) (store.PutResult, error)
}

type Dependencies struct {
	Contracts      rpc.ContractCaller
	Logs           rpc.LogReader
	Blocks         BlockReader
	LogSubscriber  rpc.LogSubscriber
	HeadSubscriber rpc.HeadSubscriber
	Codec          *contracts.ERC20Codec
	Clock          clock.Clock
	Observer       observability.Observer
	Queue          CandidateQueue
	Source         common.Address
	Network        domain.Network
	Generation     uint64
}

type Service struct {
	contracts      rpc.ContractCaller
	logs           rpc.LogReader
	blocks         BlockReader
	logSubscriber  rpc.LogSubscriber
	headSubscriber rpc.HeadSubscriber
	codec          *contracts.ERC20Codec
	clock          clock.Clock
	observer       observability.Observer
	queue          CandidateQueue
	source         common.Address
	networkID      domain.NetworkID
	networkName    string
	generation     uint64
	knownTokens    map[common.Address]domain.Token
}

func NewService(dependencies Dependencies) (*Service, error) {
	if dependencies.Contracts == nil || dependencies.Logs == nil || dependencies.Blocks == nil ||
		dependencies.LogSubscriber == nil || dependencies.HeadSubscriber == nil || dependencies.Codec == nil ||
		dependencies.Clock == nil || dependencies.Observer == nil || dependencies.Queue == nil {
		return nil, ErrInvalidDependencies
	}

	knownTokens := make(map[common.Address]domain.Token, len(dependencies.Network.Tokens))
	for _, token := range dependencies.Network.Tokens {
		knownTokens[token.Address] = token
	}

	return &Service{
		contracts:      dependencies.Contracts,
		logs:           dependencies.Logs,
		blocks:         dependencies.Blocks,
		logSubscriber:  dependencies.LogSubscriber,
		headSubscriber: dependencies.HeadSubscriber,
		codec:          dependencies.Codec,
		clock:          dependencies.Clock,
		observer:       dependencies.Observer,
		queue:          dependencies.Queue,
		source:         dependencies.Source,
		networkID:      dependencies.Network.ChainID,
		networkName:    dependencies.Network.Name,
		generation:     dependencies.Generation,
		knownTokens:    knownTokens,
	}, nil
}

func (service *Service) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	query := service.transferQuery()
	if err := service.watchSubscriptions(ctx, query); !errors.Is(err, errPollingFallback) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return service.poll(ctx, query)
}

func (service *Service) transferQuery() ethereum.FilterQuery {
	return ethereum.FilterQuery{
		Topics: [][]common.Hash{
			{service.codec.TransferTopic()},
			nil,
			{common.BytesToHash(service.source.Bytes())},
		},
	}
}

func (service *Service) watchSubscriptions(ctx context.Context, query ethereum.FilterQuery) error {
	logs := make(chan types.Log, 100)
	logSubscription, err := service.logSubscriber.SubscribeFilterLogs(ctx, query, logs)
	if err != nil || logSubscription == nil {
		if logSubscription != nil {
			logSubscription.Unsubscribe()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		service.recordFailure(eventSubscriptionFallback, errorLogSubscription)
		return errPollingFallback
	}
	defer logSubscription.Unsubscribe()

	heads := make(chan *types.Header, 16)
	headSubscription, err := service.headSubscriber.SubscribeNewHead(ctx, heads)
	if err != nil || headSubscription == nil {
		if headSubscription != nil {
			headSubscription.Unsubscribe()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		service.recordFailure(eventHeadUnavailable, errorHeadSubscription)
		headSubscription = nil
		heads = nil
	}
	defer func() {
		if headSubscription != nil {
			headSubscription.Unsubscribe()
		}
	}()

	var headErrors <-chan error
	if headSubscription != nil {
		headErrors = headSubscription.Err()
	}
	logErrors := logSubscription.Err()
	periodic := service.clock.NewTicker(periodicInterval)
	if periodic == nil {
		return domain.NewError("watcher.ticker", domain.ErrorInternal, errorTickerClosed, false, false, nil)
	}
	defer periodic.Stop()

	disableHeads := func() {
		if headSubscription != nil {
			headSubscription.Unsubscribe()
			headSubscription = nil
		}
		heads = nil
		headErrors = nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-logErrors:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			service.recordFailure(eventSubscriptionFallback, errorLogSubscription)
			return domain.NewError("watcher.subscription", domain.ErrorRPCTransient, errorLogSubscription, true, false, nil)
		case logEntry, ok := <-logs:
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				service.recordFailure(eventSubscriptionFallback, errorLogSubscription)
				return domain.NewError("watcher.subscription", domain.ErrorRPCTransient, errorLogSubscription, true, false, nil)
			}
			if err := service.putLogCandidate(ctx, logEntry); err != nil {
				return err
			}
		case header, ok := <-heads:
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				service.recordFailure(eventHeadUnavailable, errorHeadSubscription)
				disableHeads()
				continue
			}
			if err := service.putHeadCandidate(ctx, header); err != nil {
				return err
			}
		case <-headErrors:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			service.recordFailure(eventHeadUnavailable, errorHeadSubscription)
			disableHeads()
		case _, ok := <-periodic.C():
			if !ok {
				return domain.NewError("watcher.ticker", domain.ErrorInternal, errorTickerClosed, false, false, nil)
			}
			candidate := service.nextPeriodicCandidate()
			if err := service.putCandidate(ctx, candidate); err != nil {
				return err
			}
		}
	}
}

func (service *Service) poll(ctx context.Context, query ethereum.FilterQuery) error {
	ticker := service.clock.NewTicker(pollInterval)
	if ticker == nil {
		return domain.NewError("watcher.ticker", domain.ErrorInternal, errorTickerClosed, false, false, nil)
	}
	defer ticker.Stop()

	var (
		lastBlock   uint64
		initialized bool
		polls       uint64
	)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-ticker.C():
			if !ok {
				return domain.NewError("watcher.ticker", domain.ErrorInternal, errorTickerClosed, false, false, nil)
			}
		}

		polls++
		if polls%pollPeriodicEvery == 0 {
			candidate := service.nextPeriodicCandidate()
			if err := service.putCandidate(ctx, candidate); err != nil {
				return err
			}
		}

		currentBlock, err := service.blocks.BlockNumber(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			service.recordFailure(eventReadFailed, errorBlockRead)
			continue
		}
		if !initialized {
			lastBlock = currentBlock
			initialized = true
			continue
		}
		if currentBlock <= lastBlock {
			continue
		}

		fromBlock := lastBlock + 1
		lastBlock = currentBlock
		if err := service.putPolledHeadCandidate(ctx, currentBlock); err != nil {
			return err
		}

		pollQuery := query
		pollQuery.FromBlock = new(big.Int).SetUint64(fromBlock)
		pollQuery.ToBlock = new(big.Int).SetUint64(currentBlock)
		logs, err := service.logs.FilterLogs(ctx, pollQuery)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			service.recordFailure(eventReadFailed, errorLogRead)
			continue
		}
		for _, logEntry := range logs {
			if err := service.putLogCandidate(ctx, logEntry); err != nil {
				return err
			}
		}
	}
}

func (service *Service) nextPeriodicCandidate() domain.RescueCandidate {
	observation := service.clock.Now().Unix()
	if observation < 0 {
		observation = 0
	}
	slot := uint64(observation / int64(periodicInterval/time.Second))
	return domain.NewPeriodicCandidate(service.networkID, service.source, service.generation, slot)
}

func (service *Service) putPolledHeadCandidate(ctx context.Context, blockNumber uint64) error {
	header, err := service.blocks.HeaderByNumber(ctx, new(big.Int).SetUint64(blockNumber))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		service.recordFailure(eventReadFailed, errorHeaderRead)
		return nil
	}
	if header == nil || header.Number == nil || !header.Number.IsUint64() || header.Number.Uint64() != blockNumber {
		service.recordFailure(eventReadFailed, errorInvalidHead)
		return nil
	}
	return service.putHeadCandidate(ctx, header)
}

func (service *Service) putHeadCandidate(ctx context.Context, header *types.Header) error {
	if header == nil || header.Number == nil || !header.Number.IsUint64() {
		service.recordFailure(eventReadFailed, errorInvalidHead)
		return nil
	}
	candidate := domain.NewBlockCandidate(
		service.networkID,
		domain.CandidateNative,
		service.source,
		header.Hash(),
		header.Number.Uint64(),
	)
	return service.putCandidate(ctx, candidate)
}

func (service *Service) putLogCandidate(ctx context.Context, logEntry types.Log) error {
	if len(logEntry.Topics) < 3 || logEntry.Topics[0] != service.codec.TransferTopic() ||
		logEntry.Topics[2] != common.BytesToHash(service.source.Bytes()) {
		service.recordFailure(eventLogRejected, errorInvalidLog)
		return nil
	}

	token := service.resolveToken(ctx, logEntry.Address)
	if err := ctx.Err(); err != nil {
		return err
	}
	candidate := domain.NewLogCandidate(
		service.networkID,
		service.source,
		token,
		logEntry.BlockHash,
		logEntry.TxHash,
		logEntry.BlockNumber,
		logEntry.Index,
	)
	return service.putCandidate(ctx, candidate)
}

func (service *Service) putCandidate(ctx context.Context, candidate domain.RescueCandidate) error {
	result, err := service.queue.Put(ctx, candidate)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		service.recordFailure(eventCandidatePutFailed, errorCandidatePut)
		return domain.NewError("watcher.candidate.put", domain.ErrorInternal, errorCandidatePut, true, false, err)
	}

	var code observability.EventCode
	switch result {
	case store.PutInserted:
		code = eventCandidateInserted
	case store.PutAlreadyPending:
		code = eventCandidatePending
	case store.PutAlreadyAcknowledged:
		code = eventCandidateAcknowledged
	default:
		service.recordFailure(eventCandidatePutFailed, errorCandidateResult)
		return domain.NewError("watcher.candidate.result", domain.ErrorInternal, errorCandidateResult, false, false, nil)
	}

	tokenSymbol := sanitizeSymbol(candidate.Token.Symbol)
	if tokenSymbol == "" && candidate.Token.Address != (common.Address{}) {
		tokenSymbol = addressFallback(candidate.Token.Address)
	}
	service.observer.Record(observability.Event{
		Level:       observability.LevelInfo,
		Code:        code,
		NetworkName: service.networkName,
		Candidate:   candidate.ID,
		TokenSymbol: tokenSymbol,
	})
	return nil
}

func (service *Service) resolveToken(ctx context.Context, address common.Address) domain.Token {
	if token, ok := service.knownTokens[address]; ok {
		return token
	}

	token := domain.Token{Address: address, Symbol: addressFallback(address), Decimals: 18}
	metadataFallback := false

	if data, err := service.codec.PackSymbol(); err != nil {
		metadataFallback = true
	} else if result, err := service.contracts.CallContract(ctx, ethereum.CallMsg{To: &address, Data: data}, nil); err != nil {
		metadataFallback = true
	} else if symbol, err := service.codec.DecodeSymbol(result); err != nil {
		metadataFallback = true
	} else if symbol = sanitizeSymbol(symbol); symbol == "" {
		metadataFallback = true
	} else {
		token.Symbol = symbol
	}

	if ctx.Err() == nil {
		if data, err := service.codec.PackDecimals(); err != nil {
			metadataFallback = true
		} else if result, err := service.contracts.CallContract(ctx, ethereum.CallMsg{To: &address, Data: data}, nil); err != nil {
			metadataFallback = true
		} else if decimals, err := service.codec.DecodeDecimals(result); err != nil {
			metadataFallback = true
		} else {
			token.Decimals = decimals
		}
	}

	if metadataFallback && ctx.Err() == nil {
		service.recordFailure(eventMetadataFallback, errorMetadata)
	}
	return token
}

func (service *Service) recordFailure(code observability.EventCode, errorCode domain.ErrorCode) {
	service.observer.Record(observability.Event{
		Level:       observability.LevelWarning,
		Code:        code,
		NetworkName: service.networkName,
		ErrorCode:   errorCode,
	})
}

func sanitizeSymbol(symbol string) string {
	var sanitized strings.Builder
	for _, character := range symbol {
		if character >= 0x20 && character <= 0x7e {
			sanitized.WriteByte(byte(character))
		}
	}

	result := strings.TrimSpace(sanitized.String())
	const maxLength = 16
	if len(result) > maxLength {
		return result[:maxLength] + "..."
	}
	return result
}

func addressFallback(address common.Address) string {
	const prefixLength = 10
	return address.Hex()[:prefixLength] + "..."
}
