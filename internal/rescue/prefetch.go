package rescue

import (
	"context"
	"math/big"
	"time"

	"guard-daemon/internal/budget"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/rpc"
	"guard-daemon/internal/store"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const maxAttemptSnapshotAge = 5 * time.Second

type latestReader interface {
	Latest(context.Context) (rpc.BlockRef, error)
	MinedReceipt(context.Context, common.Hash) (*types.Receipt, error)
}

type attemptSnapshot struct {
	incident                     domain.IncidentID
	attempt                      uint32
	started                      time.Time
	block                        rpc.BlockRef
	reader                       FinalityReader
	source, destination, sponsor *big.Int
	sponsorNonce                 uint64
	fees                         FeeQuote
}

// parallelReads cancels siblings on failure and joins them before returning, so
// callers can safely inspect the outputs without racing abandoned goroutines.
func parallelReads(ctx context.Context, reads ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(reads))
	for _, read := range reads {
		go func() { results <- read(ctx) }()
	}
	var first error
	for range reads {
		if err := <-results; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	return first
}

func (session *Session) preflightBlock(ctx context.Context) (rpc.BlockRef, error) {
	if session.coordinator.latestState {
		return session.finality.(latestReader).Latest(ctx)
	}
	return session.finality.Finalized(ctx)
}

func (session *Session) prefetchAttempt(ctx context.Context, incident *store.RescueIncident, attempt uint32) (*attemptSnapshot, error) {
	data, asset, err := session.operationData(incident)
	if err != nil {
		return nil, err
	}
	snapshot := &attemptSnapshot{incident: incident.ID, attempt: attempt, started: session.coordinator.clock.Now(), reader: session.finality}
	err = parallelReads(ctx,
		func(ctx context.Context) error {
			var err error
			reader, ok := session.finality.(latestReader)
			if !ok {
				return rpc.ErrInvalidProviders
			}
			snapshot.block, err = reader.Latest(ctx)
			return err
		},
		func(ctx context.Context) error {
			var err error
			snapshot.fees, err = session.readFees(ctx, asset)
			return err
		},
		func(ctx context.Context) error {
			var err error
			snapshot.sponsorNonce, err = session.sponsorNonce(ctx)
			return err
		},
		func(ctx context.Context) error { return session.checkFastInFlight(ctx, incident) },
	)
	if err != nil {
		return nil, err
	}
	if snapshot.block.Hash == (common.Hash{}) {
		return nil, rpc.ErrInvalidBlockRef
	}
	if reader, ok := session.finality.(interface {
		Pin(rpc.BlockRef) *rpc.PinnedState
	}); ok {
		snapshot.reader = reader.Pin(snapshot.block)
	}
	readBalance := func(ctx context.Context, account common.Address) (*big.Int, error) {
		if !isTokenOperation(incident.Kind) {
			return snapshot.reader.BalanceAt(ctx, snapshot.block, account)
		}
		data, err := session.coordinator.erc20.PackBalanceOf(account)
		if err != nil {
			return nil, err
		}
		result, err := snapshot.reader.CallContract(ctx, snapshot.block, ethereum.CallMsg{To: &incident.Asset, Data: data})
		if err != nil {
			return nil, err
		}
		return session.coordinator.erc20.DecodeBalanceOf(result)
	}
	reads := []func(context.Context) error{
		func(ctx context.Context) error {
			var err error
			snapshot.source, err = readBalance(ctx, session.coordinator.source)
			return err
		},
		func(ctx context.Context) error {
			var err error
			snapshot.destination, err = readBalance(ctx, session.coordinator.destination)
			return err
		},
		func(ctx context.Context) error {
			var err error
			snapshot.sponsor, err = snapshot.reader.BalanceAt(ctx, snapshot.block, session.coordinator.sponsor)
			return err
		},
	}
	if session.coordinator.feePolicy.EstimatedBaseFees {
		gas := snapshot.fees.GasLimit
		if incident.Kind == domain.CandidateClaim {
			gas = session.coordinator.claim.GasLimit
		}
		reads = append(reads, func(ctx context.Context) error {
			extra, err := rpc.EstimateBaseAdditionalFee(ctx, snapshot.reader, snapshot.block, len(data), gas)
			if err != nil {
				return err
			}
			if extra.Cmp(session.coordinator.feePolicy.Overhead.ToBig()) > 0 {
				return newError("rescue.l2_fee_estimate", domain.ErrorBudget, codeBudgetExceeded, true, false, nil)
			}
			return nil
		})
	}
	if err := parallelReads(ctx, reads...); err != nil {
		return nil, err
	}
	for _, balance := range []*big.Int{snapshot.source, snapshot.destination, snapshot.sponsor} {
		if _, err := amountBytes(balance); err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}

func (session *Session) cachedAttempt(incident *store.RescueIncident) *attemptSnapshot {
	snapshot := session.attempt
	if snapshot == nil || snapshot.incident != incident.ID || snapshot.attempt != incident.Attempts {
		return nil
	}
	age := session.coordinator.clock.Now().Sub(snapshot.started)
	if age < 0 || age > maxAttemptSnapshotAge {
		return nil
	}
	return snapshot
}

func (session *Session) restartPreparedAttempt(ctx context.Context, incident *store.RescueIncident) error {
	reservation, found, err := session.coordinator.budget.ReservationByAttempt(ctx, budget.Attempt{Incident: incident.ID, Number: incident.Attempts})
	if err != nil {
		return err
	}
	if found {
		if _, err := session.coordinator.budget.ReleaseProvenUnused(ctx, reservation.ID); err != nil {
			return err
		}
	}
	return session.beforeSignedFailure(ctx, incident, newError("rescue.snapshot_expired", domain.ErrorRPCTransient, codeRetryPending, true, false, nil))
}
