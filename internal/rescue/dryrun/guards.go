package dryrun

import (
	"context"
	"math/big"
	"sync/atomic"

	"guard-daemon/internal/domain"
	"guard-daemon/internal/rescue"
	"guard-daemon/internal/rpc"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	codeSigningForbidden   domain.ErrorCode = "dry_run_signing_forbidden"
	codeBroadcastForbidden domain.ErrorCode = "dry_run_broadcast_forbidden"
)

type Attempts struct {
	authorizationSignatures atomic.Uint64
	transactionSignatures   atomic.Uint64
	broadcasts              atomic.Uint64
}

func (attempts *Attempts) AuthorizationSignatures() uint64 {
	return attempts.authorizationSignatures.Load()
}

func (attempts *Attempts) TransactionSignatures() uint64 {
	return attempts.transactionSignatures.Load()
}

func (attempts *Attempts) Broadcasts() uint64 {
	return attempts.broadcasts.Load()
}

type authorizationSignerGuard struct {
	address  common.Address
	attempts *Attempts
}

func (guard *authorizationSignerGuard) Address() common.Address {
	return guard.address
}

func (guard *authorizationSignerGuard) SignAuthorization(context.Context, types.SetCodeAuthorization) (types.SetCodeAuthorization, error) {
	guard.attempts.authorizationSignatures.Add(1)
	return types.SetCodeAuthorization{}, newError("dry_run.sign_authorization", domain.ErrorSigning, codeSigningForbidden, false)
}

type transactionSignerGuard struct {
	address  common.Address
	attempts *Attempts
}

func (guard *transactionSignerGuard) Address() common.Address {
	return guard.address
}

func (guard *transactionSignerGuard) SignTransaction(context.Context, *types.Transaction, *big.Int) (*types.Transaction, error) {
	guard.attempts.transactionSignatures.Add(1)
	return nil, newError("dry_run.sign_transaction", domain.ErrorSigning, codeSigningForbidden, false)
}

type broadcasterGuard struct {
	attempts *Attempts
}

func (guard *broadcasterGuard) SendTransaction(context.Context, *types.Transaction) error {
	guard.attempts.broadcasts.Add(1)
	return newError("dry_run.broadcast", domain.ErrorBroadcast, codeBroadcastForbidden, false)
}

func NewGuards(source, sponsor common.Address) (rescue.AuthorizationSigner, rescue.TransactionSigner, rpc.Broadcaster, *Attempts) {
	attempts := &Attempts{}
	return &authorizationSignerGuard{address: source, attempts: attempts},
		&transactionSignerGuard{address: sponsor, attempts: attempts},
		&broadcasterGuard{attempts: attempts},
		attempts
}

var _ rescue.AuthorizationSigner = (*authorizationSignerGuard)(nil)
var _ rescue.TransactionSigner = (*transactionSignerGuard)(nil)
var _ rpc.Broadcaster = (*broadcasterGuard)(nil)
