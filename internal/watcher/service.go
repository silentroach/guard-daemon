package watcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
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
	pollInterval            = 3 * time.Second
	reconciliationInterval  = time.Minute
	metadataReturnLimit     = 4 * 1024
	metadataCacheLimit      = 256
	maximumLookbackBlocks   = uint64(10_000)
	maximumDiscoveredTokens = 1024

	eventCandidateInserted     observability.EventCode = "watcher_candidate_inserted"
	eventCandidatePending      observability.EventCode = "watcher_candidate_pending"
	eventCandidateAcknowledged observability.EventCode = "watcher_candidate_acknowledged"
	eventSubscriptionFallback  observability.EventCode = "watcher_subscription_fallback"
	eventHeadUnavailable       observability.EventCode = "watcher_head_unavailable"
	eventReadFailed            observability.EventCode = "watcher_read_failed"
	eventMetadataFallback      observability.EventCode = "watcher_metadata_fallback"
	eventLogRejected           observability.EventCode = "watcher_log_rejected"
	eventCandidatePutFailed    observability.EventCode = "watcher_candidate_put_failed"
	eventObservationSaturated  observability.EventCode = "watcher_observation_saturated"
	eventDiscoveryOverflow     observability.EventCode = "watcher_discovery_overflow"

	errorLogSubscription  domain.ErrorCode = "watcher_log_subscription_failed"
	errorHeadSubscription domain.ErrorCode = "watcher_head_subscription_failed"
	errorFinalizedRead    domain.ErrorCode = "watcher_finalized_read_failed"
	errorFinalizedReorg   domain.ErrorCode = "watcher_finalized_reorg"
	errorLogRead          domain.ErrorCode = "watcher_log_read_failed"
	errorInvalidHead      domain.ErrorCode = "watcher_invalid_head"
	errorInvalidLog       domain.ErrorCode = "watcher_invalid_log"
	errorMetadata         domain.ErrorCode = "watcher_metadata_unavailable"
	errorCandidatePut     domain.ErrorCode = "watcher_candidate_put_failed"
	errorCandidateResult  domain.ErrorCode = "watcher_candidate_result_invalid"
	errorStateRead        domain.ErrorCode = "watcher_state_read_failed"
	errorStateCommit      domain.ErrorCode = "watcher_state_commit_failed"
	errorTickerClosed     domain.ErrorCode = "watcher_ticker_closed"
	errorObservationLimit domain.ErrorCode = "watcher_observation_limit"
	errorDiscoveryLimit   domain.ErrorCode = "watcher_discovery_limit"
)

var (
	ErrInvalidDependencies = errors.New("некорректные зависимости watcher")
	errPollingFallback     = errors.New("переход watcher на polling")
)

type WatchStore interface {
	Put(context.Context, domain.RescueCandidate) (store.PutResult, error)
	PutObserved(context.Context, domain.RescueCandidate) (store.PutResult, error)
	MarkRemoved(context.Context, domain.CandidateID) error
	LoadScanCursor(context.Context, domain.NetworkID) (store.Checkpoint, bool, error)
	CommitCanonicalBlock(context.Context, store.CanonicalBlock) error
	DiscoveredTokens(context.Context, domain.NetworkID) ([]common.Address, error)
	DiscoveryOverflowed(context.Context, domain.NetworkID) (bool, error)
}

type Dependencies struct {
	Contracts      rpc.ContractCaller
	Finalized      rpc.FinalizedReader
	LogSubscriber  rpc.LogSubscriber
	HeadSubscriber rpc.HeadSubscriber
	Codec          *contracts.ERC20Codec
	Clock          clock.Clock
	Observer       observability.Observer
	Store          WatchStore
	Source         common.Address
	Network        domain.Network
	Generation     uint64
	LookbackBlocks uint64
	ReadTimeout    time.Duration
}

type Service struct {
	contracts        rpc.ContractCaller
	finalized        rpc.FinalizedReader
	logSubscriber    rpc.LogSubscriber
	headSubscriber   rpc.HeadSubscriber
	codec            *contracts.ERC20Codec
	clock            clock.Clock
	observer         observability.Observer
	store            WatchStore
	source           common.Address
	networkID        domain.NetworkID
	networkName      string
	generation       uint64
	lookbackBlocks   uint64
	readTimeout      time.Duration
	knownTokens      map[common.Address]domain.Token
	allowedTokens    map[common.Address]struct{}
	allowUnknown     bool
	metadata         map[common.Address]domain.Token
	metadataOrder    []common.Address
	overflowReported bool
}

func NewService(dependencies Dependencies) (*Service, error) {
	if dependencies.Contracts == nil || dependencies.Finalized == nil || dependencies.LogSubscriber == nil ||
		dependencies.HeadSubscriber == nil || dependencies.Codec == nil || dependencies.Clock == nil ||
		dependencies.Observer == nil || dependencies.Store == nil || dependencies.Source == (common.Address{}) ||
		dependencies.Network.ChainID <= 0 || dependencies.LookbackBlocks == 0 ||
		dependencies.LookbackBlocks > maximumLookbackBlocks || dependencies.ReadTimeout <= 0 {
		return nil, ErrInvalidDependencies
	}

	knownTokens := make(map[common.Address]domain.Token, len(dependencies.Network.Tokens))
	allowedTokens := make(map[common.Address]struct{}, len(dependencies.Network.Tokens))
	for _, token := range dependencies.Network.Tokens {
		if token.Address == (common.Address{}) {
			return nil, ErrInvalidDependencies
		}
		allowedTokens[token.Address] = struct{}{}
		if token.Symbol != "" {
			knownTokens[token.Address] = token
		}
	}

	return &Service{
		contracts:      dependencies.Contracts,
		finalized:      dependencies.Finalized,
		logSubscriber:  dependencies.LogSubscriber,
		headSubscriber: dependencies.HeadSubscriber,
		codec:          dependencies.Codec,
		clock:          dependencies.Clock,
		observer:       dependencies.Observer,
		store:          dependencies.Store,
		source:         dependencies.Source,
		networkID:      dependencies.Network.ChainID,
		networkName:    dependencies.Network.Name,
		generation:     dependencies.Generation,
		lookbackBlocks: dependencies.LookbackBlocks,
		readTimeout:    dependencies.ReadTimeout,
		knownTokens:    knownTokens,
		allowedTokens:  allowedTokens,
		allowUnknown:   dependencies.Network.AllowUnknownTokens,
		metadata:       make(map[common.Address]domain.Token),
	}, nil
}

// PolicyFingerprint связывает state с фильтром и политикой первого backfill.
func PolicyFingerprint(network domain.Network, lookbackBlocks uint64) [sha256.Size]byte {
	hash := sha256.New()
	hash.Write([]byte("guard-daemon/watcher-policy/v1"))
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(network.ChainID))
	hash.Write(number[:])
	binary.BigEndian.PutUint64(number[:], lookbackBlocks)
	hash.Write(number[:])
	if network.AllowUnknownTokens {
		hash.Write([]byte{1})
	} else {
		hash.Write([]byte{0})
	}
	addresses := make([]common.Address, 0, len(network.Tokens))
	for _, token := range network.Tokens {
		addresses = append(addresses, token.Address)
	}
	sort.Slice(addresses, func(i, j int) bool { return bytes.Compare(addresses[i][:], addresses[j][:]) < 0 })
	for _, address := range addresses {
		hash.Write(address[:])
	}
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hash.Sum(nil))
	return fingerprint
}

func (service *Service) Run(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidDependencies
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := service.scan(ctx); err != nil {
		return err
	}

	query := service.transferQuery()
	if err := service.watchSubscriptions(ctx, query); !errors.Is(err, errPollingFallback) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return service.poll(ctx)
}

func (service *Service) transferQuery() ethereum.FilterQuery {
	addresses := make([]common.Address, 0, len(service.allowedTokens))
	if !service.allowUnknown {
		for address := range service.allowedTokens {
			addresses = append(addresses, address)
		}
		sort.Slice(addresses, func(i, j int) bool { return bytes.Compare(addresses[i][:], addresses[j][:]) < 0 })
		if len(addresses) == 0 {
			addresses = append(addresses, common.Address{})
		}
	}
	return ethereum.FilterQuery{
		Addresses: addresses,
		Topics: [][]common.Hash{
			{service.codec.TransferTopic()},
			nil,
			{common.BytesToHash(service.source.Bytes())},
		},
	}
}

func (service *Service) watchSubscriptions(ctx context.Context, query ethereum.FilterQuery) error {
	logs := make(chan types.Log, 100)
	setupContext, cancelSetup := context.WithTimeout(ctx, service.readTimeout)
	logSubscription, err := service.logSubscriber.SubscribeFilterLogs(setupContext, query, logs)
	cancelSetup()
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
	setupContext, cancelSetup = context.WithTimeout(ctx, service.readTimeout)
	headSubscription, err := service.headSubscriber.SubscribeNewHead(setupContext, heads)
	cancelSetup()
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

	// Закрывает окно между первым backfill и регистрацией subscription.
	if err := service.scan(ctx); err != nil {
		return err
	}

	scanTicker := service.clock.NewTicker(pollInterval)
	reconcileTicker := service.clock.NewTicker(reconciliationInterval)
	if scanTicker == nil || reconcileTicker == nil {
		if scanTicker != nil {
			scanTicker.Stop()
		}
		if reconcileTicker != nil {
			reconcileTicker.Stop()
		}
		return service.failure("watcher.ticker", domain.ErrorInternal, errorTickerClosed, false, nil)
	}
	defer scanTicker.Stop()
	defer reconcileTicker.Stop()

	logErrors := logSubscription.Err()
	var headErrors <-chan error
	if headSubscription != nil {
		headErrors = headSubscription.Err()
	}
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
		case _, ok := <-logErrors:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !ok {
				service.recordFailure(eventSubscriptionFallback, errorLogSubscription)
			}
			return service.failure("watcher.subscription", domain.ErrorRPCTransient, errorLogSubscription, true, nil)
		case logEntry, ok := <-logs:
			if !ok {
				return service.failure("watcher.subscription", domain.ErrorRPCTransient, errorLogSubscription, true, nil)
			}
			if err := service.observeLog(ctx, logEntry); err != nil {
				return err
			}
		case header, ok := <-heads:
			if !ok {
				service.recordFailure(eventHeadUnavailable, errorHeadSubscription)
				disableHeads()
				continue
			}
			if header == nil || header.Number == nil || !header.Number.IsUint64() {
				service.recordFailure(eventReadFailed, errorInvalidHead)
				continue
			}
			if err := service.scan(ctx); err != nil {
				return err
			}
		case <-headErrors:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			service.recordFailure(eventHeadUnavailable, errorHeadSubscription)
			disableHeads()
		case _, ok := <-scanTicker.C():
			if !ok {
				return service.failure("watcher.ticker", domain.ErrorInternal, errorTickerClosed, false, nil)
			}
			if err := service.scan(ctx); err != nil {
				return err
			}
		case _, ok := <-reconcileTicker.C():
			if !ok {
				return service.failure("watcher.ticker", domain.ErrorInternal, errorTickerClosed, false, nil)
			}
			if err := service.reconcile(ctx); err != nil {
				return err
			}
		}
	}
}

func (service *Service) poll(ctx context.Context) error {
	scanTicker := service.clock.NewTicker(pollInterval)
	reconcileTicker := service.clock.NewTicker(reconciliationInterval)
	if scanTicker == nil || reconcileTicker == nil {
		if scanTicker != nil {
			scanTicker.Stop()
		}
		if reconcileTicker != nil {
			reconcileTicker.Stop()
		}
		return service.failure("watcher.ticker", domain.ErrorInternal, errorTickerClosed, false, nil)
	}
	defer scanTicker.Stop()
	defer reconcileTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-scanTicker.C():
			if !ok {
				return service.failure("watcher.ticker", domain.ErrorInternal, errorTickerClosed, false, nil)
			}
			if err := service.scan(ctx); err != nil {
				return err
			}
		case _, ok := <-reconcileTicker.C():
			if !ok {
				return service.failure("watcher.ticker", domain.ErrorInternal, errorTickerClosed, false, nil)
			}
			if err := service.reconcile(ctx); err != nil {
				return err
			}
		}
	}
}

func (service *Service) scan(ctx context.Context) error {
	finalized, err := service.finalized.Finalized(ctx)
	if err != nil {
		return service.readFailure("watcher.finalized", errorFinalizedRead, err)
	}
	cursor, found, err := service.store.LoadScanCursor(ctx, service.networkID)
	if err != nil {
		return service.failure("watcher.state.load", domain.ErrorInternal, errorStateRead, true, err)
	}

	var next uint64
	if found {
		persisted, headerErr := service.finalized.Header(ctx, cursor.BlockNumber)
		if headerErr != nil {
			return service.readFailure("watcher.cursor.verify", errorFinalizedRead, headerErr)
		}
		if persisted.Hash != cursor.BlockHash || finalized.Number < cursor.BlockNumber {
			return service.failure("watcher.cursor.reorg", domain.ErrorRPCInvalidResponse, errorFinalizedReorg, false, nil)
		}
		if finalized.Number == cursor.BlockNumber {
			return nil
		}
		next = cursor.BlockNumber + 1
	} else if finalized.Number >= service.lookbackBlocks-1 {
		next = finalized.Number - service.lookbackBlocks + 1
	}

	previousHash := common.Hash{}
	if found {
		previousHash = cursor.BlockHash
	}
	for number := next; ; number++ {
		if number > finalized.Number {
			return nil
		}
		block, headerErr := service.finalized.Header(ctx, number)
		if headerErr != nil {
			return service.readFailure("watcher.block.header", errorFinalizedRead, headerErr)
		}
		if previousHash != (common.Hash{}) && block.ParentHash != previousHash {
			return service.failure("watcher.block.parent", domain.ErrorRPCInvalidResponse, errorFinalizedReorg, false, nil)
		}

		query := service.transferQuery()
		query.BlockHash = &block.Hash
		logs, logErr := service.finalized.FilterLogs(ctx, query)
		if logErr != nil {
			return service.readFailure("watcher.block.logs", errorLogRead, logErr)
		}
		candidates := make([]domain.RescueCandidate, 0, len(logs)+1)
		candidates = append(candidates, domain.NewBlockCandidate(service.networkID, domain.CandidateNative, service.source, block.Hash, number))
		seenLogs := make(map[domain.CandidateID]struct{}, len(logs))
		for _, logEntry := range logs {
			if logEntry.BlockNumber != number || logEntry.BlockHash != block.Hash || logEntry.Removed {
				return service.failure("watcher.block.log", domain.ErrorRPCInvalidResponse, errorInvalidLog, false, nil)
			}
			candidate, accepted := service.logCandidate(ctx, logEntry, true)
			if err := ctx.Err(); err != nil {
				return err
			}
			if !accepted {
				return service.failure("watcher.block.log", domain.ErrorRPCInvalidResponse, errorInvalidLog, false, nil)
			}
			if _, duplicate := seenLogs[candidate.ID]; duplicate {
				continue
			}
			seenLogs[candidate.ID] = struct{}{}
			candidates = append(candidates, candidate)
		}
		if err := service.store.CommitCanonicalBlock(ctx, store.CanonicalBlock{
			Checkpoint: store.Checkpoint{Network: service.networkID, BlockNumber: number, BlockHash: block.Hash},
			ParentHash: block.ParentHash,
			Candidates: candidates,
		}); err != nil {
			return service.failure("watcher.state.commit", domain.ErrorInternal, errorStateCommit, true, err)
		}
		if !service.overflowReported {
			overflowed, overflowErr := service.store.DiscoveryOverflowed(ctx, service.networkID)
			if overflowErr != nil {
				return service.failure("watcher.discovery.state", domain.ErrorInternal, errorStateRead, true, overflowErr)
			}
			if overflowed {
				service.recordFailure(eventDiscoveryOverflow, errorDiscoveryLimit)
				service.overflowReported = true
			}
		}
		for _, candidate := range candidates {
			service.recordCandidate(candidate, store.PutInserted)
		}
		previousHash = block.Hash
		if number == finalized.Number || number == ^uint64(0) {
			return nil
		}
	}
}

func (service *Service) observeLog(ctx context.Context, logEntry types.Log) error {
	candidate, accepted := service.logCandidate(ctx, logEntry, false)
	if !accepted {
		service.recordFailure(eventLogRejected, errorInvalidLog)
		return nil
	}
	result, err := service.store.PutObserved(ctx, candidate)
	if err != nil {
		if errors.Is(err, store.ErrObservationSaturated) {
			service.recordFailure(eventObservationSaturated, errorObservationLimit)
			return nil
		}
		return service.failure("watcher.observation.put", domain.ErrorInternal, errorCandidatePut, true, err)
	}
	service.recordCandidate(candidate, result)
	if !logEntry.Removed {
		return nil
	}
	if err := service.store.MarkRemoved(ctx, candidate.ID); err != nil {
		return service.failure("watcher.observation.remove", domain.ErrorInternal, errorStateCommit, true, err)
	}
	return nil
}

func (service *Service) logCandidate(ctx context.Context, logEntry types.Log, resolveMetadata bool) (domain.RescueCandidate, bool) {
	if len(logEntry.Topics) != 3 || len(logEntry.Data) != common.HashLength || logEntry.Topics[0] != service.codec.TransferTopic() ||
		logEntry.Topics[2] != common.BytesToHash(service.source.Bytes()) || logEntry.Address == (common.Address{}) ||
		logEntry.BlockHash == (common.Hash{}) || logEntry.TxHash == (common.Hash{}) {
		return domain.RescueCandidate{}, false
	}
	if _, allowed := service.allowedTokens[logEntry.Address]; !service.allowUnknown && !allowed {
		return domain.RescueCandidate{}, false
	}
	token := service.fallbackToken(logEntry.Address)
	if resolveMetadata {
		token = service.resolveToken(ctx, logEntry.Address)
	}
	return domain.NewLogCandidate(
		service.networkID,
		service.source,
		token,
		logEntry.BlockHash,
		logEntry.TxHash,
		logEntry.BlockNumber,
		logEntry.Index,
	), true
}

func (service *Service) reconcile(ctx context.Context) error {
	observation := service.observationSlot(reconciliationInterval)
	if err := service.putCandidate(ctx, domain.NewPeriodicCandidate(service.networkID, service.source, service.generation, observation)); err != nil {
		return err
	}
	discovered, err := service.store.DiscoveredTokens(ctx, service.networkID)
	if err != nil {
		return service.failure("watcher.reconcile.load", domain.ErrorInternal, errorStateRead, true, err)
	}
	if len(discovered) > maximumDiscoveredTokens {
		return service.failure("watcher.reconcile.limit", domain.ErrorInternal, errorStateRead, false, nil)
	}
	for _, address := range discovered {
		if _, configured := service.allowedTokens[address]; configured {
			continue
		}
		candidate := domain.NewTokenReconciliationCandidate(
			service.networkID,
			service.source,
			service.resolveToken(ctx, address),
			service.generation,
			observation,
		)
		if err := service.putCandidate(ctx, candidate); err != nil {
			return err
		}
	}
	return nil
}

func (service *Service) observationSlot(interval time.Duration) uint64 {
	observation := service.clock.Now().Unix()
	if observation < 0 {
		observation = 0
	}
	return uint64(observation / int64(interval/time.Second))
}

func (service *Service) putCandidate(ctx context.Context, candidate domain.RescueCandidate) error {
	result, err := service.store.Put(ctx, candidate)
	if err != nil {
		return service.failure("watcher.candidate.put", domain.ErrorInternal, errorCandidatePut, true, err)
	}
	return service.recordCandidate(candidate, result)
}

func (service *Service) recordCandidate(candidate domain.RescueCandidate, result store.PutResult) error {
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
		return service.failure("watcher.candidate.result", domain.ErrorInternal, errorCandidateResult, false, nil)
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

func (service *Service) fallbackToken(address common.Address) domain.Token {
	if token, ok := service.knownTokens[address]; ok {
		return token
	}
	return domain.Token{Address: address, Symbol: addressFallback(address), Decimals: 18}
}

func (service *Service) resolveToken(ctx context.Context, address common.Address) domain.Token {
	if token, ok := service.knownTokens[address]; ok {
		return token
	}
	if token, ok := service.metadata[address]; ok {
		return token
	}

	token := service.fallbackToken(address)
	fallback := false
	if data, err := service.codec.PackSymbol(); err != nil {
		fallback = true
	} else if result, err := service.metadataCall(ctx, address, data); err != nil {
		fallback = true
	} else if symbol, err := service.codec.DecodeSymbol(result); err != nil {
		fallback = true
	} else if symbol = sanitizeSymbol(symbol); symbol == "" {
		fallback = true
	} else {
		token.Symbol = symbol
	}

	if ctx.Err() == nil {
		if data, err := service.codec.PackDecimals(); err != nil {
			fallback = true
		} else if result, err := service.metadataCall(ctx, address, data); err != nil {
			fallback = true
		} else if decimals, err := service.codec.DecodeDecimals(result); err != nil {
			fallback = true
		} else {
			token.Decimals = decimals
		}
	}
	if fallback && ctx.Err() == nil {
		service.recordFailure(eventMetadataFallback, errorMetadata)
	}
	service.cacheMetadata(address, token)
	return token
}

func (service *Service) metadataCall(ctx context.Context, address common.Address, data []byte) ([]byte, error) {
	callContext, cancel := context.WithTimeout(ctx, service.readTimeout)
	defer cancel()
	result, err := service.contracts.CallContract(callContext, ethereum.CallMsg{To: &address, Data: data}, nil)
	if err != nil || result == nil || len(result) > metadataReturnLimit {
		return nil, errors.New("недопустимый ответ metadata")
	}
	return result, nil
}

func (service *Service) cacheMetadata(address common.Address, token domain.Token) {
	if _, exists := service.metadata[address]; exists {
		service.metadata[address] = token
		return
	}
	if len(service.metadataOrder) == metadataCacheLimit {
		delete(service.metadata, service.metadataOrder[0])
		service.metadataOrder = service.metadataOrder[1:]
	}
	service.metadata[address] = token
	service.metadataOrder = append(service.metadataOrder, address)
}

func (service *Service) readFailure(operation string, code domain.ErrorCode, cause error) error {
	if cause != nil && errors.Is(cause, context.Canceled) {
		return cause
	}
	service.recordFailure(eventReadFailed, code)
	return service.failure(operation, domain.ErrorRPCTransient, code, true, cause)
}

func (service *Service) failure(operation string, class domain.ErrorClass, code domain.ErrorCode, retryable bool, cause error) error {
	return domain.NewError(operation, class, code, retryable, false, cause)
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
