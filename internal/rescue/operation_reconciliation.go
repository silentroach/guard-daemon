package rescue

import (
	"context"
	"errors"
	"time"

	"guard-daemon/internal/budget"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

const receiptPollingInterval = 400 * time.Millisecond

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
