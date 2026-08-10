package budget

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	bolt "go.etcd.io/bbolt"
)

func TestCostQuoteMaximumRejectsOverflow(t *testing.T) {
	t.Parallel()
	maximum := uint256.Int{^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)}

	tests := []struct {
		name     string
		quote    CostQuote
		overhead uint256.Int
		want     uint64
		wantErr  error
	}{
		{name: "checked result", quote: CostQuote{GasLimit: 21, MaxFeePerGas: amount(2)}, overhead: amount(3), want: 45},
		{name: "multiplication overflow", quote: CostQuote{GasLimit: 2, MaxFeePerGas: maximum}, wantErr: ErrArithmeticOverflow},
		{name: "addition overflow", quote: CostQuote{GasLimit: 1, MaxFeePerGas: maximum}, overhead: amount(1), wantErr: ErrArithmeticOverflow},
		{name: "zero gas", quote: CostQuote{MaxFeePerGas: amount(1)}, wantErr: ErrInvalidRequest},
		{name: "zero fee", quote: CostQuote{GasLimit: 1}, wantErr: ErrInvalidRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.quote.Maximum(test.overhead)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Maximum returned error %v, want %v", err, test.wantErr)
			}
			if err == nil && got.Uint64() != test.want {
				t.Fatalf("Maximum returned %d, want %d", got.Uint64(), test.want)
			}
		})
	}
}

func TestLedgerLifecycleIdempotencyAndSnapshots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newTestClock()
	policy := testPolicy()
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "budget.db"), testOptions(policy, clock))
	defer ledger.Close()

	request := testRequest(policy.Networks[0], 1, 42)
	reservation, err := ledger.Reserve(ctx, request)
	if err != nil {
		t.Fatalf("Reserve returned an error: %v", err)
	}
	if reservation.ID != NewReservationID(request.Attempt) || reservation.State != ReservationHeld || reservation.Maximum.Uint64() != 42 {
		t.Fatalf("held reservation = %+v", reservation)
	}

	replayedRequest := request
	replayedRequest.SponsorBalance = amount(999)
	replayed, err := ledger.Reserve(ctx, replayedRequest)
	if err != nil || replayed != reservation {
		t.Fatalf("idempotent Reserve returned (%+v, %v), want original reservation", replayed, err)
	}
	conflict := request
	conflict.Quote.MaxFeePerGas = amount(43 - policy.Networks[0].TransactionOverhead.Uint64())
	if _, err := ledger.Reserve(ctx, conflict); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("conflicting Reserve error = %v, want ErrStateConflict", err)
	}

	stored, found, err := ledger.ReservationByAttempt(ctx, request.Attempt)
	if err != nil || !found || stored != reservation {
		t.Fatalf("ReservationByAttempt returned (%+v, %t, %v)", stored, found, err)
	}
	assertSnapshot(t, ledger, 0, 42, 958)

	txHash := testHash(1)
	exposed, err := ledger.MarkExposed(ctx, reservation.ID, txHash)
	if err != nil || exposed.State != ReservationExposed || exposed.TxHash != txHash || exposed.ExposedAt.IsZero() {
		t.Fatalf("MarkExposed returned (%+v, %v)", exposed, err)
	}
	if duplicate, err := ledger.MarkExposed(ctx, reservation.ID, txHash); err != nil || duplicate != exposed {
		t.Fatalf("duplicate MarkExposed returned (%+v, %v)", duplicate, err)
	}
	if _, err := ledger.MarkExposed(ctx, reservation.ID, testHash(2)); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("conflicting MarkExposed error: %v", err)
	}
	if _, err := ledger.ReleaseProvenUnused(ctx, reservation.ID); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("exposed reservation release error: %v", err)
	}

	charge := FinalizedCharge{ReservationID: reservation.ID, TxHash: txHash, Actual: amount(30)}
	committed, err := ledger.CommitFinalized(ctx, charge)
	if err != nil || committed.State != ReservationCommitted || committed.Actual.Uint64() != 30 {
		t.Fatalf("CommitFinalized returned (%+v, %v)", committed, err)
	}
	if duplicate, err := ledger.CommitFinalized(ctx, charge); err != nil || duplicate != committed {
		t.Fatalf("duplicate CommitFinalized returned (%+v, %v)", duplicate, err)
	}
	tooLarge := charge
	tooLarge.Actual = amount(43)
	if _, err := ledger.CommitFinalized(ctx, tooLarge); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("conflicting CommitFinalized error: %v", err)
	}
	if _, err := ledger.ReleaseProvenUnused(ctx, reservation.ID); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("committed reservation release error: %v", err)
	}
	assertSnapshot(t, ledger, 30, 0, 970)

	held, err := ledger.Reserve(ctx, testRequest(policy.Networks[1], 2, 25))
	if err != nil {
		t.Fatal(err)
	}
	released, err := ledger.ReleaseProvenUnused(ctx, held.ID)
	if err != nil || released.State != ReservationReleased {
		t.Fatalf("ReleaseProvenUnused returned (%+v, %v)", released, err)
	}
	if duplicate, err := ledger.ReleaseProvenUnused(ctx, held.ID); err != nil || duplicate != released {
		t.Fatalf("duplicate ReleaseProvenUnused returned (%+v, %v)", duplicate, err)
	}
	open, err := ledger.OpenReservations(ctx)
	if err != nil || len(open) != 0 {
		t.Fatalf("OpenReservations returned (%+v, %v), want empty result", open, err)
	}
	reopened, err := ledger.Reserve(ctx, testRequest(policy.Networks[1], 2, 25))
	if err != nil || reopened.ID != held.ID || reopened.State != ReservationHeld {
		t.Fatalf("Reserve after a crash past the release boundary returned (%+v, %v)", reopened, err)
	}
	if _, err := ledger.MarkExposed(ctx, reopened.ID, testHash(3)); err != nil {
		t.Fatalf("MarkExposed after reopening returned an error: %v", err)
	}
}

func TestLedgerEnforcesSponsorAndRollingLimits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("per transaction", func(t *testing.T) {
		clock := newTestClock()
		policy := testPolicy()
		ledger := openTestLedger(t, filepath.Join(t.TempDir(), "budget.db"), testOptions(policy, clock))
		defer ledger.Close()
		if _, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 1, 81)); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("network per-transaction limit error: %v", err)
		}
	})

	t.Run("sponsor emergency reserve", func(t *testing.T) {
		clock := newTestClock()
		policy := testPolicy()
		ledger := openTestLedger(t, filepath.Join(t.TempDir(), "budget.db"), testOptions(policy, clock))
		defer ledger.Close()
		firstRequest := testRequest(policy.Networks[0], 1, 70)
		firstRequest.SponsorBalance = amount(100)
		first, err := ledger.Reserve(ctx, firstRequest)
		if err != nil {
			t.Fatalf("Reserve preserving the exact reserve returned an error: %v", err)
		}
		secondRequest := testRequest(policy.Networks[0], 2, 11)
		secondRequest.SponsorBalance = amount(100)
		if _, err := ledger.Reserve(ctx, secondRequest); !errors.Is(err, ErrSponsorReserve) {
			t.Fatalf("second Reserve returned error %v, want ErrSponsorReserve", err)
		}
		if _, err := ledger.ReleaseProvenUnused(ctx, first.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.Reserve(ctx, secondRequest); err != nil {
			t.Fatalf("Reserve after proven release returned an error: %v", err)
		}
	})

	t.Run("rolling hour and cumulative", func(t *testing.T) {
		clock := newTestClock()
		policy := uniformPolicy(60, 100, 150, 200)
		ledger := openTestLedger(t, filepath.Join(t.TempDir(), "budget.db"), testOptions(policy, clock))
		defer ledger.Close()

		first := reserveExposeCommit(t, ledger, testRequest(policy.Networks[0], 1, 60), 60, testHash(10))
		if first.State != ReservationCommitted {
			t.Fatal("first reservation was not committed")
		}
		held, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 2, 40))
		if err != nil {
			t.Fatalf("Reserve up to the exact hourly limit returned an error: %v", err)
		}
		if _, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 3, 1)); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("hourly limit excess error: %v", err)
		}
		if _, err := ledger.ReleaseProvenUnused(ctx, held.ID); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Hour + time.Nanosecond)
		reserveExposeCommit(t, ledger, testRequest(policy.Networks[0], 4, 60), 60, testHash(11))
		if _, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 5, 31)); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("daily limit excess error: %v", err)
		}
		if _, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 6, 30)); err != nil {
			t.Fatalf("Reserve up to the exact daily limit returned an error: %v", err)
		}
	})
}

func TestLedgerPersistentRecordCapacityReusesReleasedSlots(t *testing.T) {
	clock := newTestClock()
	policy := testPolicy()
	options := testOptions(policy, clock)
	options.MaxRecords = 2
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "budget.db"), options)
	defer ledger.Close()
	for attempt := uint32(1); attempt <= 2; attempt++ {
		reservation, err := ledger.Reserve(context.Background(), testRequest(policy.Networks[0], uint64(attempt), 5))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.ReleaseProvenUnused(context.Background(), reservation.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ledger.Reserve(context.Background(), testRequest(policy.Networks[0], 3, 5)); err != nil {
		t.Fatalf("Reserve after compacting a released reservation returned an error: %v", err)
	}
}

func TestLedgerPersistentCapacityNeverPurgesOpenReservation(t *testing.T) {
	clock := newTestClock()
	policy := testPolicy()
	options := testOptions(policy, clock)
	options.MaxRecords = 1
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "budget.db"), options)
	defer ledger.Close()
	if _, err := ledger.Reserve(context.Background(), testRequest(policy.Networks[0], 1, 5)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(context.Background(), testRequest(policy.Networks[0], 2, 5)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Reserve beyond available capacity returned an error: %v", err)
	}
}

func TestForeignNetworkTimestampCannotAdvanceBudgetWindow(t *testing.T) {
	clock := newTestClock()
	policy := uniformPolicy(100, 100, 200, 300)
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "budget.db"), testOptions(policy, clock))
	defer ledger.Close()
	firstRequest := testRequest(policy.Networks[0], 1, 60)
	firstRequest.ObservedAt = clock.Now()
	first, err := ledger.Reserve(context.Background(), firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.MarkExposed(context.Background(), first.ID, testHash(30)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.CommitFinalized(context.Background(), FinalizedCharge{
		ReservationID: first.ID, TxHash: testHash(30), Actual: amount(60), ObservedAt: clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	foreign := testRequest(policy.Networks[1], 2, 1)
	foreign.ObservedAt = clock.Now().Add(maximumObservedSkew + time.Second)
	if _, err := ledger.Reserve(context.Background(), foreign); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("foreign timestamp from the future: %v", err)
	}
	second := testRequest(policy.Networks[0], 3, 50)
	second.ObservedAt = clock.Now()
	if _, err := ledger.Reserve(context.Background(), second); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("foreign timestamp advanced the hourly budget: %v", err)
	}
}

func TestScopeSnapshotBlockedIncludesRollingWindows(t *testing.T) {
	available := amount(1)
	for _, snapshot := range []ScopeSnapshot{
		{Remaining: Totals{PerHour: amount(0), PerDay: available, Cumulative: available}},
		{Remaining: Totals{PerHour: available, PerDay: amount(0), Cumulative: available}},
		{Remaining: Totals{PerHour: available, PerDay: available, Cumulative: amount(0)}},
	} {
		if !snapshot.Blocked() {
			t.Fatalf("exhausted scope was not blocked: %+v", snapshot)
		}
	}
	if (ScopeSnapshot{Remaining: Totals{PerHour: available, PerDay: available, Cumulative: available}}).Blocked() {
		t.Fatal("non-empty scope was blocked")
	}
}

func TestSnapshotAdvancesAndExpiresIdleRollingWindow(t *testing.T) {
	clock := newTestClock()
	policy := uniformPolicy(100, 100, 200, 300)
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "budget.db"), testOptions(policy, clock))
	defer ledger.Close()
	reservation, err := ledger.Reserve(context.Background(), testRequest(policy.Networks[0], 1, 100))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.MarkExposed(context.Background(), reservation.ID, testHash(31)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.CommitFinalized(context.Background(), FinalizedCharge{
		ReservationID: reservation.ID, TxHash: testHash(31), Actual: amount(100), ObservedAt: clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	before, err := ledger.Snapshot(context.Background())
	if err != nil || !before.Global.Blocked() {
		t.Fatalf("initial snapshot = %+v, %v", before.Global, err)
	}
	clock.Advance(time.Hour + time.Nanosecond)
	after, err := ledger.Snapshot(context.Background())
	if err != nil || after.Global.Blocked() || after.Global.Spent.PerHour.Uint64() != 0 {
		t.Fatalf("snapshot after expiration = %+v, %v", after.Global, err)
	}
}

func TestSnapshotDoesNotPersistForwardHostClockJump(t *testing.T) {
	clock := newTestClock()
	original := clock.Now()
	policy := uniformPolicy(100, 100, 200, 300)
	path := filepath.Join(t.TempDir(), "budget.db")
	options := testOptions(policy, clock)
	ledger := openTestLedger(t, path, options)
	reservation, err := ledger.Reserve(context.Background(), testRequest(policy.Networks[0], 1, 100))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.MarkExposed(context.Background(), reservation.ID, testHash(32)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.CommitFinalized(context.Background(), FinalizedCharge{
		ReservationID: reservation.ID, TxHash: testHash(32), Actual: amount(100), ObservedAt: original,
	}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(48 * time.Hour)
	if snapshot, err := ledger.Snapshot(context.Background()); err != nil || snapshot.Global.Blocked() {
		t.Fatalf("diagnostic snapshot after clock jump = %+v, %v", snapshot.Global, err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Set(original.Add(30 * time.Minute))
	reopened := openTestLedger(t, path, options)
	defer reopened.Close()
	if snapshot, err := reopened.Snapshot(context.Background()); err != nil || !snapshot.Global.Blocked() {
		t.Fatalf("snapshot after restart and clock correction = %+v, %v", snapshot.Global, err)
	}
}

func TestTrustedObservedTimeIgnoresHostClockForwardJump(t *testing.T) {
	clock := newTestClock()
	policy := uniformPolicy(100, 100, 200, 300)
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "budget.db"), testOptions(policy, clock))
	defer ledger.Close()
	chainTime := clock.Now()
	firstRequest := testRequest(policy.Networks[0], 1, 60)
	firstRequest.ObservedAt = chainTime
	first, err := ledger.Reserve(context.Background(), firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.MarkExposed(context.Background(), first.ID, testHash(20)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.CommitFinalized(context.Background(), FinalizedCharge{
		ReservationID: first.ID, TxHash: testHash(20), Actual: amount(60), ObservedAt: chainTime,
	}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(48 * time.Hour)
	secondRequest := testRequest(policy.Networks[0], 2, 50)
	secondRequest.ObservedAt = chainTime.Add(30 * time.Minute)
	if _, err := ledger.Reserve(context.Background(), secondRequest); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("clock skew was not rejected: %v", err)
	}
}

func TestLedgerConcurrentNetworksCannotExceedGlobalBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newTestClock()
	policy := uniformPolicy(10, 100, 100, 100)
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "budget.db"), testOptions(policy, clock))
	defer ledger.Close()

	const workers = 64
	results := make(chan error, workers)
	var group sync.WaitGroup
	for index := 1; index <= workers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			network := policy.Networks[index%len(policy.Networks)]
			_, err := ledger.Reserve(ctx, testRequest(network, uint64(index), 10))
			results <- err
		}(index)
	}
	group.Wait()
	close(results)

	succeeded := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrBudgetExceeded):
		default:
			t.Fatalf("unexpected concurrent Reserve error: %v", err)
		}
	}
	if succeeded != 10 {
		t.Fatalf("successful reservations = %d, want 10", succeeded)
	}
	snapshot, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Global.Reserved.Cumulative.Uint64() != 100 || snapshot.Global.Remaining.Cumulative.Uint64() != 0 {
		t.Fatalf("global concurrent snapshot = %+v", snapshot.Global)
	}
	open, err := ledger.OpenReservations(ctx)
	if err != nil || len(open) != 10 {
		t.Fatalf("OpenReservations count = %d, error %v", len(open), err)
	}
}

func TestLedgerAppliesGlobalAndNetworkScopesIndependently(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newTestClock()
	global := Limits{PerTransaction: amount(100), PerHour: amount(200), PerDay: amount(200), Cumulative: amount(200)}
	policy := Policy{
		Global: global,
		Networks: []NetworkPolicy{
			{
				Network: 1, Sponsor: testAddress(1),
				Limits:                  Limits{PerTransaction: amount(60), PerHour: amount(60), PerDay: amount(60), Cumulative: amount(60)},
				EmergencySponsorReserve: amount(10),
			},
			{
				Network: 2, Sponsor: testAddress(2),
				Limits:                  Limits{PerTransaction: amount(120), PerHour: amount(200), PerDay: amount(200), Cumulative: amount(200)},
				EmergencySponsorReserve: amount(10),
			},
		},
	}
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "budget.db"), testOptions(policy, clock))
	defer ledger.Close()

	if _, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 1, 60)); err != nil {
		t.Fatalf("exact cumulative limit for first network: %v", err)
	}
	if _, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 2, 1)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("network cumulative limit excess error: %v", err)
	}
	if _, err := ledger.Reserve(ctx, testRequest(policy.Networks[1], 3, 101)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("global per-transaction limit excess error: %v", err)
	}
	if _, err := ledger.Reserve(ctx, testRequest(policy.Networks[1], 4, 100)); err != nil {
		t.Fatalf("second network reservation: %v", err)
	}
	snapshot, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	networkOne := snapshot.Networks[1]
	networkTwo := snapshot.Networks[2]
	if snapshot.Global.Reserved.Cumulative.Uint64() != 160 || networkOne.Reserved.Cumulative.Uint64() != 60 ||
		networkTwo.Reserved.Cumulative.Uint64() != 100 {
		t.Fatalf("independent scope snapshot = %+v", snapshot)
	}
}

func TestLedgerPersistsCrashBoundariesAndConservativeClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newTestClock()
	policy := uniformPolicy(80, 100, 300, 500)
	path := filepath.Join(t.TempDir(), "budget.db")
	options := testOptions(policy, clock)
	ledger := openTestLedger(t, path, options)

	held, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 1, 40))
	if err != nil {
		t.Fatal(err)
	}
	exposed, err := ledger.Reserve(ctx, testRequest(policy.Networks[1], 2, 50))
	if err != nil {
		t.Fatal(err)
	}
	exposed, err = ledger.MarkExposed(ctx, exposed.ID, testHash(20))
	if err != nil {
		t.Fatal(err)
	}
	ledger = reopenTestLedger(t, ledger, path, options)

	open, err := ledger.OpenReservations(ctx)
	if err != nil || len(open) != 2 || open[0].State == ReservationCommitted || open[1].State == ReservationCommitted {
		t.Fatalf("open reservations after restart = (%+v, %v)", open, err)
	}
	if _, err := ledger.ReleaseProvenUnused(ctx, held.ID); err != nil {
		t.Fatalf("releasing reservation after pre-signing crash: %v", err)
	}
	if _, err := ledger.ReleaseProvenUnused(ctx, exposed.ID); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("reservation release error after post-signing crash: %v", err)
	}
	if _, err := ledger.CommitFinalized(ctx, FinalizedCharge{ReservationID: exposed.ID, TxHash: exposed.TxHash, Actual: amount(50)}); err != nil {
		t.Fatalf("reconciling exposed reservation: %v", err)
	}

	clock.Set(clock.Now().Add(-2 * time.Hour))
	snapshot, err := ledger.Snapshot(ctx)
	if err != nil || !snapshot.At.Equal(exposed.ExposedAt) {
		t.Fatalf("snapshot time after rollback = (%s, %v), want %s", snapshot.At, err, exposed.ExposedAt)
	}
	if _, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 3, 51)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("clock rollback reset the hourly budget: %v", err)
	}
	ledger = reopenTestLedger(t, ledger, path, options)
	snapshot, err = ledger.Snapshot(ctx)
	if err != nil || !snapshot.At.Equal(exposed.ExposedAt) {
		t.Fatalf("persisted rollback time = (%s, %v), want %s", snapshot.At, err, exposed.ExposedAt)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerBindingPermissionsCorruptionAndClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := newTestClock()
	policy := testPolicy()
	root := t.TempDir()
	path := filepath.Join(root, "private", "budget.db")
	options := testOptions(policy, clock)
	ledger := openTestLedger(t, path, options)

	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil || directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("ledger directory permissions = (%v, %v), want 0700", directoryInfo, err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("ledger file permissions = (%v, %v), want 0600", fileInfo, err)
	}

	started := time.Now()
	_, lockErr := Open(path, options)
	if !errors.Is(lockErr, ErrOpenFailed) || time.Since(started) > 2*time.Second || strings.Contains(lockErr.Error(), path) {
		t.Fatalf("second Open returned %v after %s", lockErr, time.Since(started))
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	fingerprintMismatch := options
	fingerprintMismatch.PolicyFingerprint[0]++
	if opened, err := Open(path, fingerprintMismatch); !errors.Is(err, ErrPolicyMismatch) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("fingerprint mismatch error: %v", err)
	}
	policyMismatch := options
	policyMismatch.Policy.Global.Cumulative = amount(1001)
	if opened, err := Open(path, policyMismatch); !errors.Is(err, ErrPolicyMismatch) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("policy mismatch error: %v", err)
	}

	ledger = openTestLedger(t, path, options)
	reservation, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 1, 10))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	mutateLedger(t, path, func(tx *bolt.Tx) error {
		return tx.Bucket(ledgerReservationsBucket).Put(reservation.ID[:], []byte{reservationRecordVersion})
	})
	if opened, err := Open(path, options); !errors.Is(err, ErrCorrupt) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("corrupt reservation reopen error: %v", err)
	} else if strings.Contains(err.Error(), path) {
		t.Fatalf("corruption error exposed path: %v", err)
	}

	closedPath := filepath.Join(root, "closed", "budget.db")
	closed := openTestLedger(t, closedPath, options)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatalf("idempotent Close returned an error: %v", err)
	}
	if _, err := closed.Snapshot(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("operation error after Close: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	reopened := openTestLedger(t, closedPath, options)
	defer reopened.Close()
	if _, err := reopened.Snapshot(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Snapshot error: %v", err)
	}
}

func TestLedgerRejectsMissingIndexAndExistingEmptyFile(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	policy := testPolicy()
	options := testOptions(policy, clock)

	emptyPath := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(emptyPath, options); !errors.Is(err, ErrCorrupt) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("existing empty file error: %v", err)
	}

	path := filepath.Join(t.TempDir(), "budget.db")
	ledger := openTestLedger(t, path, options)
	reservation, err := ledger.Reserve(context.Background(), testRequest(policy.Networks[0], 1, 10))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	mutateLedger(t, path, func(tx *bolt.Tx) error {
		return tx.Bucket(ledgerAttemptsBucket).Delete(attemptKey(reservation.Request.Attempt))
	})
	if opened, err := Open(path, options); !errors.Is(err, ErrCorrupt) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("missing attempt index error: %v", err)
	}
}

func reserveExposeCommit(t *testing.T, ledger *BudgetLedger, request ReservationRequest, actual uint64, hash common.Hash) Reservation {
	t.Helper()
	reservation, err := ledger.Reserve(context.Background(), request)
	if err != nil {
		t.Fatalf("Reserve returned an error: %v", err)
	}
	reservation, err = ledger.MarkExposed(context.Background(), reservation.ID, hash)
	if err != nil {
		t.Fatalf("MarkExposed returned an error: %v", err)
	}
	reservation, err = ledger.CommitFinalized(context.Background(), FinalizedCharge{ReservationID: reservation.ID, TxHash: hash, Actual: amount(actual)})
	if err != nil {
		t.Fatalf("CommitFinalized returned an error: %v", err)
	}
	return reservation
}

func assertSnapshot(t *testing.T, ledger *BudgetLedger, spent, reserved, remaining uint64) {
	t.Helper()
	snapshot, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot returned an error: %v", err)
	}
	if snapshot.Global.Spent.Cumulative.Uint64() != spent || snapshot.Global.Reserved.Cumulative.Uint64() != reserved ||
		snapshot.Global.Remaining.Cumulative.Uint64() != remaining {
		t.Fatalf("global snapshot = %+v, want spent/reserved/remaining %d/%d/%d", snapshot.Global, spent, reserved, remaining)
	}
}

func openTestLedger(t *testing.T, path string, options OpenOptions) *BudgetLedger {
	t.Helper()
	ledger, err := Open(path, options)
	if err != nil {
		t.Fatalf("Open returned an error: %v", err)
	}
	return ledger
}

func reopenTestLedger(t *testing.T, ledger *BudgetLedger, path string, options OpenOptions) *BudgetLedger {
	t.Helper()
	if err := ledger.Close(); err != nil {
		t.Fatalf("Close before reopening returned an error: %v", err)
	}
	return openTestLedger(t, path, options)
}

func mutateLedger(t *testing.T, path string, mutate func(*bolt.Tx) error) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	mutateErr := db.Update(mutate)
	closeErr := db.Close()
	if mutateErr != nil {
		t.Fatal(mutateErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func testOptions(policy Policy, clock *testClock) OpenOptions {
	var fingerprint [32]byte
	fingerprint[len(fingerprint)-1] = 1
	return OpenOptions{Policy: policy, PolicyFingerprint: fingerprint, Now: clock.Now}
}

func testPolicy() Policy {
	return Policy{
		Global: Limits{PerTransaction: amount(100), PerHour: amount(300), PerDay: amount(500), Cumulative: amount(1000)},
		Networks: []NetworkPolicy{
			{
				Network: 1, Sponsor: testAddress(1),
				Limits:              Limits{PerTransaction: amount(80), PerHour: amount(200), PerDay: amount(400), Cumulative: amount(800)},
				TransactionOverhead: amount(2), EmergencySponsorReserve: amount(20),
			},
			{
				Network: 2, Sponsor: testAddress(2),
				Limits:              Limits{PerTransaction: amount(90), PerHour: amount(250), PerDay: amount(450), Cumulative: amount(900)},
				TransactionOverhead: amount(3), EmergencySponsorReserve: amount(30),
			},
		},
	}
}

func uniformPolicy(perTransaction, perHour, perDay, cumulative uint64) Policy {
	limits := Limits{
		PerTransaction: amount(perTransaction), PerHour: amount(perHour),
		PerDay: amount(perDay), Cumulative: amount(cumulative),
	}
	return Policy{
		Global: limits,
		Networks: []NetworkPolicy{
			{Network: 1, Sponsor: testAddress(1), Limits: limits, EmergencySponsorReserve: amount(10)},
			{Network: 2, Sponsor: testAddress(2), Limits: limits, EmergencySponsorReserve: amount(10)},
		},
	}
}

func testRequest(network NetworkPolicy, sequence, maximum uint64) ReservationRequest {
	var candidate domain.CandidateID
	binary.BigEndian.PutUint64(candidate[len(candidate)-8:], sequence)
	incident := domain.NewIncidentID(candidate)
	overhead := network.TransactionOverhead.Uint64()
	return ReservationRequest{
		Network: network.Network, Sponsor: network.Sponsor, Candidate: candidate,
		Attempt:        Attempt{Incident: incident, Number: 1},
		Quote:          CostQuote{GasLimit: 1, MaxFeePerGas: amount(maximum - overhead)},
		SponsorBalance: amount(10_000),
	}
}

func amount(value uint64) uint256.Int {
	var result uint256.Int
	result.SetUint64(value)
	return result
}

func testAddress(value byte) common.Address {
	var address common.Address
	address[len(address)-1] = value
	return address
}

func testHash(value byte) common.Hash {
	var hash common.Hash
	hash[len(hash)-1] = value
	return hash
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
}

func (clock *testClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *testClock) Set(value time.Time) {
	clock.mu.Lock()
	clock.now = value
	clock.mu.Unlock()
}

func (clock *testClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}
