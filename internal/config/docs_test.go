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
