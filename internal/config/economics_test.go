package config

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
)

func TestEconomicPolicyDefaultsAndOverrides(t *testing.T) {
	tests := []struct {
		name      string
		configure func(map[string]string)
		want      [10]string
		wantGas   [2]uint64
	}{
		{
			name:    "base defaults",
			want:    [10]string{"8000000000000000", "15000000000000000", "25000000000000000", "40000000000000000", "20000000000000000", "20000000000", "2000000000", "500000000000000", "1000000000000000", "2000000000000000"},
			wantGas: [2]uint64{300000, 100000},
		},
		{
			name: "explicit values",
			configure: func(values map[string]string) {
				values["NETWORK_MAX_TRANSACTION_COST_WEI_BASE"] = "9000000000000000"
				values["NETWORK_HOURLY_BUDGET_WEI_BASE"] = "18000000000000000"
				values["NETWORK_DAILY_BUDGET_WEI_BASE"] = "28000000000000000"
				values["NETWORK_CUMULATIVE_BUDGET_WEI_BASE"] = "45000000000000000"
				values["NETWORK_SPONSOR_MIN_BALANCE_WEI_BASE"] = "21000000000000000"
				values["MAX_FEE_PER_GAS_WEI_BASE"] = "10000000000"
				values["MAX_PRIORITY_FEE_PER_GAS_WEI_BASE"] = "1000000000"
				values["TOKEN_GAS_LIMIT_BASE"] = "400000"
				values["NATIVE_GAS_LIMIT_BASE"] = "120000"
				values["CHAIN_OVERHEAD_MAX_WEI_BASE"] = "1000000000000000"
				values["NATIVE_MIN_NET_VALUE_WEI_BASE"] = "2000000000000000"
				values["UNKNOWN_TOKEN_MAX_TRANSACTION_COST_WEI_BASE"] = "3000000000000000"
			},
			want:    [10]string{"9000000000000000", "18000000000000000", "28000000000000000", "45000000000000000", "21000000000000000", "10000000000", "1000000000", "1000000000000000", "2000000000000000", "3000000000000000"},
			wantGas: [2]uint64{400000, 120000},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := validEnvironment()
			if test.configure != nil {
				test.configure(values)
			}
			runtimeConfig, err := LoadFrom(mapLookup(values))
			if err != nil {
				t.Fatal(err)
			}
			policy := runtimeConfig.Networks[0].EconomicPolicy
			got := [10]string{
				policy.MaxTransactionCostWei.String(), policy.HourlyBudgetWei.String(), policy.DailyBudgetWei.String(), policy.CumulativeBudgetWei.String(),
				policy.SponsorMinimumBalanceWei.String(), policy.MaxFeePerGasWei.String(), policy.MaxPriorityFeePerGasWei.String(),
				policy.TransactionOverheadWei.String(), policy.NativeMinimumNetValueWei.String(), policy.UnknownTokenMaxTransactionCostWei.String(),
			}
			if got != test.want || [2]uint64{policy.TokenGasLimit, policy.NativeGasLimit} != test.wantGas {
				t.Fatalf("EconomicPolicy: values %v, gas [%d %d]", got, policy.TokenGasLimit, policy.NativeGasLimit)
			}
		})
	}
}

func TestEconomicPolicyRejectsHierarchyOverflowAndUnsafeGasBounds(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
	}{
		{name: "network per-tx above global", field: "NETWORK_MAX_TRANSACTION_COST_WEI_BASE", value: "11000000000000000"},
		{name: "network hour below per-tx", field: "NETWORK_HOURLY_BUDGET_WEI_BASE", value: "7000000000000000"},
		{name: "network hour above global", field: "NETWORK_HOURLY_BUDGET_WEI_BASE", value: "21000000000000000"},
		{name: "network day below hour", field: "NETWORK_DAILY_BUDGET_WEI_BASE", value: "14000000000000000"},
		{name: "network cumulative below day", field: "NETWORK_CUMULATIVE_BUDGET_WEI_BASE", value: "24000000000000000"},
		{name: "network reserve below global", field: "NETWORK_SPONSOR_MIN_BALANCE_WEI_BASE", value: "19000000000000000"},
		{name: "priority fee above max fee", field: "MAX_PRIORITY_FEE_PER_GAS_WEI_BASE", value: "21000000000"},
		{name: "token gas violates per-tx", field: "TOKEN_GAS_LIMIT_BASE", value: "500000"},
		{name: "native gas violates per-tx", field: "NATIVE_GAS_LIMIT_BASE", value: "400000"},
		{name: "overhead violates per-tx", field: "CHAIN_OVERHEAD_MAX_WEI_BASE", value: "8000000000000000"},
		{name: "unknown cap above network per-tx", field: "UNKNOWN_TOKEN_MAX_TRANSACTION_COST_WEI_BASE", value: "9000000000000000"},
		{name: "uint256 overflow", field: "MAX_FEE_PER_GAS_WEI_BASE", value: "115792089237316195423570985008687907853269984665640564039457584007913129639936"},
		{name: "gas uint64 overflow", field: "TOKEN_GAS_LIMIT_BASE", value: "18446744073709551616"},
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

func TestEveryNetworkUsesItsOwnBoundedEconomicProfile(t *testing.T) {
	wantProfiles := map[string][3]string{
		"base":     {"20000000000", "2000000000", "500000000000000"},
		"ethereum": {"25000000000", "3000000000", "0"},
		"arbitrum": {"3000000000", "100000000", "1100000000000000"},
		"optimism": {"4000000000", "50000000", "1200000000000000"},
		"polygon":  {"25000000000", "20000000000", "0"},
		"ink":      {"5000000000", "100000000", "700000000000000"},
		"scroll":   {"5000000000", "500000000", "1500000000000000"},
		"linea":    {"5000000000", "500000000", "1800000000000000"},
		"metis":    {"10000000000", "1000000000", "800000000000000"},
		"bnb":      {"10000000000", "2000000000", "0"},
	}
	seen := make(map[[3]string]string, len(wantProfiles))
	for _, definition := range networkRegistry() {
		t.Run(definition.name, func(t *testing.T) {
			runtimeConfig, err := LoadFrom(mapLookup(validEnvironmentForNetwork(definition)))
			if err != nil {
				t.Fatal(err)
			}
			policy := runtimeConfig.Networks[0].EconomicPolicy
			profile := [3]string{policy.MaxFeePerGasWei.String(), policy.MaxPriorityFeePerGasWei.String(), policy.TransactionOverheadWei.String()}
			if profile != wantProfiles[definition.name] {
				t.Fatalf("network profile = %v, want %v", profile, wantProfiles[definition.name])
			}
			if other, duplicate := seen[profile]; duplicate {
				t.Fatalf("networks %s and %s share one fee, tip, and overhead profile", definition.name, other)
			}
			seen[profile] = definition.name

			maximum := new(big.Int).Mul(new(big.Int).SetUint64(policy.TokenGasLimit), policy.MaxFeePerGasWei)
			maximum.Add(maximum, policy.TransactionOverheadWei)
			if maximum.Cmp(policy.MaxTransactionCostWei) > 0 || policy.MaxTransactionCostWei.Cmp(runtimeConfig.Policy.MaxTransactionCostWei) > 0 || policy.HourlyBudgetWei.Cmp(runtimeConfig.Policy.HourlyBudgetWei) > 0 || policy.DailyBudgetWei.Cmp(runtimeConfig.Policy.DailyBudgetWei) > 0 || policy.CumulativeBudgetWei.Cmp(runtimeConfig.Policy.CumulativeBudgetWei) > 0 {
				t.Fatalf("unbounded economic profile: maximum=%s, policy=%+v", maximum, policy)
			}
		})
	}
}

func TestEconomicPolicyCloneDoesNotAliasBigIntegers(t *testing.T) {
	values := validEnvironment()
	address := networkRegistry()[0].tokens[0].Address.Hex()
	values["TOKEN_VALUE_RULES_BASE"] = address + ":1000000:1000000000000000"
	runtimeConfig, err := LoadFrom(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	original := runtimeConfig.Networks[0].EconomicPolicy
	cloned := original.Clone()
	cloned.MaxFeePerGasWei.SetInt64(1)
	cloned.TransactionOverheadWei.SetInt64(2)
	cloned.TokenValueRules[0].MinimumBalance.SetInt64(3)
	if original.MaxFeePerGasWei.String() != "20000000000" || original.TransactionOverheadWei.String() != "500000000000000" {
		t.Fatal("EconomicPolicy.Clone reused mutable big.Int")
	}
	if original.TokenValueRules[0].MinimumBalance.String() != "1000000" {
		t.Fatal("EconomicPolicy.Clone reused token value rule")
	}
}

func TestTokenValueRulesAreBoundToTrustedTokensAndNetworkCap(t *testing.T) {
	known := networkRegistry()[0].tokens[0].Address.Hex()
	unknown := "0x00000000000000000000000000000000000000f1"
	tests := []string{
		unknown + ":1:1",
		known + ":0:1",
		known + ":1:9000000000000000",
		known + ":1:1," + known + ":2:2",
		known + ":broken:1",
	}
	for _, encoded := range tests {
		values := validEnvironment()
		values["TOKEN_VALUE_RULES_BASE"] = encoded
		if _, err := LoadFromMap(values); err == nil || !strings.Contains(err.Error(), "TOKEN_VALUE_RULES_BASE") {
			t.Fatalf("rule %q accepted or error does not name field: %v", encoded, err)
		}
	}
}

func validEnvironmentForNetwork(definition networkDefinition) map[string]string {
	values := map[string]string{
		"SOURCE_ADDRESS":      testAddress(1),
		"SPONSOR_ADDRESS":     testAddress(2),
		"DESTINATION_ADDRESS": testAddress(3),
		"ENABLED_NETWORKS":    definition.name,
	}
	suffix := strings.ToUpper(definition.name)
	values["RPC_READ_1_HTTP_"+suffix] = fmt.Sprintf("https://read-one.invalid/%s", definition.name)
	values["RPC_READ_1_WS_"+suffix] = fmt.Sprintf("wss://read-one.invalid/%s", definition.name)
	values["RPC_READ_1_TRUST_DOMAIN_"+suffix] = "provider-one-" + definition.name
	values["RPC_READ_2_HTTP_"+suffix] = fmt.Sprintf("https://read-two.invalid/%s", definition.name)
	values["RPC_READ_2_WS_"+suffix] = fmt.Sprintf("wss://read-two.invalid/%s", definition.name)
	values["RPC_READ_2_TRUST_DOMAIN_"+suffix] = "provider-two-" + definition.name
	values["RESCUER_MANIFEST_"+suffix] = "testdata/" + definition.name + "-manifest.json"
	return values
}
