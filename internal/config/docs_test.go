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
