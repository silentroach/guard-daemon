package main

import (
	"math/big"
	"testing"

	"guard-daemon/internal/config"
	"guard-daemon/internal/rescue"
)

func TestBaseDefaultBudgetsAllowUnknownTokenRescue(t *testing.T) {
	runtime, err := config.LoadFromMap(map[string]string{
		"SOURCE_ADDRESS":               "0x0000000000000000000000000000000000000001",
		"SPONSOR_ADDRESS":              "0x0000000000000000000000000000000000000002",
		"DESTINATION_ADDRESS":          "0x0000000000000000000000000000000000000003",
		"ENABLED_NETWORKS":             "base",
		"RPC_READ_1_HTTP_BASE":         "https://read-one.invalid/",
		"RPC_READ_1_WS_BASE":           "wss://read-one.invalid/ws",
		"RPC_READ_1_TRUST_DOMAIN_BASE": "provider-one",
		"RPC_READ_2_HTTP_BASE":         "https://read-two.invalid/",
		"RPC_READ_2_WS_BASE":           "wss://read-two.invalid/ws",
		"RPC_READ_2_TRUST_DOMAIN_BASE": "provider-two",
		"RESCUER_MANIFEST_BASE":        "testdata/base-manifest.json",
		"NETWORK_FEE_MODE_BASE":        "estimated",
		"TOKEN_MODE_BASE":              "all",
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, minimum, _, err := rescuePolicy(runtime.Networks[0])
	if err != nil {
		t.Fatal(err)
	}
	quote, err := rescue.BuildFeeQuote(policy, rescue.FeeAssetUnknownToken, big.NewInt(5_000_000), big.NewInt(6_000_000))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := rescue.EvaluateMinimumValue(policy, rescue.FeeAssetUnknownToken, nil, minimum, quote.MaximumCost)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Allowed {
		t.Fatal("default unknown-token cap blocks the Base fee quote including the L1/operator reserve")
	}
}
