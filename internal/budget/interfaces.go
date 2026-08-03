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
	ErrInvalidOptions     = errors.New("budget ledger: некорректные параметры")
	ErrOpenFailed         = errors.New("budget ledger: не удалось открыть ledger")
	ErrPolicyMismatch     = errors.New("budget ledger: сохранённая policy не совпадает")
	ErrCorrupt            = errors.New("budget ledger: данные повреждены")
	ErrClosed             = errors.New("budget ledger: закрыт")
	ErrInvalidRequest     = errors.New("budget ledger: некорректный запрос")
	ErrArithmeticOverflow = errors.New("budget ledger: переполнение стоимости")
	ErrBudgetExceeded     = errors.New("budget ledger: исчерпан лимит")
	ErrSponsorReserve     = errors.New("budget ledger: недостаточный emergency reserve sponsor")
	ErrStateConflict      = errors.New("budget ledger: конфликт состояния reservation")
	ErrCapacity           = errors.New("budget ledger: достигнут предел persistent records")
)

// Limits задаёт верхние границы расходов в wei. Нулевые границы запрещены.
type Limits struct {
	PerTransaction uint256.Int
	PerHour        uint256.Int
	PerDay         uint256.Int
	Cumulative     uint256.Int
}

// NetworkPolicy связывает бюджет сети с единственным sponsor и неизменяемыми
// chain-specific накладными расходами.
type NetworkPolicy struct {
	Network                 domain.NetworkID
	Sponsor                 common.Address
	Limits                  Limits
	TransactionOverhead     uint256.Int
	EmergencySponsorReserve uint256.Int
}

// Policy задаёт global limits и ограничения каждой обслуживаемой сети.
type Policy struct {
	Global   Limits
	Networks []NetworkPolicy
}

// CostQuote содержит только входы, полученные до signing. Maximum вычисляется
// ledger с накладными расходами из привязанной NetworkPolicy.
type CostQuote struct {
	GasLimit     uint64
	MaxFeePerGas uint256.Int
	MaximumCost  uint256.Int
}

// Maximum возвращает gasLimit*maxFeePerGas+overhead без арифметики по модулю.
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

// Attempt является durable identity одной попытки rescue incident.
type Attempt struct {
	Incident domain.IncidentID
	Number   uint32
}

// ReservationID детерминирован только Attempt и не зависит от времени или
// порядка конкурентных запросов.
type ReservationID [sha256.Size]byte

func (id ReservationID) String() string { return hex.EncodeToString(id[:]) }

// NewReservationID возвращает стабильный ID для идемпотентного replay.
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

// Reservation хранит исходный запрос, вычисленный maximum и доказательства
// переходов. Actual заполнен только для committed reservation.
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

// Totals содержит значения для rolling hour/day и lifetime cumulative окна.
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

// Blocked сообщает, что хотя бы одно rolling или cumulative окно полностью
// исчерпано и новая ненулевая reservation невозможна.
func (snapshot ScopeSnapshot) Blocked() bool {
	return snapshot.Remaining.PerHour.IsZero() || snapshot.Remaining.PerDay.IsZero() || snapshot.Remaining.Cumulative.IsZero()
}

// Snapshot является атомарным срезом global и всех per-network counters.
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

// Ledger является единым persistent ledger для coordinators всех сетей.
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
