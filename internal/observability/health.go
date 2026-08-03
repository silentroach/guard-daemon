package observability

import (
	"errors"
	"sync"

	"guard-daemon/internal/domain"
)

var ErrInvalidHealthCondition = errors.New("health condition недопустимо")

// HealthState имеет детерминированный приоритет: stopped, ambiguous rescue,
// blocked budget, degraded RPC, healthy idle.
type HealthState uint8

const (
	HealthHealthyIdle HealthState = iota + 1
	HealthDegradedRPC
	HealthBlockedBudget
	HealthAmbiguousRescue
	HealthStopped
)

func (state HealthState) String() string {
	switch state {
	case HealthHealthyIdle:
		return "healthy_idle"
	case HealthDegradedRPC:
		return "degraded_rpc"
	case HealthBlockedBudget:
		return "blocked_budget"
	case HealthAmbiguousRescue:
		return "ambiguous_rescue"
	case HealthStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

type HealthCondition uint8

const (
	ConditionRPCDegraded HealthCondition = iota + 1
	ConditionBudgetBlocked
	ConditionAmbiguousRescue
)

type HealthConditions struct {
	RPCDegraded     bool
	BudgetBlocked   bool
	AmbiguousRescue bool
	Stopped         bool
}

type ChainHealth struct {
	State      HealthState
	Conditions HealthConditions
}

type HealthSnapshot struct {
	State  HealthState
	Chains map[domain.NetworkID]ChainHealth
}

// EvaluateHealth зависит только от текущих conditions, а не от свежести логов.
func EvaluateHealth(conditions HealthConditions) HealthState {
	switch {
	case conditions.Stopped:
		return HealthStopped
	case conditions.AmbiguousRescue:
		return HealthAmbiguousRescue
	case conditions.BudgetBlocked:
		return HealthBlockedBudget
	case conditions.RPCDegraded:
		return HealthDegradedRPC
	default:
		return HealthHealthyIdle
	}
}

// Health хранит conditions только для заранее настроенных chain ID.
type Health struct {
	mu      sync.RWMutex
	stopped bool
	chains  map[domain.NetworkID]HealthConditions
}

func NewHealth(chainIDs []domain.NetworkID) (*Health, error) {
	configured, err := configuredChainSet(chainIDs)
	if err != nil {
		return nil, err
	}
	health := &Health{chains: make(map[domain.NetworkID]HealthConditions, len(configured))}
	for chainID := range configured {
		health.chains[chainID] = HealthConditions{RPCDegraded: true}
	}
	return health, nil
}

func (health *Health) SetCondition(chainID domain.NetworkID, condition HealthCondition, active bool) error {
	if !validHealthCondition(condition) {
		return ErrInvalidHealthCondition
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	conditions, exists := health.chains[chainID]
	if !exists {
		return ErrUnknownChain
	}
	switch condition {
	case ConditionRPCDegraded:
		conditions.RPCDegraded = active
	case ConditionBudgetBlocked:
		conditions.BudgetBlocked = active
	case ConditionAmbiguousRescue:
		conditions.AmbiguousRescue = active
	}
	health.chains[chainID] = conditions
	return nil
}

// Stop необратимо переводит общий health state в stopped.
func (health *Health) Stop() {
	health.mu.Lock()
	health.stopped = true
	health.mu.Unlock()
}

func (health *Health) Snapshot() HealthSnapshot {
	health.mu.RLock()
	defer health.mu.RUnlock()

	snapshot := HealthSnapshot{State: HealthHealthyIdle, Chains: make(map[domain.NetworkID]ChainHealth, len(health.chains))}
	overall := HealthConditions{Stopped: health.stopped}
	for chainID, stored := range health.chains {
		conditions := stored
		conditions.Stopped = health.stopped
		snapshot.Chains[chainID] = ChainHealth{
			State:      EvaluateHealth(conditions),
			Conditions: conditions,
		}
		overall.RPCDegraded = overall.RPCDegraded || stored.RPCDegraded
		overall.BudgetBlocked = overall.BudgetBlocked || stored.BudgetBlocked
		overall.AmbiguousRescue = overall.AmbiguousRescue || stored.AmbiguousRescue
	}
	snapshot.State = EvaluateHealth(overall)
	return snapshot
}

func validHealthCondition(condition HealthCondition) bool {
	return condition >= ConditionRPCDegraded && condition <= ConditionAmbiguousRescue
}
