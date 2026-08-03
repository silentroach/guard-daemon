package observability

import (
	"errors"
	"testing"

	"guard-daemon/internal/domain"
)

func TestEvaluateHealthUsesDeterministicPriority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		conditions HealthConditions
		want       HealthState
	}{
		{name: "healthy idle", want: HealthHealthyIdle},
		{name: "degraded rpc", conditions: HealthConditions{RPCDegraded: true}, want: HealthDegradedRPC},
		{name: "blocked budget", conditions: HealthConditions{RPCDegraded: true, BudgetBlocked: true}, want: HealthBlockedBudget},
		{name: "ambiguous rescue", conditions: HealthConditions{RPCDegraded: true, BudgetBlocked: true, AmbiguousRescue: true}, want: HealthAmbiguousRescue},
		{name: "stopped", conditions: HealthConditions{RPCDegraded: true, BudgetBlocked: true, AmbiguousRescue: true, Stopped: true}, want: HealthStopped},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := EvaluateHealth(test.conditions); got != test.want {
				t.Fatalf("EvaluateHealth() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestHealthAggregatesConfiguredChainsFromConditions(t *testing.T) {
	t.Parallel()

	health, err := NewHealth([]domain.NetworkID{1, 8453})
	if err != nil {
		t.Fatal(err)
	}
	if err := health.SetCondition(1, ConditionRPCDegraded, true); err != nil {
		t.Fatal(err)
	}
	if err := health.SetCondition(8453, ConditionBudgetBlocked, true); err != nil {
		t.Fatal(err)
	}
	snapshot := health.Snapshot()
	if snapshot.State != HealthBlockedBudget || snapshot.Chains[1].State != HealthDegradedRPC || snapshot.Chains[8453].State != HealthBlockedBudget {
		t.Fatalf("health snapshot = %#v", snapshot)
	}

	delete(snapshot.Chains, 1)
	if len(health.Snapshot().Chains) != 2 {
		t.Fatal("mutation snapshot изменила configured health cardinality")
	}
	health.Stop()
	stopped := health.Snapshot()
	if stopped.State != HealthStopped || stopped.Chains[1].State != HealthStopped || !stopped.Chains[8453].Conditions.Stopped {
		t.Fatalf("stopped snapshot = %#v", stopped)
	}
}

func TestHealthStartsDegradedUntilRPCInitialization(t *testing.T) {
	health, err := NewHealth([]domain.NetworkID{1})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := health.Snapshot(); snapshot.State != HealthDegradedRPC || !snapshot.Chains[1].Conditions.RPCDegraded {
		t.Fatalf("initial health = %#v", snapshot)
	}
}

func TestHealthRejectsUnconfiguredAndUnknownConditions(t *testing.T) {
	t.Parallel()

	health, err := NewHealth([]domain.NetworkID{1})
	if err != nil {
		t.Fatal(err)
	}
	if err := health.SetCondition(2, ConditionRPCDegraded, true); !errors.Is(err, ErrUnknownChain) {
		t.Fatalf("unknown chain error = %v, want ErrUnknownChain", err)
	}
	if err := health.SetCondition(1, HealthCondition(255), true); !errors.Is(err, ErrInvalidHealthCondition) {
		t.Fatalf("unknown condition error = %v, want ErrInvalidHealthCondition", err)
	}
	if len(health.Snapshot().Chains) != 1 {
		t.Fatal("invalid conditions увеличили health cardinality")
	}
}

func TestHealthStateStringsAreStable(t *testing.T) {
	t.Parallel()

	states := []HealthState{
		HealthHealthyIdle,
		HealthDegradedRPC,
		HealthBlockedBudget,
		HealthAmbiguousRescue,
		HealthStopped,
	}
	want := []string{"healthy_idle", "degraded_rpc", "blocked_budget", "ambiguous_rescue", "stopped"}
	for index, state := range states {
		if got := state.String(); got != want[index] {
			t.Fatalf("state %d String() = %q, want %q", state, got, want[index])
		}
	}
}
