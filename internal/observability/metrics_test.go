package observability

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"guard-daemon/internal/domain"

	"github.com/holiman/uint256"
)

func TestMetricsTrackBoundedChainState(t *testing.T) {
	t.Parallel()

	metrics, err := NewMetrics([]domain.NetworkID{1, 8453})
	if err != nil {
		t.Fatal(err)
	}
	reconciledAt := time.Date(2026, time.August, 3, 12, 30, 0, 0, time.FixedZone("test", 2*60*60))
	operations := []func() error{
		func() error { return metrics.SetQueueDepth(1, 7) },
		func() error { return metrics.RecordCandidate(1) },
		func() error { return metrics.RecordAttempt(1) },
		func() error {
			return metrics.SetBudget(1, *uint256.NewInt(11), *uint256.NewInt(12), *uint256.NewInt(13))
		},
		func() error { return metrics.RecordRPCError(1) },
		func() error { return metrics.RecordReconnect(1) },
		func() error { return metrics.RecordLostRace(1) },
		func() error { return metrics.OpenAmbiguous(1) },
		func() error { return metrics.OpenAmbiguous(1) },
		func() error { return metrics.ResolveAmbiguous(1) },
		func() error { return metrics.SetDelegationState(1, DelegationUnexpected) },
		func() error { return metrics.RecordSuccessfulReconciliation(1, reconciledAt) },
	}
	for _, operation := range operations {
		if err := operation(); err != nil {
			t.Fatal(err)
		}
	}

	want := ChainMetrics{
		QueueDepth:                   7,
		Candidates:                   1,
		Attempts:                     1,
		SpentBudget:                  *uint256.NewInt(11),
		ReservedBudget:               *uint256.NewInt(12),
		RemainingBudget:              *uint256.NewInt(13),
		RPCErrors:                    1,
		Reconnects:                   1,
		LostRaces:                    1,
		ActiveAmbiguous:              1,
		TotalAmbiguous:               2,
		Delegation:                   DelegationUnexpected,
		LastSuccessfulReconciliation: reconciledAt.UTC(),
	}
	got := metrics.Snapshot()
	if len(got.Chains) != 2 || !reflect.DeepEqual(got.Chains[1], want) {
		t.Fatalf("snapshot = %#v, want chain 1 %#v and two configured chains", got, want)
	}
	if got.Chains[8453].Delegation != DelegationUnknown {
		t.Fatalf("initial delegation = %v, want DelegationUnknown", got.Chains[8453].Delegation)
	}
}

func TestMetricsSnapshotsAreDeepCopies(t *testing.T) {
	t.Parallel()

	metrics, err := NewMetrics([]domain.NetworkID{1})
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.SetBudget(1, *uint256.NewInt(10), *uint256.NewInt(20), *uint256.NewInt(30)); err != nil {
		t.Fatal(err)
	}

	first := metrics.Snapshot()
	mutated := first.Chains[1]
	mutated.SpentBudget.SetUint64(999)
	first.Chains[1] = mutated
	delete(first.Chains, 1)
	second := metrics.Snapshot()
	stored := second.Chains[1]
	if len(second.Chains) != 1 || stored.SpentBudget.Uint64() != 10 {
		t.Fatalf("mutation leaked into Metrics: %#v", second)
	}
}

func TestMetricsConcurrentCountersAreExact(t *testing.T) {
	t.Parallel()

	const (
		workers    = 32
		increments = 100
	)
	metrics, err := NewMetrics([]domain.NetworkID{1})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := 0; index < increments; index++ {
				if err := metrics.RecordCandidate(1); err != nil {
					results <- err
					return
				}
				if err := metrics.RecordAttempt(1); err != nil {
					results <- err
					return
				}
			}
			results <- nil
		}()
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}

	chain := metrics.Snapshot().Chains[1]
	want := uint64(workers * increments)
	if chain.Candidates != want || chain.Attempts != want {
		t.Fatalf("concurrent counters = candidates %d, attempts %d; want %d", chain.Candidates, chain.Attempts, want)
	}
}

func TestMetricsRejectUnconfiguredCardinalityAndInvalidTransitions(t *testing.T) {
	t.Parallel()

	metrics, err := NewMetrics([]domain.NetworkID{1})
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.RecordCandidate(2); !errors.Is(err, ErrUnknownChain) {
		t.Fatalf("unknown chain error = %v, want ErrUnknownChain", err)
	}
	if err := metrics.ResolveAmbiguous(1); !errors.Is(err, ErrAmbiguousUnderflow) {
		t.Fatalf("resolve error = %v, want ErrAmbiguousUnderflow", err)
	}
	if err := metrics.SetDelegationState(1, DelegationState(255)); !errors.Is(err, ErrInvalidDelegation) {
		t.Fatalf("delegation error = %v, want ErrInvalidDelegation", err)
	}
	if len(metrics.Snapshot().Chains) != 1 {
		t.Fatal("unknown chain увеличил cardinality")
	}

	tooMany := make([]domain.NetworkID, MaxConfiguredChains+1)
	for index := range tooMany {
		tooMany[index] = domain.NetworkID(index + 1)
	}
	if _, err := NewMetrics(tooMany); !errors.Is(err, ErrInvalidChains) {
		t.Fatalf("NewMetrics error = %v, want ErrInvalidChains", err)
	}
}
