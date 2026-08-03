package rescue

import (
	"errors"
	"math/big"

	"guard-daemon/internal/domain"

	"github.com/holiman/uint256"
)

var (
	ErrInvalidFeePolicy        = errors.New("rescue fee policy: некорректная конфигурация")
	ErrInvalidFeeInput         = errors.New("rescue fee policy: некорректные fee inputs")
	ErrFeeUnderpriced          = errors.New("rescue fee policy: max fee ниже base fee и priority fee")
	ErrFeeCostOverflow         = errors.New("rescue fee policy: переполнение максимальной стоимости")
	ErrInvalidMinimumValue     = errors.New("rescue value policy: некорректные входные данные")
	ErrMinimumValueOverflow    = errors.New("rescue value policy: переполнение минимальной стоимости")
	ErrTrustedTokenValueNeeded = errors.New("rescue value policy: для известного token требуется доверенная оценка")
)

type FeeAsset uint8

const (
	FeeAssetNative FeeAsset = iota + 1
	FeeAssetToken
	FeeAssetUnknownToken
)

// FeePolicy contains only chain-specific limits. Overhead and
// UnknownTokenCostCap are wei amounts, not gas units.
type FeePolicy struct {
	Network                 domain.NetworkID
	MaxFeePerGas            uint256.Int
	MaxPriorityFeePerGas    uint256.Int
	TokenGasLimit           uint64
	NativeGasLimit          uint64
	Overhead                uint256.Int
	UnknownTokenCostCap     uint256.Int
	TransactionCostCap      uint256.Int
	UnboundedAdditionalFees bool
}

type FeeQuote struct {
	Network     domain.NetworkID
	Asset       FeeAsset
	TipCap      *big.Int
	FeeCap      *big.Int
	GasLimit    uint64
	MaximumCost uint256.Int
}

// BuildFeeQuote derives the priority fee from the suggested legacy gas price
// and base fee, then applies only the supplied network policy caps.
func BuildFeeQuote(policy FeePolicy, asset FeeAsset, baseFee, suggestedPrice *big.Int) (FeeQuote, error) {
	if !validFeePolicy(policy) {
		return FeeQuote{}, ErrInvalidFeePolicy
	}
	gasLimit, ok := policy.gasLimit(asset)
	if !ok {
		return FeeQuote{}, ErrInvalidFeeInput
	}
	base, ok := positiveUint256(baseFee)
	if !ok {
		return FeeQuote{}, ErrInvalidFeeInput
	}
	suggested, ok := positiveUint256(suggestedPrice)
	if !ok || suggested.Cmp(&base) <= 0 {
		return FeeQuote{}, ErrInvalidFeeInput
	}

	var tip uint256.Int
	tip.Sub(&suggested, &base)
	if tip.Cmp(&policy.MaxPriorityFeePerGas) > 0 {
		tip.Set(&policy.MaxPriorityFeePerGas)
	}
	var inclusionFloor uint256.Int
	if _, overflow := inclusionFloor.AddOverflow(&base, &tip); overflow {
		return FeeQuote{}, ErrFeeCostOverflow
	}

	feeCap := policy.MaxFeePerGas
	var doubledBase, target uint256.Int
	if _, overflow := doubledBase.AddOverflow(&base, &base); !overflow {
		if _, overflow = target.AddOverflow(&doubledBase, &tip); !overflow && target.Cmp(&feeCap) < 0 {
			feeCap.Set(&target)
		}
	}
	if feeCap.Cmp(&inclusionFloor) < 0 {
		return FeeQuote{}, ErrFeeUnderpriced
	}

	maximum, err := maximumFeeCost(gasLimit, &feeCap, &policy.Overhead)
	if err != nil {
		return FeeQuote{}, err
	}
	return FeeQuote{
		Network:     policy.Network,
		Asset:       asset,
		TipCap:      tip.ToBig(),
		FeeCap:      feeCap.ToBig(),
		GasLimit:    gasLimit,
		MaximumCost: maximum,
	}, nil
}

func validFeePolicy(policy FeePolicy) bool {
	return policy.Network > 0 && !policy.MaxFeePerGas.IsZero() && !policy.MaxPriorityFeePerGas.IsZero() &&
		!policy.TransactionCostCap.IsZero() && policy.MaxPriorityFeePerGas.Cmp(&policy.MaxFeePerGas) <= 0 &&
		policy.TokenGasLimit > 0 && policy.NativeGasLimit > 0
}

func (policy FeePolicy) gasLimit(asset FeeAsset) (uint64, bool) {
	switch asset {
	case FeeAssetNative:
		return policy.NativeGasLimit, true
	case FeeAssetToken, FeeAssetUnknownToken:
		return policy.TokenGasLimit, true
	default:
		return 0, false
	}
}

func positiveUint256(value *big.Int) (uint256.Int, bool) {
	if value == nil || value.Sign() <= 0 {
		return uint256.Int{}, false
	}
	converted, overflow := uint256.FromBig(value)
	if overflow {
		return uint256.Int{}, false
	}
	return *converted, true
}

func maximumFeeCost(gasLimit uint64, feeCap, overhead *uint256.Int) (uint256.Int, error) {
	var gas, maximum uint256.Int
	gas.SetUint64(gasLimit)
	if _, overflow := maximum.MulOverflow(&gas, feeCap); overflow {
		return uint256.Int{}, ErrFeeCostOverflow
	}
	if _, overflow := maximum.AddOverflow(&maximum, overhead); overflow || maximum.IsZero() {
		return uint256.Int{}, ErrFeeCostOverflow
	}
	return maximum, nil
}

type MinimumValueDecision struct {
	Allowed  bool
	Trusted  bool
	Required uint256.Int
}

// EvaluateMinimumValue compares native value in wei. Unknown token value is
// never trusted; opt-in can only bound the sponsor's maximum cost. Known-token
// valuation must be supplied by a separate trusted valuation policy.
func EvaluateMinimumValue(policy FeePolicy, asset FeeAsset, sourceValue *big.Int, minimumNativeNetValue, maximumCost uint256.Int) (MinimumValueDecision, error) {
	if !validFeePolicy(policy) || maximumCost.IsZero() {
		return MinimumValueDecision{}, ErrInvalidMinimumValue
	}
	switch asset {
	case FeeAssetNative:
		if sourceValue == nil || sourceValue.Sign() < 0 {
			return MinimumValueDecision{}, ErrInvalidMinimumValue
		}
		source, overflow := uint256.FromBig(sourceValue)
		if overflow {
			return MinimumValueDecision{}, ErrInvalidMinimumValue
		}
		var required uint256.Int
		if _, overflow := required.AddOverflow(&maximumCost, &minimumNativeNetValue); overflow {
			return MinimumValueDecision{}, ErrMinimumValueOverflow
		}
		return MinimumValueDecision{Allowed: source.Cmp(&required) >= 0, Trusted: true, Required: required}, nil
	case FeeAssetUnknownToken:
		return MinimumValueDecision{Allowed: maximumCost.Cmp(&policy.UnknownTokenCostCap) <= 0, Trusted: false}, nil
	case FeeAssetToken:
		return MinimumValueDecision{}, ErrTrustedTokenValueNeeded
	default:
		return MinimumValueDecision{}, ErrInvalidMinimumValue
	}
}

func copyBig(value *big.Int) *big.Int {
	if value == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(value)
}
