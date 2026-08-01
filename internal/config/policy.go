package config

import (
	"fmt"
	"io"
	"math/big"
	"strconv"
	"time"
)

const (
	defaultArtifactPath           = "artifacts/contracts/RescuerV2.json"
	pinnedArtifactSHA256          = "sha256:96c6c0d358e44980814b40553358c53ce5231b7c2ce4ef9bf99add3b9a96a4af"
	pinnedArtifactSourceKind      = "source-tree-sha256"
	pinnedArtifactSourceValue     = "sha256:e7fcd5fa1da201b9a4aa728ad6758032250919eb438733e3f57c61eef8f2971a"
	pinnedArtifactCompilerVersion = "0.8.36+commit.8a079791.Emscripten.clang"
)

// ArtifactTrust закрепляет происхождение и идентичность contract artifact.
type ArtifactTrust struct {
	Path            string
	SHA256          string
	SourceKind      string
	SourceValue     string
	CompilerVersion string
}

// Format исключает локальный путь из случайного форматирования.
func (ArtifactTrust) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "ArtifactTrust{закреплён}")
}

// RuntimePolicy содержит проверенные ограничения для последующего enforcement.
type RuntimePolicy struct {
	MaxTransactionCostWei    *big.Int
	CumulativeBudgetWei      *big.Int
	SponsorMinimumBalanceWei *big.Int
	RateLimitPerMinute       uint32
}

func loadArtifactTrust(lookup func(string) (string, bool)) (ArtifactTrust, error) {
	path, ok := lookup("RESCUER_ARTIFACT")
	if !ok {
		path = defaultArtifactPath
	}
	if path == "" {
		return ArtifactTrust{}, fmt.Errorf("переменная RESCUER_ARTIFACT не должна быть пустой")
	}
	return ArtifactTrust{
		Path:            path,
		SHA256:          pinnedArtifactSHA256,
		SourceKind:      pinnedArtifactSourceKind,
		SourceValue:     pinnedArtifactSourceValue,
		CompilerVersion: pinnedArtifactCompilerVersion,
	}, nil
}

func loadRuntimePolicy(lookup func(string) (string, bool)) (RuntimePolicy, error) {
	maxCost, err := loadPositiveDecimal(lookup, "MAX_TRANSACTION_COST_WEI", "10000000000000000")
	if err != nil {
		return RuntimePolicy{}, err
	}
	cumulative, err := loadPositiveDecimal(lookup, "CUMULATIVE_BUDGET_WEI", "50000000000000000")
	if err != nil {
		return RuntimePolicy{}, err
	}
	if cumulative.Cmp(maxCost) < 0 {
		return RuntimePolicy{}, fmt.Errorf("переменная CUMULATIVE_BUDGET_WEI не должна быть меньше MAX_TRANSACTION_COST_WEI")
	}
	minimumBalance, err := loadPositiveDecimal(lookup, "SPONSOR_MIN_BALANCE_WEI", "20000000000000000")
	if err != nil {
		return RuntimePolicy{}, err
	}
	rateLimit, err := loadPositiveUint32(lookup, "RATE_LIMIT_PER_MINUTE", "6")
	if err != nil {
		return RuntimePolicy{}, err
	}
	return RuntimePolicy{
		MaxTransactionCostWei:    maxCost,
		CumulativeBudgetWei:      cumulative,
		SponsorMinimumBalanceWei: minimumBalance,
		RateLimitPerMinute:       rateLimit,
	}, nil
}

func loadPositiveDecimal(lookup func(string) (string, bool), name, defaultValue string) (*big.Int, error) {
	value, ok := lookup(name)
	if !ok {
		value = defaultValue
	}
	if !decimalDigits(value) {
		return nil, fmt.Errorf("переменная %s должна быть положительным целым десятичным числом", name)
	}
	result, ok := new(big.Int).SetString(value, 10)
	if !ok || result.Sign() <= 0 {
		return nil, fmt.Errorf("переменная %s должна быть положительным целым десятичным числом", name)
	}
	return result, nil
}

func loadPositiveUint32(lookup func(string) (string, bool), name, defaultValue string) (uint32, error) {
	value, ok := lookup(name)
	if !ok {
		value = defaultValue
	}
	if !decimalDigits(value) {
		return 0, fmt.Errorf("переменная %s должна быть положительным целым десятичным числом", name)
	}
	result, err := strconv.ParseUint(value, 10, 32)
	if err != nil || result == 0 {
		return 0, fmt.Errorf("переменная %s должна быть положительным целым десятичным числом", name)
	}
	return uint32(result), nil
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
	value, ok := lookup("RPC_READ_TIMEOUT")
	if !ok {
		value = "10s"
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("переменная RPC_READ_TIMEOUT должна задавать положительную длительность")
	}
	return duration, nil
}
