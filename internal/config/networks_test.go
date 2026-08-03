package config

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestLoadFromRejectsInvalidEnabledNetworks(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "пустой список", value: ""},
		{name: "пустое имя", value: "base,,ethereum"},
		{name: "неизвестная сеть", value: "unknown"},
		{name: "не нижний регистр", value: "Base"},
		{name: "повтор", value: "base,base"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := validEnvironment()
			values["ENABLED_NETWORKS"] = test.value
			_, err := LoadFrom(mapLookup(values))
			assertErrorField(t, err, "ENABLED_NETWORKS", values)
		})
	}
}

func TestNetworkRegistryHasPinnedChainIDs(t *testing.T) {
	want := map[string]int64{
		"base": 8453, "ethereum": 1, "arbitrum": 42161, "optimism": 10, "polygon": 137,
		"ink": 57073, "scroll": 534352, "linea": 59144, "metis": 1088, "bnb": 56,
	}
	definitions := networkRegistry()
	if len(definitions) != len(want) {
		t.Fatalf("сетей в реестре = %d, нужно %d", len(definitions), len(want))
	}
	for _, definition := range definitions {
		if int64(definition.chainID) != want[definition.name] {
			t.Errorf("chain ID сети %s = %d", definition.name, definition.chainID)
		}
	}
}

func TestLoadFromUsesIndependentProviderOverrides(t *testing.T) {
	values := validEnvironment()
	values["RPC_READ_1_HTTP_BASE"] = "https://read-one.invalid/rpc?b=2&a=1"
	values["RPC_READ_2_HTTP_BASE"] = "http://read-two.invalid:8080/other"
	runtime, err := LoadFrom(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	network := runtime.Networks[0]
	if len(network.ReadProviders) != 2 {
		t.Fatalf("read providers = %d, нужно 2", len(network.ReadProviders))
	}
	first, second := network.ReadProviders[0], network.ReadProviders[1]
	if first.HTTPURL != values["RPC_READ_1_HTTP_BASE"] || second.HTTPURL != values["RPC_READ_2_HTTP_BASE"] || first.WSURL != values["RPC_READ_1_WS_BASE"] || second.WSURL != values["RPC_READ_2_WS_BASE"] {
		t.Fatal("RPC overrides не сохранены независимо и без подмены")
	}
	digest := sha256.Sum256([]byte("https://read-one.invalid/rpc?a=1&b=2"))
	if first.EndpointFingerprint != hex.EncodeToString(digest[:]) || len(first.EndpointFingerprint) != 64 {
		t.Fatalf("неверный безопасный fingerprint: %q", first.EndpointFingerprint)
	}
	if first.ID == second.ID || first.TrustDomain == second.TrustDomain {
		t.Fatal("провайдеры не получили независимые идентификаторы")
	}

	rescuer := common.HexToAddress(testAddress(9))
	domainNetwork := network.Domain(rescuer)
	if domainNetwork.HTTPURL != first.HTTPURL || domainNetwork.WSURL != first.WSURL || !domainNetwork.HasRescuer || domainNetwork.Rescuer != rescuer || domainNetwork.AllowUnknownTokens {
		t.Fatal("Domain() не перенёс параметры daemon integration")
	}
}

func TestLoadFromRejectsIncompleteOrDependentProviders(t *testing.T) {
	tests := []struct {
		name      string
		configure func(map[string]string)
		wantField string
	}{
		{name: "нет первого HTTP", configure: func(values map[string]string) { delete(values, "RPC_READ_1_HTTP_BASE") }, wantField: "RPC_READ_1_HTTP_BASE"},
		{name: "нет второго WS", configure: func(values map[string]string) { delete(values, "RPC_READ_2_WS_BASE") }, wantField: "RPC_READ_2_WS_BASE"},
		{name: "HTTP имеет WS схему", configure: func(values map[string]string) { values["RPC_READ_1_HTTP_BASE"] = "wss://test-only-rpc.invalid" }, wantField: "RPC_READ_1_HTTP_BASE"},
		{name: "WS имеет HTTP схему", configure: func(values map[string]string) { values["RPC_READ_1_WS_BASE"] = "https://test-only-rpc.invalid" }, wantField: "RPC_READ_1_WS_BASE"},
		{name: "один endpoint", configure: func(values map[string]string) { values["RPC_READ_2_HTTP_BASE"] = "https://READ-ONE.invalid:443" }, wantField: "RPC_READ_2_HTTP_BASE"},
		{name: "один WebSocket endpoint", configure: func(values map[string]string) { values["RPC_READ_2_WS_BASE"] = "wss://READ-ONE.invalid:443/ws" }, wantField: "RPC_READ_2_WS_BASE"},
		{name: "один trust domain", configure: func(values map[string]string) { values["RPC_READ_2_TRUST_DOMAIN_BASE"] = "PROVIDER-ONE" }, wantField: "RPC_READ_2_TRUST_DOMAIN_BASE"},
		{name: "нет manifest", configure: func(values map[string]string) { delete(values, "RESCUER_MANIFEST_BASE") }, wantField: "RESCUER_MANIFEST_BASE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := validEnvironment()
			test.configure(values)
			_, err := LoadFrom(mapLookup(values))
			assertErrorField(t, err, test.wantField, values)
		})
	}
}

func TestBroadcastIsRequiredOnlyInLiveMode(t *testing.T) {
	values := validEnvironment()
	if runtime, err := LoadFrom(mapLookup(values)); err != nil || runtime.Networks[0].BroadcastHTTP != "" {
		t.Fatalf("dry run с пустым broadcast: runtime=%v error=%v", runtime, err)
	}
	values["RPC_BROADCAST_HTTP_BASE"] = "https://broadcast.invalid/rpc"
	_, err := LoadFrom(mapLookup(values))
	assertErrorField(t, err, "RPC_BROADCAST_HTTP_BASE", values)
	delete(values, "RPC_BROADCAST_HTTP_BASE")
	values["DRY_RUN"] = "false"
	_, err = LoadFrom(mapLookup(values))
	assertErrorField(t, err, "RPC_BROADCAST_HTTP_BASE", values)
	values["RPC_BROADCAST_HTTP_BASE"] = values["RPC_READ_1_HTTP_BASE"]
	values["SOURCE_PRIVATE_KEY"] = strings.Repeat("0", 63) + "1"
	values["SPONSOR_PRIVATE_KEY"] = strings.Repeat("0", 63) + "2"
	_, err = LoadFrom(mapLookup(values))
	assertErrorField(t, err, "RPC_BROADCAST_HTTP_BASE", values)
}

func TestTokenModes(t *testing.T) {
	knownAddress := "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	unknownAddress := testAddress(99)
	tests := []struct {
		name        string
		mode        string
		allowlist   string
		wantTokens  int
		wantTrusted int
		wantUnknown bool
		wantError   string
		check       func(*testing.T, Network)
	}{
		{name: "known-only по умолчанию", wantTokens: 7, wantTrusted: 7},
		{name: "allowlist", mode: "allowlist", allowlist: knownAddress + "," + unknownAddress, wantTokens: 2, wantTrusted: 1, check: func(t *testing.T, network Network) {
			if network.Tokens[0].Symbol != "USDC" || network.Tokens[0].Decimals != 6 {
				t.Fatal("allowlist потерял metadata известного токена")
			}
			if network.Tokens[1].Symbol != "" || network.Tokens[1].Decimals != 0 || network.Tokens[1].Address != common.HexToAddress(unknownAddress) {
				t.Fatal("неизвестный allowlisted токен получил недостоверную metadata")
			}
			if network.TrustedTokens[0] != common.HexToAddress(knownAddress) {
				t.Fatal("пересечение allowlist с известными токенами потеряло trust")
			}
			domainNetwork := network.Domain(common.Address{})
			if len(domainNetwork.Tokens) != 2 || domainNetwork.Tokens[1].Address != common.HexToAddress(unknownAddress) {
				t.Fatal("watcher allowlist потерял разрешённый неизвестный адрес")
			}
		}},
		{name: "all", mode: "all", wantTokens: 7, wantTrusted: 7, wantUnknown: true},
		{name: "allowlist вне режима", mode: "known-only", allowlist: unknownAddress, wantError: "TOKEN_ALLOWLIST_BASE"},
		{name: "пустой allowlist", mode: "allowlist", wantError: "TOKEN_ALLOWLIST_BASE"},
		{name: "повтор в allowlist", mode: "allowlist", allowlist: unknownAddress + "," + unknownAddress, wantError: "TOKEN_ALLOWLIST_BASE"},
		{name: "повреждённый токен", mode: "allowlist", allowlist: "test-only-token-address", wantError: "TOKEN_ALLOWLIST_BASE"},
		{name: "нулевой токен", mode: "allowlist", allowlist: common.Address{}.Hex(), wantError: "TOKEN_ALLOWLIST_BASE"},
		{name: "неизвестный режим", mode: "automatic", wantError: "TOKEN_MODE_BASE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := validEnvironment()
			if test.mode != "" {
				values["TOKEN_MODE_BASE"] = test.mode
			}
			if test.allowlist != "" || test.mode == "allowlist" {
				values["TOKEN_ALLOWLIST_BASE"] = test.allowlist
			}
			runtime, err := LoadFrom(mapLookup(values))
			if test.wantError != "" {
				assertErrorField(t, err, test.wantError, values)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			network := runtime.Networks[0]
			if len(network.Tokens) != test.wantTokens || len(network.TrustedTokens) != test.wantTrusted || network.AllowUnknownTokens != test.wantUnknown {
				t.Fatalf("политика токенов: count=%d trusted=%d allowUnknown=%t", len(network.Tokens), len(network.TrustedTokens), network.AllowUnknownTokens)
			}
			if test.check != nil {
				test.check(t, network)
			}
		})
	}
}

func TestUnknownAllowlistedTokenIsAllowedButNotTrusted(t *testing.T) {
	known := common.HexToAddress("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913")
	unknown := common.HexToAddress(testAddress(99))
	values := validEnvironment()
	values["TOKEN_MODE_BASE"] = "allowlist"
	values["TOKEN_ALLOWLIST_BASE"] = known.Hex() + "," + unknown.Hex()
	runtimeConfig, err := LoadFrom(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	network := runtimeConfig.Networks[0]
	if len(network.Tokens) != 2 || network.Tokens[1].Address != unknown {
		t.Fatal("allowlist не сохранил unknown address для watcher")
	}
	if len(network.TrustedTokens) != 1 || network.TrustedTokens[0] != known {
		t.Fatalf("trusted intersection = %v, нужен только известный токен", network.TrustedTokens)
	}
}

func TestSupportedEnvironmentFieldsIncludeEveryNetwork(t *testing.T) {
	supported := make(map[string]bool)
	for _, field := range SupportedEnvironmentFields() {
		supported[field] = true
	}
	for _, network := range []string{"BASE", "ETHEREUM", "ARBITRUM", "OPTIMISM", "POLYGON", "INK", "SCROLL", "LINEA", "METIS", "BNB"} {
		for _, template := range EnvironmentFieldTemplates() {
			field := strings.ReplaceAll(template, "<N>", network)
			if !supported[field] {
				t.Errorf("не зарегистрировано динамическое поле %s", field)
			}
		}
	}
	for _, field := range []string{"SOURCE_PRIVATE_KEY", "SPONSOR_PRIVATE_KEY"} {
		if !supported[field] {
			t.Errorf("не зарегистрировано секретное поле %s", field)
		}
	}
	for _, field := range removedEnvironmentFields {
		if supported[field] {
			t.Errorf("удалённое поле отмечено поддерживаемым: %s", field)
		}
	}
}

func TestEveryRegisteredNetworkUsesItsDynamicFields(t *testing.T) {
	for _, definition := range networkRegistry() {
		t.Run(definition.name, func(t *testing.T) {
			values := map[string]string{
				"SOURCE_ADDRESS":      testAddress(1),
				"SPONSOR_ADDRESS":     testAddress(2),
				"DESTINATION_ADDRESS": testAddress(3),
				"ENABLED_NETWORKS":    definition.name,
			}
			suffix := strings.ToUpper(definition.name)
			values["RPC_READ_1_HTTP_"+suffix] = "https://read-one.invalid/" + definition.name
			values["RPC_READ_1_WS_"+suffix] = "wss://read-one.invalid/" + definition.name
			values["RPC_READ_1_TRUST_DOMAIN_"+suffix] = "provider-one-" + definition.name
			values["RPC_READ_2_HTTP_"+suffix] = "https://read-two.invalid/" + definition.name
			values["RPC_READ_2_WS_"+suffix] = "wss://read-two.invalid/" + definition.name
			values["RPC_READ_2_TRUST_DOMAIN_"+suffix] = "provider-two-" + definition.name
			values["RESCUER_MANIFEST_"+suffix] = "testdata/" + definition.name + "-manifest.json"

			runtimeConfig, err := LoadFrom(mapLookup(values))
			if err != nil {
				t.Fatal(err)
			}
			if len(runtimeConfig.Networks) != 1 || runtimeConfig.Networks[0].Name != definition.name || runtimeConfig.Networks[0].ChainID != definition.chainID {
				t.Fatalf("network config = %#v", runtimeConfig.Networks)
			}
		})
	}
}
