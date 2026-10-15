package rpc

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
)

// PinnedState shares one already agreed block across a burst of reads. Callers
// recheck its canonical hash before signing instead of repeating Header for each
// balance and simulation call. Every value still requires provider agreement.
type PinnedState struct {
	*QuorumReader
	block BlockRef
}

func (reader *QuorumReader) Pin(block BlockRef) *PinnedState {
	return &PinnedState{QuorumReader: reader, block: block}
}

func (reader *PinnedState) BalanceAt(ctx context.Context, block BlockRef, account common.Address) (*big.Int, error) {
	if block != reader.block || block.Hash == (common.Hash{}) {
		return nil, ErrInvalidBlockRef
	}
	return reader.balanceAtHash(ctx, block, account)
}

func (reader *PinnedState) CallContract(ctx context.Context, block BlockRef, call ethereum.CallMsg) ([]byte, error) {
	if block != reader.block || block.Hash == (common.Hash{}) {
		return nil, ErrInvalidBlockRef
	}
	return reader.callAtHash(ctx, block, call)
}
