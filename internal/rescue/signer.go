package rescue

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

var errNilPrivateKey = errors.New("nil private key")

type AuthorizationSigner interface {
	Address() common.Address
	SignAuthorization(context.Context, types.SetCodeAuthorization) (types.SetCodeAuthorization, error)
}

type TransactionSigner interface {
	Address() common.Address
	SignTransaction(context.Context, *types.Transaction, *big.Int) (*types.Transaction, error)
}

type PrivateKeyAuthorizationSigner struct {
	key     *ecdsa.PrivateKey
	address common.Address
}

func NewPrivateKeyAuthorizationSigner(key *ecdsa.PrivateKey) (*PrivateKeyAuthorizationSigner, error) {
	if key == nil {
		return nil, newError("rescue.authorization_signer", domain.ErrorConfiguration, codeInvalidConfig, false, false, errNilPrivateKey)
	}
	return &PrivateKeyAuthorizationSigner{key: key, address: crypto.PubkeyToAddress(key.PublicKey)}, nil
}

func (signer *PrivateKeyAuthorizationSigner) Address() common.Address {
	return signer.address
}

func (signer *PrivateKeyAuthorizationSigner) SignAuthorization(ctx context.Context, authorization types.SetCodeAuthorization) (types.SetCodeAuthorization, error) {
	if err := ctx.Err(); err != nil {
		return types.SetCodeAuthorization{}, newError("rescue.sign_authorization", domain.ErrorSigning, codeContextCanceled, true, true, err)
	}
	signed, err := types.SignSetCode(signer.key, authorization)
	if err != nil {
		return types.SetCodeAuthorization{}, newError("rescue.sign_authorization", domain.ErrorSigning, codeSigning, false, false, err)
	}
	if err := ctx.Err(); err != nil {
		return types.SetCodeAuthorization{}, newError("rescue.sign_authorization", domain.ErrorSigning, codeContextCanceled, true, true, err)
	}
	return signed, nil
}

type PrivateKeyTransactionSigner struct {
	key     *ecdsa.PrivateKey
	address common.Address
}

func NewPrivateKeyTransactionSigner(key *ecdsa.PrivateKey) (*PrivateKeyTransactionSigner, error) {
	if key == nil {
		return nil, newError("rescue.transaction_signer", domain.ErrorConfiguration, codeInvalidConfig, false, false, errNilPrivateKey)
	}
	return &PrivateKeyTransactionSigner{key: key, address: crypto.PubkeyToAddress(key.PublicKey)}, nil
}

func (signer *PrivateKeyTransactionSigner) Address() common.Address {
	return signer.address
}

func (signer *PrivateKeyTransactionSigner) SignTransaction(ctx context.Context, transaction *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, newError("rescue.sign_transaction", domain.ErrorSigning, codeContextCanceled, true, true, err)
	}
	signed, err := types.SignTx(transaction, types.LatestSignerForChainID(chainID), signer.key)
	if err != nil {
		return nil, newError("rescue.sign_transaction", domain.ErrorSigning, codeSigning, false, false, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, newError("rescue.sign_transaction", domain.ErrorSigning, codeContextCanceled, true, true, err)
	}
	return signed, nil
}

var _ AuthorizationSigner = (*PrivateKeyAuthorizationSigner)(nil)
var _ TransactionSigner = (*PrivateKeyTransactionSigner)(nil)
