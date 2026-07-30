package rescue

import (
	"math/big"
	"testing"
)

func TestApplyFeePolicy(t *testing.T) {
	tests := []struct {
		name       string
		quote      FeeQuote
		multiplier int64
		wantTip    int64
		wantCap    int64
	}{
		{
			name:       "below caps",
			quote:      FeeQuote{TipCap: big.NewInt(2), FeeCap: big.NewInt(5)},
			multiplier: 50,
			wantTip:    100,
			wantCap:    250,
		},
		{
			name:       "hard caps after multiplication",
			quote:      FeeQuote{TipCap: big.NewInt(3_000_000), FeeCap: big.NewInt(10_000_000)},
			multiplier: 2000,
			wantTip:    5_000_000_000,
			wantCap:    15_000_000_000,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ApplyFeePolicy(test.quote, test.multiplier)
			if got.TipCap.Cmp(big.NewInt(test.wantTip)) != 0 || got.FeeCap.Cmp(big.NewInt(test.wantCap)) != 0 {
				t.Fatalf("ApplyFeePolicy() = (%s, %s), want (%d, %d)", got.TipCap, got.FeeCap, test.wantTip, test.wantCap)
			}
		})
	}
}
