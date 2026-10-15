package rescue

import (
	"context"
	"fmt"
	"testing"
	"time"

	"guard-daemon/internal/domain"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

func TestClaimUnknownTokenHonorsPolicyAndCap(t *testing.T) {
	for _, scenario := range []string{"all", "allowlist", "over cap"} {
		t.Run(scenario, func(t *testing.T) {
			plan := domain.ClaimPlan{
				Target: testAddress(20), Token: testAddress(21), Data: []byte{1, 2, 3, 4},
				MinimumReceived: *uint256.NewInt(1), GasLimit: 300000,
				MaximumCost: *uint256.NewInt(1_000_000_000_000_000), ValidUntil: 1893456000,
			}
			if scenario == "over cap" {
				plan.MaximumCost.AddUint64(&plan.MaximumCost, 1)
			}
			_, err := buildTestRig(t, rigOptions{allowUnknown: scenario != "allowlist", claim: &plan})
			if (err == nil) != (scenario == "all") {
				t.Fatalf("claim admission: %v", err)
			}
		})
	}
}

func TestPersistedFastCandidatesResumeAfterDisablingLatest(t *testing.T) {
	ctx := context.Background()
	for _, status := range []store.RescueStatus{store.RescuePending, store.RescuePrepared, store.RescueRetryable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			token := testAddress(22)
			rig := newTestRig(t, rigOptions{tokens: []common.Address{token}})
			candidate := domain.NewFastTokenCandidate(rig.chainID, rig.source, domain.Token{Address: token}, testHash(1), testHash(2), 1, 0, false)
			// New fast observations remain disabled in finalized mode.
			if err := rig.session.Handle(ctx, candidate); errorCode(err) != codeUnsupportedCandidate {
				t.Fatalf("new fast candidate accepted: %v", err)
			}
			incident := store.RescueIncident{
				ID: domain.NewAssetIncidentID(candidate.ID, domain.CandidateToken, token), Parent: domain.NewIncidentID(candidate.ID),
				Candidate: candidate.ID, Network: rig.chainID, Kind: domain.CandidateToken, Asset: token, Generation: 1, Trusted: true,
				Policy: store.RescuePolicySnapshot{LatestState: true, MaxAttempts: 3, RetryDelay: time.Second, FinalityTimeout: time.Second},
				Status: status, RetryAt: rig.clock.Now().Add(time.Hour), CreatedAt: rig.clock.Now(), UpdatedAt: rig.clock.Now(),
			}
			if _, err := rig.state.PutRescueIncident(ctx, incident); err != nil {
				t.Fatal(err)
			}
			err := rig.session.Handle(ctx, candidate)
			if status == store.RescuePrepared {
				// Empty prepared balance reaches the normal no-paid-action outcome.
				if err != nil || rig.incident(candidate, domain.CandidateToken, token).Status != store.RescueFailed {
					t.Fatalf("prepared incident was not resumed: %v", err)
				}
			} else if errorCode(err) != codeRetryPending {
				t.Fatalf("durable retry was abandoned: %v", err)
			}
		})
	}
}
