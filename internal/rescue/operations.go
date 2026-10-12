package rescue

import (
	"context"
	"math/big"
	"time"

	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum/common"
)

func (session *Session) handlePeriodic(ctx context.Context, candidate domain.RescueCandidate) error {
	parent := domain.NewIncidentID(candidate.ID)
	var firstError error
	remember := func(err error) {
		if err != nil && firstError == nil {
			firstError = err
		}
	}

	nativeErr := session.handleAsset(ctx, candidate, domain.CandidateNative, common.Address{}, parent)
	session.recordAssetFailure(ctx, candidate, domain.CandidateNative, common.Address{}, nativeErr)
	remember(nativeErr)
	for _, token := range session.coordinator.network.Tokens {
		assetErr := session.handleAsset(ctx, candidate, domain.CandidateToken, token.Address, parent)
		session.recordAssetFailure(ctx, candidate, domain.CandidateToken, token.Address, assetErr)
		remember(assetErr)
	}
	return firstError
}

func (session *Session) recordAssetFailure(ctx context.Context, candidate domain.RescueCandidate, kind domain.CandidateKind, asset common.Address, failure error) {
	if failure == nil {
		return
	}
	incident, found, err := session.coordinator.state.RescueIncident(ctx, domain.NewAssetIncidentID(candidate.ID, kind, asset))
	if err == nil && found {
		session.coordinator.recordFailure(candidate.ID, publicTransactionHash(incident), failure)
		session.coordinator.recordIncidentFailure(&incident, failure)
		return
	}
	session.coordinator.recordFailure(candidate.ID, common.Hash{}, failure)
}

func (session *Session) handleAsset(ctx context.Context, candidate domain.RescueCandidate, kind domain.CandidateKind, asset common.Address, parent domain.IncidentID) error {
	coordinator := session.coordinator
	if parent == (domain.IncidentID{}) {
		parent = domain.NewIncidentID(candidate.ID)
	}
	trusted := kind == domain.CandidateNative
	if kind == domain.CandidateToken {
		_, trusted = coordinator.trustedTokens[asset]
	}
	now := coordinator.clock.Now()
	generation := candidate.Generation
	if generation == 0 {
		generation = session.generation
	}
	incidentID := domain.NewAssetIncidentID(candidate.ID, kind, asset)
	incident := store.RescueIncident{
		ID:         incidentID,
		Parent:     parent,
		Candidate:  candidate.ID,
		Network:    coordinator.network.ChainID,
		Kind:       kind,
		Asset:      asset,
		Generation: generation,
		Trusted:    trusted,
		Policy: store.RescuePolicySnapshot{
			MaxAttempts:     coordinator.maxAttempts,
			RetryDelay:      coordinator.retryDelay,
			FinalityTimeout: coordinator.finalityTimeout,
		},
		Status:    store.RescuePending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	stored, found, err := coordinator.state.RescueIncident(ctx, incidentID)
	if err != nil {
		return newError("rescue.state_get", domain.ErrorInternal, codeStateRead, true, true, err)
	}
	if !found {
		stored, err = coordinator.state.PutRescueIncident(ctx, incident)
		if err != nil {
			return newError("rescue.state_put", domain.ErrorInternal, codeStateWrite, true, true, err)
		}
	}
	incident = stored
	if !sameIncidentIdentity(incident, candidate, kind, asset) {
		return newError("rescue.state_identity", domain.ErrorInternal, codeStateRead, false, true, nil)
	}
	if incident.Trusted != trusted {
		return newError("rescue.state_trust", domain.ErrorInternal, codeStateRead, false, true, nil)
	}
	return session.resumeIncident(ctx, &incident)
}

func sameIncidentIdentity(incident store.RescueIncident, candidate domain.RescueCandidate, kind domain.CandidateKind, asset common.Address) bool {
	return incident.ID == domain.NewAssetIncidentID(candidate.ID, kind, asset) && incident.Candidate == candidate.ID &&
		incident.Network == candidate.Network && incident.Kind == kind && incident.Asset == asset
}

func (session *Session) resumeIncident(ctx context.Context, incident *store.RescueIncident) error {
	switch incident.Status {
	case store.RescueTrustedSuccess, store.RescueTokenReported, store.RescueLostRace, store.RescueExhausted, store.RescueFailed:
		return session.pruneTerminal(ctx)
	case store.RescueSigned, store.RescueBroadcast, store.RescueAmbiguous:
		if incident.Status == store.RescueAmbiguous && !incident.RetryAt.IsZero() && session.coordinator.clock.Now().Before(incident.RetryAt) {
			return retryPendingError(true)
		}
		return session.reconcileOnce(ctx, incident)
	case store.RescuePrepared:
		if session.coordinator.nonceBlocked {
			return retryPendingError(true)
		}
		sourceBefore := new(big.Int).SetBytes(incident.SourceBefore[:])
		if sourceBefore.Sign() == 0 || (incident.Kind == domain.CandidateNative && sourceBefore.Cmp(session.coordinator.nativeThreshold) <= 0) {
			if err := session.setTerminal(ctx, incident, store.RescueFailed, codeNoPaidAction); err != nil {
				return err
			}
			return nil
		}
		_, explicitlyAllowed := session.coordinator.allowedUntrustedTokens[incident.Asset]
		if incident.Kind == domain.CandidateToken && !incident.Trusted && !session.coordinator.network.AllowUnknownTokens && !explicitlyAllowed {
			if err := session.setTerminal(ctx, incident, store.RescueFailed, codeUnknownToken); err != nil {
				return err
			}
			return newError("rescue.token_policy", domain.ErrorConfiguration, codeUnknownToken, false, false, nil)
		}
		return session.signSubmitAndConfirm(ctx, incident)
	case store.RescuePending, store.RescueRetryable:
		if session.coordinator.nonceBlocked {
			return retryPendingError(true)
		}
		now := session.coordinator.clock.Now()
		if incident.Attempts >= incident.Policy.MaxAttempts {
			if !incident.RetryAt.IsZero() && now.Before(incident.RetryAt) {
				return retryPendingError(false)
			}
			if err := session.setTerminal(ctx, incident, store.RescueExhausted, incident.LastCode); err != nil {
				return err
			}
			return nil
		}
		if !incident.RetryAt.IsZero() && now.Before(incident.RetryAt) {
			return retryPendingError(false)
		}
		return session.prepareAndSubmit(ctx, incident)
	default:
		return newError("rescue.state_status", domain.ErrorInternal, codeStateRead, false, true, nil)
	}
}

func retryPendingError(ambiguous bool) error {
	return newError("rescue.retry", domain.ErrorRPCTransient, codeRetryPending, true, ambiguous, nil)
}

func (session *Session) prepareAndSubmit(ctx context.Context, incident *store.RescueIncident) error {
	attempt := incident.Attempts + 1
	block, err := session.finality.Finalized(ctx)
	if err != nil || block.Hash == (common.Hash{}) {
		failure := contextOrError(ctx, "rescue.prestate_finalized", domain.ErrorRPCTransient, codeFinalityRead, true, true, err)
		return session.preparationFailure(ctx, incident, attempt, session.lastFinalized, nil, nil, failure)
	}
	session.lastFinalized = block

	var sourceBefore, destinationBefore *big.Int
	if incident.Kind == domain.CandidateToken {
		sourceBefore, err = session.tokenBalanceAt(ctx, block, incident.Asset, session.coordinator.source)
		if err == nil {
			destinationBefore, err = session.tokenBalanceAt(ctx, block, incident.Asset, session.coordinator.destination)
		}
	} else {
		sourceBefore, err = session.nativeBalanceAt(ctx, block, session.coordinator.source)
		if err == nil {
			destinationBefore, err = session.nativeBalanceAt(ctx, block, session.coordinator.destination)
		}
	}
	if err != nil {
		return session.preparationFailure(ctx, incident, attempt, block, sourceBefore, destinationBefore, err)
	}
	session.rpcSucceeded = true
	sourceBytes, err := amountBytes(sourceBefore)
	if err != nil {
		failure := newError("rescue.prestate_source", domain.ErrorRPCInvalidResponse, codeBalanceDecode, true, true, err)
		return session.preparationFailure(ctx, incident, attempt, block, nil, destinationBefore, failure)
	}
	destinationBytes, err := amountBytes(destinationBefore)
	if err != nil {
		failure := newError("rescue.prestate_destination", domain.ErrorRPCInvalidResponse, codeBalanceDecode, true, true, err)
		return session.preparationFailure(ctx, incident, attempt, block, sourceBefore, nil, failure)
	}

	setPreparedAttempt(incident, attempt, block, sourceBytes, destinationBytes)
	if sourceBefore.Sign() == 0 || (incident.Kind == domain.CandidateNative && sourceBefore.Cmp(session.coordinator.nativeThreshold) <= 0) {
		if err := session.updateIncident(ctx, incident); err != nil {
			return err
		}
		if err := session.setTerminal(ctx, incident, store.RescueFailed, codeNoPaidAction); err != nil {
			return err
		}
		return nil
	}
	_, explicitlyAllowed := session.coordinator.allowedUntrustedTokens[incident.Asset]
	if incident.Kind == domain.CandidateToken && !incident.Trusted && !session.coordinator.network.AllowUnknownTokens && !explicitlyAllowed {
		if err := session.updateIncident(ctx, incident); err != nil {
			return err
		}
		if err := session.setTerminal(ctx, incident, store.RescueFailed, codeUnknownToken); err != nil {
			return err
		}
		return newError("rescue.token_policy", domain.ErrorConfiguration, codeUnknownToken, false, false, nil)
	}

	sponsorNonce, err := session.sponsorNonce(ctx)
	if err != nil {
		return session.preparationFailure(ctx, incident, attempt, block, sourceBefore, destinationBefore, err)
	}
	sourceNonce, err := session.checkedPendingNonce(ctx, session.coordinator.source)
	if err != nil {
		return session.preparationFailure(ctx, incident, attempt, block, sourceBefore, destinationBefore, err)
	}
	incident.SponsorNonce = sponsorNonce
	incident.SourceNonce = sourceNonce
	if err := session.updateIncident(ctx, incident); err != nil {
		return err
	}
	if session.coordinator.metrics != nil {
		_ = session.coordinator.metrics.RecordAttempt(session.coordinator.network.ChainID)
	}
	session.coordinator.recordSafe(observability.SafeEvent{
		Level: observability.LevelInfo, Code: observability.LogAttemptStarted,
		ChainID: session.coordinator.network.ChainID, Incident: incident.ID,
		State: observability.DurableProcessing, Result: observability.ResultAccepted,
	})
	session.coordinator.record(eventOperationPrepared, observability.LevelInfo, incident.Candidate, common.Hash{}, "")
	return session.signSubmitAndConfirm(ctx, incident)
}

func (session *Session) preparationFailure(ctx context.Context, incident *store.RescueIncident, attempt uint32, block rpc.BlockRef, source, destination *big.Int, failure error) error {
	incident.Attempts = attempt
	incident.Status = store.RescueRetryable
	incident.SponsorNonce = 0
	incident.SourceNonce = 0
	incident.TxHash = common.Hash{}
	incident.SignedTransaction = nil
	incident.SnapshotBlockNumber = block.Number
	incident.SnapshotBlockHash = block.Hash
	incident.SourceBefore = knownAmountBytes(source)
	incident.DestinationBefore = knownAmountBytes(destination)
	incident.ReconcileUntil = time.Time{}
	incident.LastCode = errorCode(failure)
	if err := session.updateIncident(ctx, incident); err != nil {
		return err
	}
	return failure
}

func setPreparedAttempt(incident *store.RescueIncident, attempt uint32, block rpc.BlockRef, source, destination [32]byte) {
	incident.Attempts = attempt
	incident.Status = store.RescuePrepared
	incident.SponsorNonce = 0
	incident.SourceNonce = 0
	incident.TxHash = common.Hash{}
	incident.SignedTransaction = nil
	incident.SnapshotBlockNumber = block.Number
	incident.SnapshotBlockHash = block.Hash
	incident.SourceBefore = source
	incident.DestinationBefore = destination
	incident.RetryAt = time.Time{}
	incident.ReconcileUntil = time.Time{}
	incident.LastCode = ""
}

func knownAmountBytes(amount *big.Int) [32]byte {
	if amount == nil {
		return [32]byte{}
	}
	encoded, _ := amountBytes(amount)
	return encoded
}
