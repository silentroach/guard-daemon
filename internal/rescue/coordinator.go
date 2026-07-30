package rescue

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"time"

	"guard-daemon/internal/clock"
	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rpc"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	codeInvalidConfig        domain.ErrorCode = "rescue_invalid_config"
	codeCandidateMismatch    domain.ErrorCode = "rescue_candidate_mismatch"
	codeUnsupportedCandidate domain.ErrorCode = "rescue_candidate_unsupported"
	codeChainIDRead          domain.ErrorCode = "rescue_chain_id_read_failed"
	codeChainIDMismatch      domain.ErrorCode = "rescue_chain_id_mismatch"
	codeDestinationRead      domain.ErrorCode = "rescue_destination_read_failed"
	codeDestinationDecode    domain.ErrorCode = "rescue_destination_decode_failed"
	codeDestinationMismatch  domain.ErrorCode = "rescue_destination_mismatch"
	codeCodeRead             domain.ErrorCode = "rescue_code_read_failed"
	codeBalanceRead          domain.ErrorCode = "rescue_balance_read_failed"
	codeBalanceDecode        domain.ErrorCode = "rescue_balance_decode_failed"
	codeNonceRead            domain.ErrorCode = "rescue_nonce_read_failed"
	codeSigning              domain.ErrorCode = "rescue_signing_failed"
	codeSignerMismatch       domain.ErrorCode = "rescue_signer_mismatch"
	codeBroadcast            domain.ErrorCode = "rescue_broadcast_failed"
	codeReceiptTimeout       domain.ErrorCode = "rescue_receipt_timeout"
	codeReceiptInvalid       domain.ErrorCode = "rescue_receipt_invalid"
	codeReverted             domain.ErrorCode = "rescue_transaction_reverted"
	codePostcondition        domain.ErrorCode = "rescue_postcondition_failed"
	codeContextCanceled      domain.ErrorCode = "rescue_context_canceled"
	codeEncoding             domain.ErrorCode = "rescue_encoding_failed"
)

const (
	eventStartupVerified    observability.EventCode = "rescue_startup_verified"
	eventDelegationActive   observability.EventCode = "rescue_delegation_active"
	eventOperationSkipped   observability.EventCode = "rescue_operation_skipped"
	eventBroadcastAccepted  observability.EventCode = "rescue_broadcast_accepted"
	eventOperationConfirmed observability.EventCode = "rescue_operation_confirmed"
	eventOperationFailed    observability.EventCode = "rescue_operation_failed"
)

const (
	defaultNativeThresholdWei = int64(100_000_000_000_000)
	defaultStartupTimeout     = 10 * time.Second
	defaultFeeReadTimeout     = 5 * time.Second
)

// RPCReader is the read-only RPC surface needed by one rescue generation.
type RPCReader interface {
	ChainID(context.Context) (*big.Int, error)
	BalanceAt(context.Context, common.Address, *big.Int) (*big.Int, error)
	CodeAt(context.Context, common.Address, *big.Int) ([]byte, error)
	PendingNonceAt(context.Context, common.Address) (uint64, error)
	HeaderByNumber(context.Context, *big.Int) (*types.Header, error)
	SuggestGasPrice(context.Context) (*big.Int, error)
	CallContract(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error)
	TransactionReceipt(context.Context, common.Hash) (*types.Receipt, error)
}

// Config contains immutable values used by a network coordinator.
type Config struct {
	Network         domain.Network
	Source          common.Address
	Sponsor         common.Address
	Destination     common.Address
	NativeThreshold *big.Int
	StartupTimeout  time.Duration
	FeeReadTimeout  time.Duration
}

type retryEntry struct {
	token    domain.Token
	attempts int
}

// Coordinator owns mutable transaction exclusion and token retry state.
type Coordinator struct {
	network         domain.Network
	source          common.Address
	sponsor         common.Address
	destination     common.Address
	rescuer         common.Address
	hasRescuer      bool
	nativeThreshold *big.Int
	startupTimeout  time.Duration
	feeReadTimeout  time.Duration
	authorizer      AuthorizationSigner
	transactioner   TransactionSigner
	clock           clock.Clock
	observer        observability.Observer
	erc20           *contracts.ERC20Codec
	rescuerCodec    *contracts.RescuerCodec

	mu      sync.Mutex
	busy    bool
	retries map[common.Address]retryEntry
}

// Session binds a coordinator to one immutable RPC client generation.
type Session struct {
	coordinator *Coordinator
	generation  uint64
	reader      RPCReader
	broadcaster rpc.Broadcaster
}

func NewCoordinator(config Config, authorizer AuthorizationSigner, transactioner TransactionSigner, serviceClock clock.Clock, observer observability.Observer) (*Coordinator, error) {
	if config.Network.ChainID <= 0 || config.Source == (common.Address{}) || config.Sponsor == (common.Address{}) || config.Destination == (common.Address{}) || authorizer == nil || transactioner == nil || serviceClock == nil || observer == nil {
		return nil, newError("rescue.coordinator", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}
	if config.Network.HasRescuer && config.Network.Rescuer == (common.Address{}) {
		return nil, newError("rescue.coordinator", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}

	erc20, err := contracts.NewERC20Codec()
	if err != nil {
		return nil, newError("rescue.erc20_codec", domain.ErrorInternal, codeEncoding, false, false, err)
	}
	rescuerCodec, err := contracts.NewRescuerCodec()
	if err != nil {
		return nil, newError("rescue.rescuer_codec", domain.ErrorInternal, codeEncoding, false, false, err)
	}

	network := config.Network
	network.Tokens = append([]domain.Token(nil), config.Network.Tokens...)
	threshold := big.NewInt(defaultNativeThresholdWei)
	if config.NativeThreshold != nil {
		if config.NativeThreshold.Sign() <= 0 {
			return nil, newError("rescue.native_threshold", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
		}
		threshold.Set(config.NativeThreshold)
	}
	startupTimeout := config.StartupTimeout
	if startupTimeout == 0 {
		startupTimeout = defaultStartupTimeout
	}
	feeReadTimeout := config.FeeReadTimeout
	if feeReadTimeout == 0 {
		feeReadTimeout = defaultFeeReadTimeout
	}
	if startupTimeout < 0 || feeReadTimeout < 0 {
		return nil, newError("rescue.timeouts", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}

	return &Coordinator{
		network:         network,
		source:          config.Source,
		sponsor:         config.Sponsor,
		destination:     config.Destination,
		rescuer:         network.Rescuer,
		hasRescuer:      network.HasRescuer,
		nativeThreshold: threshold,
		startupTimeout:  startupTimeout,
		feeReadTimeout:  feeReadTimeout,
		authorizer:      authorizer,
		transactioner:   transactioner,
		clock:           serviceClock,
		observer:        observer,
		erc20:           erc20,
		rescuerCodec:    rescuerCodec,
		retries:         make(map[common.Address]retryEntry),
	}, nil
}

// NewSession verifies chain and configured destination state before exposing a
// generation for candidate handling. Delegation renewal remains non-fatal, as
// in the legacy network loop, but any failure is emitted only as a safe code.
func (coordinator *Coordinator) NewSession(ctx context.Context, generation uint64, reader RPCReader, broadcaster rpc.Broadcaster) (*Session, error) {
	if reader == nil || broadcaster == nil {
		return nil, newError("rescue.session", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}
	session := &Session{
		coordinator: coordinator,
		generation:  generation,
		reader:      reader,
		broadcaster: broadcaster,
	}
	if err := session.verifyStartup(ctx); err != nil {
		coordinator.recordFailure(err)
		return nil, err
	}
	coordinator.record(eventStartupVerified, observability.LevelInfo, common.Hash{}, "")

	if coordinator.hasRescuer {
		if err := session.reconcileDelegation(ctx); err != nil {
			coordinator.recordFailure(err)
			if ctx.Err() != nil {
				return nil, err
			}
		}
	}
	return session, nil
}

func (session *Session) Generation() uint64 {
	return session.generation
}

// Handle consumes one watcher candidate without taking ownership of watcher or
// RPC connection lifecycle.
func (session *Session) Handle(ctx context.Context, candidate domain.RescueCandidate) error {
	coordinator := session.coordinator
	if candidate.Network != coordinator.network.ChainID || candidate.Source != coordinator.source || (candidate.Generation != 0 && candidate.Generation != session.generation) {
		err := newError("rescue.handle", domain.ErrorConfiguration, codeCandidateMismatch, false, false, nil)
		coordinator.recordFailure(err)
		return err
	}

	var err error
	switch candidate.Kind {
	case domain.CandidateToken:
		err = session.handleToken(ctx, candidate.Token)
	case domain.CandidateNative:
		err = session.reconcileNative(ctx)
	case domain.CandidatePeriodic:
		return session.reconcilePeriodic(ctx)
	default:
		err = newError("rescue.handle", domain.ErrorConfiguration, codeUnsupportedCandidate, false, false, nil)
	}
	if err != nil {
		coordinator.recordFailure(err)
	}
	return err
}

func (session *Session) verifyStartup(ctx context.Context) error {
	coordinator := session.coordinator
	readContext, cancel := context.WithTimeout(ctx, coordinator.startupTimeout)
	defer cancel()

	chainID, err := session.reader.ChainID(readContext)
	if err != nil {
		return contextOrError(readContext, "rescue.chain_id", domain.ErrorRPCTransient, codeChainIDRead, true, false, err)
	}
	if chainID == nil || chainID.Cmp(big.NewInt(int64(coordinator.network.ChainID))) != 0 {
		return newError("rescue.chain_id", domain.ErrorConfiguration, codeChainIDMismatch, false, false, nil)
	}
	if !coordinator.hasRescuer {
		return nil
	}

	data, err := coordinator.rescuerCodec.PackDestination()
	if err != nil {
		return newError("rescue.destination", domain.ErrorInternal, codeEncoding, false, false, err)
	}
	result, err := session.reader.CallContract(readContext, ethereum.CallMsg{To: &coordinator.rescuer, Data: data}, nil)
	if err != nil {
		return contextOrError(readContext, "rescue.destination", domain.ErrorRPCTransient, codeDestinationRead, true, false, err)
	}
	destination, err := coordinator.rescuerCodec.DecodeDestination(result)
	if err != nil {
		return newError("rescue.destination", domain.ErrorRPCInvalidResponse, codeDestinationDecode, true, true, err)
	}
	if destination != coordinator.destination {
		return newError("rescue.destination", domain.ErrorConfiguration, codeDestinationMismatch, false, false, nil)
	}
	return nil
}

func (coordinator *Coordinator) beginOperation() bool {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.busy {
		return false
	}
	coordinator.busy = true
	return true
}

func (coordinator *Coordinator) endOperation() {
	coordinator.mu.Lock()
	coordinator.busy = false
	coordinator.mu.Unlock()
}

func (coordinator *Coordinator) retryExhausted(address common.Address) bool {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return coordinator.retries[address].attempts >= maxRetryAttempts
}

func (coordinator *Coordinator) registerFailure(token domain.Token) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	entry := coordinator.retries[token.Address]
	if entry.attempts >= maxRetryAttempts {
		return
	}
	entry.token = token
	entry.attempts++
	coordinator.retries[token.Address] = entry
}

func (coordinator *Coordinator) clearRetry(address common.Address) {
	coordinator.mu.Lock()
	delete(coordinator.retries, address)
	coordinator.mu.Unlock()
}

func (coordinator *Coordinator) retrySnapshot() []domain.Token {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	tokens := make([]domain.Token, 0, len(coordinator.retries))
	for _, entry := range coordinator.retries {
		tokens = append(tokens, entry.token)
	}
	return tokens
}

func (coordinator *Coordinator) record(code observability.EventCode, level observability.Level, hash common.Hash, errorCode domain.ErrorCode) {
	coordinator.observer.Record(observability.Event{
		Level:       level,
		Code:        code,
		NetworkName: coordinator.network.Name,
		TxHash:      hash,
		ErrorCode:   errorCode,
	})
}

func (coordinator *Coordinator) recordFailure(err error) {
	var classified *domain.ClassifiedError
	code := domain.ErrorCode("rescue_internal_failure")
	if errors.As(err, &classified) {
		code = classified.Code
	}
	coordinator.record(eventOperationFailed, observability.LevelError, common.Hash{}, code)
}

func newError(operation string, class domain.ErrorClass, code domain.ErrorCode, retryable, ambiguous bool, cause error) *domain.ClassifiedError {
	return domain.NewError(operation, class, code, retryable, ambiguous, cause)
}

func contextOrError(ctx context.Context, operation string, class domain.ErrorClass, code domain.ErrorCode, retryable, ambiguous bool, cause error) *domain.ClassifiedError {
	if ctx.Err() != nil {
		return newError(operation, domain.ErrorRPCTransient, codeContextCanceled, true, true, ctx.Err())
	}
	return newError(operation, class, code, retryable, ambiguous, cause)
}
