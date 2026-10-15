package rescue

import (
	"math/big"

	"guard-daemon/internal/domain"
	"guard-daemon/internal/store"

	"github.com/holiman/uint256"
)

func (session *Session) claimFor(incident *store.RescueIncident) (*domain.ClaimPlan, error) {
	plan := session.coordinator.claim
	if plan == nil || incident.Kind != domain.CandidateClaim || incident.Asset != plan.Token ||
		incident.Candidate != domain.NewClaimCandidate(session.coordinator.network.ChainID, session.coordinator.source, *plan).ID {
		return nil, newError("rescue.claim_identity", domain.ErrorConfiguration, codeCandidateMismatch, false, true, nil)
	}
	return plan, nil
}

func (session *Session) operationValue(incident *store.RescueIncident) (uint256.Int, error) {
	if incident.Kind != domain.CandidateClaim {
		return uint256.Int{}, nil
	}
	plan, err := session.claimFor(incident)
	if err != nil {
		return uint256.Int{}, err
	}
	return plan.Value, nil
}

func (session *Session) claimExpired(incident *store.RescueIncident) bool {
	if incident.Kind != domain.CandidateClaim {
		return false
	}
	plan, err := session.claimFor(incident)
	return err != nil || session.coordinator.clock.Now().Unix() < 0 || uint64(session.coordinator.clock.Now().Unix()) >= plan.ValidUntil
}

func isTokenOperation(kind domain.CandidateKind) bool {
	return kind == domain.CandidateToken || kind == domain.CandidateClaim
}

func (session *Session) requiredDestinationDelta(incident *store.RescueIncident) (*big.Int, error) {
	required := new(big.Int).SetBytes(incident.SourceBefore[:])
	if incident.Kind == domain.CandidateClaim {
		plan, err := session.claimFor(incident)
		if err != nil {
			return nil, err
		}
		required.Add(required, plan.MinimumReceived.ToBig())
	}
	return required, nil
}
