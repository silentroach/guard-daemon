package watcher

import (
	"context"

	"guard-daemon/internal/domain"
	"guard-daemon/internal/rpc"

	"github.com/ethereum/go-ethereum/common"
)

func (service *Service) scanLive(ctx context.Context) error {
	if service.fast {
		return service.scanFast(ctx)
	}
	return service.scan(ctx)
}

func (service *Service) runFinalizedBackfill(ctx context.Context) error {
	if err := service.scan(ctx); err != nil {
		return err
	}
	ticker := service.clock.NewTicker(pollInterval)
	if ticker == nil {
		return ErrInvalidDependencies
	}
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-ticker.C():
			if !ok {
				return ErrInvalidDependencies
			}
			if err := service.scan(ctx); err != nil {
				return err
			}
		}
	}
}

// The fast cursor is only a bounded cache. Durable candidates deduplicate replay;
// the finalized scanner remains the authority for the persistent checkpoint.
func (service *Service) scanFast(ctx context.Context) error {
	reader := service.finalized.(interface {
		Latest(context.Context) (rpc.BlockRef, error)
	})
	head, err := reader.Latest(ctx)
	if err != nil {
		return service.readFailure("watcher.latest", errorFinalizedRead, err)
	}
	if head.Hash == (common.Hash{}) {
		return ErrInvalidDependencies
	}
	if service.fastSeen[head.Number] == head.Hash {
		return nil
	}
	var first uint64
	if head.Number >= service.lookbackBlocks-1 {
		first = head.Number - service.lookbackBlocks + 1
	}
	expectedHash := head.Hash
	for number := head.Number; ; number-- {
		block := head
		if number != head.Number {
			block, err = service.finalized.Header(ctx, number)
			if err != nil {
				return service.readFailure("watcher.latest_header", errorFinalizedRead, err)
			}
		}
		if block.Hash != expectedHash {
			return service.readFailure("watcher.latest_branch", errorFinalizedReorg, rpc.ErrQuorumMismatch)
		}
		if service.fastSeen[number] == block.Hash {
			break
		}
		query := service.transferQuery()
		query.BlockHash = &block.Hash
		logs, err := service.finalized.FilterLogs(ctx, query)
		if err != nil {
			return service.readFailure("watcher.latest_logs", errorLogRead, err)
		}
		for _, entry := range logs {
			candidate, ok := service.logCandidate(ctx, entry, false)
			if !ok || entry.Removed || entry.BlockHash != block.Hash || entry.BlockNumber != number {
				return service.failure("watcher.latest_log", domain.ErrorRPCInvalidResponse, errorInvalidLog, false, nil)
			}
			fast := domain.NewFastTokenCandidate(candidate.Network, candidate.Source, candidate.Token, candidate.BlockHash, candidate.TxHash, candidate.BlockNumber, candidate.LogIndex, service.dryRun)
			if err := service.putCandidate(ctx, fast); err != nil {
				return err
			}
		}
		service.fastSeen[number] = block.Hash
		expectedHash = block.ParentHash
		if number == first {
			break
		}
	}
	for number := range service.fastSeen {
		if number < first || number > head.Number {
			delete(service.fastSeen, number)
		}
	}
	return nil
}
