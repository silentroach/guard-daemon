package rescue

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"strings"
	"time"

	"guard-daemon/internal/budget"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

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
