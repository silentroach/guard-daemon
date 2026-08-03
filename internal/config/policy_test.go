package config

import (
	"testing"
	"time"
)

func TestPolicyDefaultsAndOverrides(t *testing.T) {
	tests := []struct {
		name              string
		configure         func(map[string]string)
		wantLimits        [5]string
		wantRate          uint32
		wantAbuseWindow   time.Duration
		wantUnknown       uint32
		wantTokenAttempts uint32
		wantEventAttempts uint32
		wantEmergency     bool
		wantAlertCooldown time.Duration
		wantTimeout       time.Duration
	}{
		{
			name:              "безопасные defaults",
			wantLimits:        [5]string{"10000000000000000", "20000000000000000", "30000000000000000", "50000000000000000", "20000000000000000"},
			wantRate:          6,
			wantAbuseWindow:   time.Hour,
			wantUnknown:       16,
			wantTokenAttempts: 3,
			wantEventAttempts: 3,
			wantAlertCooldown: 15 * time.Minute,
			wantTimeout:       10 * time.Second,
		},
		{name: "явные значения", configure: func(values map[string]string) {
			values["MAX_TRANSACTION_COST_WEI"] = "7000000000000000"
			values["HOURLY_BUDGET_WEI"] = "8000000000000000"
			values["DAILY_BUDGET_WEI"] = "9000000000000000"
			values["CUMULATIVE_BUDGET_WEI"] = "11000000000000000"
			values["SPONSOR_MIN_BALANCE_WEI"] = "13000000000000000"
			values["RATE_LIMIT_PER_MINUTE"] = "17"
			values["ABUSE_WINDOW"] = "37m"
			values["MAX_NEW_UNKNOWN_TOKENS_PER_WINDOW"] = "19"
			values["MAX_ATTEMPTS_PER_TOKEN_WINDOW"] = "5"
			values["MAX_ATTEMPTS_PER_SOURCE_EVENT"] = "7"
			values["EMERGENCY_STOP"] = "true"
			values["ALERT_COOLDOWN"] = "23m"
			values["RPC_READ_TIMEOUT"] = "2500ms"
		}, wantLimits: [5]string{"7000000000000000", "8000000000000000", "9000000000000000", "11000000000000000", "13000000000000000"}, wantRate: 17, wantAbuseWindow: 37 * time.Minute, wantUnknown: 19, wantTokenAttempts: 5, wantEventAttempts: 7, wantEmergency: true, wantAlertCooldown: 23 * time.Minute, wantTimeout: 2500 * time.Millisecond},
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
			gotLimits := [5]string{policy.MaxTransactionCostWei.String(), policy.HourlyBudgetWei.String(), policy.DailyBudgetWei.String(), policy.CumulativeBudgetWei.String(), policy.SponsorMinimumBalanceWei.String()}
			if gotLimits != test.wantLimits || policy.RateLimitPerMinute != test.wantRate || policy.AbuseWindow != test.wantAbuseWindow || policy.MaxNewUnknownTokensPerWindow != test.wantUnknown || policy.MaxAttemptsPerTokenWindow != test.wantTokenAttempts || policy.MaxAttemptsPerSourceEvent != test.wantEventAttempts || policy.EmergencyStop != test.wantEmergency || policy.AlertCooldown != test.wantAlertCooldown || runtime.ReadTimeout != test.wantTimeout {
				t.Fatalf("получена неверная policy: limits=%v policy=%+v timeout=%s", gotLimits, policy, runtime.ReadTimeout)
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
		{name: "hour меньше transaction", field: "HOURLY_BUDGET_WEI", value: "9999999999999999"},
		{name: "day меньше hour", field: "DAILY_BUDGET_WEI", value: "19999999999999999"},
		{name: "cumulative меньше day", field: "CUMULATIVE_BUDGET_WEI", value: "29999999999999999"},
		{name: "переполнение uint256", field: "CUMULATIVE_BUDGET_WEI", value: "115792089237316195423570985008687907853269984665640564039457584007913129639936"},
		{name: "нулевой минимум", field: "SPONSOR_MIN_BALANCE_WEI", value: "0"},
		{name: "нулевой rate", field: "RATE_LIMIT_PER_MINUTE", value: "0"},
		{name: "переполнение rate", field: "RATE_LIMIT_PER_MINUTE", value: "4294967296"},
		{name: "нулевое abuse окно", field: "ABUSE_WINDOW", value: "0s"},
		{name: "знак у abuse окна", field: "ABUSE_WINDOW", value: "+1h"},
		{name: "нулевой лимит unknown", field: "MAX_NEW_UNKNOWN_TOKENS_PER_WINDOW", value: "0"},
		{name: "нулевой лимит token attempts", field: "MAX_ATTEMPTS_PER_TOKEN_WINDOW", value: "0"},
		{name: "нулевой лимит event attempts", field: "MAX_ATTEMPTS_PER_SOURCE_EVENT", value: "0"},
		{name: "нулевой alert cooldown", field: "ALERT_COOLDOWN", value: "0s"},
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

func TestEmergencyStopRequiresStrictBoolean(t *testing.T) {
	for _, value := range []string{"", "TRUE", "False", "1", " true", "false "} {
		t.Run(value, func(t *testing.T) {
			values := validEnvironment()
			values["EMERGENCY_STOP"] = value
			_, err := LoadFrom(mapLookup(values))
			assertErrorField(t, err, "EMERGENCY_STOP", values)
		})
	}
}

func TestEmergencyStopLiveModeDoesNotRequireOrParsePrivateKeys(t *testing.T) {
	values := validEnvironment()
	values["DRY_RUN"] = "false"
	values["EMERGENCY_STOP"] = "true"
	values["RPC_BROADCAST_HTTP_BASE"] = "https://broadcast.invalid/"
	runtimeConfig, err := LoadFromMap(values)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := runtimeConfig.LiveSecrets.PrivateKeys(); ok {
		t.Fatal("emergency stop сохранил private keys")
	}
}

func TestRuntimePolicyCloneDoesNotAliasBigIntegers(t *testing.T) {
	runtimeConfig, err := LoadFrom(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	cloned := runtimeConfig.Policy.Clone()
	cloned.MaxTransactionCostWei.SetInt64(1)
	cloned.HourlyBudgetWei.SetInt64(2)
	if runtimeConfig.Policy.MaxTransactionCostWei.String() != defaultMaxTransactionCostWei || runtimeConfig.Policy.HourlyBudgetWei.String() != defaultHourlyBudgetWei {
		t.Fatal("RuntimePolicy.Clone переиспользовал mutable big.Int")
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
