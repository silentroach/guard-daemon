package config

import (
	"crypto/ecdsa"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func requiredValue(lookup func(string) (string, bool), name string) (string, error) {
	value, ok := lookup(name)
	if !ok || value == "" {
		return "", fmt.Errorf("не задана обязательная переменная окружения %s", name)
	}
	return value, nil
}

func loadAddress(lookup func(string) (string, bool), name string) (common.Address, error) {
	value, err := requiredValue(lookup, name)
	if err != nil {
		return common.Address{}, err
	}
	if !common.IsHexAddress(value) {
		return common.Address{}, fmt.Errorf("переменная %s содержит некорректный EVM-адрес", name)
	}
	address := common.HexToAddress(value)
	if address == (common.Address{}) {
		return common.Address{}, fmt.Errorf("переменная %s содержит нулевой EVM-адрес", name)
	}
	return address, nil
}

func validateDistinctRoles(source, sponsor, destination common.Address) error {
	switch {
	case source == sponsor:
		return fmt.Errorf("переменные SOURCE_ADDRESS и SPONSOR_ADDRESS должны задавать разные адреса")
	case source == destination:
		return fmt.Errorf("переменные SOURCE_ADDRESS и DESTINATION_ADDRESS должны задавать разные адреса")
	case sponsor == destination:
		return fmt.Errorf("переменные SPONSOR_ADDRESS и DESTINATION_ADDRESS должны задавать разные адреса")
	default:
		return nil
	}
}

func loadPrivateKey(lookup func(string) (string, bool), name string) (*ecdsa.PrivateKey, error) {
	value, err := requiredValue(lookup, name)
	if err != nil {
		return nil, err
	}
	material := value
	if strings.HasPrefix(material, "0x") {
		material = material[2:]
	}
	key, err := crypto.HexToECDSA(material)
	if err != nil {
		return nil, fmt.Errorf("переменная %s содержит некорректный приватный ключ", name)
	}
	return key, nil
}

var removedEnvironmentFields = []string{
	"RESCUE_TOKENS",
	"TOKENS_TO_SWEEP",
	"CLAIM_CONTRACT",
	"CLAIM_DATA_HEX",
	"RPC_URL_BASE",
	"RPC_URL_ETHEREUM",
	"RPC_URL_ARBITRUM",
	"RPC_URL_OPTIMISM",
	"RPC_URL_POLYGON",
	"RPC_URL_INK",
	"RPC_URL_SCROLL",
	"RPC_URL_LINEA",
	"RPC_URL_METIS",
	"RPC_URL_BNB",
	"SPONSOR_MIN_BALANCE",
	"RESCUER_BASE",
	"RESCUER_ETHEREUM",
	"RESCUER_ARBITRUM",
	"RESCUER_OPTIMISM",
	"RESCUER_POLYGON",
	"RESCUER_INK",
	"RESCUER_SCROLL",
	"RESCUER_LINEA",
	"RESCUER_METIS",
	"RESCUER_BNB",
	"RESCUER_ZKSYNC",
	"RESCUER_GNOSIS",
	"RESCUER_BERACHAIN",
	"RESCUER_MODE",
	"RESCUER_ZORA",
	"RESCUER_BOB",
	"RESCUER_LOCAL",
}

func rejectUnsupportedFields(lookup func(string) (string, bool)) error {
	for _, name := range removedEnvironmentFields {
		if _, ok := lookup(name); ok {
			return fmt.Errorf("переменная окружения %s больше не поддерживается", name)
		}
	}
	return nil
}

func rejectUnsupportedNames(names []string) error {
	supported := make(map[string]struct{})
	for _, name := range SupportedEnvironmentFields() {
		supported[name] = struct{}{}
	}
	for _, name := range names {
		if _, ok := supported[name]; ok || !reservedEnvironmentName(name) {
			continue
		}
		return fmt.Errorf("переменная окружения %s не поддерживается", name)
	}
	return nil
}

func reservedEnvironmentName(name string) bool {
	for _, prefix := range []string{
		"ABUSE_",
		"ALERT_",
		"CHAIN_OVERHEAD_",
		"CLAIM_",
		"CUMULATIVE_",
		"DAILY_",
		"EMERGENCY_",
		"HOURLY_",
		"MAX_ATTEMPTS_",
		"MAX_FEE_",
		"MAX_NEW_UNKNOWN_",
		"MAX_PRIORITY_",
		"MAX_TRANSACTION_",
		"NATIVE_",
		"NETWORK_",
		"RATE_LIMIT_",
		"RESCUER_",
		"RPC_BROADCAST_",
		"RPC_READ_",
		"RPC_URL_",
		"SPONSOR_",
		"STATE_",
		"TOKEN_",
		"TOKENS_",
		"UNKNOWN_TOKEN_",
		"WATCH_",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
