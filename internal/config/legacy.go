package config

import (
	"crypto/ecdsa"
	"fmt"
	"os"
	"strings"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/joho/godotenv"
)

type Runtime struct {
	SourcePrivateKey  *ecdsa.PrivateKey
	SponsorPrivateKey *ecdsa.PrivateKey
	SourceAddress     common.Address
	SponsorAddress    common.Address
	Destination       common.Address
	Networks          []domain.Network
}

func LoadLegacy() (Runtime, error) {
	// The legacy entry point treated .env as optional and let process values win.
	_ = godotenv.Load()
	return LoadLegacyFrom(os.LookupEnv)
}

func LoadLegacyFrom(lookup func(string) (string, bool)) (Runtime, error) {
	if lookup == nil {
		return Runtime{}, fmt.Errorf("функция чтения окружения не задана")
	}

	sourceValue, err := required(lookup, "SOURCE_PRIVATE_KEY")
	if err != nil {
		return Runtime{}, err
	}
	sourceKey, err := privateKey("SOURCE_PRIVATE_KEY", sourceValue)
	if err != nil {
		return Runtime{}, err
	}

	sponsorValue, err := required(lookup, "SPONSOR_PRIVATE_KEY")
	if err != nil {
		return Runtime{}, err
	}
	sponsorKey, err := privateKey("SPONSOR_PRIVATE_KEY", sponsorValue)
	if err != nil {
		return Runtime{}, err
	}

	destinationValue, err := required(lookup, "DESTINATION_ADDRESS")
	if err != nil {
		return Runtime{}, err
	}
	destination, err := address("DESTINATION_ADDRESS", destinationValue)
	if err != nil {
		return Runtime{}, err
	}

	networks := DefaultNetworks()
	for i := range networks {
		name := "RESCUER_" + strings.ToUpper(networks[i].Name)
		value, ok := lookup(name)
		if !ok || value == "" {
			continue
		}

		rescuer, err := address(name, value)
		if err != nil {
			return Runtime{}, err
		}
		networks[i].Rescuer = rescuer
		networks[i].HasRescuer = true
	}

	return Runtime{
		SourcePrivateKey:  sourceKey,
		SponsorPrivateKey: sponsorKey,
		SourceAddress:     crypto.PubkeyToAddress(sourceKey.PublicKey),
		SponsorAddress:    crypto.PubkeyToAddress(sponsorKey.PublicKey),
		Destination:       destination,
		Networks:          networks,
	}, nil
}

func DefaultNetworks() []domain.Network {
	return []domain.Network{
		{
			Name:    "Base",
			ChainID: 8453,
			HTTPURL: "https://mainnet.base.org",
			WSURL:   "wss://base-rpc.publicnode.com",
			Tokens: []domain.Token{
				token("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", "USDC", 6),
				token("0x752c5a95d202972e124390f30a50154409d3c858", "OFC", 18),
				token("0x50c5725949A6F0c72E6C4a641F24049A917DB0Cb", "DAI", 18),
				token("0x4200000000000000000000000000000000000006", "WETH", 18),
				token("0xd9aAEc86B65D86f6A7B5B1b0c42FFA531710b6CA", "USDbC", 6),
				token("0x2Ae3F1Ec7F1F5012CFEab0185bfc7aa3cf0DEc22", "cbETH", 18),
				token("0x0555E30da8f98308EdB960aa94C0Db47230d2B9c", "WBTC", 8),
				token("0x60a3E35Cc302bFA44Cb288Bc5a4F316Fdb1adb42", "EURC", 6),
			},
		},
		{
			Name:    "Ethereum",
			ChainID: 1,
			HTTPURL: "https://ethereum.publicnode.com",
			WSURL:   "wss://ethereum.publicnode.com",
			Tokens: []domain.Token{
				token("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", "USDC", 6),
				token("0x6B175474E89094C44Da98b954EedeAC495271d0F", "DAI", 18),
				token("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2", "WETH", 18),
				token("0xdAC17F958D2ee523a2206206994597C13D831ec7", "USDT", 6),
			},
		},
		{
			Name:    "Arbitrum",
			ChainID: 42161,
			HTTPURL: "https://arb1.arbitrum.io/rpc",
			WSURL:   "wss://arbitrum-one.publicnode.com",
			Tokens: []domain.Token{
				token("0xaf88d065e77c8cC2239327C5EDb3A432268e5831", "USDC", 6),
				token("0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9", "USDT", 6),
				token("0x82aF49447D8a07e3bd95BD0d56f35241523fBab1", "WETH", 18),
				token("0xDA10009cBd5D07dd0CeCc66161FC93D7c9000da1", "DAI", 18),
			},
		},
		{
			Name:    "Optimism",
			ChainID: 10,
			HTTPURL: "https://mainnet.optimism.io",
			WSURL:   "wss://optimism.publicnode.com",
			Tokens: []domain.Token{
				token("0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85", "USDC", 6),
				token("0x4200000000000000000000000000000000000006", "WETH", 18),
				token("0x94b008aA00579c1307B0EF2c499aD98a8ce58e58", "USDT", 6),
				token("0xDA10009cBd5D07dd0CeCc66161FC93D7c9000da1", "DAI", 18),
			},
		},
		{
			Name:    "Polygon",
			ChainID: 137,
			HTTPURL: "https://polygon.publicnode.com",
			WSURL:   "wss://polygon.publicnode.com",
			Tokens: []domain.Token{
				token("0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359", "USDC", 6),
				token("0xc2132D05D31c914a87C6611C10748AEb04B58e8F", "USDT", 6),
				token("0x7ceB23fD6bC0adD59E62ac25578270cFf1b9f619", "WETH", 18),
				token("0x8f3Cf7ad23Cd3CaDbD9735AFf958023239c6A063", "DAI", 18),
			},
		},
		{
			Name:    "Ink",
			ChainID: 57073,
			HTTPURL: "https://rpc-gel.inkonchain.com",
			WSURL:   "wss://ink.publicnode.com",
			Tokens: []domain.Token{
				token("0x4200000000000000000000000000000000000006", "WETH", 18),
				token("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", "USDC", 6),
			},
		},
		{
			Name:    "Scroll",
			ChainID: 534352,
			HTTPURL: "https://rpc.scroll.io",
			WSURL:   "wss://rpc.scroll.io",
			Tokens: []domain.Token{
				token("0x5300000000000000000000000000000000000004", "WETH", 18),
				token("0x06eFdBFf2a14a7c8E15944D1F4A48F9F95F663A4", "USDC", 6),
			},
		},
		{
			Name:    "Linea",
			ChainID: 59144,
			HTTPURL: "https://rpc.linea.build",
			WSURL:   "wss://rpc.linea.build",
			Tokens: []domain.Token{
				token("0xe5D7C2a44FfDe5B02f4fcB0a0e87B2178a5b5d35", "WETH", 18),
				token("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", "USDC", 6),
			},
		},
		{
			Name:    "Metis",
			ChainID: 1088,
			HTTPURL: "https://andromeda.metis.io/?owner=1088",
			WSURL:   "wss://andromeda.metis.io/?owner=1088",
			Tokens: []domain.Token{
				token("0xDeadDeAddeAddEAddeadDEaDDEAdDeAdDEadDEaD", "WETH", 18),
				token("0xEA32e89a684d7796fdB3aCa11481D38271531D15", "USDC", 6),
			},
		},
		{
			Name:    "BNB",
			ChainID: 56,
			HTTPURL: "https://bsc-rpc.publicnode.com",
			WSURL:   "wss://bsc-rpc.publicnode.com",
			Tokens: []domain.Token{
				token("0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c", "WBNB", 18),
				token("0x8AC76a51cc950d9822D68b83FE1ad97B32Cd580d", "USDC", 18),
				token("0x55d398326f99059fF775485246999027B3197955", "USDT", 18),
			},
		},
	}
}

func required(lookup func(string) (string, bool), name string) (string, error) {
	value, ok := lookup(name)
	if !ok || value == "" {
		return "", fmt.Errorf("не задана обязательная переменная окружения %s", name)
	}
	return value, nil
}

func privateKey(name, value string) (*ecdsa.PrivateKey, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(value, "0x"))
	if err != nil {
		return nil, fmt.Errorf("переменная %s содержит некорректный приватный ключ", name)
	}
	return key, nil
}

func address(name, value string) (common.Address, error) {
	if !common.IsHexAddress(value) {
		return common.Address{}, fmt.Errorf("переменная %s содержит некорректный EVM-адрес", name)
	}
	return common.HexToAddress(value), nil
}

func token(address, symbol string, decimals uint8) domain.Token {
	return domain.Token{
		Address:  common.HexToAddress(address),
		Symbol:   symbol,
		Decimals: decimals,
	}
}
