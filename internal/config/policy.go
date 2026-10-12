package config

import (
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"time"
)

const (
	defaultArtifactPath           = "artifacts/contracts/RescuerV2.json"
	pinnedArtifactSHA256          = "sha256:96c6c0d358e44980814b40553358c53ce5231b7c2ce4ef9bf99add3b9a96a4af"
	pinnedArtifactSourceKind      = "source-tree-sha256"
	pinnedArtifactSourceValue     = "sha256:e7fcd5fa1da201b9a4aa728ad6758032250919eb438733e3f57c61eef8f2971a"
	pinnedArtifactCompilerVersion = "0.8.36+commit.8a079791.Emscripten.clang"

	defaultMaxTransactionCostWei        = "10000000000000000"
	defaultHourlyBudgetWei              = "20000000000000000"
	defaultDailyBudgetWei               = "30000000000000000"
	defaultCumulativeBudgetWei          = "50000000000000000"
	defaultSponsorMinimumBalanceWei     = "20000000000000000"
	defaultRateLimitPerMinute           = "6"
	defaultAbuseWindow                  = "1h"
	defaultMaxNewUnknownTokensPerWindow = "16"
	defaultMaxAttemptsPerTokenWindow    = "3"
	defaultMaxAttemptsPerSourceEvent    = "3"
	defaultEmergencyStop                = "false"
	defaultAlertCooldown                = "15m"
	defaultReadTimeout                  = "10s"
)

// ArtifactTrust фиксирует источник и идентификатор артефакта контракта.
type ArtifactTrust struct {
	Path            string
	SHA256          string
	SourceKind      string
	SourceValue     string
	CompilerVersion string
}

// Format исключает локальный путь из форматированного вывода.
func (ArtifactTrust) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "ArtifactTrust{pinned}")
}

// RuntimePolicy содержит проверенные глобальные ограничения расходов и меры
// против злоупотреблений.
type RuntimePolicy struct {
	MaxTransactionCostWei        *big.Int
	HourlyBudgetWei              *big.Int
	DailyBudgetWei               *big.Int
	CumulativeBudgetWei          *big.Int
	SponsorMinimumBalanceWei     *big.Int
	RateLimitPerMinute           uint32
	AbuseWindow                  time.Duration
	MaxNewUnknownTokensPerWindow uint32
	MaxAttemptsPerTokenWindow    uint32
	MaxAttemptsPerSourceEvent    uint32
	EmergencyStop                bool
	AlertCooldown                time.Duration
}

// Clone возвращает независимую копию политики с отдельными копиями значений big.Int.
func (policy RuntimePolicy) Clone() RuntimePolicy {
	result := policy
	result.MaxTransactionCostWei = cloneBigInt(policy.MaxTransactionCostWei)
	result.HourlyBudgetWei = cloneBigInt(policy.HourlyBudgetWei)
	result.DailyBudgetWei = cloneBigInt(policy.DailyBudgetWei)
	result.CumulativeBudgetWei = cloneBigInt(policy.CumulativeBudgetWei)
	result.SponsorMinimumBalanceWei = cloneBigInt(policy.SponsorMinimumBalanceWei)
	return result
}

func loadArtifactTrust(lookup func(string) (string, bool)) (ArtifactTrust, error) {
	path, ok := lookup("RESCUER_ARTIFACT")
	if !ok {
		path = defaultArtifactPath
	}
	if path == "" {
		return ArtifactTrust{}, fmt.Errorf("RESCUER_ARTIFACT must not be empty")
	}
	return ArtifactTrust{
		Path:            path,
		SHA256:          pinnedArtifactSHA256,
		SourceKind:      pinnedArtifactSourceKind,
		SourceValue:     pinnedArtifactSourceValue,
		CompilerVersion: pinnedArtifactCompilerVersion,
	}, nil
}

func lowercaseHex(value string) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func loadRuntimePolicy(lookup func(string) (string, bool)) (RuntimePolicy, error) {
	maxCost, err := loadPositiveDecimal(lookup, "MAX_TRANSACTION_COST_WEI", defaultMaxTransactionCostWei)
	if err != nil {
		return RuntimePolicy{}, err
	}
	hourly, err := loadPositiveDecimal(lookup, "HOURLY_BUDGET_WEI", defaultHourlyBudgetWei)
	if err != nil {
		return RuntimePolicy{}, err
	}
	daily, err := loadPositiveDecimal(lookup, "DAILY_BUDGET_WEI", defaultDailyBudgetWei)
	if err != nil {
		return RuntimePolicy{}, err
	}
	cumulative, err := loadPositiveDecimal(lookup, "CUMULATIVE_BUDGET_WEI", defaultCumulativeBudgetWei)
	if err != nil {
		return RuntimePolicy{}, err
	}
	if maxCost.Cmp(hourly) > 0 {
		return RuntimePolicy{}, fmt.Errorf("HOURLY_BUDGET_WEI must not be less than MAX_TRANSACTION_COST_WEI")
	}
	if hourly.Cmp(daily) > 0 {
		return RuntimePolicy{}, fmt.Errorf("DAILY_BUDGET_WEI must not be less than HOURLY_BUDGET_WEI")
	}
	if daily.Cmp(cumulative) > 0 {
		return RuntimePolicy{}, fmt.Errorf("CUMULATIVE_BUDGET_WEI must not be less than DAILY_BUDGET_WEI")
	}
	minimumBalance, err := loadPositiveDecimal(lookup, "SPONSOR_MIN_BALANCE_WEI", defaultSponsorMinimumBalanceWei)
	if err != nil {
		return RuntimePolicy{}, err
	}
	rateLimit, err := loadPositiveUint32(lookup, "RATE_LIMIT_PER_MINUTE", defaultRateLimitPerMinute)
	if err != nil {
		return RuntimePolicy{}, err
	}
	abuseWindow, err := loadPositiveDuration(lookup, "ABUSE_WINDOW", defaultAbuseWindow)
	if err != nil {
		return RuntimePolicy{}, err
	}
	maxNewUnknown, err := loadPositiveUint32(lookup, "MAX_NEW_UNKNOWN_TOKENS_PER_WINDOW", defaultMaxNewUnknownTokensPerWindow)
	if err != nil {
		return RuntimePolicy{}, err
	}
	maxTokenAttempts, err := loadPositiveUint32(lookup, "MAX_ATTEMPTS_PER_TOKEN_WINDOW", defaultMaxAttemptsPerTokenWindow)
	if err != nil {
		return RuntimePolicy{}, err
	}
	maxSourceAttempts, err := loadPositiveUint32(lookup, "MAX_ATTEMPTS_PER_SOURCE_EVENT", defaultMaxAttemptsPerSourceEvent)
	if err != nil {
		return RuntimePolicy{}, err
	}
	emergencyStop, err := loadStrictBoolean(lookup, "EMERGENCY_STOP", defaultEmergencyStop)
	if err != nil {
		return RuntimePolicy{}, err
	}
	alertCooldown, err := loadPositiveDuration(lookup, "ALERT_COOLDOWN", defaultAlertCooldown)
	if err != nil {
		return RuntimePolicy{}, err
	}
	return (RuntimePolicy{
		MaxTransactionCostWei:        maxCost,
		HourlyBudgetWei:              hourly,
		DailyBudgetWei:               daily,
		CumulativeBudgetWei:          cumulative,
		SponsorMinimumBalanceWei:     minimumBalance,
		RateLimitPerMinute:           rateLimit,
		AbuseWindow:                  abuseWindow,
		MaxNewUnknownTokensPerWindow: maxNewUnknown,
		MaxAttemptsPerTokenWindow:    maxTokenAttempts,
		MaxAttemptsPerSourceEvent:    maxSourceAttempts,
		EmergencyStop:                emergencyStop,
		AlertCooldown:                alertCooldown,
	}).Clone(), nil
}

func loadPositiveDecimal(lookup func(string) (string, bool), name, defaultValue string) (*big.Int, error) {
	return loadDecimal(lookup, name, defaultValue, false)
}

func loadNonNegativeDecimal(lookup func(string) (string, bool), name, defaultValue string) (*big.Int, error) {
	return loadDecimal(lookup, name, defaultValue, true)
}

func loadDecimal(lookup func(string) (string, bool), name, defaultValue string, allowZero bool) (*big.Int, error) {
	value, ok := lookup(name)
	if !ok {
		value = defaultValue
	}
	if !decimalDigits(value) {
		return nil, decimalError(name, allowZero)
	}
	result, ok := new(big.Int).SetString(value, 10)
	if !ok || result.Sign() < 0 || (!allowZero && result.Sign() == 0) || result.BitLen() > 256 {
		return nil, decimalError(name, allowZero)
	}
	return result, nil
}

func decimalError(name string, allowZero bool) error {
	if allowZero {
		return fmt.Errorf("environment variable %s must be a non-negative decimal uint256 integer", name)
	}
	return fmt.Errorf("environment variable %s must be a positive decimal uint256 integer", name)
}

func loadPositiveUint32(lookup func(string) (string, bool), name, defaultValue string) (uint32, error) {
	value, ok := lookup(name)
	if !ok {
		value = defaultValue
	}
	if !decimalDigits(value) {
		return 0, fmt.Errorf("environment variable %s must be a positive decimal integer", name)
	}
	result, err := strconv.ParseUint(value, 10, 32)
	if err != nil || result == 0 {
		return 0, fmt.Errorf("environment variable %s must be a positive decimal integer", name)
	}
	return uint32(result), nil
}

func loadPositiveUint64(lookup func(string) (string, bool), name, defaultValue string) (uint64, error) {
	value, ok := lookup(name)
	if !ok {
		value = defaultValue
	}
	if !decimalDigits(value) {
		return 0, fmt.Errorf("environment variable %s must be a positive decimal integer", name)
	}
	result, err := strconv.ParseUint(value, 10, 64)
	if err != nil || result == 0 {
		return 0, fmt.Errorf("environment variable %s must be a positive decimal integer", name)
	}
	return result, nil
}

func loadPositiveDuration(lookup func(string) (string, bool), name, defaultValue string) (time.Duration, error) {
	value, ok := lookup(name)
	if !ok {
		value = defaultValue
	}
	if value == "" || strings.TrimSpace(value) != value || value[0] == '+' || value[0] == '-' {
		return 0, fmt.Errorf("environment variable %s must specify a positive duration", name)
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("environment variable %s must specify a positive duration", name)
	}
	return duration, nil
}

func loadStrictBoolean(lookup func(string) (string, bool), name, defaultValue string) (bool, error) {
	value, ok := lookup(name)
	if !ok {
		value = defaultValue
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("environment variable %s must be true or false", name)
	}
}

func cloneBigInt(value *big.Int) *big.Int {
	if value == nil {
		return nil
	}
	return new(big.Int).Set(value)
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func loadReadTimeout(lookup func(string) (string, bool)) (time.Duration, error) {
	return loadPositiveDuration(lookup, "RPC_READ_TIMEOUT", defaultReadTimeout)
}
