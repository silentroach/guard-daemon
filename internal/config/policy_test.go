package config

import (
	"testing"
	"time"
)

func TestPolicyDefaultsAndOverrides(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(map[string]string)
		wantMax     string
		wantBudget  string
		wantMin     string
		wantRate    uint32
		wantTimeout time.Duration
	}{
		{name: "безопасные defaults", wantMax: "10000000000000000", wantBudget: "50000000000000000", wantMin: "20000000000000000", wantRate: 6, wantTimeout: 10 * time.Second},
		{name: "явные значения", configure: func(values map[string]string) {
			values["MAX_TRANSACTION_COST_WEI"] = "7"
			values["CUMULATIVE_BUDGET_WEI"] = "11"
			values["SPONSOR_MIN_BALANCE_WEI"] = "13"
			values["RATE_LIMIT_PER_MINUTE"] = "17"
			values["RPC_READ_TIMEOUT"] = "2500ms"
		}, wantMax: "7", wantBudget: "11", wantMin: "13", wantRate: 17, wantTimeout: 2500 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := validEnvironment()
			if test.configure != nil {
				test.configure(values)
			}
			runtime, err := LoadFrom(mapLookup(values))
			if err != nil {
				t.Fatal(err)
			}
			policy := runtime.Policy
			if policy.MaxTransactionCostWei.String() != test.wantMax || policy.CumulativeBudgetWei.String() != test.wantBudget || policy.SponsorMinimumBalanceWei.String() != test.wantMin || policy.RateLimitPerMinute != test.wantRate || runtime.ReadTimeout != test.wantTimeout {
				t.Fatalf("получена неверная policy: max=%s budget=%s min=%s rate=%d timeout=%s", policy.MaxTransactionCostWei, policy.CumulativeBudgetWei, policy.SponsorMinimumBalanceWei, policy.RateLimitPerMinute, runtime.ReadTimeout)
			}
		})
	}
}

func TestPolicyRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
	}{
		{name: "нулевая стоимость", field: "MAX_TRANSACTION_COST_WEI", value: "0"},
		{name: "отрицательная стоимость", field: "MAX_TRANSACTION_COST_WEI", value: "-1"},
		{name: "дробная стоимость", field: "MAX_TRANSACTION_COST_WEI", value: "1.5"},
		{name: "budget меньше transaction", field: "CUMULATIVE_BUDGET_WEI", value: "1"},
		{name: "нулевой минимум", field: "SPONSOR_MIN_BALANCE_WEI", value: "0"},
		{name: "нулевой rate", field: "RATE_LIMIT_PER_MINUTE", value: "0"},
		{name: "переполнение rate", field: "RATE_LIMIT_PER_MINUTE", value: "4294967296"},
		{name: "нулевой timeout", field: "RPC_READ_TIMEOUT", value: "0s"},
		{name: "повреждённый timeout", field: "RPC_READ_TIMEOUT", value: "test-only-timeout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := validEnvironment()
			values[test.field] = test.value
			_, err := LoadFrom(mapLookup(values))
			assertErrorField(t, err, test.field, values)
		})
	}
}

func TestArtifactTrustIsPinned(t *testing.T) {
	runtime, err := LoadFrom(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	want := ArtifactTrust{
		Path:            "artifacts/contracts/RescuerV2.json",
		SHA256:          "sha256:96c6c0d358e44980814b40553358c53ce5231b7c2ce4ef9bf99add3b9a96a4af",
		SourceKind:      "source-tree-sha256",
		SourceValue:     "sha256:e7fcd5fa1da201b9a4aa728ad6758032250919eb438733e3f57c61eef8f2971a",
		CompilerVersion: "0.8.36+commit.8a079791.Emscripten.clang",
	}
	if runtime.Artifact != want {
		t.Fatalf("artifact trust = %#v", runtime.Artifact)
	}
}
