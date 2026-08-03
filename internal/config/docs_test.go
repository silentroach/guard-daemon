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
			t.Fatalf("строка .env.example не является присваиванием: %q", line)
		}
		if _, exists := supported[name]; !exists {
			t.Errorf(".env.example документирует неподдерживаемое поле %s", name)
		}
		assignments[name] = value
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if assignments["DRY_RUN"] != "true" {
		t.Fatal(".env.example не сохраняет безопасный dry-run default")
	}
	if assignments["STATE_DIRECTORY"] != defaultStateDirectory {
		t.Fatal(".env.example не совпадает с default STATE_DIRECTORY")
	}
	if assignments["WATCH_LOOKBACK_BLOCKS"] != defaultLookbackBlocks {
		t.Fatal(".env.example не совпадает с default WATCH_LOOKBACK_BLOCKS")
	}
	if _, exists := assignments["SOURCE_PRIVATE_KEY"]; exists {
		t.Fatal(".env.example не должен предлагать хранить source key в файле")
	}
	if _, exists := assignments["SPONSOR_PRIVATE_KEY"]; exists {
		t.Fatal(".env.example не должен предлагать хранить sponsor key в файле")
	}
	for _, role := range []string{"SOURCE_ADDRESS", "SPONSOR_ADDRESS", "DESTINATION_ADDRESS"} {
		if !strings.HasPrefix(assignments[role], "0xYOUR_") {
			t.Errorf(".env.example содержит не-placeholder для %s", role)
		}
	}
	if _, exists := assignments["RPC_BROADCAST_HTTP_BASE"]; exists {
		t.Fatal(".env.example не должен включать live broadcast RPC по умолчанию")
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
			t.Errorf(".env.example: %s=%q, нужно %q", field, assignments[field], want)
		}
	}

	documentation, err := os.ReadFile(filepath.Join(root, "docs", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(documentation)
	for _, field := range staticEnvironmentFields {
		if !strings.Contains(text, "`"+field+"`") {
			t.Errorf("docs/configuration.md не документирует поле %s", field)
		}
	}
	for _, template := range EnvironmentFieldTemplates() {
		if !strings.Contains(text, "`"+template+"`") {
			t.Errorf("docs/configuration.md не документирует шаблон %s", template)
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
			t.Errorf("%s содержит устаревшее phantom-описание Task 08", path)
		}
	}

	documentation, err := os.ReadFile(filepath.Join(root, "docs", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(documentation)
	for _, statement := range []string{
		"Allowlisted unknown address остаётся разрешённым для watcher, но не становится",
		"Неизвестный токен без",
		"доверенной оценки стоимости никогда не получает доверенный результат",
		"дополнительно ограничена",
		"UNKNOWN_TOKEN_MAX_TRANSACTION_COST_WEI_<N>",
		"при запуске демон выдаёт предупреждение оператору",
		"TOKEN_VALUE_RULES_<N>",
	} {
		if !strings.Contains(text, statement) {
			t.Errorf("docs/configuration.md не фиксирует token trust policy: %q", statement)
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
		"ограниченное окно ретроспективного просмотра до согласованного финализированного блока",
		"Локальное состояние обязательно для восстановления после сбоя",
		"Повреждение state, несовпадение сохранённой идентичности сети или конфигурации и невозможность получить эксклюзивную блокировку являются fail-closed ошибками запуска",
		"Checkpoint продвигается только после durable `Ack` соответствующих candidates",
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range want {
				if !strings.Contains(string(contents), statement) {
					t.Errorf("%s не фиксирует policy: %q", path, statement)
				}
			}
		})
	}
}
