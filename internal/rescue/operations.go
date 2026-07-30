package rescue

import (
	"context"
	"math/big"
	"strings"
	"time"

	"guard-daemon/internal/contracts"
	"guard-daemon/internal/domain"
	"guard-daemon/internal/observability"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

const (
	maxRetryAttempts       = 3
	tokenSweepGas          = uint64(220_000)
	delegationRenewalGas   = uint64(60_000)
	nativeSweepGas         = uint64(80_000)
	receiptTimeout         = 60 * time.Second
	receiptPollingInterval = 400 * time.Millisecond
)

func (session *Session) handleToken(ctx context.Context, token domain.Token) error {
	coordinator := session.coordinator
	if token.Address == (common.Address{}) {
		return newError("rescue.token", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}
	balance, err := session.tokenBalance(ctx, token.Address)
	if err != nil {
		return err
	}
	if balance.Sign() == 0 {
		coordinator.record(eventOperationSkipped, observability.LevelInfo, common.Hash{}, "")
		return nil
	}
	if !coordinator.hasRescuer {
		return newError("rescue.token", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}
	return session.renewAndSweep(ctx, token)
}

func (session *Session) reconcileNative(ctx context.Context) error {
	coordinator := session.coordinator
	balance, err := session.reader.BalanceAt(ctx, coordinator.source, nil)
	if err != nil {
		return contextOrError(ctx, "rescue.native_balance", domain.ErrorRPCTransient, codeBalanceRead, true, false, err)
	}
	if balance == nil {
		return newError("rescue.native_balance", domain.ErrorRPCInvalidResponse, codeBalanceDecode, true, true, nil)
	}
	if balance.Cmp(coordinator.nativeThreshold) <= 0 {
		return nil
	}
	if !coordinator.hasRescuer {
		return newError("rescue.native", domain.ErrorConfiguration, codeInvalidConfig, false, false, nil)
	}
	delegated, err := session.isDelegated(ctx)
	if err != nil {
		return err
	}
	if !delegated {
		coordinator.record(eventOperationSkipped, observability.LevelWarning, common.Hash{}, "")
		return nil
	}
	return session.sweepNative(ctx)
}

func (session *Session) reconcilePeriodic(ctx context.Context) error {
	coordinator := session.coordinator
	var firstError error
	remember := func(err error) {
		if err != nil {
			coordinator.recordFailure(err)
			if firstError == nil {
				firstError = err
			}
		}
	}

	remember(session.reconcileNative(ctx))
	if coordinator.hasRescuer {
		remember(session.reconcileDelegation(ctx))
	}

	seen := make(map[common.Address]struct{}, len(coordinator.network.Tokens))
	for _, token := range coordinator.network.Tokens {
		seen[token.Address] = struct{}{}
		remember(session.reconcileToken(ctx, token))
	}
	for _, token := range coordinator.retrySnapshot() {
		if _, configured := seen[token.Address]; configured {
			continue
		}
		remember(session.reconcileToken(ctx, token))
	}
	return firstError
}

func (session *Session) reconcileToken(ctx context.Context, token domain.Token) error {
	balance, err := session.tokenBalance(ctx, token.Address)
	if err != nil {
		return err
	}
	if balance.Sign() == 0 {
		return nil
	}
	return session.renewAndSweep(ctx, token)
}

func (session *Session) reconcileDelegation(ctx context.Context) error {
	if !session.coordinator.hasRescuer {
		return nil
	}
	delegated, err := session.isDelegated(ctx)
	if err != nil {
		return err
	}
	if delegated {
		session.coordinator.record(eventDelegationActive, observability.LevelInfo, common.Hash{}, "")
		return nil
	}
	return session.renewDelegation(ctx)
}

func (session *Session) isDelegated(ctx context.Context) (bool, error) {
	coordinator := session.coordinator
	code, err := session.reader.CodeAt(ctx, coordinator.source, nil)
	if err != nil {
		return false, contextOrError(ctx, "rescue.delegation", domain.ErrorRPCTransient, codeCodeRead, true, false, err)
	}
	target, err := contracts.ParseDelegation(code)
	if err != nil {
		return false, nil
	}
	return target == coordinator.rescuer, nil
}

func (session *Session) renewDelegation(ctx context.Context) error {
	coordinator := session.coordinator
	if !coordinator.beginOperation() {
		coordinator.record(eventOperationSkipped, observability.LevelWarning, common.Hash{}, "")
		return nil
	}
	defer coordinator.endOperation()

	chainID := big.NewInt(int64(coordinator.network.ChainID))
	chainU256, _ := uint256.FromBig(chainID)
	sourceNonce, err := session.pendingNonce(ctx, coordinator.source)
	if err != nil {
		return err
	}
	sponsorNonce, err := session.pendingNonce(ctx, coordinator.sponsor)
	if err != nil {
		return err
	}
	if err := coordinator.checkSignerAddresses(); err != nil {
		return err
	}
	authorization, err := coordinator.authorizer.SignAuthorization(ctx, types.SetCodeAuthorization{
		ChainID: *chainU256,
		Address: coordinator.rescuer,
		Nonce:   sourceNonce,
	})
	if err != nil {
		return contextOrError(ctx, "rescue.sign_authorization", domain.ErrorSigning, codeSigning, false, false, err)
	}
	fees, err := session.readFees(ctx)
	if err != nil {
		return err
	}
	fees = ApplyFeePolicy(fees, sponsorFeeMultiplier)
	tip, _ := uint256.FromBig(fees.TipCap)
	feeCap, _ := uint256.FromBig(fees.FeeCap)
	transaction := types.NewTx(&types.SetCodeTx{
		ChainID:   chainU256,
		Nonce:     sponsorNonce,
		To:        coordinator.source,
		Gas:       delegationRenewalGas,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		AuthList:  []types.SetCodeAuthorization{authorization},
	})
	return session.signBroadcastAndConfirm(ctx, transaction)
}

func (session *Session) renewAndSweep(ctx context.Context, token domain.Token) error {
	coordinator := session.coordinator
	if coordinator.retryExhausted(token.Address) {
		coordinator.record(eventOperationSkipped, observability.LevelWarning, common.Hash{}, "")
		return nil
	}
	if !coordinator.beginOperation() {
		coordinator.record(eventOperationSkipped, observability.LevelWarning, common.Hash{}, "")
		return nil
	}
	defer coordinator.endOperation()

	chainID := big.NewInt(int64(coordinator.network.ChainID))
	chainU256, _ := uint256.FromBig(chainID)
	sponsorNonce, err := session.pendingNonce(ctx, coordinator.sponsor)
	if err != nil {
		return err
	}
	fees, err := session.readFees(ctx)
	if err != nil {
		return err
	}
	data, err := coordinator.rescuerCodec.PackSweepAll([]common.Address{token.Address})
	if err != nil {
		return newError("rescue.sweep_all", domain.ErrorInternal, codeEncoding, false, false, err)
	}

	// The compromised source nonce is deliberately the final RPC read before
	// authorization signing and broadcast.
	sourceNonce, err := session.pendingNonce(ctx, coordinator.source)
	if err != nil {
		return err
	}
	if err := coordinator.checkSignerAddresses(); err != nil {
		return err
	}
	authorization, err := coordinator.authorizer.SignAuthorization(ctx, types.SetCodeAuthorization{
		ChainID: *chainU256,
		Address: coordinator.rescuer,
		Nonce:   sourceNonce,
	})
	if err != nil {
		return contextOrError(ctx, "rescue.sign_authorization", domain.ErrorSigning, codeSigning, false, false, err)
	}
	fees = ApplyFeePolicy(fees, tokenFeeMultiplier)
	tip, _ := uint256.FromBig(fees.TipCap)
	feeCap, _ := uint256.FromBig(fees.FeeCap)
	transaction := types.NewTx(&types.SetCodeTx{
		ChainID:   chainU256,
		Nonce:     sponsorNonce,
		To:        coordinator.source,
		Gas:       tokenSweepGas,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Data:      data,
		AuthList:  []types.SetCodeAuthorization{authorization},
	})

	receipt, err := session.signBroadcastAndWait(ctx, transaction)
	if err != nil {
		return err
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		coordinator.registerFailure(token)
		return newError("rescue.sweep_all", domain.ErrorPostcondition, codeReverted, true, false, nil)
	}
	remaining, err := session.tokenBalance(ctx, token.Address)
	if err != nil {
		// The legacy path treated an unreadable postcondition as success. This
		// extraction fails closed without changing retry counters; Task 07 owns
		// the durable ambiguous/retry policy.
		return newError("rescue.sweep_all", domain.ErrorPostcondition, codePostcondition, true, true, err)
	}
	if remaining.Sign() > 0 {
		coordinator.registerFailure(token)
		return newError("rescue.sweep_all", domain.ErrorPostcondition, codePostcondition, true, true, nil)
	}
	coordinator.clearRetry(token.Address)
	coordinator.record(eventOperationConfirmed, observability.LevelInfo, receipt.TxHash, "")
	return nil
}

func (session *Session) sweepNative(ctx context.Context) error {
	coordinator := session.coordinator
	if !coordinator.beginOperation() {
		coordinator.record(eventOperationSkipped, observability.LevelWarning, common.Hash{}, "")
		return nil
	}
	defer coordinator.endOperation()

	data, err := coordinator.rescuerCodec.PackSweepEth()
	if err != nil {
		return newError("rescue.sweep_native", domain.ErrorInternal, codeEncoding, false, false, err)
	}
	fees, err := session.readFees(ctx)
	if err != nil {
		return err
	}
	fees = ApplyFeePolicy(fees, sponsorFeeMultiplier)
	nonce, err := session.pendingNonce(ctx, coordinator.sponsor)
	if err != nil {
		return err
	}
	if coordinator.transactioner.Address() != coordinator.sponsor {
		return newError("rescue.signer", domain.ErrorSigning, codeSignerMismatch, false, false, nil)
	}
	chainID := big.NewInt(int64(coordinator.network.ChainID))
	transaction := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		To:        &coordinator.source,
		Gas:       nativeSweepGas,
		GasTipCap: fees.TipCap,
		GasFeeCap: fees.FeeCap,
		Data:      data,
	})
	return session.signBroadcastAndConfirm(ctx, transaction)
}

func (session *Session) signBroadcastAndConfirm(ctx context.Context, transaction *types.Transaction) error {
	receipt, err := session.signBroadcastAndWait(ctx, transaction)
	if err != nil {
		return err
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return newError("rescue.receipt", domain.ErrorPostcondition, codeReverted, true, false, nil)
	}
	session.coordinator.record(eventOperationConfirmed, observability.LevelInfo, receipt.TxHash, "")
	return nil
}

func (session *Session) signBroadcastAndWait(ctx context.Context, transaction *types.Transaction) (*types.Receipt, error) {
	coordinator := session.coordinator
	if coordinator.transactioner.Address() != coordinator.sponsor {
		return nil, newError("rescue.signer", domain.ErrorSigning, codeSignerMismatch, false, false, nil)
	}
	chainID := big.NewInt(int64(coordinator.network.ChainID))
	signed, err := coordinator.transactioner.SignTransaction(ctx, transaction, chainID)
	if err != nil {
		return nil, contextOrError(ctx, "rescue.sign_transaction", domain.ErrorSigning, codeSigning, false, false, err)
	}
	if signed == nil {
		return nil, newError("rescue.sign_transaction", domain.ErrorSigning, codeSigning, false, false, nil)
	}
	if err := session.broadcaster.SendTransaction(ctx, signed); err != nil {
		return nil, contextOrError(ctx, "rescue.broadcast", domain.ErrorBroadcast, codeBroadcast, true, true, err)
	}
	coordinator.record(eventBroadcastAccepted, observability.LevelInfo, common.Hash{}, "")
	return session.waitReceipt(ctx, signed.Hash())
}

func (session *Session) waitReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	deadline := session.coordinator.clock.Now().Add(receiptTimeout)
	for session.coordinator.clock.Now().Before(deadline) {
		receipt, err := session.reader.TransactionReceipt(ctx, hash)
		if err == nil {
			if receipt == nil {
				return nil, newError("rescue.receipt", domain.ErrorRPCInvalidResponse, codeReceiptInvalid, true, true, nil)
			}
			return receipt, nil
		}
		if ctx.Err() != nil {
			return nil, contextOrError(ctx, "rescue.receipt", domain.ErrorRPCTransient, codeContextCanceled, true, true, err)
		}
		delay := receiptPollingInterval
		if remaining := deadline.Sub(session.coordinator.clock.Now()); remaining < delay {
			delay = remaining
		}
		if delay <= 0 {
			break
		}
		if err := session.coordinator.clock.Sleep(ctx, delay); err != nil {
			return nil, contextOrError(ctx, "rescue.receipt", domain.ErrorRPCTransient, codeContextCanceled, true, true, err)
		}
	}
	return nil, newError("rescue.receipt", domain.ErrorRPCTransient, codeReceiptTimeout, true, true, nil)
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
	message := err.Error()
	return strings.Contains(message, "429") || strings.Contains(message, "Too Many Requests")
}

func (session *Session) readFees(ctx context.Context) (FeeQuote, error) {
	readContext, cancel := context.WithTimeout(ctx, session.coordinator.feeReadTimeout)
	defer cancel()

	head, headerError := session.reader.HeaderByNumber(readContext, nil)
	if readContext.Err() != nil {
		return FeeQuote{}, contextOrError(readContext, "rescue.fees", domain.ErrorRPCTransient, codeContextCanceled, true, true, headerError)
	}
	if headerError == nil && head != nil && head.BaseFee != nil && head.BaseFee.Sign() > 0 {
		gasPrice, gasPriceError := session.reader.SuggestGasPrice(readContext)
		if readContext.Err() != nil {
			return FeeQuote{}, contextOrError(readContext, "rescue.fees", domain.ErrorRPCTransient, codeContextCanceled, true, true, gasPriceError)
		}
		tip := big.NewInt(1_500_000_000)
		if gasPriceError == nil && gasPrice != nil && gasPrice.Sign() > 0 {
			tip.Div(gasPrice, big.NewInt(10))
			if tip.Cmp(big.NewInt(minimumTipWei)) < 0 {
				tip.SetInt64(minimumTipWei)
			}
		}
		feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
		return FeeQuote{TipCap: tip, FeeCap: feeCap}, nil
	}

	gasPrice, gasPriceError := session.reader.SuggestGasPrice(readContext)
	if readContext.Err() != nil {
		return FeeQuote{}, contextOrError(readContext, "rescue.fees", domain.ErrorRPCTransient, codeContextCanceled, true, true, gasPriceError)
	}
	if gasPriceError != nil || gasPrice == nil || gasPrice.Sign() == 0 {
		gasPrice = big.NewInt(1_000_000_000)
	}
	return FeeQuote{
		TipCap: new(big.Int).Set(gasPrice),
		FeeCap: new(big.Int).Mul(gasPrice, big.NewInt(12)),
	}, nil
}

func (session *Session) tokenBalance(ctx context.Context, token common.Address) (*big.Int, error) {
	data, err := session.coordinator.erc20.PackBalanceOf(session.coordinator.source)
	if err != nil {
		return nil, newError("rescue.token_balance", domain.ErrorInternal, codeEncoding, false, false, err)
	}
	result, err := session.reader.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil {
		return nil, contextOrError(ctx, "rescue.token_balance", domain.ErrorRPCTransient, codeBalanceRead, true, false, err)
	}
	balance, err := session.coordinator.erc20.DecodeBalanceOf(result)
	if err != nil {
		return nil, newError("rescue.token_balance", domain.ErrorRPCInvalidResponse, codeBalanceDecode, true, true, err)
	}
	return balance, nil
}

func (coordinator *Coordinator) checkSignerAddresses() error {
	if coordinator.authorizer.Address() != coordinator.source || coordinator.transactioner.Address() != coordinator.sponsor {
		return newError("rescue.signer", domain.ErrorSigning, codeSignerMismatch, false, false, nil)
	}
	return nil
}
