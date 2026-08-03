package observability

import (
	"encoding/json"
	"errors"
	"io"
	"sync"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
)

var (
	ErrInvalidLogEvent = errors.New("событие журнала имеет недопустимую безопасную схему")
	ErrInvalidTxHash   = errors.New("публичный хеш транзакции пуст")
)

type Level uint8

const (
	LevelInfo Level = iota + 1
	LevelWarning
	LevelError
)

// EventCode сохранён для совместимости старых production call sites. Новый код
// должен использовать закрытый LogCode через SafeEvent.
type EventCode string

// Event сохранён как переходный вход Observer. TokenSymbol, Amount, Candidate
// и TxHash намеренно не попадают в журнал: их происхождение и момент
// публичности нельзя доказать из legacy-схемы.
type Event struct {
	Level       Level
	Code        EventCode
	ChainID     domain.NetworkID
	NetworkName string
	Candidate   domain.CandidateID
	TokenSymbol string
	Amount      string
	TxHash      common.Hash
	ErrorCode   domain.ErrorCode
}

type Observer interface {
	Record(Event)
}

type StructuredObserver interface {
	Write(SafeEvent) error
}

type Discard struct{}

func (Discard) Record(Event) {}

func (Discard) Write(SafeEvent) error { return nil }

// LogCode задаёт закрытый набор безопасных событий журнала.
type LogCode uint8

const (
	LogCandidateQueued LogCode = iota + 1
	LogAttemptStarted
	LogBudgetBlocked
	LogBroadcastAccepted
	LogReconciliationCompleted
	LogDelegationChecked
	LogRPCFailure
	LogStateTransition
	LogPaidActionsStopped
	LogAlertState
)

// DurableState описывает только состояние, сохранённое либо подлежащее
// сохранению durable coordinator.
type DurableState uint8

const (
	DurablePending DurableState = iota + 1
	DurableProcessing
	DurableRetryPending
	DurableBroadcast
	DurableAmbiguous
	DurableConfirmed
	DurableFailed
	DurableAcknowledged
)

// Result задаёт закрытую классификацию результата без произвольного текста.
type Result uint8

const (
	ResultAccepted Result = iota + 1
	ResultSkipped
	ResultFailed
	ResultBroadcast
	ResultConfirmed
	ResultTokenReported
	ResultLostRace
	ResultAmbiguous
)

// ErrorClass задаёт безопасный класс ошибки для журнала и wiring.
type ErrorClass uint8

const (
	ErrorConfiguration ErrorClass = iota + 1
	ErrorRPCTransient
	ErrorRPCInvalidResponse
	ErrorBudget
	ErrorSigning
	ErrorBroadcast
	ErrorPostcondition
	ErrorInternal
)

// ErrorCode задаёт закрытые операторские коды без текста исходной ошибки.
type ErrorCode uint8

const (
	ErrorInvalidConfiguration ErrorCode = iota + 1
	ErrorRPCUnavailable
	ErrorRPCResponseInvalid
	ErrorBudgetExhausted
	ErrorSponsorReserve
	ErrorSigningFailed
	ErrorBroadcastFailed
	ErrorPostconditionFailed
	ErrorAmbiguousOutcome
	ErrorPaidActionsStopped
	ErrorInternalFailure
)

// SafeError не хранит cause или message, поэтому raw error нельзя случайно
// сериализовать. Значение создаётся только из закрытых class/code.
type SafeError struct {
	class     ErrorClass
	code      ErrorCode
	retryable bool
	ambiguous bool
}

func NewSafeError(class ErrorClass, code ErrorCode, retryable, ambiguous bool) (SafeError, error) {
	if !validErrorClass(class) || !validErrorCode(code) {
		return SafeError{}, ErrInvalidLogEvent
	}
	return SafeError{class: class, code: code, retryable: retryable, ambiguous: ambiguous}, nil
}

func (safeError SafeError) Class() ErrorClass { return safeError.class }

func (safeError SafeError) Code() ErrorCode { return safeError.code }

func (safeError SafeError) Retryable() bool { return safeError.retryable }

func (safeError SafeError) Ambiguous() bool { return safeError.ambiguous }

// PublicTxHash может быть создан только в точке, где broadcast уже принят.
// Тип не предоставляет сериализацию raw signed transaction.
type PublicTxHash struct {
	hash common.Hash
}

func NewPublicTxHashAfterBroadcast(hash common.Hash) (PublicTxHash, error) {
	if hash == (common.Hash{}) {
		return PublicTxHash{}, ErrInvalidTxHash
	}
	return PublicTxHash{hash: hash}, nil
}

func (hash PublicTxHash) Hash() common.Hash { return hash.hash }

// SafeEvent содержит только поля, допустимые в публичном structured log.
type SafeEvent struct {
	Level    Level
	Code     LogCode
	ChainID  domain.NetworkID
	Incident domain.IncidentID
	State    DurableState
	Result   Result
	TxHash   PublicTxHash
	Error    *SafeError
	Alert    AlertCode
}

// Logger записывает одно JSON-событие на строку и сериализует concurrent writes.
type Logger struct {
	mu     sync.Mutex
	writer io.Writer
}

func NewLogger(writer io.Writer) *Logger {
	if writer == nil {
		writer = io.Discard
	}
	return &Logger{writer: writer}
}

// Write проверяет закрытую схему до записи. При ошибке в output не появляется
// частичная строка.
func (logger *Logger) Write(event SafeEvent) error {
	record, err := newSafeLogRecord(event)
	if err != nil {
		return err
	}
	return logger.writeRecord(record)
}

// Record поддерживает legacy Observer, но отбрасывает все потенциально
// чувствительные incident-specific поля.
func (logger *Logger) Record(event Event) {
	record := legacyLogRecord{
		Level:   legacyLevel(event.Level),
		Code:    legacyEventCode(event.Code),
		ChainID: event.ChainID,
	}
	if event.ErrorCode != "" {
		record.ErrorCode = "error_redacted"
	}
	_ = logger.writeRecord(record)
}

func (logger *Logger) writeRecord(record any) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	logger.mu.Lock()
	defer logger.mu.Unlock()
	n, err := logger.writer.Write(encoded)
	if err == nil && n != len(encoded) {
		return io.ErrShortWrite
	}
	return err
}

// Console сохранён как совместимое имя; формат теперь является NDJSON.
type Console struct {
	*Logger
}

func NewConsole(writer io.Writer) *Console {
	return &Console{Logger: NewLogger(writer)}
}

type safeLogRecord struct {
	Level      string           `json:"level"`
	Code       string           `json:"code"`
	ChainID    domain.NetworkID `json:"chain_id"`
	Incident   string           `json:"incident,omitempty"`
	State      string           `json:"durable_state,omitempty"`
	Result     string           `json:"result,omitempty"`
	TxHash     string           `json:"tx_hash,omitempty"`
	ErrorClass string           `json:"error_class,omitempty"`
	ErrorCode  string           `json:"error_code,omitempty"`
	AlertCode  string           `json:"alert_code,omitempty"`
	Retryable  *bool            `json:"retryable,omitempty"`
	Ambiguous  *bool            `json:"ambiguous,omitempty"`
}

type legacyLogRecord struct {
	Level     string           `json:"level"`
	Code      string           `json:"code"`
	ChainID   domain.NetworkID `json:"chain_id,omitempty"`
	ErrorCode string           `json:"error_code,omitempty"`
}

func newSafeLogRecord(event SafeEvent) (safeLogRecord, error) {
	if !validLevel(event.Level) || !validLogCode(event.Code) || event.ChainID <= 0 ||
		(event.State != 0 && !validDurableState(event.State)) ||
		(event.Result != 0 && !validResult(event.Result)) {
		return safeLogRecord{}, ErrInvalidLogEvent
	}

	record := safeLogRecord{
		Level:   levelName(event.Level),
		Code:    logCodeName(event.Code),
		ChainID: event.ChainID,
		State:   durableStateName(event.State),
		Result:  resultName(event.Result),
	}
	if event.Incident != (domain.IncidentID{}) {
		record.Incident = event.Incident.String()
	}
	if event.TxHash.hash != (common.Hash{}) {
		if event.Result == 0 {
			return safeLogRecord{}, ErrInvalidLogEvent
		}
		record.TxHash = event.TxHash.hash.Hex()
	}
	if event.Error != nil {
		if !validErrorClass(event.Error.class) || !validErrorCode(event.Error.code) {
			return safeLogRecord{}, ErrInvalidLogEvent
		}
		record.ErrorClass = errorClassName(event.Error.class)
		record.ErrorCode = errorCodeName(event.Error.code)
		record.Retryable = boolPointer(event.Error.retryable)
		record.Ambiguous = boolPointer(event.Error.ambiguous)
	}
	if event.Alert != 0 {
		if !validAlertCode(event.Alert) {
			return safeLogRecord{}, ErrInvalidLogEvent
		}
		record.AlertCode = event.Alert.String()
	}
	return record, nil
}

func validLevel(level Level) bool {
	return level >= LevelInfo && level <= LevelError
}

func validLogCode(code LogCode) bool {
	return code >= LogCandidateQueued && code <= LogAlertState
}

func validDurableState(state DurableState) bool {
	return state >= DurablePending && state <= DurableAcknowledged
}

func validResult(result Result) bool {
	return result >= ResultAccepted && result <= ResultAmbiguous
}

func validErrorClass(class ErrorClass) bool {
	return class >= ErrorConfiguration && class <= ErrorInternal
}

func validErrorCode(code ErrorCode) bool {
	return code >= ErrorInvalidConfiguration && code <= ErrorInternalFailure
}

func legacyLevel(level Level) string {
	if !validLevel(level) {
		return "warning"
	}
	return levelName(level)
}

func levelName(level Level) string {
	return [...]string{"", "info", "warning", "error"}[level]
}

func logCodeName(code LogCode) string {
	return [...]string{
		"",
		"candidate_queued",
		"attempt_started",
		"budget_blocked",
		"broadcast_accepted",
		"reconciliation_completed",
		"delegation_checked",
		"rpc_failure",
		"state_transition",
		"paid_actions_stopped",
		"alert_state",
	}[code]
}

func durableStateName(state DurableState) string {
	return [...]string{
		"",
		"pending",
		"processing",
		"retry_pending",
		"broadcast",
		"ambiguous",
		"confirmed",
		"failed",
		"acknowledged",
	}[state]
}

func resultName(result Result) string {
	return [...]string{
		"",
		"accepted",
		"skipped",
		"failed",
		"broadcast",
		"confirmed",
		"token_reported",
		"lost_race",
		"ambiguous",
	}[result]
}

func errorClassName(class ErrorClass) string {
	return [...]string{
		"",
		"configuration",
		"rpc_transient",
		"rpc_invalid_response",
		"budget",
		"signing",
		"broadcast",
		"postcondition",
		"internal",
	}[class]
}

func errorCodeName(code ErrorCode) string {
	return [...]string{
		"",
		"invalid_configuration",
		"rpc_unavailable",
		"rpc_response_invalid",
		"budget_exhausted",
		"sponsor_reserve",
		"signing_failed",
		"broadcast_failed",
		"postcondition_failed",
		"ambiguous_outcome",
		"paid_actions_stopped",
		"internal_failure",
	}[code]
}

func legacyEventCode(code EventCode) string {
	switch code {
	case "safe_event",
		"watcher_candidate_inserted",
		"watcher_candidate_pending",
		"watcher_candidate_acknowledged",
		"watcher_subscription_fallback",
		"watcher_head_unavailable",
		"watcher_read_failed",
		"watcher_metadata_fallback",
		"watcher_log_rejected",
		"watcher_candidate_put_failed",
		"watcher_observation_saturated",
		"watcher_discovery_overflow",
		"rescue_startup_verified",
		"rescue_operation_prepared",
		"rescue_broadcast_accepted",
		"rescue_operation_confirmed",
		"rescue_operation_failed",
		"daemon_rpc_http_fallback",
		"daemon_generation_failed",
		"daemon_unknown_token_opt_in":
		return string(code)
	default:
		return "event_redacted"
	}
}

func boolPointer(value bool) *bool {
	return &value
}

var (
	_ Observer           = (*Logger)(nil)
	_ StructuredObserver = (*Logger)(nil)
	_ Observer           = Discard{}
	_ StructuredObserver = Discard{}
)
