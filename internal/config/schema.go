package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/joho/godotenv"
)

// Runtime содержит полностью проверенную конфигурацию процесса.
type Runtime struct {
	Mode           Mode
	SourceAddress  common.Address
	SponsorAddress common.Address
	Destination    common.Address
	Networks       []Network
	Artifact       ArtifactTrust
	Policy         RuntimePolicy
	Watch          WatchPolicy
	ReadTimeout    time.Duration
	LiveSecrets    LiveSecrets
}

// Format исключает секреты, RPC URL и локальные пути из форматированного вывода.
func (runtime Runtime) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "Runtime{Mode:"+runtime.Mode.String()+" Networks:"+strconv.Itoa(len(runtime.Networks))+"}")
}

// Load загружает необязательный файл .env, сохраняя приоритет окружения процесса.
func Load() (Runtime, error) {
	return loadRuntime(nil, false)
}

// LoadWithSecrets загружает открытую часть конфигурации, а ключи рабочего режима
// принимает только из уже защищённого источника учётных данных.
func LoadWithSecrets(secrets map[string]string) (Runtime, error) {
	for _, name := range []string{"SOURCE_PRIVATE_KEY", "SPONSOR_PRIVATE_KEY"} {
		if _, present := os.LookupEnv(name); present {
			return Runtime{}, fmt.Errorf("environment variable %s is forbidden; use the systemd credentials mechanism", name)
		}
	}
	runtime, err := loadRuntime(secrets, true)
	if err != nil {
		return Runtime{}, err
	}
	if runtime.Mode.IsDryRun() || runtime.Policy.EmergencyStop {
		for _, name := range []string{"SOURCE_PRIVATE_KEY", "SPONSOR_PRIVATE_KEY"} {
			if secrets[name] != "" {
				return Runtime{}, fmt.Errorf("credential %s must be empty when signing is disabled", name)
			}
		}
	}
	return runtime, nil
}

func loadRuntime(secrets map[string]string, credentialsOnly bool) (Runtime, error) {
	path := os.Getenv("GUARD_DAEMON_CONFIG")
	if path == "" {
		path = ".env"
	}
	dotenv, dotenvNames, err := readPublicDotEnv(path)
	if err != nil {
		return Runtime{}, err
	}
	names := append(dotenvNames, processEnvironmentNames()...)
	for name, value := range secrets {
		if value != "" {
			names = append(names, name)
		}
	}
	return loadFrom(func(name string) (string, bool) {
		if credentialsOnly && (name == "SOURCE_PRIVATE_KEY" || name == "SPONSOR_PRIVATE_KEY") {
			value, ok := secrets[name]
			return value, ok && value != ""
		}
		if value, ok := os.LookupEnv(name); ok {
			return value, true
		}
		value, ok := dotenv[name]
		return value, ok
	}, names)
}

// LoadFrom разбирает и проверяет конфигурацию из заданного источника.
func LoadFrom(lookup func(string) (string, bool)) (Runtime, error) {
	return loadFrom(lookup, nil)
}

// LoadFromMap разбирает конфигурацию из map и отклоняет неподдерживаемые имена
// с зарезервированными префиксами.
func LoadFromMap(values map[string]string) (Runtime, error) {
	if values == nil {
		return Runtime{}, fmt.Errorf("environment variables are not provided")
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	return loadFrom(func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}, names)
}

func loadFrom(lookup func(string) (string, bool), names []string) (Runtime, error) {
	if lookup == nil {
		return Runtime{}, fmt.Errorf("environment lookup function is not provided")
	}
	if err := rejectUnsupportedFields(lookup); err != nil {
		return Runtime{}, err
	}
	if err := rejectUnsupportedNames(names); err != nil {
		return Runtime{}, err
	}

	mode, err := loadMode(lookup)
	if err != nil {
		return Runtime{}, err
	}
	source, err := loadAddress(lookup, "SOURCE_ADDRESS")
	if err != nil {
		return Runtime{}, err
	}
	sponsor, err := loadAddress(lookup, "SPONSOR_ADDRESS")
	if err != nil {
		return Runtime{}, err
	}
	destination, err := loadAddress(lookup, "DESTINATION_ADDRESS")
	if err != nil {
		return Runtime{}, err
	}
	if err := validateDistinctRoles(source, sponsor, destination); err != nil {
		return Runtime{}, err
	}

	artifact, err := loadArtifactTrust(lookup)
	if err != nil {
		return Runtime{}, err
	}
	policy, err := loadRuntimePolicy(lookup)
	if err != nil {
		return Runtime{}, err
	}
	networks, err := loadNetworks(lookup, mode, policy)
	if err != nil {
		return Runtime{}, err
	}
	watch, err := loadWatchPolicy(lookup)
	if err != nil {
		return Runtime{}, err
	}
	readTimeout, err := loadReadTimeout(lookup)
	if err != nil {
		return Runtime{}, err
	}

	secrets, err := loadLiveSecrets(lookup, mode, policy.EmergencyStop)
	if err != nil {
		return Runtime{}, err
	}

	return Runtime{
		Mode:           mode,
		SourceAddress:  source,
		SponsorAddress: sponsor,
		Destination:    destination,
		Networks:       networks,
		Artifact:       artifact,
		Policy:         policy,
		Watch:          watch,
		ReadTimeout:    readTimeout,
		LiveSecrets:    secrets,
	}, nil
}

func readPublicDotEnv(path string) (map[string]string, []string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil, nil
		}
		return nil, nil, fmt.Errorf("failed to read .env file safely")
	}
	defer file.Close()

	var public strings.Builder
	var names []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		name, present, valid := strictDotEnvField(line)
		if !valid {
			return nil, nil, fmt.Errorf(".env file contains invalid data")
		}
		if !present {
			public.WriteString(line)
			public.WriteByte('\n')
			continue
		}
		if name != "" {
			names = append(names, name)
		}
		if name == "SOURCE_PRIVATE_KEY" || name == "SPONSOR_PRIVATE_KEY" {
			continue
		}
		public.WriteString(line)
		public.WriteByte('\n')
	}
	if scanner.Err() != nil {
		return nil, nil, fmt.Errorf("failed to read .env file safely")
	}
	values, err := godotenv.Unmarshal(public.String())
	if err != nil {
		return nil, nil, fmt.Errorf(".env file contains invalid data")
	}
	return values, names, nil
}

func strictDotEnvField(line string) (string, bool, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", false, true
	}
	name, _, ok := strings.Cut(trimmed, "=")
	if !ok || name == "" || name != strings.TrimSpace(name) {
		return "", false, false
	}
	for index, character := range name {
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '_' {
			return "", false, false
		}
		if index == 0 && character >= '0' && character <= '9' {
			return "", false, false
		}
	}
	return name, true, true
}

func processEnvironmentNames() []string {
	environment := os.Environ()
	names := make([]string, 0, len(environment))
	for _, assignment := range environment {
		name, _, ok := strings.Cut(assignment, "=")
		if ok {
			names = append(names, name)
		}
	}
	return names
}

var staticEnvironmentFields = []string{
	"ABUSE_WINDOW",
	"ALERT_COOLDOWN",
	"CUMULATIVE_BUDGET_WEI",
	"DAILY_BUDGET_WEI",
	"DESTINATION_ADDRESS",
	"DRY_RUN",
	"EMERGENCY_STOP",
	"ENABLED_NETWORKS",
	"HOURLY_BUDGET_WEI",
	"MAX_ATTEMPTS_PER_SOURCE_EVENT",
	"MAX_ATTEMPTS_PER_TOKEN_WINDOW",
	"MAX_NEW_UNKNOWN_TOKENS_PER_WINDOW",
	"MAX_TRANSACTION_COST_WEI",
	"RATE_LIMIT_PER_MINUTE",
	"RESCUER_ARTIFACT",
	"RPC_READ_TIMEOUT",
	"STATE_DIRECTORY",
	"SOURCE_ADDRESS",
	"SOURCE_PRIVATE_KEY",
	"SPONSOR_ADDRESS",
	"SPONSOR_MIN_BALANCE_WEI",
	"SPONSOR_PRIVATE_KEY",
	"WATCH_LOOKBACK_BLOCKS",
}

var environmentFieldTemplates = []string{
	"CHAIN_OVERHEAD_MAX_WEI_<N>",
	"MAX_FEE_PER_GAS_WEI_<N>",
	"MAX_PRIORITY_FEE_PER_GAS_WEI_<N>",
	"NATIVE_GAS_LIMIT_<N>",
	"NATIVE_MIN_NET_VALUE_WEI_<N>",
	"NETWORK_CUMULATIVE_BUDGET_WEI_<N>",
	"NETWORK_DAILY_BUDGET_WEI_<N>",
	"NETWORK_HOURLY_BUDGET_WEI_<N>",
	"NETWORK_MAX_TRANSACTION_COST_WEI_<N>",
	"NETWORK_SPONSOR_MIN_BALANCE_WEI_<N>",
	"RPC_READ_1_HTTP_<N>",
	"RPC_READ_1_WS_<N>",
	"RPC_READ_1_TRUST_DOMAIN_<N>",
	"RPC_READ_2_HTTP_<N>",
	"RPC_READ_2_WS_<N>",
	"RPC_READ_2_TRUST_DOMAIN_<N>",
	"RPC_BROADCAST_HTTP_<N>",
	"RESCUER_MANIFEST_<N>",
	"RESCUER_RELEASE_COMMIT_<N>",
	"TOKEN_MODE_<N>",
	"TOKEN_ALLOWLIST_<N>",
	"TOKEN_GAS_LIMIT_<N>",
	"TOKEN_VALUE_RULES_<N>",
	"UNKNOWN_TOKEN_MAX_TRANSACTION_COST_WEI_<N>",
}

// SupportedEnvironmentFields возвращает все точные поддерживаемые имена полей.
func SupportedEnvironmentFields() []string {
	fields := append([]string(nil), staticEnvironmentFields...)
	for _, definition := range networkRegistry() {
		suffix := strings.ToUpper(definition.name)
		for _, template := range environmentFieldTemplates {
			fields = append(fields, strings.ReplaceAll(template, "<N>", suffix))
		}
	}
	sort.Strings(fields)
	return fields
}

// EnvironmentFieldTemplates возвращает шаблоны полей с сетевым суффиксом.
func EnvironmentFieldTemplates() []string {
	return append([]string(nil), environmentFieldTemplates...)
}
