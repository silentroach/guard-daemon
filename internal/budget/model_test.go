package budget

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

type modelReservation struct {
	state  ReservationState
	amount uint64
	actual uint64
	txHash common.Hash
}

func TestLedgerDeterministicModelSequence(t *testing.T) {
	ctx := context.Background()
	clock := newTestClock()
	policy := uniformPolicy(30, 500, 500, 500)
	path := filepath.Join(t.TempDir(), "budget.db")
	options := testOptions(policy, clock)
	ledger := openTestLedger(t, path, options)
	random := rand.New(rand.NewSource(8))
	model := make(map[ReservationID]modelReservation)
	sequence := uint64(0)

	for step := 0; step < 500; step++ {
		if step > 0 && step%100 == 0 {
			ledger = reopenTestLedger(t, ledger, path, options)
		}
		openIDs := modelIDs(model, ReservationHeld, ReservationExposed)
		action := random.Intn(4)
		if len(openIDs) == 0 {
			action = 0
		}
		switch action {
		case 0:
			sequence++
			maximum := uint64(random.Intn(30) + 1)
			network := policy.Networks[int(sequence)%len(policy.Networks)]
			request := testRequest(network, sequence, maximum)
			reservation, err := ledger.Reserve(ctx, request)
			spent, reserved := modelTotals(model)
			if spent+reserved+maximum > 500 {
				if !errors.Is(err, ErrBudgetExceeded) {
					t.Fatalf("step %d: Reserve returned error %v, want ErrBudgetExceeded", step, err)
				}
				break
			}
			if err != nil {
				t.Fatalf("step %d: Reserve returned an error: %v", step, err)
			}
			model[reservation.ID] = modelReservation{state: ReservationHeld, amount: maximum}
		case 1:
			held := modelIDs(model, ReservationHeld)
			if len(held) == 0 {
				break
			}
			id := held[random.Intn(len(held))]
			hash := testHash(byte(step%250 + 1))
			reservation, err := ledger.MarkExposed(ctx, id, hash)
			if err != nil {
				t.Fatalf("step %d: MarkExposed returned an error: %v", step, err)
			}
			entry := model[id]
			entry.state = reservation.State
			entry.txHash = hash
			model[id] = entry
		case 2:
			held := modelIDs(model, ReservationHeld)
			if len(held) == 0 {
				break
			}
			id := held[random.Intn(len(held))]
			reservation, err := ledger.ReleaseProvenUnused(ctx, id)
			if err != nil {
				t.Fatalf("step %d: ReleaseProvenUnused returned an error: %v", step, err)
			}
			entry := model[id]
			entry.state = reservation.State
			model[id] = entry
		case 3:
			exposed := modelIDs(model, ReservationExposed)
			if len(exposed) == 0 {
				break
			}
			id := exposed[random.Intn(len(exposed))]
			entry := model[id]
			actual := uint64(random.Intn(int(entry.amount)) + 1)
			reservation, err := ledger.CommitFinalized(ctx, FinalizedCharge{ReservationID: id, TxHash: entry.txHash, Actual: amount(actual)})
			if err != nil {
				t.Fatalf("step %d: CommitFinalized returned an error: %v", step, err)
			}
			entry.state = reservation.State
			entry.actual = actual
			model[id] = entry
		}

		spent, reserved := modelTotals(model)
		snapshot, err := ledger.Snapshot(ctx)
		if err != nil {
			t.Fatalf("step %d: Snapshot returned an error: %v", step, err)
		}
		if spent+reserved > 500 || snapshot.Global.Spent.Cumulative.Uint64() != spent ||
			snapshot.Global.Reserved.Cumulative.Uint64() != reserved || snapshot.Global.Remaining.Cumulative.Uint64() != 500-spent-reserved {
			t.Fatalf("step %d: model=%d/%d, snapshot=%+v", step, spent, reserved, snapshot.Global)
		}
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
}

func modelIDs(model map[ReservationID]modelReservation, states ...ReservationState) []ReservationID {
	wanted := make(map[ReservationState]bool, len(states))
	for _, state := range states {
		wanted[state] = true
	}
	result := make([]ReservationID, 0)
	for id, reservation := range model {
		if wanted[reservation.state] {
			result = append(result, id)
		}
	}
	sort.Slice(result, func(i, j int) bool { return bytes.Compare(result[i][:], result[j][:]) < 0 })
	return result
}

func modelTotals(model map[ReservationID]modelReservation) (uint64, uint64) {
	var spent, reserved uint64
	for _, reservation := range model {
		switch reservation.state {
		case ReservationHeld, ReservationExposed:
			reserved += reservation.amount
		case ReservationCommitted:
			spent += reservation.actual
		}
	}
	return spent, reserved
}
