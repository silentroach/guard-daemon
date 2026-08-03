package rescue

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"strings"
	"time"

	"guard-daemon/internal/budget"
	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

const (
	receiptPollingInterval = 400 * time.Millisecond
	rescueIncidentRetain   = 1024
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

func (session *Session) signSubmitAndConfirm(ctx context.Context, incident *store.RescueIncident) error {
	coordinator := session.coordinator
	sponsorNonce, err := session.sponsorNonce(ctx)
	if err != nil {
		return session.beforeSignedFailure(ctx, incident, err)
	}
	if sponsorNonce != incident.SponsorNonce {
		failure := newError("rescue.sponsor_nonce", domain.ErrorRPCTransient, codeNonceRead, true, false, nil)
		return session.beforeSignedFailure(ctx, incident, failure)
	}
	data, asset, err := session.operationData(incident)
	if err != nil {
		return session.beforeSignedFailure(ctx, incident, err)
	}
	fees, err := session.readFees(ctx, asset)
	if err != nil {
		return session.beforeSignedFailure(ctx, incident, err)
	}
	gas := fees.GasLimit

	unknown := incident.Kind == domain.CandidateToken && !incident.Trusted
	budgetBlock, err := session.finality.Finalized(ctx)
	if err != nil || budgetBlock.Hash == (common.Hash{}) {
		return session.beforeSignedFailure(ctx, incident, contextOrError(ctx, "rescue.budget_finalized", domain.ErrorRPCTransient, codeFinalityRead, true, true, err))
	}
	sponsorBalance, err := session.nativeBalanceAt(ctx, budgetBlock, coordinator.sponsor)
	if err != nil {
		return session.beforeSignedFailure(ctx, incident, err)
	}
	decision, admissionErr := coordinator.admission.Admit(AdmissionRequest{
		Network:     coordinator.network.ChainID,
		Incident:    incident.ID,
		Attempt:     incident.Attempts,
		Token:       incident.Asset,
		Parent:      incident.Parent,
		SourceEvent: incident.Candidate,
		Unknown:     unknown,
		ObservedAt:  session.blockTime(budgetBlock),
	})
	if admissionErr != nil || !decision.Allowed {
		return session.admissionBlocked(incident, admissionErr)
	}
	reservationMaximum := coordinator.feePolicy.TransactionCostCap
	if incident.Kind == domain.CandidateNative {
		value, valueErr := EvaluateMinimumValue(coordinator.feePolicy, FeeAssetNative, new(big.Int).SetBytes(incident.SourceBefore[:]), coordinator.nativeMinimum, reservationMaximum)
		if valueErr != nil || !value.Allowed {
			return session.beforeSignedFailure(ctx, incident, newError("rescue.minimum_value", domain.ErrorPostcondition, codeMinimumValue, false, false, valueErr))
		}
	} else if unknown {
		reservationMaximum = coordinator.feePolicy.UnknownTokenCostCap
		value, valueErr := EvaluateMinimumValue(coordinator.feePolicy, FeeAssetUnknownToken, nil, uint256.Int{}, fees.MaximumCost)
		if valueErr != nil || !value.Allowed || value.Trusted {
			return session.beforeSignedFailure(ctx, incident, newError("rescue.minimum_value", domain.ErrorPostcondition, codeMinimumValue, false, false, valueErr))
		}
	} else {
		valuePolicy, ok := coordinator.trustedTokenValues[incident.Asset]
		reservationMaximum = valuePolicy.MaximumCost
		sourceValue, sourceOverflow := uint256.FromBig(new(big.Int).SetBytes(incident.SourceBefore[:]))
		if !ok || sourceOverflow || sourceValue.Cmp(&valuePolicy.MinimumBalance) < 0 || fees.MaximumCost.Cmp(&valuePolicy.MaximumCost) > 0 {
			return session.beforeSignedFailure(ctx, incident, newError("rescue.minimum_value", domain.ErrorPostcondition, codeMinimumValue, false, false, nil))
		}
	}
	if reservationMaximum.Lt(&fees.MaximumCost) {
		return session.beforeSignedFailure(ctx, incident, newError("rescue.maximum_cost", domain.ErrorBudget, codeBudgetExceeded, true, false, nil))
	}
	sponsorBalanceU256, overflow := uint256.FromBig(sponsorBalance)
	if overflow {
		return session.beforeSignedFailure(ctx, incident, newError("rescue.sponsor_balance", domain.ErrorRPCInvalidResponse, codeBalanceDecode, true, true, nil))
	}
	maximumFee, feeOverflow := uint256.FromBig(fees.FeeCap)
	if feeOverflow {
		return session.beforeSignedFailure(ctx, incident, newError("rescue.fees", domain.ErrorRPCInvalidResponse, codeFeeInvalid, true, true, nil))
	}
	reservation, err := coordinator.budget.Reserve(ctx, budget.ReservationRequest{
		Network:        coordinator.network.ChainID,
		Sponsor:        coordinator.sponsor,
		Candidate:      incident.Candidate,
		Attempt:        budget.Attempt{Incident: incident.ID, Number: incident.Attempts},
		Quote:          budget.CostQuote{GasLimit: gas, MaxFeePerGas: *maximumFee, MaximumCost: reservationMaximum},
		SponsorBalance: *sponsorBalanceU256,
		ObservedAt:     session.blockTime(budgetBlock),
	})
	if err != nil {
		return session.budgetBlocked(incident, err)
	}
	if err := session.updateBudgetMetrics(ctx); err != nil {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, err)
	}
	chainID := big.NewInt(int64(coordinator.network.ChainID))
	chainU256, overflow := uint256.FromBig(chainID)
	if overflow {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, newError("rescue.chain_id", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil))
	}
	tip, tipOverflow := uint256.FromBig(fees.TipCap)
	feeCap, capOverflow := uint256.FromBig(fees.FeeCap)
	if tipOverflow || capOverflow {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, newError("rescue.fees", domain.ErrorRPCInvalidResponse, codeFeeInvalid, true, true, nil))
	}

	// This is intentionally the final RPC read before authorization signing.
	sourceNonce, err := session.checkedPendingNonce(ctx, coordinator.source)
	if err != nil {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, err)
	}
	if sourceNonce != incident.SourceNonce {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, newError("rescue.source_nonce", domain.ErrorRPCTransient, codeNonceRead, true, false, nil))
	}
	authorizationRequest := types.SetCodeAuthorization{ChainID: *chainU256, Address: coordinator.rescuer, Nonce: sourceNonce}
	var authorization types.SetCodeAuthorization
	err = coordinator.gate.Do(func() error {
		if err := coordinator.guardSigning(ctx); err != nil {
			return err
		}
		if err := session.recheckSponsorReserve(ctx, reservation.ID); err != nil {
			return err
		}
		var signErr error
		authorization, signErr = coordinator.authorizer.SignAuthorization(ctx, authorizationRequest)
		return signErr
	})
	if err != nil {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, paidActionError(ctx, "rescue.sign_authorization", err))
	}
	authority, authorityErr := authorization.Authority()
	if authorityErr != nil || authority != coordinator.source || authorization.ChainID != authorizationRequest.ChainID ||
		authorization.Address != authorizationRequest.Address || authorization.Nonce != authorizationRequest.Nonce {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, newError("rescue.sign_authorization", domain.ErrorSigning, codeSignerMismatch, true, true, nil))
	}
	simulation := EIP7702SimulationRequest{
		Sponsor: coordinator.sponsor, Source: coordinator.source, GasLimit: gas,
		GasTipCap: fees.TipCap, GasFeeCap: fees.FeeCap, Data: data,
		AuthorizationList: []types.SetCodeAuthorization{authorization},
	}
	simulationBlock, err := session.finality.Finalized(ctx)
	if err != nil || simulationBlock.Hash == (common.Hash{}) {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, contextOrError(ctx, "rescue.simulation_finalized", domain.ErrorRPCTransient, codeFinalityRead, true, true, err))
	}
	if err := SimulateEIP7702At(ctx, session.finality, simulationBlock, simulation); err != nil {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, contextOrError(ctx, "rescue.simulation_quorum", domain.ErrorRPCTransient, codeSimulation, true, false, err))
	}
	if _, err := EstimateEIP7702Gas(ctx, session.reader, simulation); err != nil {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, contextOrError(ctx, "rescue.simulation", domain.ErrorRPCTransient, codeSimulation, true, false, err))
	}

	transaction := types.NewTx(&types.SetCodeTx{
		ChainID: chainU256, Nonce: incident.SponsorNonce, To: coordinator.source, Gas: gas,
		GasTipCap: tip, GasFeeCap: feeCap, Data: data, AuthList: []types.SetCodeAuthorization{authorization},
	})
	var signed *types.Transaction
	err = coordinator.gate.Do(func() error {
		if err := coordinator.guardSigning(ctx); err != nil {
			return err
		}
		if err := session.recheckSponsorReserve(ctx, reservation.ID); err != nil {
			return err
		}
		var signErr error
		signed, signErr = coordinator.transactioner.SignTransaction(ctx, transaction, chainID)
		return signErr
	})
	if err != nil {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, paidActionError(ctx, "rescue.sign_transaction", err))
	}
	if !validSignedOperation(signed, transaction) {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, newError("rescue.sign_transaction", domain.ErrorSigning, codeSigning, true, true, nil))
	}
	sender, senderErr := types.Sender(types.LatestSignerForChainID(chainID), signed)
	if senderErr != nil || sender != coordinator.sponsor {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, newError("rescue.sign_transaction", domain.ErrorSigning, codeSignerMismatch, true, true, nil))
	}
	encoded, err := signed.MarshalBinary()
	if err != nil {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, newError("rescue.sign_transaction", domain.ErrorSigning, codeSigning, true, true, err))
	}
	if err := coordinator.guardSigning(ctx); err != nil {
		return session.releaseBeforeSigned(ctx, incident, reservation.ID, err)
	}

	now := coordinator.clock.Now()
	incident.Status = store.RescueSigned
	incident.TxHash = signed.Hash()
	incident.SignedTransaction = append([]byte(nil), encoded...)
	incident.ReconcileUntil = now.Add(incident.Policy.FinalityTimeout)
	incident.RetryAt = time.Time{}
	incident.LastCode = ""
	if err := session.updateIncidentAt(ctx, incident, now); err != nil {
		return session.releaseReservation(ctx, reservation.ID, err)
	}
	coordinator.nonceBlocked = true
	floor := incident.SponsorNonce + 1
	if err := coordinator.state.RaiseNonceFloor(ctx, floor); err != nil {
		return newError("rescue.nonce_floor", domain.ErrorInternal, codeStateWrite, true, true, err)
	}
	coordinator.raiseNonceFloor(floor)
	return session.broadcastAndWait(ctx, incident, signed)
}

func (session *Session) operationData(incident *store.RescueIncident) ([]byte, FeeAsset, error) {
	if incident.Kind == domain.CandidateToken {
		data, err := session.coordinator.rescuerCodec.PackSweepAll([]common.Address{incident.Asset})
		if err != nil || len(data) == 0 {
			return nil, 0, newError("rescue.operation_data", domain.ErrorInternal, codeEncoding, false, false, err)
		}
		if incident.Trusted {
			return data, FeeAssetToken, nil
		}
		return data, FeeAssetUnknownToken, nil
	}
	data, err := session.coordinator.rescuerCodec.PackSweepEth()
	if err != nil || len(data) == 0 {
		return nil, 0, newError("rescue.operation_data", domain.ErrorInternal, codeEncoding, false, false, err)
	}
	return data, FeeAssetNative, nil
}

func validSignedOperation(signed, unsigned *types.Transaction) bool {
	if signed == nil || signed.Type() != types.SetCodeTxType || signed.Nonce() != unsigned.Nonce() || signed.To() == nil || unsigned.To() == nil ||
		*signed.To() != *unsigned.To() || !bytes.Equal(signed.Data(), unsigned.Data()) || signed.Gas() != unsigned.Gas() ||
		signed.ChainId().Cmp(unsigned.ChainId()) != 0 || signed.GasTipCap().Cmp(unsigned.GasTipCap()) != 0 ||
		signed.GasFeeCap().Cmp(unsigned.GasFeeCap()) != 0 || signed.Value().Sign() != 0 || len(signed.AccessList()) != 0 {
		return false
	}
	authorizations := signed.SetCodeAuthorizations()
	want := unsigned.SetCodeAuthorizations()
	return len(authorizations) == 1 && len(want) == 1 && authorizations[0] == want[0]
}

func (session *Session) validatePersistedTransaction(incident *store.RescueIncident) (*types.Transaction, error) {
	if len(incident.SignedTransaction) == 0 || incident.TxHash == (common.Hash{}) {
		return nil, newError("rescue.signed_payload", domain.ErrorInternal, codeSignedPayloadInvalid, false, true, nil)
	}
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(incident.SignedTransaction); err != nil {
		return nil, newError("rescue.signed_payload", domain.ErrorInternal, codeSignedPayloadInvalid, false, true, err)
	}
	reencoded, err := transaction.MarshalBinary()
	if err != nil || !bytes.Equal(reencoded, incident.SignedTransaction) || transaction.Hash() != incident.TxHash ||
		transaction.Type() != types.SetCodeTxType || transaction.Nonce() != incident.SponsorNonce || transaction.To() == nil ||
		*transaction.To() != session.coordinator.source || transaction.Value().Sign() != 0 {
		return nil, newError("rescue.signed_payload", domain.ErrorInternal, codeSignedPayloadInvalid, false, true, err)
	}
	data, asset, err := session.operationData(incident)
	gas, gasOK := session.coordinator.feePolicy.gasLimit(asset)
	if err != nil || !gasOK || !bytes.Equal(transaction.Data(), data) || transaction.Gas() != gas ||
		transaction.GasTipCap().Sign() <= 0 || transaction.GasFeeCap().Sign() <= 0 ||
		transaction.ChainId().Cmp(big.NewInt(int64(session.coordinator.network.ChainID))) != 0 || len(transaction.AccessList()) != 0 {
		return nil, newError("rescue.signed_payload", domain.ErrorInternal, codeSignedPayloadInvalid, false, true, err)
	}
	authorizations := transaction.SetCodeAuthorizations()
	if len(authorizations) != 1 || authorizations[0].Address != session.coordinator.rescuer || authorizations[0].Nonce != incident.SourceNonce ||
		authorizations[0].ChainID.ToBig().Cmp(big.NewInt(int64(session.coordinator.network.ChainID))) != 0 {
		return nil, newError("rescue.signed_payload", domain.ErrorInternal, codeSignedPayloadInvalid, false, true, nil)
	}
	authority, err := authorizations[0].Authority()
	if err != nil || authority != session.coordinator.source {
		return nil, newError("rescue.signed_payload", domain.ErrorSigning, codeSignerMismatch, false, true, nil)
	}
	sender, err := types.Sender(types.LatestSignerForChainID(transaction.ChainId()), &transaction)
	if err != nil || sender != session.coordinator.sponsor {
		return nil, newError("rescue.signed_payload", domain.ErrorSigning, codeSignerMismatch, false, true, nil)
	}
	return &transaction, nil
}

func (session *Session) sponsorNonce(ctx context.Context) (uint64, error) {
	pending, err := session.checkedPendingNonce(ctx, session.coordinator.sponsor)
	if err != nil {
		return 0, err
	}
	if pending > session.coordinator.nonceFloor {
		session.coordinator.raiseNonceFloor(pending)
	}
	if session.coordinator.nonceFloor == ^uint64(0) {
		return 0, newError("rescue.sponsor_nonce", domain.ErrorRPCInvalidResponse, codeNonceRead, false, true, nil)
	}
	return session.coordinator.nonceFloor, nil
}

func (session *Session) checkedPendingNonce(ctx context.Context, address common.Address) (uint64, error) {
	nonce, err := session.pendingNonce(ctx, address)
	if err != nil {
		return 0, err
	}
	if nonce == ^uint64(0) {
		return 0, newError("rescue.nonce", domain.ErrorRPCInvalidResponse, codeNonceRead, false, true, nil)
	}
	return nonce, nil
}

func (session *Session) beforeSignedFailure(ctx context.Context, incident *store.RescueIncident, failure error) error {
	incident.LastCode = errorCode(failure)
	incident.Status = store.RescueRetryable
	incident.TxHash = common.Hash{}
	incident.SignedTransaction = nil
	incident.ReconcileUntil = time.Time{}
	if err := session.updateIncident(ctx, incident); err != nil {
		return err
	}
	return failure
}

func (session *Session) releaseBeforeSigned(ctx context.Context, incident *store.RescueIncident, id budget.ReservationID, failure error) error {
	failure = session.releaseReservation(ctx, id, failure)
	if errorCode(failure) == codePaidActionsStopped {
		return failure
	}
	return session.beforeSignedFailure(ctx, incident, failure)
}

func (session *Session) releaseReservation(ctx context.Context, id budget.ReservationID, original error) error {
	if _, err := session.coordinator.budget.ReleaseProvenUnused(ctx, id); err != nil {
		return session.budgetError("rescue.budget_release", errors.Join(original, err))
	}
	if err := session.updateBudgetMetrics(ctx); err != nil {
		return err
	}
	return original
}

func (session *Session) admissionBlocked(incident *store.RescueIncident, cause error) error {
	return newError("rescue.admission", domain.ErrorBudget, codeAdmissionLimited, true, false, cause)
}

func (session *Session) budgetBlocked(incident *store.RescueIncident, cause error) error {
	coordinator := session.coordinator
	code := codeBudgetExceeded
	alertCode := observability.AlertBudgetBlocked
	if errors.Is(cause, budget.ErrSponsorReserve) {
		code = codeSponsorReserve
	}
	if coordinator.health != nil {
		_ = coordinator.health.SetCondition(coordinator.network.ChainID, observability.ConditionBudgetBlocked, true)
	}
	if coordinator.alerts != nil {
		if _, alertErr := coordinator.alerts.Raise(coordinator.network.ChainID, alertCode); alertErr != nil {
			cause = errors.Join(cause, alertErr)
		}
	}
	coordinator.recordSafe(observability.SafeEvent{
		Level: observability.LevelWarning, Code: observability.LogBudgetBlocked,
		ChainID: coordinator.network.ChainID, Incident: incident.ID,
		State: observability.DurableProcessing, Result: observability.ResultSkipped,
		Error: coordinator.safeError(domain.ErrorBudget, code, true, false),
	})
	return newError("rescue.budget", domain.ErrorBudget, code, true, false, cause)
}

func (session *Session) budgetError(operation string, cause error) error {
	return newError(operation, domain.ErrorBudget, codeBudgetExceeded, true, true, cause)
}

func (session *Session) updateBudgetMetrics(ctx context.Context) error {
	coordinator := session.coordinator
	snapshot, err := coordinator.budget.Snapshot(ctx)
	if err != nil {
		return session.budgetError("rescue.budget_snapshot", err)
	}
	network, ok := snapshot.Networks[coordinator.network.ChainID]
	if !ok {
		return session.budgetError("rescue.budget_snapshot", budget.ErrCorrupt)
	}
	if coordinator.metrics != nil {
		coordinator.metrics.SetGlobalBudget(snapshot.Global.Spent.Cumulative, snapshot.Global.Reserved.Cumulative, snapshot.Global.Remaining.Cumulative)
		if err := coordinator.metrics.SetBudget(coordinator.network.ChainID, network.Spent.Cumulative, network.Reserved.Cumulative, network.Remaining.Cumulative); err != nil {
			return newError("rescue.metrics", domain.ErrorInternal, codeStateWrite, true, true, err)
		}
	}
	blocked := snapshot.Global.Blocked() || network.Blocked()
	if coordinator.health != nil {
		_ = coordinator.health.SetCondition(coordinator.network.ChainID, observability.ConditionBudgetBlocked, blocked)
	}
	if coordinator.alerts != nil {
		var alertErr error
		if blocked {
			_, alertErr = coordinator.alerts.Raise(coordinator.network.ChainID, observability.AlertBudgetBlocked)
		} else {
			_, alertErr = coordinator.alerts.Resolve(coordinator.network.ChainID, observability.AlertBudgetBlocked)
		}
		if alertErr != nil {
			return newError("rescue.budget_alert", domain.ErrorInternal, codeStateWrite, true, true, alertErr)
		}
	}
	return nil
}

func paidActionError(ctx context.Context, operation string, err error) error {
	if errors.Is(err, observability.ErrPaidActionsStopped) {
		return newError(operation, domain.ErrorSigning, codePaidActionsStopped, true, false, err)
	}
	return contextOrError(ctx, operation, domain.ErrorSigning, codeSigning, true, false, err)
}

func (session *Session) broadcastAndWait(ctx context.Context, incident *store.RescueIncident, transaction *types.Transaction) error {
	sent, sendErr := session.sendPersisted(ctx, incident, transaction)
	if !sent {
		return sendErr
	}
	var sendFailure error
	if sendErr == nil {
		incident.Status = store.RescueBroadcast
		incident.LastCode = ""
		incident.RetryAt = time.Time{}
		if err := session.updateIncident(ctx, incident); err != nil {
			return err
		}
		if err := session.refreshNonceBlock(ctx); err != nil {
			return err
		}
		session.recordBroadcast(incident)
		session.coordinator.record(eventBroadcastAccepted, observability.LevelInfo, incident.Candidate, incident.TxHash, "")
	} else {
		sendFailure = contextOrError(ctx, "rescue.broadcast", domain.ErrorBroadcast, codeBroadcast, true, true, sendErr)
		if err := session.persistAmbiguousCode(ctx, incident, codeBroadcast); err != nil {
			return err
		}
		if err := session.refreshNonceBlock(ctx); err != nil {
			return err
		}
	}

	receipt, err := session.waitReceipt(ctx, incident.TxHash, incident.ReconcileUntil)
	if err != nil {
		if sendFailure != nil {
			return sendFailure
		}
		if persistErr := session.persistAmbiguous(ctx, incident, err); persistErr != nil {
			return persistErr
		}
		return err
	}
	return session.applyReceipt(ctx, incident, receipt)
}

func (session *Session) reconcileOnce(ctx context.Context, incident *store.RescueIncident) error {
	transaction, err := session.validatePersistedTransaction(incident)
	if err != nil {
		if persistErr := session.persistAmbiguous(ctx, incident, err); persistErr != nil {
			return persistErr
		}
		return err
	}
	if receipt, receiptErr := session.finality.Receipt(ctx, incident.TxHash); receiptErr == nil && receipt != nil {
		if incident.Status == store.RescueSigned {
			incident.Status = store.RescueAmbiguous
			incident.LastCode = codeBroadcast
			if err := session.updateIncident(ctx, incident); err != nil {
				return err
			}
		}
		return session.applyReceipt(ctx, incident, receipt)
	}
	if !session.coordinator.clock.Now().Before(incident.ReconcileUntil) {
		if err := session.persistExpiredAmbiguous(ctx, incident); err != nil {
			return err
		}
		if err := session.refreshNonceBlock(ctx); err != nil {
			return err
		}
		return nil
	}
	if err := session.ensureDurableNonceFloor(ctx, incident.SponsorNonce); err != nil {
		return err
	}

	if incident.Status == store.RescueSigned || incident.Status == store.RescueAmbiguous {
		sent, sendErr := session.sendPersisted(ctx, incident, transaction)
		if !sent {
			if incident.Status == store.RescueAmbiguous {
				if err := session.persistAmbiguous(ctx, incident, sendErr); err != nil {
					return err
				}
			}
			return sendErr
		}
		var sendFailure error
		if sendErr == nil {
			incident.Status = store.RescueBroadcast
			incident.LastCode = ""
			incident.RetryAt = time.Time{}
			if err := session.updateIncident(ctx, incident); err != nil {
				return err
			}
			if err := session.refreshNonceBlock(ctx); err != nil {
				return err
			}
			session.recordBroadcast(incident)
			session.coordinator.record(eventBroadcastAccepted, observability.LevelInfo, incident.Candidate, incident.TxHash, "")
		} else {
			sendFailure = contextOrError(ctx, "rescue.rebroadcast", domain.ErrorBroadcast, codeBroadcast, true, true, sendErr)
			if err := session.persistAmbiguousCode(ctx, incident, codeBroadcast); err != nil {
				return err
			}
			if err := session.refreshNonceBlock(ctx); err != nil {
				return err
			}
		}

		receipt, err := session.waitReceipt(ctx, incident.TxHash, incident.ReconcileUntil)
		if err != nil {
			if sendFailure != nil {
				return sendFailure
			}
			if persistErr := session.persistAmbiguous(ctx, incident, err); persistErr != nil {
				return persistErr
			}
			return err
		}
		return session.applyReceipt(ctx, incident, receipt)
	}

	receipt, err := session.waitReceipt(ctx, incident.TxHash, incident.ReconcileUntil)
	if err != nil {
		if persistErr := session.persistAmbiguous(ctx, incident, err); persistErr != nil {
			return persistErr
		}
		return err
	}
	return session.applyReceipt(ctx, incident, receipt)
}

func (session *Session) reconcileExpiredIncident(ctx context.Context, incident *store.RescueIncident) error {
	if _, err := session.validatePersistedTransaction(incident); err != nil {
		if persistErr := session.persistAmbiguous(ctx, incident, err); persistErr != nil {
			return persistErr
		}
		return err
	}
	receipt, err := session.finality.Receipt(ctx, incident.TxHash)
	if ctx.Err() != nil {
		return contextOrError(ctx, "rescue.receipt", domain.ErrorRPCTransient, codeContextCanceled, true, true, err)
	}
	if err != nil {
		failure := contextOrError(ctx, "rescue.expired_receipt", domain.ErrorRPCTransient, codeReceiptTimeout, true, true, err)
		if persistErr := session.persistExpiredAmbiguous(ctx, incident); persistErr != nil {
			return persistErr
		}
		return failure
	}
	if receipt == nil {
		if err := session.persistExpiredAmbiguous(ctx, incident); err != nil {
			return err
		}
		return session.refreshNonceBlock(ctx)
	}
	return session.applyReceipt(ctx, incident, receipt)
}

func (session *Session) ensureDurableNonceFloor(ctx context.Context, nonce uint64) error {
	if nonce == ^uint64(0) {
		return newError("rescue.nonce_floor", domain.ErrorInternal, codeNonceRead, false, true, nil)
	}
	floor := nonce + 1
	if err := session.coordinator.state.RaiseNonceFloor(ctx, floor); err != nil {
		return newError("rescue.nonce_floor", domain.ErrorInternal, codeStateWrite, true, true, err)
	}
	session.coordinator.raiseNonceFloor(floor)
	return nil
}

func (session *Session) guardPersistedSend(ctx context.Context, incident *store.RescueIncident) error {
	if err := session.coordinator.guardSigning(ctx); err != nil {
		if persistErr := session.persistAmbiguousCode(ctx, incident, errorCode(err)); persistErr != nil {
			return persistErr
		}
		if refreshErr := session.refreshNonceBlock(ctx); refreshErr != nil {
			return refreshErr
		}
		return err
	}
	return nil
}

func (session *Session) sendPersisted(ctx context.Context, incident *store.RescueIncident, transaction *types.Transaction) (bool, error) {
	reservation, err := session.ensureReservation(ctx, incident, transaction)
	if err != nil {
		return false, err
	}
	var sendErr error
	err = session.coordinator.gate.Do(func() error {
		if err := session.coordinator.guardSigning(ctx); err != nil {
			return err
		}
		if err := session.recheckSponsorReserve(ctx, reservation.ID); err != nil {
			return err
		}
		if _, err := session.coordinator.budget.MarkExposed(ctx, reservation.ID, incident.TxHash); err != nil {
			return session.budgetError("rescue.budget_expose", err)
		}
		if err := session.updateBudgetMetrics(ctx); err != nil {
			return err
		}
		sendErr = session.broadcaster.SendTransaction(ctx, transaction)
		return nil
	})
	if err != nil {
		if errors.Is(err, observability.ErrPaidActionsStopped) {
			return false, newError("rescue.broadcast_gate", domain.ErrorSigning, codePaidActionsStopped, true, false, err)
		}
		return false, err
	}
	return true, sendErr
}

func (session *Session) recheckSponsorReserve(ctx context.Context, reservationID budget.ReservationID) error {
	block, err := session.finality.Finalized(ctx)
	if err != nil || block.Hash == (common.Hash{}) {
		return contextOrError(ctx, "rescue.sponsor_finalized", domain.ErrorRPCTransient, codeFinalityRead, true, true, err)
	}
	balance, err := session.nativeBalanceAt(ctx, block, session.coordinator.sponsor)
	if err != nil {
		return err
	}
	converted, overflow := uint256.FromBig(balance)
	if overflow {
		return newError("rescue.sponsor_balance", domain.ErrorRPCInvalidResponse, codeBalanceDecode, true, true, nil)
	}
	if err := session.coordinator.budget.CheckSponsorCapacity(ctx, reservationID, *converted); err != nil {
		if errors.Is(err, budget.ErrSponsorReserve) {
			return newError("rescue.sponsor_reserve", domain.ErrorBudget, codeSponsorReserve, true, false, err)
		}
		return session.budgetError("rescue.sponsor_capacity", err)
	}
	return nil
}

func (session *Session) ensureReservation(ctx context.Context, incident *store.RescueIncident, transaction *types.Transaction) (budget.Reservation, error) {
	attempt := budget.Attempt{Incident: incident.ID, Number: incident.Attempts}
	reservation, found, err := session.coordinator.budget.ReservationByAttempt(ctx, attempt)
	if err != nil {
		return budget.Reservation{}, session.budgetError("rescue.budget_lookup", err)
	}
	if found {
		return reservation, nil
	}
	block, err := session.finality.Finalized(ctx)
	if err != nil || block.Hash == (common.Hash{}) {
		return budget.Reservation{}, contextOrError(ctx, "rescue.budget_finalized", domain.ErrorRPCTransient, codeFinalityRead, true, true, err)
	}
	balance, err := session.nativeBalanceAt(ctx, block, session.coordinator.sponsor)
	if err != nil {
		return budget.Reservation{}, err
	}
	balanceU256, overflow := uint256.FromBig(balance)
	feeU256, feeOverflow := uint256.FromBig(transaction.GasFeeCap())
	if overflow || feeOverflow {
		return budget.Reservation{}, newError("rescue.budget_restore", domain.ErrorRPCInvalidResponse, codeBalanceDecode, true, true, nil)
	}
	maximum := session.coordinator.feePolicy.TransactionCostCap
	if incident.Kind == domain.CandidateToken && !incident.Trusted {
		maximum = session.coordinator.feePolicy.UnknownTokenCostCap
	} else if incident.Kind == domain.CandidateToken {
		value, ok := session.coordinator.trustedTokenValues[incident.Asset]
		if !ok {
			return budget.Reservation{}, newError("rescue.budget_restore", domain.ErrorConfiguration, codeMinimumValue, false, true, nil)
		}
		maximum = value.MaximumCost
	}
	reservation, err = session.coordinator.budget.Reserve(ctx, budget.ReservationRequest{
		Network: session.coordinator.network.ChainID, Sponsor: session.coordinator.sponsor,
		Candidate: incident.Candidate, Attempt: attempt,
		Quote:          budget.CostQuote{GasLimit: transaction.Gas(), MaxFeePerGas: *feeU256, MaximumCost: maximum},
		SponsorBalance: *balanceU256,
		ObservedAt:     session.blockTime(block),
	})
	if err != nil {
		return budget.Reservation{}, session.budgetError("rescue.budget_restore", err)
	}
	return reservation, session.updateBudgetMetrics(ctx)
}

func (session *Session) persistAmbiguous(ctx context.Context, incident *store.RescueIncident, failure error) error {
	return session.persistAmbiguousCode(ctx, incident, errorCode(failure))
}

func (session *Session) persistAmbiguousCode(ctx context.Context, incident *store.RescueIncident, code domain.ErrorCode) error {
	now := session.coordinator.clock.Now()
	wasAmbiguous := incident.Status == store.RescueAmbiguous
	if !now.Before(incident.ReconcileUntil) {
		return session.persistExpiredAmbiguous(ctx, incident)
	}
	incident.Status = store.RescueAmbiguous
	if nonceBlockingCode(incident.LastCode) && !nonceBlockingCode(code) {
		code = incident.LastCode
	}
	incident.LastCode = code
	retryAt := now.Add(incident.Policy.RetryDelay)
	if retryAt.After(incident.ReconcileUntil) {
		retryAt = incident.ReconcileUntil
	}
	incident.RetryAt = retryAt
	if err := session.updateIncidentAt(ctx, incident, now); err != nil {
		return err
	}
	if !wasAmbiguous && session.coordinator.metrics != nil {
		_ = session.coordinator.metrics.OpenAmbiguous(session.coordinator.network.ChainID)
	}
	return session.refreshAmbiguousTelemetry(ctx)
}

func nonceBlockingCode(code domain.ErrorCode) bool {
	return code == codeBroadcast || code == CodeLeaseLost || code == codeSignerMismatch || code == codeContextCanceled || code == codeSignedPayloadInvalid
}

func (session *Session) persistExpiredAmbiguous(ctx context.Context, incident *store.RescueIncident) error {
	if incident.Status == store.RescueAmbiguous && incident.LastCode == codeReceiptTimeout && incident.RetryAt.IsZero() {
		return nil
	}
	updatedAt := incident.ReconcileUntil.Add(-time.Nanosecond)
	if updatedAt.Before(incident.UpdatedAt) {
		updatedAt = incident.UpdatedAt
	}
	incident.Status = store.RescueAmbiguous
	incident.LastCode = codeReceiptTimeout
	incident.RetryAt = time.Time{}
	return session.updateIncidentAt(ctx, incident, updatedAt)
}

func (session *Session) refreshNonceBlock(ctx context.Context) error {
	incidents, err := session.coordinator.state.RescueIncidents(ctx, session.coordinator.network.ChainID)
	if err != nil {
		session.coordinator.nonceBlocked = true
		return newError("rescue.nonce_block", domain.ErrorInternal, codeStateRead, true, true, err)
	}
	now := session.coordinator.clock.Now()
	blocked := false
	for _, incident := range incidents {
		switch incident.Status {
		case store.RescueSigned:
			blocked = true
		case store.RescueAmbiguous:
			if !now.Before(incident.ReconcileUntil) || nonceBlockingCode(incident.LastCode) {
				blocked = true
			}
		}
		if blocked {
			break
		}
	}
	session.coordinator.nonceBlocked = blocked
	return nil
}

func (session *Session) refreshAmbiguousTelemetry(ctx context.Context) error {
	coordinator := session.coordinator
	incidents, err := coordinator.state.RescueIncidents(ctx, coordinator.network.ChainID)
	if err != nil {
		return newError("rescue.ambiguity_state", domain.ErrorInternal, codeStateRead, true, true, err)
	}
	var active uint64
	for _, incident := range incidents {
		if incident.Status == store.RescueAmbiguous {
			active++
		}
	}
	if coordinator.metrics != nil {
		if err := coordinator.metrics.SetActiveAmbiguous(coordinator.network.ChainID, active); err != nil {
			return newError("rescue.ambiguity_metrics", domain.ErrorInternal, codeStateWrite, true, true, err)
		}
	}
	if coordinator.health != nil {
		_ = coordinator.health.SetCondition(coordinator.network.ChainID, observability.ConditionAmbiguousRescue, active > 0)
	}
	if coordinator.alerts != nil {
		var alertErr error
		if active > 0 {
			_, alertErr = coordinator.alerts.Raise(coordinator.network.ChainID, observability.AlertAmbiguousRescue)
		} else {
			_, alertErr = coordinator.alerts.Resolve(coordinator.network.ChainID, observability.AlertAmbiguousRescue)
		}
		if alertErr != nil {
			return newError("rescue.ambiguity_alert", domain.ErrorInternal, codeStateWrite, true, true, alertErr)
		}
	}
	return nil
}

func (session *Session) recordBroadcast(incident *store.RescueIncident) {
	publicHash, err := observability.NewPublicTxHashAfterBroadcast(incident.TxHash)
	if err != nil {
		return
	}
	session.coordinator.recordSafe(observability.SafeEvent{
		Level: observability.LevelInfo, Code: observability.LogBroadcastAccepted,
		ChainID: session.coordinator.network.ChainID, Incident: incident.ID,
		State: observability.DurableBroadcast, Result: observability.ResultBroadcast,
		TxHash: publicHash,
	})
}

func (session *Session) recordTerminal(incident *store.RescueIncident, status store.RescueStatus) {
	result := observability.ResultFailed
	state := observability.DurableFailed
	switch status {
	case store.RescueTrustedSuccess:
		result, state = observability.ResultConfirmed, observability.DurableConfirmed
	case store.RescueTokenReported:
		result, state = observability.ResultTokenReported, observability.DurableConfirmed
	case store.RescueLostRace:
		result = observability.ResultLostRace
	}
	event := observability.SafeEvent{
		Level: observability.LevelInfo, Code: observability.LogStateTransition,
		ChainID: session.coordinator.network.ChainID, Incident: incident.ID,
		State: state, Result: result,
	}
	if incident.TxHash != (common.Hash{}) {
		if hash, err := observability.NewPublicTxHashAfterBroadcast(incident.TxHash); err == nil {
			event.TxHash = hash
		}
	}
	session.coordinator.recordSafe(event)
}

func (session *Session) waitReceipt(ctx context.Context, hash common.Hash, reconcileUntil time.Time) (*types.Receipt, error) {
	now := session.coordinator.clock.Now()
	deadline := now.Add(session.coordinator.receiptTimeout)
	if !reconcileUntil.IsZero() && reconcileUntil.Before(deadline) {
		deadline = reconcileUntil
	}
	for {
		receipt, err := session.finality.Receipt(ctx, hash)
		if err == nil {
			if receipt == nil {
				return nil, newError("rescue.receipt", domain.ErrorRPCInvalidResponse, codeReceiptInvalid, true, true, nil)
			}
			return receipt, nil
		}
		if ctx.Err() != nil {
			return nil, contextOrError(ctx, "rescue.receipt", domain.ErrorRPCTransient, codeContextCanceled, true, true, err)
		}
		now = session.coordinator.clock.Now()
		if !now.Before(deadline) {
			return nil, newError("rescue.receipt", domain.ErrorRPCTransient, codeReceiptTimeout, true, true, nil)
		}
		delay := receiptPollingInterval
		if remaining := deadline.Sub(now); remaining < delay {
			delay = remaining
		}
		if err := session.coordinator.clock.Sleep(ctx, delay); err != nil {
			return nil, contextOrError(ctx, "rescue.receipt", domain.ErrorRPCTransient, codeContextCanceled, true, true, err)
		}
	}
}

func (session *Session) applyReceipt(ctx context.Context, incident *store.RescueIncident, receipt *types.Receipt) error {
	if receipt == nil || receipt.TxHash != incident.TxHash || receipt.BlockHash == (common.Hash{}) || receipt.BlockNumber == nil ||
		!receipt.BlockNumber.IsUint64() || receipt.Status > types.ReceiptStatusSuccessful || receipt.GasUsed == 0 ||
		receipt.EffectiveGasPrice == nil || receipt.EffectiveGasPrice.Sign() <= 0 || receipt.EffectiveGasPrice.BitLen() > 256 {
		failure := newError("rescue.receipt", domain.ErrorRPCInvalidResponse, codeReceiptInvalid, true, true, nil)
		if err := session.persistAmbiguous(ctx, incident, failure); err != nil {
			return err
		}
		return failure
	}
	postBlock, err := session.canonicalProofStart(ctx, incident, receipt)
	if err != nil {
		if persistErr := session.persistAmbiguous(ctx, incident, err); persistErr != nil {
			return persistErr
		}
		return err
	}
	if err := session.canonicalProofEnd(ctx, incident, receipt, postBlock); err != nil {
		if persistErr := session.persistAmbiguous(ctx, incident, err); persistErr != nil {
			return persistErr
		}
		return err
	}
	session.rpcSucceeded = true
	if err := session.commitReceiptCost(ctx, incident, receipt, postBlock); err != nil {
		return session.postconditionAmbiguous(ctx, incident, err)
	}
	if receipt.Status == types.ReceiptStatusFailed {
		if err := session.setTerminal(ctx, incident, store.RescueFailed, codeReverted); err != nil {
			return err
		}
		return newError("rescue.receipt", domain.ErrorPostcondition, codeReverted, false, false, nil)
	}

	code, err := session.finality.CodeAt(ctx, postBlock, session.coordinator.source)
	if err != nil {
		if session.coordinator.metrics != nil {
			_ = session.coordinator.metrics.SetDelegationState(session.coordinator.network.ChainID, observability.DelegationUnknown)
		}
		return session.postconditionAmbiguous(ctx, incident, contextOrError(ctx, "rescue.post_code", domain.ErrorRPCTransient, codeCodeRead, true, true, err))
	}
	target, parseErr := contracts.ParseDelegation(code)
	if parseErr != nil || target != session.coordinator.rescuer {
		if session.coordinator.metrics != nil {
			_ = session.coordinator.metrics.SetDelegationState(session.coordinator.network.ChainID, observability.DelegationUnexpected)
		}
		if session.coordinator.alerts != nil {
			if _, alertErr := session.coordinator.alerts.Raise(session.coordinator.network.ChainID, observability.AlertDelegationUnexpected); alertErr != nil {
				return session.postconditionAmbiguous(ctx, incident, newError("rescue.delegation_alert", domain.ErrorInternal, codeStateWrite, true, true, alertErr))
			}
		}
		if err := session.canonicalProofEnd(ctx, incident, receipt, postBlock); err != nil {
			return session.postconditionAmbiguous(ctx, incident, err)
		}
		if err := session.setTerminal(ctx, incident, store.RescueLostRace, codeLostRace); err != nil {
			return err
		}
		return newError("rescue.post_code", domain.ErrorPostcondition, codeLostRace, false, false, nil)
	}
	if session.coordinator.metrics != nil {
		_ = session.coordinator.metrics.SetDelegationState(session.coordinator.network.ChainID, observability.DelegationExpected)
	}
	if session.coordinator.alerts != nil {
		if _, alertErr := session.coordinator.alerts.Resolve(session.coordinator.network.ChainID, observability.AlertDelegationUnexpected); alertErr != nil {
			return session.postconditionAmbiguous(ctx, incident, newError("rescue.delegation_alert", domain.ErrorInternal, codeStateWrite, true, true, alertErr))
		}
	}

	var sourceAfter, destinationAfter *big.Int
	if incident.Kind == domain.CandidateToken {
		sourceAfter, err = session.tokenBalanceAt(ctx, postBlock, incident.Asset, session.coordinator.source)
		if err == nil {
			destinationAfter, err = session.tokenBalanceAt(ctx, postBlock, incident.Asset, session.coordinator.destination)
		}
	} else {
		sourceAfter, err = session.nativeBalanceAt(ctx, postBlock, session.coordinator.source)
		if err == nil {
			destinationAfter, err = session.nativeBalanceAt(ctx, postBlock, session.coordinator.destination)
		}
	}
	if err != nil {
		return session.postconditionAmbiguous(ctx, incident, err)
	}
	if err := session.canonicalProofEnd(ctx, incident, receipt, postBlock); err != nil {
		return session.postconditionAmbiguous(ctx, incident, err)
	}
	sourceBefore := new(big.Int).SetBytes(incident.SourceBefore[:])
	destinationBefore := new(big.Int).SetBytes(incident.DestinationBefore[:])
	deltaValid := destinationAfter.Cmp(destinationBefore) >= 0 && new(big.Int).Sub(destinationAfter, destinationBefore).Cmp(sourceBefore) >= 0
	if sourceAfter.Sign() != 0 || !deltaValid {
		if err := session.setTerminal(ctx, incident, store.RescueLostRace, codePostcondition); err != nil {
			return err
		}
		return newError("rescue.post_balance", domain.ErrorPostcondition, codePostcondition, false, false, nil)
	}
	status := store.RescueTrustedSuccess
	if incident.Kind == domain.CandidateToken && !incident.Trusted {
		status = store.RescueTokenReported
	}
	if err := session.setTerminal(ctx, incident, status, ""); err != nil {
		return err
	}
	session.coordinator.record(eventOperationConfirmed, observability.LevelInfo, incident.Candidate, incident.TxHash, "")
	return nil
}

func (session *Session) commitReceiptCost(ctx context.Context, incident *store.RescueIncident, receipt *types.Receipt, postBlock rpc.BlockRef) error {
	if session.coordinator.feePolicy.UnboundedAdditionalFees {
		return newError("rescue.budget_actual", domain.ErrorConfiguration, codeInvalidConfig, false, true, nil)
	}
	transaction, err := session.validatePersistedTransaction(incident)
	if err != nil {
		return err
	}
	if receipt.GasUsed > transaction.Gas() {
		return newError("rescue.budget_actual", domain.ErrorRPCInvalidResponse, codeReceiptInvalid, true, true, nil)
	}
	fee, overflow := uint256.FromBig(receipt.EffectiveGasPrice)
	if overflow {
		return newError("rescue.budget_actual", domain.ErrorRPCInvalidResponse, codeReceiptInvalid, true, true, nil)
	}
	executionCost, err := maximumFeeCost(receipt.GasUsed, fee, &uint256.Int{})
	if err != nil {
		return session.budgetError("rescue.budget_actual", err)
	}
	reservation, err := session.ensureReservation(ctx, incident, transaction)
	if err != nil {
		return err
	}
	if reservation.State == budget.ReservationHeld {
		reservation, err = session.coordinator.budget.MarkExposed(ctx, reservation.ID, incident.TxHash)
		if err != nil {
			return session.budgetError("rescue.budget_receipt_expose", err)
		}
	}
	_, err = session.coordinator.budget.CommitFinalized(ctx, budget.FinalizedCharge{
		ReservationID: reservation.ID,
		TxHash:        incident.TxHash,
		Actual:        executionCost,
		ObservedAt:    session.blockTime(postBlock),
	})
	if err != nil {
		return session.budgetError("rescue.budget_commit", err)
	}
	return session.updateBudgetMetrics(ctx)
}

func (session *Session) blockTime(block rpc.BlockRef) time.Time {
	if block.Timestamp == 0 {
		return session.coordinator.clock.Now().UTC()
	}
	return time.Unix(int64(block.Timestamp), 0).UTC()
}

func (session *Session) canonicalProofStart(ctx context.Context, incident *store.RescueIncident, receipt *types.Receipt) (rpc.BlockRef, error) {
	postBlock, err := session.finality.Finalized(ctx)
	if err != nil || postBlock.Hash == (common.Hash{}) || postBlock.Number < incident.SnapshotBlockNumber || postBlock.Number < receipt.BlockNumber.Uint64() {
		return rpc.BlockRef{}, contextOrError(ctx, "rescue.post_finalized", domain.ErrorRPCTransient, codeFinalityRead, true, true, err)
	}
	if err := session.requireCanonicalHeader(ctx, incident.SnapshotBlockNumber, incident.SnapshotBlockHash); err != nil {
		return rpc.BlockRef{}, err
	}
	if err := session.requireCanonicalHeader(ctx, receipt.BlockNumber.Uint64(), receipt.BlockHash); err != nil {
		return rpc.BlockRef{}, err
	}
	return postBlock, nil
}

func (session *Session) canonicalProofEnd(ctx context.Context, incident *store.RescueIncident, receipt *types.Receipt, expectedFinalized rpc.BlockRef) error {
	if err := session.requireCanonicalHeader(ctx, incident.SnapshotBlockNumber, incident.SnapshotBlockHash); err != nil {
		return err
	}
	if err := session.requireCanonicalHeader(ctx, receipt.BlockNumber.Uint64(), receipt.BlockHash); err != nil {
		return err
	}
	finalized, err := session.finality.Finalized(ctx)
	if err != nil || finalized != expectedFinalized {
		return contextOrError(ctx, "rescue.post_finalized_recheck", domain.ErrorRPCTransient, codeFinalityRead, true, true, err)
	}
	return nil
}

func (session *Session) requireCanonicalHeader(ctx context.Context, number uint64, hash common.Hash) error {
	header, err := session.finality.Header(ctx, number)
	if err != nil || header.Number != number || header.Hash != hash {
		return contextOrError(ctx, "rescue.canonical_header", domain.ErrorRPCTransient, codeFinalityRead, true, true, err)
	}
	return nil
}

func (session *Session) postconditionAmbiguous(ctx context.Context, incident *store.RescueIncident, failure error) error {
	if err := session.persistAmbiguous(ctx, incident, failure); err != nil {
		return err
	}
	return failure
}

func (session *Session) setTerminal(ctx context.Context, incident *store.RescueIncident, status store.RescueStatus, code domain.ErrorCode) error {
	previous := incident.Status
	incident.Status = status
	incident.SignedTransaction = nil
	incident.RetryAt = time.Time{}
	incident.ReconcileUntil = time.Time{}
	incident.LastCode = code
	if err := session.updateIncident(ctx, incident); err != nil {
		return err
	}
	if err := session.refreshNonceBlock(ctx); err != nil {
		return err
	}
	if previous == store.RescueAmbiguous {
		if err := session.refreshAmbiguousTelemetry(ctx); err != nil {
			return err
		}
	}
	if status == store.RescueLostRace {
		if session.coordinator.metrics != nil {
			_ = session.coordinator.metrics.RecordLostRace(session.coordinator.network.ChainID)
		}
	}
	session.recordTerminal(incident, status)
	return session.pruneTerminal(ctx)
}

func (session *Session) pruneTerminal(ctx context.Context) error {
	if err := session.coordinator.state.PruneRescueIncidents(ctx, rescueIncidentRetain); err != nil {
		return newError("rescue.state_prune", domain.ErrorInternal, codeStateWrite, true, true, err)
	}
	return nil
}

func (session *Session) updateIncident(ctx context.Context, incident *store.RescueIncident) error {
	return session.updateIncidentAt(ctx, incident, session.coordinator.clock.Now())
}

func (session *Session) updateIncidentAt(ctx context.Context, incident *store.RescueIncident, now time.Time) error {
	incident.UpdatedAt = now
	if incident.Status == store.RescueRetryable {
		incident.RetryAt = now.Add(incident.Policy.RetryDelay)
	}
	if err := session.coordinator.state.UpdateRescueIncident(ctx, *incident); err != nil {
		return newError("rescue.state_update", domain.ErrorInternal, codeStateWrite, true, true, err)
	}
	return nil
}

func (session *Session) tokenBalanceAt(ctx context.Context, block rpc.BlockRef, token, account common.Address) (*big.Int, error) {
	data, err := session.coordinator.erc20.PackBalanceOf(account)
	if err != nil {
		return nil, newError("rescue.token_balance", domain.ErrorInternal, codeEncoding, false, false, err)
	}
	result, err := session.finality.CallContract(ctx, block, ethereum.CallMsg{To: &token, Data: data})
	if err != nil {
		return nil, contextOrError(ctx, "rescue.token_balance", domain.ErrorRPCTransient, codeBalanceRead, true, true, err)
	}
	balance, err := session.coordinator.erc20.DecodeBalanceOf(result)
	if err != nil {
		return nil, newError("rescue.token_balance", domain.ErrorRPCInvalidResponse, codeBalanceDecode, true, true, err)
	}
	if _, err := amountBytes(balance); err != nil {
		return nil, newError("rescue.token_balance", domain.ErrorRPCInvalidResponse, codeBalanceDecode, true, true, err)
	}
	return balance, nil
}

func (session *Session) nativeBalanceAt(ctx context.Context, block rpc.BlockRef, account common.Address) (*big.Int, error) {
	balance, err := session.finality.BalanceAt(ctx, block, account)
	if err != nil {
		return nil, contextOrError(ctx, "rescue.native_balance", domain.ErrorRPCTransient, codeBalanceRead, true, true, err)
	}
	if _, err := amountBytes(balance); err != nil {
		return nil, newError("rescue.native_balance", domain.ErrorRPCInvalidResponse, codeBalanceDecode, true, true, err)
	}
	return balance, nil
}

func amountBytes(amount *big.Int) ([32]byte, error) {
	var encoded [32]byte
	if amount == nil || amount.Sign() < 0 || amount.BitLen() > 256 {
		return encoded, newError("rescue.amount", domain.ErrorRPCInvalidResponse, codeBalanceDecode, true, true, nil)
	}
	amount.FillBytes(encoded[:])
	return encoded, nil
}

func publicTransactionHash(incident store.RescueIncident) common.Hash {
	if incident.Status == store.RescueBroadcast || incident.Status == store.RescueTrustedSuccess || incident.Status == store.RescueTokenReported || incident.Status == store.RescueLostRace ||
		(incident.Status == store.RescueAmbiguous && incident.LastCode != codeBroadcast) {
		return incident.TxHash
	}
	return common.Hash{}
}

func (session *Session) pendingNonce(ctx context.Context, address common.Address) (uint64, error) {
	delays := [...]time.Duration{0, 300 * time.Millisecond, 800 * time.Millisecond}
	var lastError error
	for _, delay := range delays {
		if delay > 0 {
			if err := session.coordinator.clock.Sleep(ctx, delay); err != nil {
				return 0, contextOrError(ctx, "rescue.nonce", domain.ErrorRPCTransient, codeContextCanceled, true, true, err)
			}
		}
		nonce, err := session.reader.PendingNonceAt(ctx, address)
		if err == nil {
			return nonce, nil
		}
		lastError = err
		if ctx.Err() != nil {
			return 0, contextOrError(ctx, "rescue.nonce", domain.ErrorRPCTransient, codeContextCanceled, true, true, err)
		}
		if !isRateLimitError(err) {
			return 0, newError("rescue.nonce", domain.ErrorRPCTransient, codeNonceRead, true, false, err)
		}
	}
	return 0, newError("rescue.nonce", domain.ErrorRPCTransient, codeNonceRead, true, false, lastError)
}

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "429") || strings.Contains(message, "Too Many Requests")
}

func (session *Session) readFees(ctx context.Context, asset FeeAsset) (FeeQuote, error) {
	readContext, cancel := context.WithTimeout(ctx, session.coordinator.feeReadTimeout)
	defer cancel()

	head, err := session.reader.HeaderByNumber(readContext, nil)
	if err != nil {
		return FeeQuote{}, contextOrError(readContext, "rescue.fees_header", domain.ErrorRPCTransient, codeFeeRead, true, true, err)
	}
	if head == nil || head.BaseFee == nil || head.BaseFee.Sign() <= 0 {
		return FeeQuote{}, newError("rescue.fees_header", domain.ErrorRPCInvalidResponse, codeFeeInvalid, true, true, nil)
	}
	gasPrice, err := session.reader.SuggestGasPrice(readContext)
	if err != nil {
		return FeeQuote{}, contextOrError(readContext, "rescue.fees_gas_price", domain.ErrorRPCTransient, codeFeeRead, true, true, err)
	}
	if gasPrice == nil || gasPrice.Sign() <= 0 {
		return FeeQuote{}, newError("rescue.fees_gas_price", domain.ErrorRPCInvalidResponse, codeFeeInvalid, true, true, nil)
	}

	quote, err := BuildFeeQuote(session.coordinator.feePolicy, asset, head.BaseFee, gasPrice)
	if err != nil {
		return FeeQuote{}, newError("rescue.fees_policy", domain.ErrorRPCInvalidResponse, codeFeeInvalid, true, false, err)
	}
	return quote, nil
}
