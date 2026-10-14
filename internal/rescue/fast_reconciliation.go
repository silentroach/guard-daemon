package rescue

import (
	"context"
	"errors"
	"time"

	"guard-daemon/internal/domain"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum/core/types"
)

const maxUnfinalizedOperations = 16

type fastPendingError struct {
	error
	next time.Time
}

func (err fastPendingError) Unwrap() error          { return err.error }
func (err fastPendingError) NextRetryAt() time.Time { return err.next }

func (session *Session) fastPending() error {
	return fastPendingError{error: retryPendingError(true), next: session.coordinator.clock.Now().Add(time.Second)}
}

// Keep at most one unresolved operation per asset. Another asset may proceed once
// the highest allocated sponsor nonce is included on both RPCs. Reservations for
// all of those transactions remain held until finality.
func (session *Session) checkFastInFlight(ctx context.Context, current *store.RescueIncident) error {
	incidents, err := session.coordinator.state.RescueIncidents(ctx, session.coordinator.network.ChainID)
	if err != nil {
		return err
	}
	count := 0
	var highest *store.RescueIncident
	for index := range incidents {
		incident := &incidents[index]
		if !statusNeedsReceipt(incident.Status) {
			continue
		}
		count++
		if incident.Asset == current.Asset {
			return session.fastPending()
		}
		if highest == nil || incident.SponsorNonce > highest.SponsorNonce {
			highest = incident
		}
	}
	if count >= maxUnfinalizedOperations {
		return session.fastPending()
	}
	if highest != nil {
		receipt, err := session.finality.(latestReader).MinedReceipt(ctx, highest.TxHash)
		if err != nil || receipt == nil || receipt.TxHash != highest.TxHash {
			return session.fastPending()
		}
	}
	return nil
}

func (session *Session) reconcileUnfinalized(ctx context.Context, incident *store.RescueIncident, transaction *types.Transaction) error {
	if incident.Status == store.RescueBroadcast {
		return nil
	}
	if receipt, err := session.finality.(latestReader).MinedReceipt(ctx, incident.TxHash); err == nil && receipt != nil && receipt.TxHash == incident.TxHash {
		incident.Status = store.RescueBroadcast
		incident.LastCode = ""
		incident.RetryAt = time.Time{}
		if err := session.updateIncident(ctx, incident); err != nil {
			return err
		}
		session.recordBroadcast(incident)
		return session.refreshNonceBlock(ctx)
	}
	// A failed/unknown send retries exactly the durable bytes, never a new nonce.
	return session.broadcastAndWait(ctx, incident, transaction)
}

// Fetch final receipts outside the signing mutex. Finality is polled once per
// pass, and unchanged finalized heads do not cause receipt re-fetching.
func (session *Session) reconcileFast(ctx context.Context) error {
	incidents, err := session.coordinator.state.RescueIncidents(ctx, session.coordinator.network.ChainID)
	if err != nil {
		return newError("rescue.reconciliation_state", domain.ErrorInternal, codeStateRead, true, true, err)
	}
	if err := session.reconcileFastPending(ctx, incidents); err != nil {
		return err
	}
	finalized, err := session.finality.Finalized(ctx)
	if err != nil {
		return session.recordReconciliation(false)
	}
	if finalized == session.lastReceiptScan {
		return session.recordReconciliation(true)
	}
	var active []store.RescueIncident
	for _, incident := range incidents {
		if statusNeedsReceipt(incident.Status) {
			active = append(active, incident)
		}
	}
	limited := len(active) > maxUnfinalizedOperations
	if limited {
		active = active[:maxUnfinalizedOperations]
	}
	receipts := make([]*types.Receipt, len(active))
	retry := make([]bool, len(active))
	reads := make([]func(context.Context) error, len(active))
	for index, incident := range active {
		reads[index] = func(ctx context.Context) error {
			receipt, err := session.finality.Receipt(ctx, incident.TxHash)
			if err == nil {
				receipts[index] = receipt
			}
			retry[index] = (err != nil && !errors.Is(err, rpc.ErrUnfinalizedReceipt)) || (err == nil && receipt == nil)
			return nil
		}
	}
	if err := parallelReads(ctx, reads...); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	coordinator := session.coordinator
	coordinator.operationMu.Lock()
	defer coordinator.operationMu.Unlock()
	if err := coordinator.validateProcessFence(); err != nil {
		return err
	}
	if err := coordinator.validateLease(ctx); err != nil {
		return err
	}
	complete := !limited
	for _, pending := range retry {
		if pending {
			complete = false
		}
	}
	for index, receipt := range receipts {
		if receipt == nil {
			continue
		}
		incident, found, err := coordinator.state.RescueIncident(ctx, active[index].ID)
		if err != nil {
			return err
		}
		if !found || !statusNeedsReceipt(incident.Status) || incident.TxHash != active[index].TxHash {
			continue
		}
		if incident.Status == store.RescueSigned {
			if !coordinator.clock.Now().Before(incident.ReconcileUntil) {
				if err := session.persistExpiredAmbiguous(ctx, &incident); err != nil {
					return err
				}
			} else {
				incident.Status = store.RescueAmbiguous
				incident.LastCode = codeBroadcast
				if err := session.updateIncident(ctx, &incident); err != nil {
					return err
				}
			}
		}
		if err := session.applyReceipt(ctx, &incident, receipt); err != nil {
			complete = false
			coordinator.recordIncidentFailure(&incident, err)
			if fatalReconciliationError(err) {
				return err
			}
		}
	}
	if complete {
		session.lastReceiptScan = finalized
	}
	if err := session.refreshNonceBlock(ctx); err != nil {
		return err
	}
	if err := session.refreshAmbiguousTelemetry(ctx); err != nil {
		return err
	}
	return session.recordReconciliation(complete)
}

// Queue acknowledgement hands responsibility to durable reconciliation. If an
// accepted transaction disappears or never gets included, retry its exact bytes
// after the ordinary receipt timeout, even while the finalized head is unchanged.
func (session *Session) reconcileFastPending(ctx context.Context, incidents []store.RescueIncident) error {
	now := session.coordinator.clock.Now()
	if !session.lastPendingProbe.IsZero() && now.Sub(session.lastPendingProbe) < session.coordinator.receiptTimeout {
		return nil
	}
	session.lastPendingProbe = now
	var pending []store.RescueIncident
	for _, incident := range incidents {
		if statusNeedsReceipt(incident.Status) && (incident.Status != store.RescueBroadcast || now.Sub(incident.UpdatedAt) >= session.coordinator.receiptTimeout) {
			pending = append(pending, incident)
			if len(pending) == maxUnfinalizedOperations {
				break
			}
		}
	}
	mined := make([]bool, len(pending))
	reads := make([]func(context.Context) error, len(pending))
	for index, incident := range pending {
		reads[index] = func(ctx context.Context) error {
			receipt, err := session.finality.(latestReader).MinedReceipt(ctx, incident.TxHash)
			mined[index] = err == nil && receipt != nil && receipt.TxHash == incident.TxHash
			return nil
		}
	}
	if err := parallelReads(ctx, reads...); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(pending) == 0 {
		return nil
	}
	coordinator := session.coordinator
	coordinator.operationMu.Lock()
	defer coordinator.operationMu.Unlock()
	for index, original := range pending {
		if err := coordinator.validateProcessFence(); err != nil {
			return err
		}
		if err := coordinator.validateLease(ctx); err != nil {
			return err
		}
		if mined[index] {
			continue
		}
		incident, found, err := coordinator.state.RescueIncident(ctx, original.ID)
		if err != nil {
			return err
		}
		if !found || !statusNeedsReceipt(incident.Status) || incident.TxHash != original.TxHash {
			continue
		}
		if !now.Before(incident.ReconcileUntil) {
			if err := session.persistExpiredAmbiguous(ctx, &incident); err != nil {
				return err
			}
			continue
		}
		if incident.Status == store.RescueBroadcast {
			if err := session.persistAmbiguousCode(ctx, &incident, codeReceiptTimeout); err != nil {
				return err
			}
		}
		if err := session.reconcileOnce(ctx, &incident); err != nil {
			coordinator.recordIncidentFailure(&incident, err)
			if fatalReconciliationError(err) {
				return err
			}
		}
	}
	return nil
}
