package observability

import (
	"errors"
	"math"
	"sync"
	"time"

	"guard-daemon/internal/domain"

	"github.com/holiman/uint256"
)

const MaxConfiguredChains = 128

var (
	ErrInvalidChains        = errors.New("invalid set of monitored chain IDs")
	ErrUnknownChain         = errors.New("chain ID is not in the configured set")
	ErrAmbiguousUnderflow   = errors.New("active ambiguous result count is already zero")
	ErrInvalidDelegation    = errors.New("invalid delegation state")
	ErrInvalidReconcileTime = errors.New("successful reconciliation time is not set")
)

// DelegationState перечисляет допустимые состояния делегирования EIP-7702.
type DelegationState uint8

const (
	DelegationUnknown DelegationState = iota + 1
	DelegationExpected
	DelegationMissing
	DelegationUnexpected
)

// ChainMetrics не содержит меток по токенам, инцидентам и другим данным
// с неограниченным числом значений.
type ChainMetrics struct {
	QueueDepth                   uint64
	Candidates                   uint64
	Attempts                     uint64
	SpentBudget                  uint256.Int
	ReservedBudget               uint256.Int
	RemainingBudget              uint256.Int
	RPCErrors                    uint64
	Reconnects                   uint64
	LostRaces                    uint64
	ActiveAmbiguous              uint64
	TotalAmbiguous               uint64
	Delegation                   DelegationState
	LastSuccessfulReconciliation time.Time
}

// MetricsSnapshot содержит глубокую копию состояния на один момент времени.
type MetricsSnapshot struct {
	Chains       map[domain.NetworkID]ChainMetrics
	GlobalBudget BudgetMetrics
}

type BudgetMetrics struct {
	Spent     uint256.Int
	Reserved  uint256.Int
	Remaining uint256.Int
}

// Metrics хранит только заранее настроенные chain ID, поэтому входящие данные
// о токенах и событиях не увеличивают число значений меток.
type Metrics struct {
	mu           sync.RWMutex
	chains       map[domain.NetworkID]*ChainMetrics
	globalBudget BudgetMetrics
}

func NewMetrics(chainIDs []domain.NetworkID) (*Metrics, error) {
	configured, err := configuredChainSet(chainIDs)
	if err != nil {
		return nil, err
	}
	metrics := &Metrics{chains: make(map[domain.NetworkID]*ChainMetrics, len(configured))}
	for chainID := range configured {
		metrics.chains[chainID] = &ChainMetrics{Delegation: DelegationUnknown}
	}
	return metrics, nil
}

func (metrics *Metrics) SetQueueDepth(chainID domain.NetworkID, depth uint64) error {
	return metrics.update(chainID, func(current *ChainMetrics) {
		current.QueueDepth = depth
	})
}

func (metrics *Metrics) RecordCandidate(chainID domain.NetworkID) error {
	return metrics.update(chainID, func(current *ChainMetrics) {
		current.Candidates = saturatingIncrement(current.Candidates)
	})
}

func (metrics *Metrics) RecordAttempt(chainID domain.NetworkID) error {
	return metrics.update(chainID, func(current *ChainMetrics) {
		current.Attempts = saturatingIncrement(current.Attempts)
	})
}

func (metrics *Metrics) SetBudget(chainID domain.NetworkID, spent, reserved, remaining uint256.Int) error {
	return metrics.update(chainID, func(current *ChainMetrics) {
		current.SpentBudget = spent
		current.ReservedBudget = reserved
		current.RemainingBudget = remaining
	})
}

func (metrics *Metrics) SetGlobalBudget(spent, reserved, remaining uint256.Int) {
	metrics.mu.Lock()
	metrics.globalBudget = BudgetMetrics{Spent: spent, Reserved: reserved, Remaining: remaining}
	metrics.mu.Unlock()
}

func (metrics *Metrics) RecordRPCError(chainID domain.NetworkID) error {
	return metrics.update(chainID, func(current *ChainMetrics) {
		current.RPCErrors = saturatingIncrement(current.RPCErrors)
	})
}

func (metrics *Metrics) RecordReconnect(chainID domain.NetworkID) error {
	return metrics.update(chainID, func(current *ChainMetrics) {
		current.Reconnects = saturatingIncrement(current.Reconnects)
	})
}

func (metrics *Metrics) RecordLostRace(chainID domain.NetworkID) error {
	return metrics.update(chainID, func(current *ChainMetrics) {
		current.LostRaces = saturatingIncrement(current.LostRaces)
	})
}

// OpenAmbiguous одновременно увеличивает текущее число операций с неопределённым
// результатом и их накопительный счётчик.
func (metrics *Metrics) OpenAmbiguous(chainID domain.NetworkID) error {
	return metrics.update(chainID, func(current *ChainMetrics) {
		current.ActiveAmbiguous = saturatingIncrement(current.ActiveAmbiguous)
		current.TotalAmbiguous = saturatingIncrement(current.TotalAmbiguous)
	})
}

func (metrics *Metrics) ResolveAmbiguous(chainID domain.NetworkID) error {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	current, ok := metrics.chains[chainID]
	if !ok {
		return ErrUnknownChain
	}
	if current.ActiveAmbiguous == 0 {
		return ErrAmbiguousUnderflow
	}
	current.ActiveAmbiguous--
	return nil
}

// SetActiveAmbiguous восстанавливает текущее значение из сохранённого состояния,
// не увеличивая накопительный счётчик при каждом подключении RPC.
func (metrics *Metrics) SetActiveAmbiguous(chainID domain.NetworkID, active uint64) error {
	return metrics.update(chainID, func(current *ChainMetrics) {
		current.ActiveAmbiguous = active
	})
}

func (metrics *Metrics) SetDelegationState(chainID domain.NetworkID, state DelegationState) error {
	if !validDelegationState(state) {
		return ErrInvalidDelegation
	}
	return metrics.update(chainID, func(current *ChainMetrics) {
		current.Delegation = state
	})
}

// RecordSuccessfulReconciliation не позволяет более старому обновлению сдвинуть
// назад время последней успешной сверки.
func (metrics *Metrics) RecordSuccessfulReconciliation(chainID domain.NetworkID, at time.Time) error {
	if at.IsZero() {
		return ErrInvalidReconcileTime
	}
	at = at.UTC().Round(0)
	return metrics.update(chainID, func(current *ChainMetrics) {
		if at.After(current.LastSuccessfulReconciliation) {
			current.LastSuccessfulReconciliation = at
		}
	})
}

func (metrics *Metrics) Snapshot() MetricsSnapshot {
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	snapshot := MetricsSnapshot{Chains: make(map[domain.NetworkID]ChainMetrics, len(metrics.chains)), GlobalBudget: metrics.globalBudget}
	for chainID, current := range metrics.chains {
		snapshot.Chains[chainID] = *current
	}
	return snapshot
}

func (metrics *Metrics) update(chainID domain.NetworkID, update func(*ChainMetrics)) error {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	current, ok := metrics.chains[chainID]
	if !ok {
		return ErrUnknownChain
	}
	update(current)
	return nil
}

func configuredChainSet(chainIDs []domain.NetworkID) (map[domain.NetworkID]struct{}, error) {
	if len(chainIDs) == 0 || len(chainIDs) > MaxConfiguredChains {
		return nil, ErrInvalidChains
	}
	configured := make(map[domain.NetworkID]struct{}, len(chainIDs))
	for _, chainID := range chainIDs {
		if chainID <= 0 {
			return nil, ErrInvalidChains
		}
		if _, exists := configured[chainID]; exists {
			return nil, ErrInvalidChains
		}
		configured[chainID] = struct{}{}
	}
	return configured, nil
}

func saturatingIncrement(value uint64) uint64 {
	if value == math.MaxUint64 {
		return value
	}
	return value + 1
}

func validDelegationState(state DelegationState) bool {
	return state >= DelegationUnknown && state <= DelegationUnexpected
}
