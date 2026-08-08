package rescue

import (
	"context"
	"math/big"
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

const rescueIncidentRetain = 1024

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
	if incident.Kind == domain.CandidateToken {
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
