package rescue

import (
	"errors"
	"math/big"
	"testing"

	"github.com/holiman/uint256"
)

func TestBuildFeeQuoteScenarios(t *testing.T) {
	policy := testFeePolicy()
	tests := []struct {
		name      string
		asset     FeeAsset
		base      int64
		suggested int64
		wantTip   int64
		wantFee   int64
		wantGas   uint64
	}{
		{name: "low base fee", asset: FeeAssetToken, base: 100, suggested: 110, wantTip: 10, wantFee: 210, wantGas: 20},
		{name: "base fee spike uses network cap", asset: FeeAssetNative, base: 400, suggested: 430, wantTip: 20, wantFee: 600, wantGas: 10},
		{name: "priority fee is capped", asset: FeeAssetUnknownToken, base: 100, suggested: 200, wantTip: 20, wantFee: 220, wantGas: 20},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			quote, err := BuildFeeQuote(policy, test.asset, big.NewInt(test.base), big.NewInt(test.suggested))
			if err != nil {
				t.Fatalf("BuildFeeQuote() error = %v", err)
			}
			if quote.TipCap.Cmp(big.NewInt(test.wantTip)) != 0 || quote.FeeCap.Cmp(big.NewInt(test.wantFee)) != 0 {
				t.Fatalf("BuildFeeQuote() fees = (%s, %s), want (%d, %d)", quote.TipCap, quote.FeeCap, test.wantTip, test.wantFee)
			}
			wantMaximum := new(uint256.Int).SetUint64(uint64(test.wantFee)*test.wantGas + 7)
			if quote.Network != policy.Network || quote.Asset != test.asset || quote.GasLimit != test.wantGas || quote.MaximumCost.Cmp(wantMaximum) != 0 {
				t.Fatalf("BuildFeeQuote() metadata = %#v, want network=%d asset=%d gas=%d maximum=%s", quote, policy.Network, test.asset, test.wantGas, wantMaximum)
			}
		})
	}
}

func TestBuildFeeQuoteRejectsUnderpricedCap(t *testing.T) {
	policy := testFeePolicy()
	_, err := BuildFeeQuote(policy, FeeAssetToken, big.NewInt(590), big.NewInt(620))
	if !errors.Is(err, ErrFeeUnderpriced) {
		t.Fatalf("BuildFeeQuote() error = %v, want ErrFeeUnderpriced", err)
	}
}

func TestBuildFeeQuoteRejectsMalformedInputs(t *testing.T) {
	policy := testFeePolicy()
	tests := []struct {
		name      string
		policy    FeePolicy
		asset     FeeAsset
		base      *big.Int
		suggested *big.Int
		want      error
	}{
		{name: "invalid policy", policy: FeePolicy{}, asset: FeeAssetNative, base: big.NewInt(1), suggested: big.NewInt(2), want: ErrInvalidFeePolicy},
		{name: "unknown asset", policy: policy, asset: 99, base: big.NewInt(1), suggested: big.NewInt(2), want: ErrInvalidFeeInput},
		{name: "nil base", policy: policy, asset: FeeAssetNative, suggested: big.NewInt(2), want: ErrInvalidFeeInput},
		{name: "negative suggested", policy: policy, asset: FeeAssetNative, base: big.NewInt(1), suggested: big.NewInt(-2), want: ErrInvalidFeeInput},
		{name: "suggested not above base", policy: policy, asset: FeeAssetNative, base: big.NewInt(2), suggested: big.NewInt(2), want: ErrInvalidFeeInput},
		{name: "base exceeds uint256", policy: policy, asset: FeeAssetNative, base: new(big.Int).Lsh(big.NewInt(1), 256), suggested: new(big.Int).Lsh(big.NewInt(1), 257), want: ErrInvalidFeeInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := BuildFeeQuote(test.policy, test.asset, test.base, test.suggested)
			if !errors.Is(err, test.want) {
				t.Fatalf("BuildFeeQuote() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestBuildFeeQuoteRejectsMaximumCostOverflow(t *testing.T) {
	policy := testFeePolicy()
	policy.MaxFeePerGas.SetAllOne()
	policy.MaxPriorityFeePerGas.SetUint64(1)
	policy.NativeGasLimit = 2
	base := new(big.Int).Lsh(big.NewInt(1), 255)
	suggested := new(big.Int).Add(new(big.Int).Set(base), big.NewInt(1))

	_, err := BuildFeeQuote(policy, FeeAssetNative, base, suggested)
	if !errors.Is(err, ErrFeeCostOverflow) {
		t.Fatalf("BuildFeeQuote() error = %v, want ErrFeeCostOverflow", err)
	}
}

func TestEvaluateMinimumValue(t *testing.T) {
	policy := testFeePolicy()
	maximum := *uint256.NewInt(100)
	minimum := *uint256.NewInt(25)

	native, err := EvaluateMinimumValue(policy, FeeAssetNative, big.NewInt(125), minimum, maximum)
	if err != nil || !native.Allowed || !native.Trusted || native.Required.Uint64() != 125 {
		t.Fatalf("EvaluateMinimumValue(native) = %#v, %v", native, err)
	}
	native, err = EvaluateMinimumValue(policy, FeeAssetNative, big.NewInt(124), minimum, maximum)
	if err != nil || native.Allowed || !native.Trusted || native.Required.Uint64() != 125 {
		t.Fatalf("EvaluateMinimumValue(insufficient native) = %#v, %v", native, err)
	}

	unknown, err := EvaluateMinimumValue(policy, FeeAssetUnknownToken, nil, uint256.Int{}, maximum)
	if err != nil || !unknown.Allowed || unknown.Trusted {
		t.Fatalf("EvaluateMinimumValue(unknown) = %#v, %v", unknown, err)
	}
	overCap := *uint256.NewInt(101)
	unknown, err = EvaluateMinimumValue(policy, FeeAssetUnknownToken, nil, uint256.Int{}, overCap)
	if err != nil || unknown.Allowed || unknown.Trusted {
		t.Fatalf("EvaluateMinimumValue(over-cap unknown) = %#v, %v", unknown, err)
	}

	_, err = EvaluateMinimumValue(policy, FeeAssetToken, nil, uint256.Int{}, maximum)
	if !errors.Is(err, ErrTrustedTokenValueNeeded) {
		t.Fatalf("EvaluateMinimumValue(known token) error = %v", err)
	}
}

func TestEvaluateMinimumValueRejectsOverflow(t *testing.T) {
	policy := testFeePolicy()
	maximum := uint256.Int{}
	maximum.SetAllOne()
	_, err := EvaluateMinimumValue(policy, FeeAssetNative, new(big.Int).Set(maximum.ToBig()), *uint256.NewInt(1), maximum)
	if !errors.Is(err, ErrMinimumValueOverflow) {
		t.Fatalf("EvaluateMinimumValue() error = %v, want ErrMinimumValueOverflow", err)
	}
}

func testFeePolicy() FeePolicy {
	return FeePolicy{
		Network:              8453,
		MaxFeePerGas:         *uint256.NewInt(600),
		MaxPriorityFeePerGas: *uint256.NewInt(20),
		TokenGasLimit:        20,
		NativeGasLimit:       10,
		Overhead:             *uint256.NewInt(7),
		UnknownTokenCostCap:  *uint256.NewInt(100),
		TransactionCostCap:   *uint256.NewInt(20_000),
	}
}
