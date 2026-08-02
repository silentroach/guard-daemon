package config

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestLoadFromDryRunDoesNotReadPrivateKeys(t *testing.T) {
	values := validEnvironment()
	read := make(map[string]bool)
	runtime, err := LoadFrom(func(name string) (string, bool) {
		if name == "SOURCE_PRIVATE_KEY" || name == "SPONSOR_PRIVATE_KEY" {
			t.Fatalf("dry run прочитал %s", name)
		}
		read[name] = true
		value, ok := values[name]
		return value, ok
	})
	if err != nil {
		t.Fatalf("LoadFrom() вернул ошибку: %v", err)
	}
	if !runtime.Mode.IsDryRun() || runtime.Mode.IsLive() {
		t.Fatalf("получен неверный режим: %v", runtime.Mode)
	}
	if source, sponsor, ok := runtime.LiveSecrets.PrivateKeys(); ok || source != nil || sponsor != nil {
		t.Fatal("dry run сохранил приватные ключи")
	}
	if read["SOURCE_PRIVATE_KEY"] || read["SPONSOR_PRIVATE_KEY"] {
		t.Fatal("dry run отметил чтение приватного ключа")
	}
}

func TestLoadFromRequiresStrictDryRunBoolean(t *testing.T) {
	for _, value := range []string{"", "TRUE", "1", " false"} {
		t.Run(fmt.Sprintf("значение_%q", value), func(t *testing.T) {
			values := validEnvironment()
			values["DRY_RUN"] = value
			_, err := LoadFrom(mapLookup(values))
			assertErrorField(t, err, "DRY_RUN", values)
		})
	}
}

func TestLoadFromLiveSecrets(t *testing.T) {
	tests := []struct {
		name      string
		configure func(map[string]string)
		wantError string
	}{
		{name: "полная конфигурация"},
		{name: "нет source key", configure: func(values map[string]string) { delete(values, "SOURCE_PRIVATE_KEY") }, wantError: "SOURCE_PRIVATE_KEY"},
		{name: "нет sponsor key", configure: func(values map[string]string) { delete(values, "SPONSOR_PRIVATE_KEY") }, wantError: "SPONSOR_PRIVATE_KEY"},
		{name: "повреждён source key", configure: func(values map[string]string) { values["SOURCE_PRIVATE_KEY"] = "test-only-secret-material" }, wantError: "SOURCE_PRIVATE_KEY"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := validEnvironment()
			values["DRY_RUN"] = "false"
			values["RPC_BROADCAST_HTTP_BASE"] = "https://broadcast.invalid/rpc"
			values["SOURCE_PRIVATE_KEY"] = fmt.Sprintf("%064x", 1)
			values["SPONSOR_PRIVATE_KEY"] = "0x" + fmt.Sprintf("%064x", 2)
			if test.configure != nil {
				test.configure(values)
			}

			runtime, err := LoadFrom(mapLookup(values))
			if test.wantError != "" {
				assertErrorField(t, err, test.wantError, values)
				return
			}
			if err != nil {
				t.Fatalf("LoadFrom() вернул ошибку: %v", err)
			}
			if !runtime.Mode.IsLive() {
				t.Fatal("DRY_RUN=false не включил live-режим")
			}
			if source, sponsor, ok := runtime.LiveSecrets.PrivateKeys(); !ok || source == nil || sponsor == nil {
				t.Fatal("live-режим не сохранил оба разобранных ключа")
			}
		})
	}
}

func TestLoadFromRejectsInvalidRoles(t *testing.T) {
	tests := []struct {
		name      string
		configure func(map[string]string)
		wantField string
	}{
		{name: "повреждён source", configure: func(values map[string]string) { values["SOURCE_ADDRESS"] = "test-only-address" }, wantField: "SOURCE_ADDRESS"},
		{name: "нулевой sponsor", configure: func(values map[string]string) { values["SPONSOR_ADDRESS"] = common.Address{}.Hex() }, wantField: "SPONSOR_ADDRESS"},
		{name: "source равен sponsor", configure: func(values map[string]string) { values["SPONSOR_ADDRESS"] = values["SOURCE_ADDRESS"] }, wantField: "SOURCE_ADDRESS"},
		{name: "source равен destination", configure: func(values map[string]string) { values["DESTINATION_ADDRESS"] = values["SOURCE_ADDRESS"] }, wantField: "DESTINATION_ADDRESS"},
		{name: "sponsor равен destination", configure: func(values map[string]string) { values["DESTINATION_ADDRESS"] = values["SPONSOR_ADDRESS"] }, wantField: "SPONSOR_ADDRESS"},
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

func TestLoadFromRejectsRemovedFields(t *testing.T) {
	for _, field := range []string{"RESCUE_TOKENS", "TOKENS_TO_SWEEP", "CLAIM_CONTRACT", "CLAIM_DATA_HEX", "RPC_URL_BASE", "RPC_URL_ETHEREUM", "RPC_URL_ARBITRUM", "RPC_URL_OPTIMISM", "RPC_URL_POLYGON", "RPC_URL_INK", "RPC_URL_SCROLL", "RPC_URL_LINEA", "RPC_URL_METIS", "RPC_URL_BNB", "RESCUER_BASE", "RESCUER_ZKSYNC"} {
		t.Run(field, func(t *testing.T) {
			values := validEnvironment()
			values[field] = "test-only-removed-value"
			_, err := LoadFrom(mapLookup(values))
			assertErrorField(t, err, field, values)
		})
	}
}

func TestLoadRejectsMalformedDotEnv(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	if err := os.WriteFile(".env", []byte("BROKEN='unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil || err.Error() != "файл .env содержит некорректные данные" {
		t.Fatalf("ошибка = %v", err)
	}
}

func TestLoadGivesProcessEnvironmentPriority(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	if err := os.WriteFile(".env", []byte("DRY_RUN=not-a-boolean\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, value := range validEnvironment() {
		t.Setenv(name, value)
	}
	t.Setenv("DRY_RUN", "true")

	runtime, err := Load()
	if err != nil {
		t.Fatalf("Load() не сохранил приоритет process env: %v", err)
	}
	if !runtime.Mode.IsDryRun() {
		t.Fatal("process env не переопределил DRY_RUN из .env")
	}
}

func TestLoadDryRunDoesNotParsePrivateKeysFromDotEnv(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	values := validEnvironment()
	var dotenv strings.Builder
	for name, value := range values {
		fmt.Fprintf(&dotenv, "%s=%s\n", name, value)
	}
	dotenv.WriteString("SOURCE_PRIVATE_KEY='unterminated private canary\n")
	dotenv.WriteString("SPONSOR_PRIVATE_KEY=test-only-private-canary\n")
	if err := os.WriteFile(".env", []byte(dotenv.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOURCE_PRIVATE_KEY", "")
	t.Setenv("SPONSOR_PRIVATE_KEY", "")

	runtimeConfig, err := Load()
	if err != nil {
		t.Fatalf("Load() разобрал private key из dry-run .env: %v", err)
	}
	if !runtimeConfig.Mode.IsDryRun() {
		t.Fatal("Load() не сохранил безопасный dry-run режим")
	}
}

func TestLoadLiveAcceptsPrivateKeysOnlyFromProcessEnvironment(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	values := validEnvironment()
	values["DRY_RUN"] = "false"
	values["RPC_BROADCAST_HTTP_BASE"] = "https://broadcast.invalid/rpc"
	values["SOURCE_PRIVATE_KEY"] = fmt.Sprintf("%064x", 1)
	values["SPONSOR_PRIVATE_KEY"] = fmt.Sprintf("%064x", 2)
	var dotenv strings.Builder
	for name, value := range values {
		fmt.Fprintf(&dotenv, "%s=%s\n", name, value)
	}
	if err := os.WriteFile(".env", []byte(dotenv.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SOURCE_PRIVATE_KEY", "SPONSOR_PRIVATE_KEY"} {
		previous, existed := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(name, previous)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "SOURCE_PRIVATE_KEY") {
		t.Fatalf("Load() использовал private key из .env: %v", err)
	}
}

func TestLoadRejectsAlternativeDotEnvGrammarBeforeParsingValues(t *testing.T) {
	tests := []string{
		"SOURCE_PRIVATE_KEY: test-only-private-canary",
		"export\tSPONSOR_PRIVATE_KEY=test-only-private-canary",
		"CLAIM_ARBITRARY: test-only-private-canary",
		"export\tRPC_URL_UNKNOWN=test-only-private-canary",
	}
	for _, extra := range tests {
		t.Run(strings.SplitN(extra, " ", 2)[0], func(t *testing.T) {
			directory := t.TempDir()
			t.Chdir(directory)
			var dotenv strings.Builder
			for name, value := range validEnvironment() {
				fmt.Fprintf(&dotenv, "%s=%s\n", name, value)
			}
			dotenv.WriteString(extra)
			dotenv.WriteByte('\n')
			if err := os.WriteFile(".env", []byte(dotenv.String()), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := Load()
			if err == nil || err.Error() != "файл .env содержит некорректные данные" {
				t.Fatalf("Load() error = %v", err)
			}
			if strings.Contains(err.Error(), "test-only-private-canary") {
				t.Fatalf("ошибка раскрывает значение: %q", err)
			}
		})
	}
}

func TestLoadFromMapRejectsReservedUnknownFields(t *testing.T) {
	for _, field := range []string{"CLAIM_ARBITRARY", "RPC_URL_UNKNOWN", "RESCUER_UNKNOWN", "RPC_READ_3_HTTP_BASE", "STATE_DIRECTOR", "TOKEN_MODE_UNKNOWN", "WATCH_LOOKBACK_BLOCK"} {
		t.Run(field, func(t *testing.T) {
			values := validEnvironment()
			values[field] = "test-only-unsupported"
			_, err := LoadFromMap(values)
			assertErrorField(t, err, field, values)
		})
	}
}

func TestFormattingRedactsRuntimeInputs(t *testing.T) {
	values := validEnvironment()
	values["RESCUER_ARTIFACT"] = "test-only-private-path"
	values["RPC_READ_1_HTTP_BASE"] = "https://test-only-private-rpc.invalid/key"
	runtime, err := LoadFrom(mapLookup(values))
	if err != nil {
		t.Fatal(err)
	}
	domainNetwork := runtime.Networks[0].Domain(common.HexToAddress(testAddress(9)))
	for _, formatted := range []string{fmt.Sprintf("%v", runtime), fmt.Sprintf("%#v", runtime.LiveSecrets), fmt.Sprintf("%+v", runtime.Networks[0]), fmt.Sprintf("%#v", runtime.Artifact), fmt.Sprintf("%+v", domainNetwork)} {
		if strings.Contains(formatted, "test-only-private") {
			t.Fatalf("форматирование раскрывает входные данные: %q", formatted)
		}
	}
}

func TestValidationErrorsRedactCredentialBearingValues(t *testing.T) {
	values := validEnvironment()
	const canary = "private-rpc-credential-canary"
	values["RPC_READ_1_HTTP_BASE"] = "https://user:" + canary + "@invalid host"
	_, err := LoadFrom(mapLookup(values))
	if err == nil || !strings.Contains(err.Error(), "RPC_READ_1_HTTP_BASE") {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), values["RPC_READ_1_HTTP_BASE"]) {
		t.Fatalf("ошибка раскрывает RPC credentials: %q", err)
	}
}

func validEnvironment() map[string]string {
	return map[string]string{
		"SOURCE_ADDRESS":               testAddress(1),
		"SPONSOR_ADDRESS":              testAddress(2),
		"DESTINATION_ADDRESS":          testAddress(3),
		"ENABLED_NETWORKS":             "base",
		"RPC_READ_1_HTTP_BASE":         "https://read-one.invalid/",
		"RPC_READ_1_WS_BASE":           "wss://read-one.invalid/ws",
		"RPC_READ_1_TRUST_DOMAIN_BASE": "provider-one",
		"RPC_READ_2_HTTP_BASE":         "https://read-two.invalid/",
		"RPC_READ_2_WS_BASE":           "wss://read-two.invalid/ws",
		"RPC_READ_2_TRUST_DOMAIN_BASE": "provider-two",
		"RESCUER_MANIFEST_BASE":        "testdata/base-manifest.json",
	}
}

func testAddress(value byte) string {
	return fmt.Sprintf("0x%040x", value)
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func assertErrorField(t *testing.T, err error, field string, values map[string]string) {
	t.Helper()
	if err == nil {
		t.Fatalf("LoadFrom() не отклонил поле %s", field)
	}
	if !strings.Contains(err.Error(), field) {
		t.Fatalf("ошибка %q не называет поле %s", err, field)
	}
	for _, value := range values {
		if strings.HasPrefix(value, "test-only-") && strings.Contains(err.Error(), value) {
			t.Fatalf("ошибка раскрывает входное значение: %q", err)
		}
	}
}
