package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
)

// ReadProvider описывает независимый источник данных для кворумной проверки.
type ReadProvider struct {
	ID                  string
	HTTPURL             string
	WSURL               string
	TrustDomain         string
	EndpointFingerprint string
}

// Format исключает RPC URL из форматированного вывода.
func (provider ReadProvider) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "ReadProvider{ID:"+provider.ID+" EndpointFingerprint:"+provider.EndpointFingerprint+"}")
}

// Network содержит настройки только явно включённой сети.
type Network struct {
	Name                string
	ChainID             domain.NetworkID
	ReadProviders       []ReadProvider
	BroadcastHTTP       string
	ManifestPath        string
	ManifestSourceKind  string
	ManifestSourceValue string
	Tokens              []domain.Token
	TrustedTokens       []common.Address
	AllowUnknownTokens  bool
	EconomicPolicy      EconomicPolicy
}

// Format исключает RPC URL и путь к манифесту из форматированного вывода.
func (network Network) Format(state fmt.State, _ rune) {
	value := "Network{Name:" + network.Name + " ChainID:" + strconv.FormatInt(int64(network.ChainID), 10) + " ReadProviders:" + strconv.Itoa(len(network.ReadProviders)) + "}"
	_, _ = io.WriteString(state, value)
}

// Domain преобразует конфигурацию сети в доменную модель для службы.
func (network Network) Domain(rescuer common.Address) domain.Network {
	result := domain.Network{
		Name:               network.Name,
		ChainID:            network.ChainID,
		Rescuer:            rescuer,
		HasRescuer:         rescuer != (common.Address{}),
		Tokens:             append([]domain.Token(nil), network.Tokens...),
		AllowUnknownTokens: network.AllowUnknownTokens,
	}
	if len(network.ReadProviders) != 0 {
		result.HTTPURL = network.ReadProviders[0].HTTPURL
		result.WSURL = network.ReadProviders[0].WSURL
	}
	return result
}

type networkDefinition struct {
	name    string
	chainID domain.NetworkID
	tokens  []domain.Token
}

func networkRegistry() []networkDefinition {
	return []networkDefinition{
		{name: "base", chainID: 8453, tokens: []domain.Token{
			token("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", "USDC", 6),
			token("0x50c5725949A6F0c72E6C4a641F24049A917DB0Cb", "DAI", 18),
			token("0x4200000000000000000000000000000000000006", "WETH", 18),
			token("0xd9aAEc86B65D86f6A7B5B1b0c42FFA531710b6CA", "USDbC", 6),
			token("0x2Ae3F1Ec7F1F5012CFEab0185bfc7aa3cf0DEc22", "cbETH", 18),
			token("0x0555E30da8f98308EdB960aa94C0Db47230d2B9c", "WBTC", 8),
			token("0x60a3E35Cc302bFA44Cb288Bc5a4F316Fdb1adb42", "EURC", 6),
		}},
		{name: "ethereum", chainID: 1, tokens: []domain.Token{
			token("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", "USDC", 6),
			token("0x6B175474E89094C44Da98b954EedeAC495271d0F", "DAI", 18),
			token("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2", "WETH", 18),
			token("0xdAC17F958D2ee523a2206206994597C13D831ec7", "USDT", 6),
		}},
		{name: "arbitrum", chainID: 42161, tokens: []domain.Token{
			token("0xaf88d065e77c8cC2239327C5EDb3A432268e5831", "USDC", 6),
			token("0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9", "USDT", 6),
			token("0x82aF49447D8a07e3bd95BD0d56f35241523fBab1", "WETH", 18),
			token("0xDA10009cBd5D07dd0CeCc66161FC93D7c9000da1", "DAI", 18),
		}},
		{name: "optimism", chainID: 10, tokens: []domain.Token{
			token("0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85", "USDC", 6),
			token("0x4200000000000000000000000000000000000006", "WETH", 18),
			token("0x94b008aA00579c1307B0EF2c499aD98a8ce58e58", "USDT", 6),
			token("0xDA10009cBd5D07dd0CeCc66161FC93D7c9000da1", "DAI", 18),
		}},
		{name: "polygon", chainID: 137, tokens: []domain.Token{
			token("0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359", "USDC", 6),
			token("0xc2132D05D31c914a87C6611C10748AEb04B58e8F", "USDT", 6),
			token("0x7ceB23fD6bC0adD59E62ac25578270cFf1b9f619", "WETH", 18),
			token("0x8f3Cf7ad23Cd3CaDbD9735AFf958023239c6A063", "DAI", 18),
		}},
		{name: "ink", chainID: 57073, tokens: []domain.Token{
			token("0x4200000000000000000000000000000000000006", "WETH", 18),
			token("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", "USDC", 6),
		}},
		{name: "scroll", chainID: 534352, tokens: []domain.Token{
			token("0x5300000000000000000000000000000000000004", "WETH", 18),
			token("0x06eFdBFf2a14a7c8E15944D1F4A48F9F95F663A4", "USDC", 6),
		}},
		{name: "linea", chainID: 59144, tokens: []domain.Token{
			token("0xe5D7C2a44FfDe5B02f4fcB0a0e87B2178a5b5d35", "WETH", 18),
			token("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", "USDC", 6),
		}},
		{name: "metis", chainID: 1088, tokens: []domain.Token{
			token("0xDeadDeAddeAddEAddeadDEaDDEAdDeAdDEadDEaD", "WETH", 18),
			token("0xEA32e89a684d7796fdB3aCa11481D38271531D15", "USDC", 6),
		}},
		{name: "bnb", chainID: 56, tokens: []domain.Token{
			token("0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c", "WBNB", 18),
			token("0x8AC76a51cc950d9822D68b83FE1ad97B32Cd580d", "USDC", 18),
			token("0x55d398326f99059fF775485246999027B3197955", "USDT", 18),
		}},
	}
}

func loadNetworks(lookup func(string) (string, bool), mode Mode, global RuntimePolicy) ([]Network, error) {
	names, err := enabledNetworkNames(lookup)
	if err != nil {
		return nil, err
	}
	registry := make(map[string]networkDefinition, len(networkRegistry()))
	for _, definition := range networkRegistry() {
		registry[definition.name] = definition
	}

	networks := make([]Network, 0, len(names))
	for _, name := range names {
		network, err := loadNetwork(lookup, mode, registry[name], global)
		if err != nil {
			return nil, err
		}
		networks = append(networks, network)
	}
	return networks, nil
}

func enabledNetworkNames(lookup func(string) (string, bool)) ([]string, error) {
	value, err := requiredValue(lookup, "ENABLED_NETWORKS")
	if err != nil {
		return nil, err
	}
	known := make(map[string]struct{}, len(networkRegistry()))
	for _, definition := range networkRegistry() {
		known[definition.name] = struct{}{}
	}

	parts := strings.Split(value, ",")
	names := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, fmt.Errorf("ENABLED_NETWORKS contains an empty network name")
		}
		if name != strings.ToLower(name) {
			return nil, fmt.Errorf("ENABLED_NETWORKS contains a network name that is not lowercase")
		}
		if _, ok := known[name]; !ok {
			return nil, fmt.Errorf("ENABLED_NETWORKS contains an unknown network name")
		}
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("ENABLED_NETWORKS contains a duplicate network name")
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

func loadNetwork(lookup func(string) (string, bool), mode Mode, definition networkDefinition, global RuntimePolicy) (Network, error) {
	suffix := strings.ToUpper(definition.name)
	providers := make([]ReadProvider, 0, 2)
	canonicalHTTP := make([]string, 0, 2)
	canonicalWS := make([]string, 0, 2)
	for index := 1; index <= 2; index++ {
		prefix := "RPC_READ_" + strconv.Itoa(index)
		httpName := prefix + "_HTTP_" + suffix
		httpValue, err := requiredValue(lookup, httpName)
		if err != nil {
			return Network{}, err
		}
		canonical, err := validateEndpoint(httpName, httpValue, false)
		if err != nil {
			return Network{}, err
		}

		wsName := prefix + "_WS_" + suffix
		wsValue, err := requiredValue(lookup, wsName)
		if err != nil {
			return Network{}, err
		}
		canonicalWebSocket, err := validateEndpoint(wsName, wsValue, true)
		if err != nil {
			return Network{}, err
		}

		trustName := prefix + "_TRUST_DOMAIN_" + suffix
		trustDomain, err := requiredValue(lookup, trustName)
		if err != nil {
			return Network{}, err
		}
		if strings.TrimSpace(trustDomain) != trustDomain {
			return Network{}, fmt.Errorf("environment variable %s contains an invalid trust domain", trustName)
		}

		digest := sha256.Sum256([]byte(canonical))
		providers = append(providers, ReadProvider{
			ID:                  definition.name + "-read-" + strconv.Itoa(index),
			HTTPURL:             httpValue,
			WSURL:               wsValue,
			TrustDomain:         trustDomain,
			EndpointFingerprint: hex.EncodeToString(digest[:]),
		})
		canonicalHTTP = append(canonicalHTTP, canonical)
		canonicalWS = append(canonicalWS, canonicalWebSocket)
	}
	if canonicalHTTP[0] == canonicalHTTP[1] || providers[0].EndpointFingerprint == providers[1].EndpointFingerprint {
		return Network{}, fmt.Errorf("RPC_READ_1_HTTP_%s and RPC_READ_2_HTTP_%s must specify distinct endpoints", suffix, suffix)
	}
	if strings.EqualFold(providers[0].TrustDomain, providers[1].TrustDomain) {
		return Network{}, fmt.Errorf("RPC_READ_1_TRUST_DOMAIN_%s and RPC_READ_2_TRUST_DOMAIN_%s must differ", suffix, suffix)
	}
	if canonicalWS[0] == canonicalWS[1] {
		return Network{}, fmt.Errorf("RPC_READ_1_WS_%s and RPC_READ_2_WS_%s must specify distinct endpoints", suffix, suffix)
	}

	broadcastName := "RPC_BROADCAST_HTTP_" + suffix
	broadcast, broadcastSet := lookup(broadcastName)
	if mode.IsDryRun() && broadcastSet {
		return Network{}, fmt.Errorf("environment variable %s is only allowed when DRY_RUN=false", broadcastName)
	}
	if mode.IsLive() && (!broadcastSet || broadcast == "") {
		return Network{}, fmt.Errorf("required environment variable %s is not set", broadcastName)
	}
	if broadcast != "" {
		canonicalBroadcast, err := validateEndpoint(broadcastName, broadcast, false)
		if err != nil {
			return Network{}, err
		}
		for _, readEndpoint := range canonicalHTTP {
			if canonicalBroadcast == readEndpoint {
				return Network{}, fmt.Errorf("environment variable %s must differ from the read RPC endpoints", broadcastName)
			}
		}
	}

	manifestName := "RESCUER_MANIFEST_" + suffix
	manifest, err := requiredValue(lookup, manifestName)
	if err != nil {
		return Network{}, err
	}
	manifestSourceKind, manifestSourceValue, err := loadManifestSourceProvenance(lookup, suffix)
	if err != nil {
		return Network{}, err
	}
	tokens, trustedTokens, allowUnknown, err := loadTokenPolicy(lookup, suffix, definition.tokens)
	if err != nil {
		return Network{}, err
	}
	economicPolicy, err := loadEconomicPolicy(lookup, suffix, economicProfileFor(definition.name), global, trustedTokens)
	if err != nil {
		return Network{}, err
	}

	return Network{
		Name:                definition.name,
		ChainID:             definition.chainID,
		ReadProviders:       providers,
		BroadcastHTTP:       broadcast,
		ManifestPath:        manifest,
		ManifestSourceKind:  manifestSourceKind,
		ManifestSourceValue: manifestSourceValue,
		Tokens:              tokens,
		TrustedTokens:       trustedTokens,
		AllowUnknownTokens:  allowUnknown,
		EconomicPolicy:      economicPolicy,
	}, nil
}

func loadManifestSourceProvenance(lookup func(string) (string, bool), suffix string) (string, string, error) {
	name := "RESCUER_RELEASE_COMMIT_" + suffix
	commit, ok := lookup(name)
	if !ok {
		return pinnedArtifactSourceKind, pinnedArtifactSourceValue, nil
	}
	if len(commit) != 40 || !lowercaseHex(commit) {
		return "", "", fmt.Errorf("environment variable %s must contain a full 40-character lowercase hexadecimal commit hash", name)
	}
	return "git-commit", commit, nil
}

func validateEndpoint(name, value string, websocket bool) (string, error) {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || parsed.Scheme == "" {
		return "", endpointError(name, websocket)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if websocket {
		if scheme != "ws" && scheme != "wss" {
			return "", endpointError(name, true)
		}
	} else if scheme != "http" && scheme != "https" {
		return "", endpointError(name, false)
	}

	parsed.Scheme = scheme
	hostname := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") || (scheme == "ws" && port == "80") || (scheme == "wss" && port == "443") {
		port = ""
	}
	if strings.Contains(hostname, ":") {
		if port == "" {
			parsed.Host = "[" + hostname + "]"
		} else {
			parsed.Host = net.JoinHostPort(hostname, port)
		}
	} else if port == "" {
		parsed.Host = hostname
	} else {
		parsed.Host = net.JoinHostPort(hostname, port)
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	parsed.RawPath = ""
	parsed.RawQuery = parsed.Query().Encode()
	return parsed.String(), nil
}

func endpointError(name string, websocket bool) error {
	if websocket {
		return fmt.Errorf("environment variable %s must contain a URL with the ws or wss scheme", name)
	}
	return fmt.Errorf("environment variable %s must contain a URL with the http or https scheme", name)
}

func loadTokenPolicy(lookup func(string) (string, bool), suffix string, known []domain.Token) ([]domain.Token, []common.Address, bool, error) {
	modeName := "TOKEN_MODE_" + suffix
	mode, ok := lookup(modeName)
	if !ok {
		mode = "known-only"
	}
	allowlistName := "TOKEN_ALLOWLIST_" + suffix
	allowlist, allowlistSet := lookup(allowlistName)

	switch mode {
	case "known-only":
		if allowlistSet {
			return nil, nil, false, fmt.Errorf("environment variable %s is only allowed when TOKEN_MODE=allowlist", allowlistName)
		}
		return append([]domain.Token(nil), known...), trustedTokenAddresses(known), false, nil
	case "all":
		if allowlistSet {
			return nil, nil, false, fmt.Errorf("environment variable %s is only allowed when TOKEN_MODE=allowlist", allowlistName)
		}
		return append([]domain.Token(nil), known...), trustedTokenAddresses(known), true, nil
	case "allowlist":
		if !allowlistSet || allowlist == "" {
			return nil, nil, false, fmt.Errorf("required environment variable %s is not set", allowlistName)
		}
		return parseTokenAllowlist(allowlistName, allowlist, known)
	default:
		return nil, nil, false, fmt.Errorf("environment variable %s contains an unsupported token mode", modeName)
	}
}

func parseTokenAllowlist(name, value string, known []domain.Token) ([]domain.Token, []common.Address, bool, error) {
	metadata := make(map[common.Address]domain.Token, len(known))
	for _, knownToken := range known {
		metadata[knownToken.Address] = knownToken
	}
	seen := make(map[common.Address]struct{})
	parts := strings.Split(value, ",")
	tokens := make([]domain.Token, 0, len(parts))
	trusted := make([]common.Address, 0, len(parts))
	for _, part := range parts {
		candidate := strings.TrimSpace(part)
		if candidate == "" || !common.IsHexAddress(candidate) {
			return nil, nil, false, fmt.Errorf("environment variable %s contains an invalid EVM address", name)
		}
		address := common.HexToAddress(candidate)
		if address == (common.Address{}) {
			return nil, nil, false, fmt.Errorf("environment variable %s contains the zero EVM address", name)
		}
		if _, ok := seen[address]; ok {
			return nil, nil, false, fmt.Errorf("environment variable %s contains a duplicate EVM address", name)
		}
		seen[address] = struct{}{}
		configured, knownToken := metadata[address]
		configured.Address = address
		tokens = append(tokens, configured)
		if knownToken {
			trusted = append(trusted, address)
		}
	}
	return tokens, trusted, false, nil
}

func trustedTokenAddresses(tokens []domain.Token) []common.Address {
	result := make([]common.Address, len(tokens))
	for index, token := range tokens {
		result[index] = token.Address
	}
	return result
}

func token(address, symbol string, decimals uint8) domain.Token {
	return domain.Token{Address: common.HexToAddress(address), Symbol: symbol, Decimals: decimals}
}
