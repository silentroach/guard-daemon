package dryrun

import (
	"context"
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
)

func TestGuardsForbidEveryProductionCapability(t *testing.T) {
	t.Parallel()

	source := testAddress(1)
	sponsor := testAddress(2)
	authorizer, transactioner, broadcaster, attempts := NewGuards(source, sponsor)
	if authorizer.Address() != source || transactioner.Address() != sponsor {
		t.Fatal("guard Address() does not match configured role")
	}

	authorization := types.SetCodeAuthorization{Nonce: 11, V: 1}
	signedAuthorization, err := authorizer.SignAuthorization(context.Background(), authorization)
	assertErrorCode(t, err, codeSigningForbidden)
	if !reflect.DeepEqual(signedAuthorization, types.SetCodeAuthorization{}) {
		t.Fatal("authorization guard returned authorization data")
	}

	transaction := types.NewTx(&types.LegacyTx{Nonce: 12})
	signedTransaction, err := transactioner.SignTransaction(context.Background(), transaction, big.NewInt(901))
	assertErrorCode(t, err, codeSigningForbidden)
	if signedTransaction != nil {
		t.Fatal("transaction guard returned a transaction")
	}

	err = broadcaster.SendTransaction(context.Background(), transaction)
	assertErrorCode(t, err, codeBroadcastForbidden)
	if attempts.AuthorizationSignatures() != 1 || attempts.TransactionSignatures() != 1 || attempts.Broadcasts() != 1 {
		t.Fatalf("guard attempts = (%d, %d, %d), want one each", attempts.AuthorizationSignatures(), attempts.TransactionSignatures(), attempts.Broadcasts())
	}
}
