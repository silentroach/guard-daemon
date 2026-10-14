package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaimRequestIsBoundToEnabledChainAndToken(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong chain", "unlisted token", "negative payment", "missing minimum"} {
		t.Run(scenario, func(t *testing.T) {
			values := validEnvironment()
			values["TOKEN_MODE_BASE"] = "allowlist"
			values["TOKEN_ALLOWLIST_BASE"] = "0x2000000000000000000000000000000000000002"
			document := claimDocument{
				ChainID: 8453, Target: "0x1000000000000000000000000000000000000001", Data: "0x12345678",
				ValueWei: "10", Token: "0x2000000000000000000000000000000000000002", MinimumReceivedRaw: "1",
				GasLimit: 300000, MaximumCostWei: "1000000000000000", ValidUntil: 1893456000,
			}
			switch scenario {
			case "wrong chain":
				document.ChainID = 1
			case "unlisted token":
				document.Token = "0x3000000000000000000000000000000000000003"
			case "negative payment":
				document.ValueWei = "-10"
			case "missing minimum":
				document.MinimumReceivedRaw = "0"
			}
			encoded, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "request.json")
			if err := os.WriteFile(path, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			values["CLAIM_REQUEST_BASE"] = path
			runtime, err := LoadFrom(mapLookup(values))
			if scenario == "valid" {
				if err != nil || runtime.Networks[0].Claim == nil {
					t.Fatalf("claim was not loaded: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "CLAIM_REQUEST_BASE") || strings.Contains(err.Error(), document.Data) {
				t.Fatalf("invalid claim was accepted or leaked data: %v", err)
			}
		})
	}
}

func TestEstimatedFeeModeIsExplicitAndBaseOnly(t *testing.T) {
	values := validEnvironment()
	values["NETWORK_FEE_MODE_BASE"] = "estimated"
	runtime, err := LoadFrom(mapLookup(values))
	if err != nil || !runtime.Networks[0].EconomicPolicy.EstimatedBaseFees {
		t.Fatalf("Base estimate mode: %v", err)
	}
	values["NETWORK_FEE_MODE_BASE"] = "unlimited"
	if _, err := LoadFrom(mapLookup(values)); err == nil {
		t.Fatal("unrecognized fee policy was accepted")
	}
	global, err := loadRuntimePolicy(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadEconomicPolicy(mapLookup(map[string]string{"NETWORK_FEE_MODE_INK": "estimated"}), "INK", economicProfileFor("ink"), global, nil); err == nil {
		t.Fatal("Base-only implementation was enabled for Ink")
	}
}
