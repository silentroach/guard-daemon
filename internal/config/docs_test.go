package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicConfigurationDocumentsOnlySupportedFields(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	supported := make(map[string]struct{})
	for _, field := range SupportedEnvironmentFields() {
		supported[field] = struct{}{}
	}

	example, err := os.Open(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	defer example.Close()
	scanner := bufio.NewScanner(example)
	assignments := make(map[string]string)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf(".env.example line is not an assignment: %q", line)
		}
		if _, exists := supported[name]; !exists {
			t.Errorf(".env.example documents unsupported field %s", name)
		}
		assignments[name] = value
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if assignments["DRY_RUN"] != "true" {
		t.Fatal(".env.example does not preserve safe dry-run mode by default")
	}
	if assignments["STATE_DIRECTORY"] != defaultStateDirectory {
		t.Fatal(".env.example does not match the default STATE_DIRECTORY")
	}
	if assignments["WATCH_LOOKBACK_BLOCKS"] != defaultLookbackBlocks {
		t.Fatal(".env.example does not match the default WATCH_LOOKBACK_BLOCKS")
	}
	if _, exists := assignments["SOURCE_PRIVATE_KEY"]; exists {
		t.Fatal(".env.example must not suggest storing the source key in a file")
	}
	if _, exists := assignments["SPONSOR_PRIVATE_KEY"]; exists {
		t.Fatal(".env.example must not suggest storing the sponsor key in a file")
	}
	for _, role := range []string{"SOURCE_ADDRESS", "SPONSOR_ADDRESS", "DESTINATION_ADDRESS"} {
		if !strings.HasPrefix(assignments[role], "0xYOUR_") {
			t.Errorf(".env.example contains a non-placeholder value for %s", role)
		}
	}
	if _, exists := assignments["RPC_BROADCAST_HTTP_BASE"]; exists {
		t.Fatal(".env.example must not enable production broadcast RPC by default")
	}
	for field, want := range map[string]string{
		"MAX_TRANSACTION_COST_WEI":                    defaultMaxTransactionCostWei,
		"HOURLY_BUDGET_WEI":                           defaultHourlyBudgetWei,
		"DAILY_BUDGET_WEI":                            defaultDailyBudgetWei,
		"CUMULATIVE_BUDGET_WEI":                       defaultCumulativeBudgetWei,
		"SPONSOR_MIN_BALANCE_WEI":                     defaultSponsorMinimumBalanceWei,
		"RATE_LIMIT_PER_MINUTE":                       defaultRateLimitPerMinute,
		"ABUSE_WINDOW":                                defaultAbuseWindow,
		"MAX_NEW_UNKNOWN_TOKENS_PER_WINDOW":           defaultMaxNewUnknownTokensPerWindow,
		"MAX_ATTEMPTS_PER_TOKEN_WINDOW":               defaultMaxAttemptsPerTokenWindow,
		"MAX_ATTEMPTS_PER_SOURCE_EVENT":               defaultMaxAttemptsPerSourceEvent,
		"EMERGENCY_STOP":                              defaultEmergencyStop,
		"ALERT_COOLDOWN":                              defaultAlertCooldown,
		"NETWORK_MAX_TRANSACTION_COST_WEI_BASE":       defaultNetworkMaxTransactionCostWei,
		"NETWORK_HOURLY_BUDGET_WEI_BASE":              defaultNetworkHourlyBudgetWei,
		"NETWORK_DAILY_BUDGET_WEI_BASE":               defaultNetworkDailyBudgetWei,
		"NETWORK_CUMULATIVE_BUDGET_WEI_BASE":          defaultNetworkCumulativeBudgetWei,
		"NETWORK_SPONSOR_MIN_BALANCE_WEI_BASE":        defaultNetworkSponsorMinimumBalanceWei,
		"TOKEN_GAS_LIMIT_BASE":                        defaultTokenGasLimit,
		"NATIVE_GAS_LIMIT_BASE":                       defaultNativeGasLimit,
		"NATIVE_MIN_NET_VALUE_WEI_BASE":               defaultNativeMinimumNetValueWei,
		"UNKNOWN_TOKEN_MAX_TRANSACTION_COST_WEI_BASE": defaultUnknownTokenMaxTransactionCostWei,
		"MAX_FEE_PER_GAS_WEI_BASE":                    "20000000000",
		"MAX_PRIORITY_FEE_PER_GAS_WEI_BASE":           "2000000000",
		"CHAIN_OVERHEAD_MAX_WEI_BASE":                 "500000000000000",
	} {
		if assignments[field] != want {
			t.Errorf(".env.example: %s=%q, want %q", field, assignments[field], want)
		}
	}

	documentation, err := os.ReadFile(filepath.Join(root, "docs", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(documentation)
	for _, field := range staticEnvironmentFields {
		if !strings.Contains(text, "`"+field+"`") {
			t.Errorf("docs/configuration.md does not document field %s", field)
		}
	}
	for _, template := range EnvironmentFieldTemplates() {
		if !strings.Contains(text, "`"+template+"`") {
			t.Errorf("docs/configuration.md does not document template %s", template)
		}
	}
}

func TestEconomicAndTokenTrustDocumentationParity(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	paths := []string{
		filepath.Join(root, ".env.example"),
		filepath.Join(root, "docs", "configuration.md"),
	}
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(contents)
		if strings.Contains(text, "пока только разбираются") || strings.Contains(text, "относится к Task 08") {
			t.Errorf("%s contains stale phantom Task 08 description", path)
		}
	}

	documentation, err := os.ReadFile(filepath.Join(root, "docs", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(documentation)
	for _, statement := range []string{
		"Неизвестный адрес из списка разрешённых остаётся доступным компоненту",
		"наблюдения, но не получает известных метаданных или доверенной оценки ценности",
		"дополнительно ограничена",
		"UNKNOWN_TOKEN_MAX_TRANSACTION_COST_WEI_<N>",
		"при запуске демон выдаёт предупреждение оператору",
		"TOKEN_VALUE_RULES_<N>",
		"Для любого ERC-20 итог всегда имеет статус",
		"token-reported",
	} {
		if !strings.Contains(text, statement) {
			t.Error("docs/configuration.md is missing a required token trust policy statement")
		}
	}
}

func TestWatchPolicyDocumentationParity(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	paths := []string{
		filepath.Join(root, ".env.example"),
		filepath.Join(root, "docs", "configuration.md"),
	}
	want := []string{
		"ограниченное число предыдущих блоков до согласованного финализированного блока",
		"Локальное состояние обязательно для восстановления после сбоя",
		"Повреждение состояния, несовпадение сохранённого идентификатора сети или конфигурации",
		"невозможность получить исключительную блокировку приводят к ошибке запуска",
		"Контрольная точка продвигается только после надёжной записи `Ack` для соответствующих кандидатов",
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range want {
				if !strings.Contains(string(contents), statement) {
					t.Errorf("%s is missing a required policy statement", path)
				}
			}
		})
	}
}
