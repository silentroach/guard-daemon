package config

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const (
	defaultNetworkMaxTransactionCostWei      = "8000000000000000"
	defaultNetworkHourlyBudgetWei            = "15000000000000000"
	defaultNetworkDailyBudgetWei             = "25000000000000000"
	defaultNetworkCumulativeBudgetWei        = "40000000000000000"
	defaultNetworkSponsorMinimumBalanceWei   = "20000000000000000"
	defaultTokenGasLimit                     = "300000"
	defaultNativeGasLimit                    = "100000"
	defaultNativeMinimumNetValueWei          = "1000000000000000"
	defaultUnknownTokenMaxTransactionCostWei = "2000000000000000"
)

// EconomicPolicy задаёт проверенные ограничения расходов одной сети.
type EconomicPolicy struct {
	MaxTransactionCostWei             *big.Int
	HourlyBudgetWei                   *big.Int
	DailyBudgetWei                    *big.Int
	CumulativeBudgetWei               *big.Int
	SponsorMinimumBalanceWei          *big.Int
	MaxFeePerGasWei                   *big.Int
	MaxPriorityFeePerGasWei           *big.Int
	TokenGasLimit                     uint64
	NativeGasLimit                    uint64
	TransactionOverheadWei            *big.Int
	UnboundedAdditionalFees           bool
	NativeMinimumNetValueWei          *big.Int
	UnknownTokenMaxTransactionCostWei *big.Int
	TokenValueRules                   []TokenValueRule
}

// TokenValueRule задаёт явное условие оператора: при указанном исходном балансе
// стоимость доверенного токена должна быть не ниже разрешённых затрат на gas.
type TokenValueRule struct {
	Address               common.Address
	MinimumBalance        *big.Int
	MaxTransactionCostWei *big.Int
}

// Clone возвращает независимую копию политики с отдельными копиями значений big.Int.
func (policy EconomicPolicy) Clone() EconomicPolicy {
	result := policy
	result.MaxTransactionCostWei = cloneBigInt(policy.MaxTransactionCostWei)
	result.HourlyBudgetWei = cloneBigInt(policy.HourlyBudgetWei)
	result.DailyBudgetWei = cloneBigInt(policy.DailyBudgetWei)
	result.CumulativeBudgetWei = cloneBigInt(policy.CumulativeBudgetWei)
	result.SponsorMinimumBalanceWei = cloneBigInt(policy.SponsorMinimumBalanceWei)
	result.MaxFeePerGasWei = cloneBigInt(policy.MaxFeePerGasWei)
	result.MaxPriorityFeePerGasWei = cloneBigInt(policy.MaxPriorityFeePerGasWei)
	result.TransactionOverheadWei = cloneBigInt(policy.TransactionOverheadWei)
	result.NativeMinimumNetValueWei = cloneBigInt(policy.NativeMinimumNetValueWei)
	result.UnknownTokenMaxTransactionCostWei = cloneBigInt(policy.UnknownTokenMaxTransactionCostWei)
	result.TokenValueRules = make([]TokenValueRule, len(policy.TokenValueRules))
	for index, rule := range policy.TokenValueRules {
		result.TokenValueRules[index] = TokenValueRule{
			Address: rule.Address, MinimumBalance: cloneBigInt(rule.MinimumBalance),
			MaxTransactionCostWei: cloneBigInt(rule.MaxTransactionCostWei),
		}
	}
	return result
}

type economicProfile struct {
	maxFeePerGasWei         string
	maxPriorityFeePerGasWei string
	transactionOverheadWei  string
	unboundedAdditionalFees bool
}

func economicProfileFor(network string) economicProfile {
	switch network {
	case "base":
		return economicProfile{"20000000000", "2000000000", "500000000000000", true}
	case "ethereum":
		return economicProfile{"25000000000", "3000000000", "0", false}
	case "arbitrum":
		return economicProfile{"3000000000", "100000000", "1100000000000000", true}
	case "optimism":
		return economicProfile{"4000000000", "50000000", "1200000000000000", true}
	case "polygon":
		return economicProfile{"25000000000", "20000000000", "0", false}
	case "ink":
		return economicProfile{"5000000000", "100000000", "700000000000000", true}
	case "scroll":
		return economicProfile{"5000000000", "500000000", "1500000000000000", true}
	case "linea":
		return economicProfile{"5000000000", "500000000", "1800000000000000", true}
	case "metis":
		return economicProfile{"10000000000", "1000000000", "800000000000000", true}
	case "bnb":
		return economicProfile{"10000000000", "2000000000", "0", false}
	default:
		panic("unknown network in internal registry")
	}
}

func loadEconomicPolicy(lookup func(string) (string, bool), suffix string, profile economicProfile, global RuntimePolicy, trustedTokens []common.Address) (EconomicPolicy, error) {
	maxCostName := "NETWORK_MAX_TRANSACTION_COST_WEI_" + suffix
	maxCost, err := loadPositiveDecimal(lookup, maxCostName, boundedDefault(defaultNetworkMaxTransactionCostWei, global.MaxTransactionCostWei))
	if err != nil {
		return EconomicPolicy{}, err
	}
	hourlyName := "NETWORK_HOURLY_BUDGET_WEI_" + suffix
	hourly, err := loadPositiveDecimal(lookup, hourlyName, boundedDefault(defaultNetworkHourlyBudgetWei, global.HourlyBudgetWei))
	if err != nil {
		return EconomicPolicy{}, err
	}
	dailyName := "NETWORK_DAILY_BUDGET_WEI_" + suffix
	daily, err := loadPositiveDecimal(lookup, dailyName, boundedDefault(defaultNetworkDailyBudgetWei, global.DailyBudgetWei))
	if err != nil {
		return EconomicPolicy{}, err
	}
	cumulativeName := "NETWORK_CUMULATIVE_BUDGET_WEI_" + suffix
	cumulative, err := loadPositiveDecimal(lookup, cumulativeName, boundedDefault(defaultNetworkCumulativeBudgetWei, global.CumulativeBudgetWei))
	if err != nil {
		return EconomicPolicy{}, err
	}
	reserveName := "NETWORK_SPONSOR_MIN_BALANCE_WEI_" + suffix
	reserve, err := loadPositiveDecimal(lookup, reserveName, lowerBoundedDefault(defaultNetworkSponsorMinimumBalanceWei, global.SponsorMinimumBalanceWei))
	if err != nil {
		return EconomicPolicy{}, err
	}
	maxFeeName := "MAX_FEE_PER_GAS_WEI_" + suffix
	maxFee, err := loadPositiveDecimal(lookup, maxFeeName, profile.maxFeePerGasWei)
	if err != nil {
		return EconomicPolicy{}, err
	}
	maxPriorityName := "MAX_PRIORITY_FEE_PER_GAS_WEI_" + suffix
	maxPriority, err := loadPositiveDecimal(lookup, maxPriorityName, profile.maxPriorityFeePerGasWei)
	if err != nil {
		return EconomicPolicy{}, err
	}
	tokenGasName := "TOKEN_GAS_LIMIT_" + suffix
	tokenGas, err := loadPositiveUint64(lookup, tokenGasName, defaultTokenGasLimit)
	if err != nil {
		return EconomicPolicy{}, err
	}
	nativeGasName := "NATIVE_GAS_LIMIT_" + suffix
	nativeGas, err := loadPositiveUint64(lookup, nativeGasName, defaultNativeGasLimit)
	if err != nil {
		return EconomicPolicy{}, err
	}
	overheadName := "CHAIN_OVERHEAD_MAX_WEI_" + suffix
	overhead, err := loadNonNegativeDecimal(lookup, overheadName, profile.transactionOverheadWei)
	if err != nil {
		return EconomicPolicy{}, err
	}
	nativeMinimumName := "NATIVE_MIN_NET_VALUE_WEI_" + suffix
	nativeMinimum, err := loadPositiveDecimal(lookup, nativeMinimumName, defaultNativeMinimumNetValueWei)
	if err != nil {
		return EconomicPolicy{}, err
	}
	unknownMaxName := "UNKNOWN_TOKEN_MAX_TRANSACTION_COST_WEI_" + suffix
	unknownMax, err := loadPositiveDecimal(lookup, unknownMaxName, boundedDefault(defaultUnknownTokenMaxTransactionCostWei, maxCost))
	if err != nil {
		return EconomicPolicy{}, err
	}
	valueRules, err := loadTokenValueRules(lookup, suffix, trustedTokens, maxCost)
	if err != nil {
		return EconomicPolicy{}, err
	}

	if maxCost.Cmp(hourly) > 0 {
		return EconomicPolicy{}, fmt.Errorf("environment variable %s must not be less than %s", hourlyName, maxCostName)
	}
	if hourly.Cmp(daily) > 0 {
		return EconomicPolicy{}, fmt.Errorf("environment variable %s must not be less than %s", dailyName, hourlyName)
	}
	if daily.Cmp(cumulative) > 0 {
		return EconomicPolicy{}, fmt.Errorf("environment variable %s must not be less than %s", cumulativeName, dailyName)
	}
	for _, limit := range []struct {
		name   string
		value  *big.Int
		global string
		bound  *big.Int
	}{
		{maxCostName, maxCost, "MAX_TRANSACTION_COST_WEI", global.MaxTransactionCostWei},
		{hourlyName, hourly, "HOURLY_BUDGET_WEI", global.HourlyBudgetWei},
		{dailyName, daily, "DAILY_BUDGET_WEI", global.DailyBudgetWei},
		{cumulativeName, cumulative, "CUMULATIVE_BUDGET_WEI", global.CumulativeBudgetWei},
	} {
		if limit.value.Cmp(limit.bound) > 0 {
			return EconomicPolicy{}, fmt.Errorf("environment variable %s must not exceed %s", limit.name, limit.global)
		}
	}
	if reserve.Cmp(global.SponsorMinimumBalanceWei) < 0 {
		return EconomicPolicy{}, fmt.Errorf("environment variable %s must not be less than SPONSOR_MIN_BALANCE_WEI", reserveName)
	}
	if maxPriority.Cmp(maxFee) > 0 {
		return EconomicPolicy{}, fmt.Errorf("environment variable %s must not exceed %s", maxPriorityName, maxFeeName)
	}
	if unknownMax.Cmp(maxCost) > 0 {
		return EconomicPolicy{}, fmt.Errorf("environment variable %s must not exceed %s", unknownMaxName, maxCostName)
	}
	if err := validateMaximumGasCost(tokenGasName, tokenGas, maxFeeName, maxFee, overheadName, overhead, maxCostName, maxCost); err != nil {
		return EconomicPolicy{}, err
	}
	if err := validateMaximumGasCost(nativeGasName, nativeGas, maxFeeName, maxFee, overheadName, overhead, maxCostName, maxCost); err != nil {
		return EconomicPolicy{}, err
	}

	return (EconomicPolicy{
		MaxTransactionCostWei:             maxCost,
		HourlyBudgetWei:                   hourly,
		DailyBudgetWei:                    daily,
		CumulativeBudgetWei:               cumulative,
		SponsorMinimumBalanceWei:          reserve,
		MaxFeePerGasWei:                   maxFee,
		MaxPriorityFeePerGasWei:           maxPriority,
		TokenGasLimit:                     tokenGas,
		NativeGasLimit:                    nativeGas,
		TransactionOverheadWei:            overhead,
		UnboundedAdditionalFees:           profile.unboundedAdditionalFees,
		NativeMinimumNetValueWei:          nativeMinimum,
		UnknownTokenMaxTransactionCostWei: unknownMax,
		TokenValueRules:                   valueRules,
	}).Clone(), nil
}

func loadTokenValueRules(lookup func(string) (string, bool), suffix string, trustedTokens []common.Address, networkMax *big.Int) ([]TokenValueRule, error) {
	name := "TOKEN_VALUE_RULES_" + suffix
	value, set := lookup(name)
	if !set {
		return nil, nil
	}
	if value == "" {
		return nil, fmt.Errorf("environment variable %s must not be empty", name)
	}
	trusted := make(map[common.Address]struct{}, len(trustedTokens))
	for _, address := range trustedTokens {
		trusted[address] = struct{}{}
	}
	seen := make(map[common.Address]struct{})
	rules := make([]TokenValueRule, 0)
	for _, encoded := range strings.Split(value, ",") {
		parts := strings.Split(encoded, ":")
		if len(parts) != 3 || !common.IsHexAddress(parts[0]) || !decimalDigits(parts[1]) || !decimalDigits(parts[2]) {
			return nil, fmt.Errorf("environment variable %s contains a malformed token value rule", name)
		}
		address := common.HexToAddress(parts[0])
		minimum, okMinimum := new(big.Int).SetString(parts[1], 10)
		maximum, okMaximum := new(big.Int).SetString(parts[2], 10)
		_, known := trusted[address]
		_, duplicate := seen[address]
		if address == (common.Address{}) || !known || duplicate || !okMinimum || !okMaximum || minimum.Sign() <= 0 || maximum.Sign() <= 0 ||
			minimum.BitLen() > 256 || maximum.BitLen() > 256 || maximum.Cmp(networkMax) > 0 {
			return nil, fmt.Errorf("environment variable %s contains an invalid token value rule", name)
		}
		seen[address] = struct{}{}
		rules = append(rules, TokenValueRule{Address: address, MinimumBalance: minimum, MaxTransactionCostWei: maximum})
	}
	return rules, nil
}

func validateMaximumGasCost(gasName string, gas uint64, feeName string, fee *big.Int, overheadName string, overhead *big.Int, limitName string, limit *big.Int) error {
	maximum := new(big.Int).Mul(new(big.Int).SetUint64(gas), fee)
	maximum.Add(maximum, overhead)
	if maximum.BitLen() > 256 || maximum.Cmp(limit) > 0 {
		return fmt.Errorf("environment variables %s, %s, and %s produce a cost above %s", gasName, feeName, overheadName, limitName)
	}
	return nil
}

func boundedDefault(value string, upper *big.Int) string {
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok {
		panic("invalid internal decimal default")
	}
	if parsed.Cmp(upper) > 0 {
		return upper.String()
	}
	return value
}

func lowerBoundedDefault(value string, lower *big.Int) string {
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok {
		panic("invalid internal decimal default")
	}
	if parsed.Cmp(lower) < 0 {
		return lower.String()
	}
	return value
}
