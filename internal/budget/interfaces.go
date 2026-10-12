package budget

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

var (
	ErrInvalidOptions     = errors.New("budget ledger: invalid options")
	ErrOpenFailed         = errors.New("budget ledger: failed to open ledger")
	ErrPolicyMismatch     = errors.New("budget ledger: stored policy does not match")
	ErrCorrupt            = errors.New("budget ledger: data is corrupt")
	ErrClosed             = errors.New("budget ledger: closed")
	ErrInvalidRequest     = errors.New("budget ledger: invalid request")
	ErrArithmeticOverflow = errors.New("budget ledger: cost overflow")
	ErrBudgetExceeded     = errors.New("budget ledger: budget exceeded")
	ErrSponsorReserve     = errors.New("budget ledger: sponsor emergency reserve is insufficient")
	ErrStateConflict      = errors.New("budget ledger: reservation state conflict")
	ErrCapacity           = errors.New("budget ledger: persistent record capacity reached")
)

// Limits задаёт предельные суммы расходов в wei. Нулевые значения запрещены.
type Limits struct {
	PerTransaction uint256.Int
	PerHour        uint256.Int
	PerDay         uint256.Int
	Cumulative     uint256.Int
}

// NetworkPolicy связывает бюджет сети с единственным спонсором и неизменными
// накладными расходами этой сети.
type NetworkPolicy struct {
	Network                 domain.NetworkID
	Sponsor                 common.Address
	Limits                  Limits
	TransactionOverhead     uint256.Int
	EmergencySponsorReserve uint256.Int
}

// Policy задаёт глобальные лимиты и ограничения для каждой обслуживаемой сети.
type Policy struct {
	Global   Limits
	Networks []NetworkPolicy
}

// CostQuote содержит только исходные данные, полученные до подписания. Метод Maximum
// вычисляет максимальную стоимость с учётом накладных расходов из связанной NetworkPolicy.
type CostQuote struct {
	GasLimit     uint64
	MaxFeePerGas uint256.Int
	MaximumCost  uint256.Int
}

// Maximum возвращает gasLimit*maxFeePerGas+overhead с проверкой переполнения, без
// арифметики по модулю.
func (quote CostQuote) Maximum(overhead uint256.Int) (uint256.Int, error) {
	if quote.GasLimit == 0 || quote.MaxFeePerGas.IsZero() {
		return uint256.Int{}, ErrInvalidRequest
	}
	var gas, maximum uint256.Int
	gas.SetUint64(quote.GasLimit)
	if _, overflow := maximum.MulOverflow(&gas, &quote.MaxFeePerGas); overflow {
		return uint256.Int{}, ErrArithmeticOverflow
	}
	if _, overflow := maximum.AddOverflow(&maximum, &overhead); overflow || maximum.IsZero() {
		return uint256.Int{}, ErrArithmeticOverflow
	}
	if !quote.MaximumCost.IsZero() {
		if quote.MaximumCost.Lt(&maximum) {
			return uint256.Int{}, ErrInvalidRequest
		}
		return quote.MaximumCost, nil
	}
	return maximum, nil
}

// Attempt идентифицирует одну сохраняемую попытку устранения инцидента.
type Attempt struct {
	Incident domain.IncidentID
	Number   uint32
}

// ReservationID определяется только значением Attempt и не зависит от времени или
// порядка конкурентных запросов.
type ReservationID [sha256.Size]byte

func (id ReservationID) String() string { return hex.EncodeToString(id[:]) }

// NewReservationID возвращает неизменный ID для идемпотентной повторной обработки.
func NewReservationID(attempt Attempt) ReservationID {
	hash := sha256.New()
	_, _ = hash.Write([]byte("guard-daemon/budget-reservation/v1"))
	_, _ = hash.Write(attempt.Incident[:])
	var number [4]byte
	binary.BigEndian.PutUint32(number[:], attempt.Number)
	_, _ = hash.Write(number[:])
	var id ReservationID
	copy(id[:], hash.Sum(nil))
	return id
}

type ReservationRequest struct {
	Network        domain.NetworkID
	Sponsor        common.Address
	Candidate      domain.CandidateID
	Attempt        Attempt
	Quote          CostQuote
	SponsorBalance uint256.Int
	ObservedAt     time.Time
}

type ReservationState uint8

const (
	ReservationHeld ReservationState = iota + 1
	ReservationExposed
	ReservationCommitted
	ReservationReleased
)

// Reservation хранит исходный запрос, вычисленный максимум и подтверждённые
// переходы состояния. Поле Actual заполняется только после подтверждения окончательных расходов.
type Reservation struct {
	ID        ReservationID
	Request   ReservationRequest
	Maximum   uint256.Int
	State     ReservationState
	TxHash    common.Hash
	Actual    uint256.Int
	CreatedAt time.Time
	ExposedAt time.Time
	UpdatedAt time.Time
}

type FinalizedCharge struct {
	ReservationID ReservationID
	TxHash        common.Hash
	Actual        uint256.Int
	ObservedAt    time.Time
}

// Totals содержит суммы за скользящие часовой и суточный периоды, а также за всё время.
type Totals struct {
	PerHour    uint256.Int
	PerDay     uint256.Int
	Cumulative uint256.Int
}

type ScopeSnapshot struct {
	Spent     Totals
	Reserved  Totals
	Remaining Totals
}

// Blocked сообщает, что хотя бы один часовой, суточный или накопительный лимит
// исчерпан и новое ненулевое резервирование невозможно.
func (snapshot ScopeSnapshot) Blocked() bool {
	return snapshot.Remaining.PerHour.IsZero() || snapshot.Remaining.PerDay.IsZero() || snapshot.Remaining.Cumulative.IsZero()
}

// Snapshot содержит атомарный снимок глобальных и всех сетевых счётчиков.
type Snapshot struct {
	At       time.Time
	Global   ScopeSnapshot
	Networks map[domain.NetworkID]ScopeSnapshot
}

type OpenOptions struct {
	Policy            Policy
	PolicyFingerprint [sha256.Size]byte
	Now               func() time.Time
	MaxRecords        uint32
}

// Ledger предоставляет координаторам всех сетей единый долговременный реестр.
type Ledger interface {
	Reserve(context.Context, ReservationRequest) (Reservation, error)
	MarkExposed(context.Context, ReservationID, common.Hash) (Reservation, error)
	CommitFinalized(context.Context, FinalizedCharge) (Reservation, error)
	ReleaseProvenUnused(context.Context, ReservationID) (Reservation, error)
	CheckSponsorCapacity(context.Context, ReservationID, uint256.Int) error
	ReservationByAttempt(context.Context, Attempt) (Reservation, bool, error)
	OpenReservations(context.Context) ([]Reservation, error)
	Snapshot(context.Context) (Snapshot, error)
	Close() error
}
