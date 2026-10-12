package rescue

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"time"

	"guard-daemon/internal/budget"
	"guard-daemon/internal/clock"
	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
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
	codeFinalityRead         domain.ErrorCode = "rescue_finality_read_failed"
	codeCodeRead             domain.ErrorCode = "rescue_code_read_failed"
	codeBalanceRead          domain.ErrorCode = "rescue_balance_read_failed"
	codeBalanceDecode        domain.ErrorCode = "rescue_balance_decode_failed"
	codeNonceRead            domain.ErrorCode = "rescue_nonce_read_failed"
	codeFeeRead              domain.ErrorCode = "rescue_fee_read_failed"
	codeFeeInvalid           domain.ErrorCode = "rescue_fee_invalid"
	codeSigning              domain.ErrorCode = "rescue_signing_failed"
	codeSignerMismatch       domain.ErrorCode = "rescue_signer_mismatch"
	codeBroadcast            domain.ErrorCode = "rescue_broadcast_failed"
	codeReceiptTimeout       domain.ErrorCode = "rescue_receipt_timeout"
	codeReceiptInvalid       domain.ErrorCode = "rescue_receipt_invalid"
	codeSignedPayloadInvalid domain.ErrorCode = "rescue_signed_payload_invalid"
	codeReverted             domain.ErrorCode = "rescue_transaction_reverted"
	codePostcondition        domain.ErrorCode = "rescue_postcondition_failed"
	codeLostRace             domain.ErrorCode = "rescue_authorization_lost_race"
	codeContextCanceled      domain.ErrorCode = "rescue_context_canceled"
	codeEncoding             domain.ErrorCode = "rescue_encoding_failed"
	codeStateRead            domain.ErrorCode = "rescue_state_read_failed"
	codeStateWrite           domain.ErrorCode = "rescue_state_write_failed"
	codeRetryPending         domain.ErrorCode = "rescue_retry_pending"
	codeNoPaidAction         domain.ErrorCode = "rescue_no_paid_action"
	codeUnknownToken         domain.ErrorCode = "rescue_unknown_token_forbidden"
	codeAdmissionLimited     domain.ErrorCode = "rescue_admission_limited"
	codeBudgetExceeded       domain.ErrorCode = "rescue_budget_exceeded"
	codeSponsorReserve       domain.ErrorCode = "rescue_sponsor_reserve"
	codeSimulation           domain.ErrorCode = "rescue_simulation_failed"
	codeMinimumValue         domain.ErrorCode = "rescue_minimum_value"
	codePaidActionsStopped   domain.ErrorCode = "rescue_paid_actions_stopped"

	// CodeLeaseLost можно безопасно раскрывать через API состояния службы и интерфейс оператора.
	CodeLeaseLost domain.ErrorCode = "rescue_lease_lost"
	// CodeLeaseReleaseFailed можно безопасно раскрывать в диагностике завершения работы.
	CodeLeaseReleaseFailed domain.ErrorCode = "rescue_lease_release_failed"
)

const (
	eventStartupVerified    observability.EventCode = "rescue_startup_verified"
	eventOperationPrepared  observability.EventCode = "rescue_operation_prepared"
	eventBroadcastAccepted  observability.EventCode = "rescue_broadcast_accepted"
	eventOperationConfirmed observability.EventCode = "rescue_operation_confirmed"
	eventOperationFailed    observability.EventCode = "rescue_operation_failed"
)

const (
	defaultNativeThresholdWei = int64(100_000_000_000_000)
	defaultStartupTimeout     = 10 * time.Second
	defaultFeeReadTimeout     = 5 * time.Second
	defaultLeaseTTL           = 30 * time.Second
	defaultRetryDelay         = 5 * time.Second
	defaultReceiptTimeout     = 60 * time.Second
	defaultFinalityTimeout    = 30 * time.Minute
	defaultMaxAttempts        = uint32(3)
)

const (
	minReconciliationInterval = 30 * time.Second
	maxReconciliationInterval = 5 * time.Minute
)

// ErrLeaseLost служит маркером потери исключительного права на подписание и не
// раскрывает подробностей. Ошибка не зависит от того, обнаружила потерю аренда
// в хранилище или локальная блокировка процесса.
var ErrLeaseLost = errors.New("rescue coordinator: lease lost")

// ErrLeaseReleaseFailed служит маркером ошибки освобождения аренды, не раскрывает
// подробностей и безопасна для журналов завершения работы.
var ErrLeaseReleaseFailed = errors.New("rescue coordinator: failed to release lease")

// IsLeaseLost сообщает, остановлено ли подписание из-за потери исключительного
// права. Подробности реализации хранилища намеренно скрыты.
func IsLeaseLost(err error) bool {
	return errors.Is(err, ErrLeaseLost)
}

// RPCReader предоставляет основные операции RPC без кворума: определение сети,
// чтение неподтверждённых nonce и исходных данных комиссий. Полученные через него
// данные не служат доказательством результата.
type RPCReader interface {
	ChainID(context.Context) (*big.Int, error)
	PendingNonceAt(context.Context, common.Address) (uint64, error)
	HeaderByNumber(context.Context, *big.Int) (*types.Header, error)
	SuggestGasPrice(context.Context) (*big.Int, error)
	EstimateGas(context.Context, ethereum.CallMsg) (uint64, error)
}

// FinalityReader предоставляет только привязанные к хешу данные, подтверждённые
// кворумом. Receipt должен возвращать финализированную квитанцию из блока, который
// остаётся каноническим.
type FinalityReader interface {
	Finalized(context.Context) (rpc.BlockRef, error)
	Header(context.Context, uint64) (rpc.BlockRef, error)
	BalanceAt(context.Context, rpc.BlockRef, common.Address) (*big.Int, error)
	CodeAt(context.Context, rpc.BlockRef, common.Address) ([]byte, error)
	CallContract(context.Context, rpc.BlockRef, ethereum.CallMsg) ([]byte, error)
	Receipt(context.Context, common.Hash) (*types.Receipt, error)
}

// Config содержит неизменяемые значения одного координатора сети. Lease и
// ProcessFence приобретаются службой до создания координатора.
type Config struct {
	Network            domain.Network
	Source             common.Address
	Sponsor            common.Address
	Destination        common.Address
	NativeThreshold    *big.Int
	StartupTimeout     time.Duration
	FeeReadTimeout     time.Duration
	State              store.RescueStateStore
	LeaseManager       store.LeaseManager
	Lease              store.Lease
	ProcessFence       store.ProcessFence
	LeaseTTL           time.Duration
	MaxAttempts        uint32
	RetryDelay         time.Duration
	ReceiptTimeout     time.Duration
	FinalityTimeout    time.Duration
	Budget             budget.Ledger
	Gate               *observability.PaidActionGate
	Admission          *AdmissionController
	FeePolicy          FeePolicy
	NativeMinimum      uint256.Int
	TrustedTokens      []common.Address
	TrustedTokenValues map[common.Address]TrustedTokenValuePolicy
	Metrics            *observability.Metrics
	Alerts             *observability.AlertManager
	Health             *observability.Health
}

type TrustedTokenValuePolicy struct {
	MinimumBalance uint256.Int
	MaximumCost    uint256.Int
}

// Coordinator последовательно выделяет nonce, подписывает и отправляет транзакции
// для заданных сети и спонсора.
type Coordinator struct {
	network                domain.Network
	source                 common.Address
	sponsor                common.Address
	destination            common.Address
	rescuer                common.Address
	nativeThreshold        *big.Int
	startupTimeout         time.Duration
	feeReadTimeout         time.Duration
	leaseTTL               time.Duration
	maxAttempts            uint32
	retryDelay             time.Duration
	receiptTimeout         time.Duration
	finalityTimeout        time.Duration
	state                  store.RescueStateStore
	leaseManager           store.LeaseManager
	processFence           store.ProcessFence
	authorizer             AuthorizationSigner
	transactioner          TransactionSigner
	clock                  clock.Clock
	observer               observability.Observer
	erc20                  *contracts.ERC20Codec
	rescuerCodec           *contracts.RescuerCodec
	trustedTokens          map[common.Address]struct{}
	trustedTokenValues     map[common.Address]TrustedTokenValuePolicy
	allowedUntrustedTokens map[common.Address]struct{}
	budget                 budget.Ledger
	gate                   *observability.PaidActionGate
	admission              *AdmissionController
	feePolicy              FeePolicy
	nativeMinimum          uint256.Int
	metrics                *observability.Metrics
	alerts                 *observability.AlertManager
	health                 *observability.Health
	structured             observability.StructuredObserver

	operationMu  sync.Mutex
	nonceFloor   uint64
	nonceBlocked bool

	leaseMu       sync.RWMutex
	leaseActionMu sync.Mutex
	lease         store.Lease
	leaseLost     chan struct{}
	leaseLostOnce sync.Once
}

// Session связывает координатор с одним поколением основного RPC и одним поставщиком
// финализированных данных, подтверждённых кворумом.
type Session struct {
	coordinator   *Coordinator
	generation    uint64
	reader        RPCReader
	finality      FinalityReader
	broadcaster   rpc.Broadcaster
	lastFinalized rpc.BlockRef
	rpcSucceeded  bool
}

func NewCoordinator(config Config, authorizer AuthorizationSigner, transactioner TransactionSigner, serviceClock clock.Clock, observer observability.Observer) (*Coordinator, error) {
	zero := common.Address{}
	if config.Network.ChainID <= 0 || !config.Network.HasRescuer || config.Network.Rescuer == zero ||
		config.Source == zero || config.Sponsor == zero || config.Destination == zero ||
		config.Source == config.Sponsor || config.Source == config.Destination || config.Sponsor == config.Destination ||
		config.State == nil || config.LeaseManager == nil || config.ProcessFence == nil || config.Budget == nil || config.Gate == nil || config.Admission == nil ||
		!validFeePolicy(config.FeePolicy) || config.FeePolicy.Network != config.Network.ChainID || authorizer == nil || transactioner == nil || serviceClock == nil || observer == nil ||
		config.Lease.Key.Network != config.Network.ChainID || config.Lease.Key.Sponsor != config.Sponsor || config.Lease.Owner == "" {
		return nil, newError("rescue.coordinator", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}
	if err := config.ProcessFence.Validate(); err != nil {
		return nil, processFenceError("rescue.process_fence")
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
	configuredTokens := make(map[common.Address]struct{}, len(network.Tokens))
	for _, token := range network.Tokens {
		if token.Address == zero {
			return nil, newError("rescue.tokens", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
		}
		configuredTokens[token.Address] = struct{}{}
	}
	trustedTokens := make(map[common.Address]struct{}, len(config.TrustedTokens))
	for _, address := range config.TrustedTokens {
		if address == zero {
			return nil, newError("rescue.trusted_tokens", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
		}
		if _, configured := configuredTokens[address]; !configured {
			return nil, newError("rescue.trusted_tokens", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
		}
		trustedTokens[address] = struct{}{}
	}
	trustedTokenValues := make(map[common.Address]TrustedTokenValuePolicy, len(config.TrustedTokenValues))
	for address, policy := range config.TrustedTokenValues {
		if _, trusted := trustedTokens[address]; !trusted || policy.MinimumBalance.IsZero() || policy.MaximumCost.IsZero() {
			return nil, newError("rescue.token_values", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
		}
		trustedTokenValues[address] = policy
	}
	if !config.Gate.Stopped() && len(trustedTokenValues) != len(trustedTokens) {
		return nil, newError("rescue.token_values", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}
	if !config.Gate.Stopped() && config.FeePolicy.UnboundedAdditionalFees {
		return nil, newError("rescue.fee_model", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}
	allowedUntrustedTokens := make(map[common.Address]struct{})
	for address := range configuredTokens {
		if _, trusted := trustedTokens[address]; !trusted {
			allowedUntrustedTokens[address] = struct{}{}
		}
	}

	threshold := big.NewInt(defaultNativeThresholdWei)
	if config.NativeThreshold != nil {
		if config.NativeThreshold.Sign() < 0 {
			return nil, newError("rescue.native_threshold", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
		}
		threshold.Set(config.NativeThreshold)
	}
	startupTimeout := durationDefault(config.StartupTimeout, defaultStartupTimeout)
	feeReadTimeout := durationDefault(config.FeeReadTimeout, defaultFeeReadTimeout)
	leaseTTL := durationDefault(config.LeaseTTL, defaultLeaseTTL)
	retryDelay := durationDefault(config.RetryDelay, defaultRetryDelay)
	receiptTimeout := durationDefault(config.ReceiptTimeout, defaultReceiptTimeout)
	finalityTimeout := durationDefault(config.FinalityTimeout, defaultFinalityTimeout)
	maxAttempts := config.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = defaultMaxAttempts
	}
	if startupTimeout <= 0 || feeReadTimeout <= 0 || leaseTTL < 3*time.Nanosecond || retryDelay <= 0 || receiptTimeout <= 0 || finalityTimeout < defaultFinalityTimeout {
		return nil, newError("rescue.timeouts", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}

	coordinator := &Coordinator{
		network:                network,
		source:                 config.Source,
		sponsor:                config.Sponsor,
		destination:            config.Destination,
		rescuer:                network.Rescuer,
		nativeThreshold:        threshold,
		startupTimeout:         startupTimeout,
		feeReadTimeout:         feeReadTimeout,
		leaseTTL:               leaseTTL,
		maxAttempts:            maxAttempts,
		retryDelay:             retryDelay,
		receiptTimeout:         receiptTimeout,
		finalityTimeout:        finalityTimeout,
		state:                  config.State,
		leaseManager:           config.LeaseManager,
		processFence:           config.ProcessFence,
		lease:                  config.Lease,
		authorizer:             authorizer,
		transactioner:          transactioner,
		clock:                  serviceClock,
		observer:               observer,
		erc20:                  erc20,
		rescuerCodec:           rescuerCodec,
		trustedTokens:          trustedTokens,
		trustedTokenValues:     trustedTokenValues,
		allowedUntrustedTokens: allowedUntrustedTokens,
		budget:                 config.Budget,
		gate:                   config.Gate,
		admission:              config.Admission,
		feePolicy:              config.FeePolicy,
		nativeMinimum:          config.NativeMinimum,
		metrics:                config.Metrics,
		alerts:                 config.Alerts,
		health:                 config.Health,
		leaseLost:              make(chan struct{}),
	}
	coordinator.structured, _ = observer.(observability.StructuredObserver)
	return coordinator, nil
}

func durationDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

// MaintainLease продлевает аренду процесса каждые TTL/3. Любая ошибка продления
// или сигнал о потере аренды навсегда запрещает координатору дальнейшее подписание.
func (coordinator *Coordinator) MaintainLease(ctx context.Context) error {
	if ctx == nil {
		return newError("rescue.lease", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}
	if err := coordinator.validateLease(ctx); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return contextOrError(ctx, "rescue.lease", domain.ErrorInternal, codeContextCanceled, false, false, ctx.Err())
	}

	ticker := coordinator.clock.NewTicker(coordinator.leaseTTL / 3)
	if ticker == nil {
		coordinator.markLeaseLost()
		return leaseError("rescue.lease_ticker")
	}
	defer ticker.Stop()

	for {
		lease := coordinator.currentLease()
		lost := coordinator.leaseManager.Lost(lease)
		select {
		case <-ctx.Done():
			return contextOrError(ctx, "rescue.lease", domain.ErrorInternal, codeContextCanceled, false, false, ctx.Err())
		case <-coordinator.leaseLost:
			if ctx.Err() != nil {
				return contextOrError(ctx, "rescue.lease", domain.ErrorInternal, codeContextCanceled, false, false, ctx.Err())
			}
			return leaseError("rescue.lease")
		case <-lost:
			if ctx.Err() != nil {
				return contextOrError(ctx, "rescue.lease", domain.ErrorInternal, codeContextCanceled, false, false, ctx.Err())
			}
			coordinator.markLeaseLost()
			return leaseError("rescue.lease")
		case <-ticker.C():
			coordinator.leaseActionMu.Lock()
			select {
			case <-coordinator.leaseLost:
				coordinator.leaseActionMu.Unlock()
				continue
			default:
			}
			lease = coordinator.currentLease()
			renewed, err := coordinator.leaseManager.Renew(ctx, lease, coordinator.leaseTTL)
			if err != nil {
				if ctx.Err() != nil {
					coordinator.leaseActionMu.Unlock()
					return contextOrError(ctx, "rescue.lease_renew", domain.ErrorInternal, codeContextCanceled, false, false, ctx.Err())
				}
				coordinator.markLeaseLost()
				coordinator.leaseActionMu.Unlock()
				return leaseError("rescue.lease_renew")
			}
			coordinator.leaseMu.Lock()
			coordinator.lease = renewed
			coordinator.leaseMu.Unlock()
			coordinator.leaseActionMu.Unlock()
		}
	}
}

// ReleaseLease навсегда останавливает подписание и освобождает последнюю продлённую
// аренду. Подробности ошибки освобождения скрываются, а безопасная классификация сохраняется.
func (coordinator *Coordinator) ReleaseLease(ctx context.Context) error {
	if ctx == nil {
		return newError("rescue.lease_release", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}
	coordinator.leaseActionMu.Lock()
	defer coordinator.leaseActionMu.Unlock()

	coordinator.markLeaseLost()
	if err := coordinator.leaseManager.Release(ctx, coordinator.currentLease()); err != nil {
		if ctx.Err() != nil {
			return contextOrError(ctx, "rescue.lease_release", domain.ErrorInternal, codeContextCanceled, false, true, ctx.Err())
		}
		return newError("rescue.lease_release", domain.ErrorInternal, CodeLeaseReleaseFailed, true, true, ErrLeaseReleaseFailed)
	}
	return nil
}

// NewSession проверяет идентификатор сети и настроенный адрес назначения по данным
// финализированного состояния, подтверждённым кворумом, а затем проверяет состояние
// сохранённых подписанных транзакций перед приёмом новых кандидатов.
func (coordinator *Coordinator) NewSession(ctx context.Context, generation uint64, reader RPCReader, finality FinalityReader, broadcaster rpc.Broadcaster) (*Session, error) {
	if ctx == nil || reader == nil || finality == nil || broadcaster == nil {
		return nil, newError("rescue.session", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}
	if err := coordinator.validateProcessFence(); err != nil {
		coordinator.recordFailure(domain.CandidateID{}, common.Hash{}, err)
		return nil, err
	}
	if err := coordinator.validateLease(ctx); err != nil {
		coordinator.recordFailure(domain.CandidateID{}, common.Hash{}, err)
		return nil, err
	}

	session := &Session{coordinator: coordinator, generation: generation, reader: reader, finality: finality, broadcaster: broadcaster}
	if err := session.verifyStartup(ctx); err != nil {
		coordinator.recordFailure(domain.CandidateID{}, common.Hash{}, err)
		return nil, err
	}

	coordinator.operationMu.Lock()
	err := session.recoverPersisted(ctx)
	coordinator.operationMu.Unlock()
	if err != nil {
		coordinator.recordFailure(domain.CandidateID{}, common.Hash{}, err)
		return nil, err
	}
	coordinator.record(eventStartupVerified, observability.LevelInfo, domain.CandidateID{}, common.Hash{}, "")
	return session, nil
}

func (session *Session) Generation() uint64 {
	return session.generation
}

// Handle дожидается доступа к координатору. Корректный кандидат не отбрасывается
// из-за того, что другому кандидату в этот момент выделяется nonce спонсора.
func (session *Session) Handle(ctx context.Context, candidate domain.RescueCandidate) error {
	coordinator := session.coordinator
	if ctx == nil || domain.ValidateCandidate(candidate) != nil || candidate.Network != coordinator.network.ChainID || candidate.Source != coordinator.source ||
		(candidate.Generation != 0 && candidate.Generation != session.generation) {
		err := newError("rescue.handle", domain.ErrorConfiguration, codeCandidateMismatch, false, false, nil)
		coordinator.recordFailure(candidate.ID, common.Hash{}, err)
		return err
	}

	coordinator.operationMu.Lock()
	defer coordinator.operationMu.Unlock()
	session.rpcSucceeded = false
	if coordinator.gate.Stopped() {
		err := newError("rescue.emergency_stop", domain.ErrorSigning, codePaidActionsStopped, true, false, observability.ErrPaidActionsStopped)
		coordinator.recordFailure(candidate.ID, common.Hash{}, err)
		return err
	}

	var err error
	switch candidate.Kind {
	case domain.CandidateToken:
		err = session.handleAsset(ctx, candidate, domain.CandidateToken, candidate.Token.Address, domain.IncidentID{})
	case domain.CandidateNative:
		err = session.handleAsset(ctx, candidate, domain.CandidateNative, common.Address{}, domain.IncidentID{})
	case domain.CandidatePeriodic:
		err = session.handlePeriodic(ctx, candidate)
	default:
		err = newError("rescue.handle", domain.ErrorConfiguration, codeUnsupportedCandidate, false, false, nil)
	}
	if err != nil {
		if candidate.Kind != domain.CandidatePeriodic {
			coordinator.recordFailure(candidate.ID, common.Hash{}, err)
			coordinator.recordCandidateFailure(ctx, candidate.ID, err)
		}
	} else if session.rpcSucceeded {
		coordinator.recordRPCHealthy()
	}
	return err
}

// RunReconciliation периодически ищет поздние финализированные квитанции для операций
// с неопределённым результатом и истёкшим сроком ожидания. Такие операции не подписываются
// и не отправляются повторно.
func (session *Session) RunReconciliation(ctx context.Context) error {
	if ctx == nil {
		return newError("rescue.reconciliation", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}
	ticker := session.coordinator.clock.NewTicker(reconciliationInterval(session.coordinator.retryDelay))
	if ticker == nil {
		return newError("rescue.reconciliation", domain.ErrorInternal, codeInvalidConfig, false, false, nil)
	}
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return contextOrError(ctx, "rescue.reconciliation", domain.ErrorInternal, codeContextCanceled, false, false, ctx.Err())
		case <-ticker.C():
			if err := session.reconcileExpired(ctx); err != nil {
				return err
			}
		}
	}
}

func reconciliationInterval(retryDelay time.Duration) time.Duration {
	if retryDelay < minReconciliationInterval {
		return minReconciliationInterval
	}
	if retryDelay > maxReconciliationInterval {
		return maxReconciliationInterval
	}
	return retryDelay
}

func (session *Session) reconcileExpired(ctx context.Context) error {
	coordinator := session.coordinator
	coordinator.operationMu.Lock()
	defer coordinator.operationMu.Unlock()

	if err := coordinator.validateProcessFence(); err != nil {
		coordinator.recordFailure(domain.CandidateID{}, common.Hash{}, err)
		return err
	}
	if err := coordinator.validateLease(ctx); err != nil {
		coordinator.recordFailure(domain.CandidateID{}, common.Hash{}, err)
		return err
	}
	incidents, err := coordinator.state.RescueIncidents(ctx, coordinator.network.ChainID)
	if err != nil {
		err = newError("rescue.reconciliation_state", domain.ErrorInternal, codeStateRead, true, true, err)
		coordinator.recordFailure(domain.CandidateID{}, common.Hash{}, err)
		return err
	}

	now := coordinator.clock.Now()
	complete := true
	for index := range incidents {
		incident := &incidents[index]
		if incident.Network != coordinator.network.ChainID {
			err = newError("rescue.reconciliation_state", domain.ErrorInternal, codeStateRead, false, true, nil)
			coordinator.recordFailure(incident.Candidate, common.Hash{}, err)
			return err
		}
		if incident.Status != store.RescueAmbiguous || incident.ReconcileUntil.IsZero() || now.Before(incident.ReconcileUntil) {
			continue
		}
		if err = session.reconcileExpiredIncident(ctx, incident); err == nil {
			continue
		}
		coordinator.recordFailure(incident.Candidate, publicTransactionHash(*incident), err)
		coordinator.recordIncidentFailure(incident, err)
		complete = false
		if fatalReconciliationError(err) {
			return err
		}
	}
	if err := session.refreshAmbiguousTelemetry(ctx); err != nil {
		return err
	}
	if coordinator.alerts != nil {
		var alertErr error
		if complete {
			_, alertErr = coordinator.alerts.Resolve(coordinator.network.ChainID, observability.AlertReconciliationStale)
		} else {
			_, alertErr = coordinator.alerts.Raise(coordinator.network.ChainID, observability.AlertReconciliationStale)
		}
		if alertErr != nil {
			return newError("rescue.reconciliation_alert", domain.ErrorInternal, codeStateWrite, true, true, alertErr)
		}
	}
	if complete && coordinator.metrics != nil {
		_ = coordinator.metrics.RecordSuccessfulReconciliation(coordinator.network.ChainID, coordinator.clock.Now())
	}
	return nil
}

func fatalReconciliationError(err error) bool {
	var classified *domain.ClassifiedError
	if !errors.As(err, &classified) {
		return true
	}
	return classified.Code == codeContextCanceled || classified.Class == domain.ErrorInternal || classified.Class == domain.ErrorSigning
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

	block, err := session.finality.Finalized(readContext)
	if err != nil || block.Hash == (common.Hash{}) {
		return contextOrError(readContext, "rescue.finalized", domain.ErrorRPCTransient, codeFinalityRead, true, true, err)
	}
	data, err := coordinator.rescuerCodec.PackDestination()
	if err != nil {
		return newError("rescue.destination", domain.ErrorInternal, codeEncoding, false, false, err)
	}
	result, err := session.finality.CallContract(readContext, block, ethereum.CallMsg{To: &coordinator.rescuer, Data: data})
	if err != nil {
		return contextOrError(readContext, "rescue.destination", domain.ErrorRPCTransient, codeDestinationRead, true, true, err)
	}
	destination, err := coordinator.rescuerCodec.DecodeDestination(result)
	if err != nil {
		return newError("rescue.destination", domain.ErrorRPCInvalidResponse, codeDestinationDecode, true, true, err)
	}
	if destination != coordinator.destination {
		return newError("rescue.destination", domain.ErrorConfiguration, codeDestinationMismatch, false, false, nil)
	}
	session.lastFinalized = block
	return nil
}

func (session *Session) recoverPersisted(ctx context.Context) error {
	durableFloor, err := session.coordinator.state.NonceFloor(ctx)
	if err != nil {
		return newError("rescue.nonce_floor", domain.ErrorInternal, codeStateRead, true, true, err)
	}
	session.coordinator.raiseNonceFloor(durableFloor)

	incidents, err := session.coordinator.state.RescueIncidents(ctx, session.coordinator.network.ChainID)
	if err != nil {
		return newError("rescue.state", domain.ErrorInternal, codeStateRead, true, true, err)
	}
	for index := range incidents {
		incident := &incidents[index]
		if incident.Network != session.coordinator.network.ChainID {
			return newError("rescue.state", domain.ErrorInternal, codeStateRead, false, true, nil)
		}
		if incidentHasSignedTransaction(*incident) {
			if incident.SponsorNonce == ^uint64(0) {
				return newError("rescue.nonce_floor", domain.ErrorInternal, codeStateRead, false, true, nil)
			}
			floor := incident.SponsorNonce + 1
			if err := session.coordinator.state.RaiseNonceFloor(ctx, floor); err != nil {
				return newError("rescue.nonce_floor", domain.ErrorInternal, codeStateWrite, true, true, err)
			}
			session.coordinator.raiseNonceFloor(floor)
		}
	}
	for index := range incidents {
		incident := &incidents[index]
		if !statusNeedsReceipt(incident.Status) {
			continue
		}
		if err := session.reconcileOnce(ctx, incident); err != nil {
			if ctx.Err() != nil || errorCode(err) == codeStateRead || errorCode(err) == codeStateWrite {
				return err
			}
			session.coordinator.recordFailure(incident.Candidate, publicTransactionHash(*incident), err)
			session.coordinator.recordIncidentFailure(incident, err)
		}
	}
	if err := session.refreshNonceBlock(ctx); err != nil {
		return err
	}
	if err := session.refreshAmbiguousTelemetry(ctx); err != nil {
		return err
	}
	if session.coordinator.metrics != nil {
		_ = session.coordinator.metrics.RecordSuccessfulReconciliation(session.coordinator.network.ChainID, session.coordinator.clock.Now())
	}
	return nil
}

func incidentHasSignedTransaction(incident store.RescueIncident) bool {
	if incident.TxHash == (common.Hash{}) {
		return false
	}
	switch incident.Status {
	case store.RescueSigned, store.RescueBroadcast, store.RescueAmbiguous, store.RescueRetryable,
		store.RescueExhausted, store.RescueFailed, store.RescueTrustedSuccess, store.RescueTokenReported, store.RescueLostRace:
		return true
	default:
		return false
	}
}

func statusNeedsReceipt(status store.RescueStatus) bool {
	return status == store.RescueSigned || status == store.RescueBroadcast || status == store.RescueAmbiguous
}

func (coordinator *Coordinator) currentLease() store.Lease {
	coordinator.leaseMu.RLock()
	defer coordinator.leaseMu.RUnlock()
	return coordinator.lease
}

func (coordinator *Coordinator) validateLease(ctx context.Context) error {
	select {
	case <-coordinator.leaseLost:
		return leaseError("rescue.lease")
	default:
	}
	coordinator.leaseActionMu.Lock()
	defer coordinator.leaseActionMu.Unlock()
	select {
	case <-coordinator.leaseLost:
		return leaseError("rescue.lease")
	default:
	}
	if err := coordinator.leaseManager.Validate(ctx, coordinator.currentLease()); err != nil {
		if ctx.Err() != nil {
			return contextOrError(ctx, "rescue.lease_validate", domain.ErrorInternal, codeContextCanceled, false, false, ctx.Err())
		}
		coordinator.markLeaseLost()
		return leaseError("rescue.lease_validate")
	}
	return nil
}

func (coordinator *Coordinator) markLeaseLost() {
	coordinator.leaseLostOnce.Do(func() { close(coordinator.leaseLost) })
}

func leaseError(operation string) *domain.ClassifiedError {
	return newError(operation, domain.ErrorSigning, CodeLeaseLost, false, true, ErrLeaseLost)
}

func processFenceError(operation string) *domain.ClassifiedError {
	return newError(operation, domain.ErrorSigning, CodeLeaseLost, false, true, errors.Join(ErrLeaseLost, store.ErrFenceLost))
}

func (coordinator *Coordinator) validateProcessFence() error {
	if err := coordinator.processFence.Validate(); err != nil {
		return processFenceError("rescue.process_fence")
	}
	return nil
}

func (coordinator *Coordinator) checkSignerAddresses() error {
	if coordinator.authorizer.Address() != coordinator.source || coordinator.transactioner.Address() != coordinator.sponsor {
		return newError("rescue.signer", domain.ErrorSigning, codeSignerMismatch, false, false, nil)
	}
	return nil
}

func (coordinator *Coordinator) guardSigning(ctx context.Context) error {
	if err := coordinator.validateProcessFence(); err != nil {
		return err
	}
	if err := coordinator.validateLease(ctx); err != nil {
		return err
	}
	return coordinator.checkSignerAddresses()
}

func (coordinator *Coordinator) raiseNonceFloor(floor uint64) {
	if floor > coordinator.nonceFloor {
		coordinator.nonceFloor = floor
	}
}

func (coordinator *Coordinator) record(code observability.EventCode, level observability.Level, candidate domain.CandidateID, hash common.Hash, errorCode domain.ErrorCode) {
	coordinator.observer.Record(observability.Event{
		Level:       level,
		Code:        code,
		ChainID:     coordinator.network.ChainID,
		NetworkName: coordinator.network.Name,
		Candidate:   candidate,
		TxHash:      hash,
		ErrorCode:   errorCode,
	})
}

func (coordinator *Coordinator) recordFailure(candidate domain.CandidateID, hash common.Hash, err error) {
	coordinator.record(eventOperationFailed, observability.LevelError, candidate, hash, errorCode(err))
	var classified *domain.ClassifiedError
	if errors.As(err, &classified) {
		if classified.Class == domain.ErrorRPCTransient || classified.Class == domain.ErrorRPCInvalidResponse {
			if coordinator.metrics != nil {
				_ = coordinator.metrics.RecordRPCError(coordinator.network.ChainID)
			}
			if coordinator.health != nil {
				_ = coordinator.health.SetCondition(coordinator.network.ChainID, observability.ConditionRPCDegraded, true)
			}
			if coordinator.alerts != nil {
				if _, alertErr := coordinator.alerts.Raise(coordinator.network.ChainID, observability.AlertRPCDegraded); alertErr != nil && coordinator.health != nil {
					_ = coordinator.health.SetCondition(coordinator.network.ChainID, observability.ConditionRPCDegraded, true)
				}
			}
		}
		coordinator.recordSafe(observability.SafeEvent{
			Level: observability.LevelError, Code: observability.LogStateTransition,
			ChainID: coordinator.network.ChainID, Result: observability.ResultFailed,
			Error: coordinator.safeError(classified.Class, classified.Code, classified.Retryable, classified.Ambiguous),
		})
	}
}

func (coordinator *Coordinator) recordCandidateFailure(ctx context.Context, candidate domain.CandidateID, failure error) {
	incidents, err := coordinator.state.RescueIncidents(ctx, coordinator.network.ChainID)
	if err != nil {
		return
	}
	for index := range incidents {
		if incidents[index].Candidate == candidate {
			coordinator.recordIncidentFailure(&incidents[index], failure)
		}
	}
}

func (coordinator *Coordinator) recordIncidentFailure(incident *store.RescueIncident, failure error) {
	if incident == nil {
		return
	}
	state, result := incidentFailureState(incident.Status)
	event := observability.SafeEvent{
		Level: observability.LevelError, Code: observability.LogStateTransition,
		ChainID: coordinator.network.ChainID, Incident: incident.ID, State: state, Result: result,
	}
	var classified *domain.ClassifiedError
	if errors.As(failure, &classified) {
		event.Error = coordinator.safeError(classified.Class, classified.Code, classified.Retryable, classified.Ambiguous)
	}
	if hash := publicTransactionHash(*incident); hash != (common.Hash{}) {
		if publicHash, err := observability.NewPublicTxHashAfterBroadcast(hash); err == nil {
			event.TxHash = publicHash
		}
	}
	coordinator.recordSafe(event)
}

func incidentFailureState(status store.RescueStatus) (observability.DurableState, observability.Result) {
	switch status {
	case store.RescuePending:
		return observability.DurablePending, observability.ResultFailed
	case store.RescuePrepared:
		return observability.DurableProcessing, observability.ResultFailed
	case store.RescueSigned, store.RescueRetryable:
		return observability.DurableRetryPending, observability.ResultFailed
	case store.RescueBroadcast:
		return observability.DurableBroadcast, observability.ResultBroadcast
	case store.RescueAmbiguous:
		return observability.DurableAmbiguous, observability.ResultAmbiguous
	case store.RescueTrustedSuccess:
		return observability.DurableConfirmed, observability.ResultConfirmed
	case store.RescueTokenReported:
		return observability.DurableConfirmed, observability.ResultTokenReported
	case store.RescueLostRace:
		return observability.DurableFailed, observability.ResultLostRace
	default:
		return observability.DurableFailed, observability.ResultFailed
	}
}

func (coordinator *Coordinator) recordRPCHealthy() {
	if coordinator.alerts != nil {
		if _, err := coordinator.alerts.Resolve(coordinator.network.ChainID, observability.AlertRPCDegraded); err != nil {
			if coordinator.health != nil {
				_ = coordinator.health.SetCondition(coordinator.network.ChainID, observability.ConditionRPCDegraded, true)
			}
			return
		}
	}
	if coordinator.health != nil {
		_ = coordinator.health.SetCondition(coordinator.network.ChainID, observability.ConditionRPCDegraded, false)
	}
}

func (coordinator *Coordinator) recordSafe(event observability.SafeEvent) {
	if coordinator.structured != nil {
		_ = coordinator.structured.Write(event)
	}
}

func (coordinator *Coordinator) safeError(class domain.ErrorClass, code domain.ErrorCode, retryable, ambiguous bool) *observability.SafeError {
	safeClass := observability.ErrorInternal
	switch class {
	case domain.ErrorConfiguration:
		safeClass = observability.ErrorConfiguration
	case domain.ErrorRPCTransient:
		safeClass = observability.ErrorRPCTransient
	case domain.ErrorRPCInvalidResponse:
		safeClass = observability.ErrorRPCInvalidResponse
	case domain.ErrorBudget:
		safeClass = observability.ErrorBudget
	case domain.ErrorSigning:
		safeClass = observability.ErrorSigning
	case domain.ErrorBroadcast:
		safeClass = observability.ErrorBroadcast
	case domain.ErrorPostcondition:
		safeClass = observability.ErrorPostcondition
	}
	safeCode := observability.ErrorInternalFailure
	switch code {
	case codeBudgetExceeded, codeAdmissionLimited:
		safeCode = observability.ErrorBudgetExhausted
	case codeSponsorReserve:
		safeCode = observability.ErrorSponsorReserve
	case codeSigning, codeSignerMismatch:
		safeCode = observability.ErrorSigningFailed
	case codeBroadcast:
		safeCode = observability.ErrorBroadcastFailed
	case codePostcondition, codeLostRace, codeMinimumValue:
		safeCode = observability.ErrorPostconditionFailed
	case codePaidActionsStopped:
		safeCode = observability.ErrorPaidActionsStopped
	case codeFinalityRead, codeBalanceRead, codeNonceRead, codeFeeRead:
		safeCode = observability.ErrorRPCUnavailable
	case codeReceiptInvalid, codeBalanceDecode, codeFeeInvalid:
		safeCode = observability.ErrorRPCResponseInvalid
	}
	result, err := observability.NewSafeError(safeClass, safeCode, retryable, ambiguous)
	if err != nil {
		return nil
	}
	return &result
}

func errorCode(err error) domain.ErrorCode {
	var classified *domain.ClassifiedError
	if errors.As(err, &classified) {
		return classified.Code
	}
	return domain.ErrorCode("rescue_internal_failure")
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
