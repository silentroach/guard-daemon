package budget

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestEstimatedFeeOverrunIsAccountedAndBlocksAfterReopen(t *testing.T) {
	ctx := context.Background()
	policy := testPolicy()
	policy.Networks[0].AllowFeeOverrun = true
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	options := testOptions(policy, clock)
	path := filepath.Join(t.TempDir(), "budget.db")
	ledger := openTestLedger(t, path, options)
	pending, err := ledger.Reserve(ctx, testRequest(policy.Networks[1], 2, 50))
	if err != nil {
		t.Fatal(err)
	}
	// The actual charge exceeds even the cumulative budget: it must be recorded,
	// rather than leave the old estimate exposed forever or treat the DB as corrupt.
	reserveExposeCommit(t, ledger, testRequest(policy.Networks[0], 1, 40), 2000, testHash(1))
	ledger = reopenTestLedger(t, ledger, path, options)
	defer ledger.Close()
	assertSnapshot(t, ledger, 2000, 50, 0)
	if _, err := ledger.Reserve(ctx, testRequest(policy.Networks[1], 3, 10)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("reservation after overrun: %v", err)
	}
	if err := ledger.CheckSponsorCapacity(ctx, pending.ID, amount(10_000)); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("already held reservation remained sendable: %v", err)
	}
}

func TestBoundedFeePolicyRejectsOverrun(t *testing.T) {
	ctx := context.Background()
	policy := testPolicy()
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "budget.db"), testOptions(policy, &testClock{now: time.Now()}))
	defer ledger.Close()
	reservation, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 1, 40))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.MarkExposed(ctx, reservation.ID, testHash(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.CommitFinalized(ctx, FinalizedCharge{ReservationID: reservation.ID, TxHash: testHash(1), Actual: amount(41)}); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("bounded policy accepted overrun: %v", err)
	}
}
