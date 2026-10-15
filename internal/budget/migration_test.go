package budget

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestBaseEstimatedMigrationPreservesAccounting(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock()
	policy := testPolicy()
	policy.Networks[0].Network = 8453
	path := filepath.Join(t.TempDir(), "budget.db")
	ledger := openTestLedger(t, path, testOptions(policy, clock))
	held, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 1, 42))
	if err != nil {
		t.Fatal(err)
	}
	spent, err := ledger.Reserve(ctx, testRequest(policy.Networks[0], 2, 30))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.MarkExposed(ctx, spent.ID, testHash(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.CommitFinalized(ctx, FinalizedCharge{ReservationID: spent.ID, TxHash: testHash(2), Actual: amount(20)}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	policy.Networks[0].AllowFeeOverrun = true
	ledger = openTestLedger(t, path, testOptions(policy, clock))
	assertSnapshot(t, ledger, 20, 42, 938)
	restored, found, err := ledger.ReservationByAttempt(ctx, held.Request.Attempt)
	if err != nil || !found || restored != held {
		t.Fatalf("held reservation changed: %+v, %v", restored, err)
	}
	ledger = reopenTestLedger(t, ledger, path, testOptions(policy, clock))
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	policy.Networks[0].AllowFeeOverrun = false
	if ledger, err := Open(path, testOptions(policy, clock)); !errors.Is(err, ErrPolicyMismatch) {
		if ledger != nil {
			ledger.Close()
		}
		t.Fatalf("downgrade accepted: %v", err)
	}
}

func TestEstimatedMigrationRejectsUnrelatedChanges(t *testing.T) {
	for _, scenario := range []string{"limits", "binding", "other network"} {
		t.Run(scenario, func(t *testing.T) {
			clock := newTestClock()
			policy := testPolicy()
			policy.Networks[0].Network = 8453
			path := filepath.Join(t.TempDir(), "budget.db")
			ledger := openTestLedger(t, path, testOptions(policy, clock))
			if err := ledger.Close(); err != nil {
				t.Fatal(err)
			}
			original := testOptions(policy, clock)
			original.Policy.Networks = append([]NetworkPolicy(nil), policy.Networks...)
			policy.Networks[0].AllowFeeOverrun = true
			options := testOptions(policy, clock)
			switch scenario {
			case "limits":
				options.Policy.Global.PerTransaction = amount(99)
			case "binding":
				options.PolicyFingerprint[0] = 1
			case "other network":
				options.Policy.Networks[1].AllowFeeOverrun = true
			}
			if ledger, err := Open(path, options); !errors.Is(err, ErrPolicyMismatch) {
				if ledger != nil {
					ledger.Close()
				}
				t.Fatalf("incompatible migration accepted: %v", err)
			}
			// The rejected transaction must leave the old policy usable.
			ledger = openTestLedger(t, path, original)
			ledger.Close()
		})
	}
}
