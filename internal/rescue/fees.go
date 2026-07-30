package rescue

import "math/big"

const (
	tokenFeeMultiplier   = int64(2000)
	sponsorFeeMultiplier = int64(50)
	maxTipWei            = int64(5_000_000_000)
	maxFeeCapWei         = maxTipWei * 3
	minimumTipWei        = int64(100_000)
)

type FeeQuote struct {
	TipCap *big.Int
	FeeCap *big.Int
}

// ApplyFeePolicy applies the legacy multiplier followed by the hard per-gas
// caps. The input integers are never mutated.
func ApplyFeePolicy(quote FeeQuote, multiplier int64) FeeQuote {
	tip := copyBig(quote.TipCap)
	feeCap := copyBig(quote.FeeCap)
	tip.Mul(tip, big.NewInt(multiplier))
	feeCap.Mul(feeCap, big.NewInt(multiplier))
	if tip.Cmp(big.NewInt(maxTipWei)) > 0 {
		tip.SetInt64(maxTipWei)
	}
	if feeCap.Cmp(big.NewInt(maxFeeCapWei)) > 0 {
		feeCap.SetInt64(maxFeeCapWei)
	}
	return FeeQuote{TipCap: tip, FeeCap: feeCap}
}

func copyBig(value *big.Int) *big.Int {
	if value == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(value)
}
