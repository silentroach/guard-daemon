package main

// EIP-7702 Token Sweeper Daemon with PermitSweeper + RescuerV2 Delegation
//
// Hybrid strategy for maximum reliability:
//   1. PermitSweeper.permitAndTransfer() for tokens with EIP-2612 (permit)
//   2. RescuerV2.sweep() via EIP-7702 delegation for other tokens
//
// Source:      0x54E5C66601b8e9386f0D74b4BD2932A3CFcDC833
// Sponsor:     0x1b3892682781cA7Af45433680E5685F669195B96
// Destination: 0x1b3892682781cA7Af45433680E5685F669195B96
// RescuerV2 (Base): 0x63635ac6b448965d08c03e9dab3066bebf050c09

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"
	"github.com/joho/godotenv"
)

const GAS_MULT_TOKEN = int64(2000)
const GAS_MULT_SPON = int64(50)

// maxTipWei is the hard cap applied after multiplying by GAS_MULT_TOKEN
// or GAS_MULT_SPON: on Base, gas usage for a sweep/delegation tx is
// modest, so this bounds worst-case cost to roughly a dollar or two even
// if the network fee spikes or the multiplier would otherwise produce an
// unpredictably large tip. Shared across renewDelegation, renewAndSweep,
// and doSweepEth so all three sponsor-paid paths have the same ceiling.
const maxTipWei = 5_000_000_000 // 5 Gwei

var ETH_THRESHOLD = big.NewInt(100_000_000_000_000) // 0.0001 ETH

// ─────────────────────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────────────────────

type Token struct {
	Address  string
	Symbol   string
	Decimals int
}

type Network struct {
	Name          string
	ChainID       int64
	RPC           string
	WS            string
	Rescuer       string // RescuerV2 forwarder
	PermitSweeper string // PermitSweeper contract
	Tokens        []Token
}

// ─────────────────────────────────────────────────────────────────────────────
// ABIs
// ─────────────────────────────────────────────────────────────────────────────

const erc20ABI = `[
	{"name":"balanceOf","type":"function","stateMutability":"view",
		"inputs":[{"name":"a","type":"address"}],"outputs":[{"type":"uint256"}]},
	{"name":"transfer","type":"function","stateMutability":"nonpayable",
		"inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],
		"outputs":[{"type":"bool"}]},
	{"name":"transferFrom","type":"function","stateMutability":"nonpayable",
		"inputs":[{"name":"from","type":"address"},{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],
		"outputs":[{"type":"bool"}]},
	{"name":"permit","type":"function","stateMutability":"nonpayable",
		"inputs":[{"name":"owner","type":"address"},{"name":"spender","type":"address"},{"name":"value","type":"uint256"},
			{"name":"deadline","type":"uint256"},{"name":"v","type":"uint8"},{"name":"r","type":"bytes32"},{"name":"s","type":"bytes32"}],
		"outputs":[]},
	{"name":"nonces","type":"function","stateMutability":"view",
		"inputs":[{"name":"owner","type":"address"}],"outputs":[{"type":"uint256"}]},
	{"name":"symbol","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"type":"string"}]},
	{"name":"decimals","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"type":"uint8"}]},
	{"name":"Transfer","type":"event","inputs":[
		{"name":"from","type":"address","indexed":true},
		{"name":"to","type":"address","indexed":true},
		{"name":"value","type":"uint256","indexed":false}
	]}
]`

const permitSweeperABI = `[
	{"name":"permitAndTransfer","type":"function","stateMutability":"nonpayable",
		"inputs":[{"name":"token","type":"address"},{"name":"from","type":"address"},{"name":"to","type":"address"},
			{"name":"amount","type":"uint256"},{"name":"deadline","type":"uint256"},
			{"name":"v","type":"uint8"},{"name":"r","type":"bytes32"},{"name":"s","type":"bytes32"}],
		"outputs":[]},
	{"name":"rescueTokens","type":"function","stateMutability":"nonpayable",
		"inputs":[{"name":"token","type":"address"},{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],
		"outputs":[]}
]`

const forwarderABI = `[
	{"name":"sweepAll","type":"function","stateMutability":"nonpayable",
		"inputs":[{"name":"tokens","type":"address[]"}],"outputs":[]},
	{"name":"sweepEth","type":"function","stateMutability":"nonpayable",
		"inputs":[],"outputs":[]},
	{"name":"destination","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"name":"","type":"address"}]}
]`

// ─────────────────────────────────────────────────────────────────────────────
// Networks
// ─────────────────────────────────────────────────────────────────────────────

var NETWORKS = []Network{
	{
		Name: "Base", ChainID: 8453,
		RPC:           "https://mainnet.base.org",
		WS:            "wss://base-rpc.publicnode.com",
		Rescuer:       "", // set via RESCUER_BASE env var only — no stale hardcoded fallback
		PermitSweeper: "0xc1Ac4208c4D57D79b62663d8555FbC0802Dc2e87",
		Tokens: []Token{
			{"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", "USDC", 6},
			{"0x752c5a95d202972e124390f30a50154409d3c858", "OFC", 18},
			{"0x50c5725949A6F0c72E6C4a641F24049A917DB0Cb", "DAI", 18},
			{"0x4200000000000000000000000000000000000006", "WETH", 18},
			{"0xd9aAEc86B65D86f6A7B5B1b0c42FFA531710b6CA", "USDbC", 6},
			{"0x2Ae3F1Ec7F1F5012CFEab0185bfc7aa3cf0DEc22", "cbETH", 18},
			{"0x0555E30da8f98308EdB960aa94C0Db47230d2B9c", "WBTC", 8},
			{"0x60a3E35Cc302bFA44Cb288Bc5a4F316Fdb1adb42", "EURC", 6},
		},
	},
	{
		Name: "Ethereum", ChainID: 1,
		RPC:           "https://ethereum.publicnode.com",
		WS:            "wss://ethereum.publicnode.com",
		PermitSweeper: "0x1d8A0d0f716c23A3Ed58ba442642C5eE8f06DB36",
		Tokens: []Token{
			{"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", "USDC", 6},
			{"0x6B175474E89094C44Da98b954EedeAC495271d0F", "DAI", 18},
			{"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2", "WETH", 18},
			{"0xdAC17F958D2ee523a2206206994597C13D831ec7", "USDT", 6},
		},
	},
	{
		Name: "Arbitrum", ChainID: 42161,
		RPC: "https://arb1.arbitrum.io/rpc",
		WS:  "wss://arbitrum-one.publicnode.com",
		Tokens: []Token{
			{"0xaf88d065e77c8cC2239327C5EDb3A432268e5831", "USDC", 6},
			{"0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9", "USDT", 6},
			{"0x82aF49447D8a07e3bd95BD0d56f35241523fBab1", "WETH", 18},
			{"0xDA10009cBd5D07dd0CeCc66161FC93D7c9000da1", "DAI", 18},
		},
	},
	{
		Name: "Optimism", ChainID: 10,
		RPC: "https://mainnet.optimism.io",
		WS:  "wss://optimism.publicnode.com",
		Tokens: []Token{
			{"0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85", "USDC", 6},
			{"0x4200000000000000000000000000000000000006", "WETH", 18},
			{"0x94b008aA00579c1307B0EF2c499aD98a8ce58e58", "USDT", 6},
			{"0xDA10009cBd5D07dd0CeCc66161FC93D7c9000da1", "DAI", 18},
		},
	},
	{
		Name: "Polygon", ChainID: 137,
		RPC:           "https://polygon.publicnode.com",
		WS:            "wss://polygon.publicnode.com",
		PermitSweeper: "0x6074B568ED9DAA01D4233128B14fCe0dA8f47A8a",
		Tokens: []Token{
			{"0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359", "USDC", 6},
			{"0xc2132D05D31c914a87C6611C10748AEb04B58e8F", "USDT", 6},
			{"0x7ceB23fD6bC0adD59E62ac25578270cFf1b9f619", "WETH", 18},
			{"0x8f3Cf7ad23Cd3CaDbD9735AFf958023239c6A063", "DAI", 18},
		},
	},
	{
		Name: "Ink", ChainID: 57073,
		RPC:           "https://rpc-gel.inkonchain.com",
		WS:            "wss://ink.publicnode.com",
		PermitSweeper: "0x23b7db1d6D500c4920c3aeEF4e82642188c1886D",
		Tokens: []Token{
			{"0x4200000000000000000000000000000000000006", "WETH", 18},
			{"0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", "USDC", 6},
		},
	},
	// ─── Layer 3 & Alternative Chains ───
	// 0G, DACC, X1ecochain: temporarily disabled — not deployed yet,
	// no active need right now. Uncomment + redeploy RescuerV2 there
	// when/if there's a concrete reason to watch them.
	// {
	// 	Name: "0G", ChainID: 16600,
	// 	RPC: "https://evmrpc.0g.ai",
	// 	WS:  "wss://evmrpc.0g.ai",
	// 	Tokens: []Token{},
	// },
	// Soneium: temporarily disabled. Despite being listed as EIP-7702-
	// supported in OP Stack/thirdweb docs, delegation fails in practice —
	// confirmed via two independent tools (this daemon AND a separate
	// browser-extension revoker using MetaMask's own RPC path), both got
	// "Temporary internal error" / failed delegation. Likely means the
	// Pectra/7702 upgrade is documented but not actually live on Soneium
	// mainnet yet. Re-enable once confirmed working via a direct test.
	// {
	// 	Name: "Soneium", ChainID: 1868,
	// 	RPC: "https://soneium.drpc.org",
	// 	WS:  "wss://soneium.drpc.org",
	// 	Tokens: []Token{},
	// },
	// {
	// 	Name: "DACC", ChainID: 6666,
	// 	RPC: "https://rpc.daccchain.io",
	// 	WS:  "wss://rpc.daccchain.io",
	// 	Tokens: []Token{},
	// },
	// {
	// 	Name: "X1ecochain", ChainID: 195,
	// 	RPC: "https://rpc.x1ecochain.io",
	// 	WS:  "wss://rpc.x1ecochain.io",
	// 	Tokens: []Token{},
	// },
	{
		Name: "Scroll", ChainID: 534352,
		RPC: "https://rpc.scroll.io",
		WS:  "wss://rpc.scroll.io",
		Tokens: []Token{
			{"0x5300000000000000000000000000000000000004", "WETH", 18},
			{"0x06eFdBFf2a14a7c8E15944D1F4A48F9F95F663A4", "USDC", 6},
		},
	},
	{
		Name: "Linea", ChainID: 59144,
		RPC: "https://rpc.linea.build",
		WS:  "wss://rpc.linea.build",
		Tokens: []Token{
			{"0xe5D7C2a44FfDe5B02f4fcB0a0e87B2178a5b5d35", "WETH", 18},
			{"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48", "USDC", 6},
		},
	},
	{
		Name: "Metis", ChainID: 1088,
		RPC: "https://andromeda.metis.io/?owner=1088",
		WS:  "wss://andromeda.metis.io/?owner=1088",
		Tokens: []Token{
			{"0xDeadDeAddeAddEAddeadDEaDDEAdDeAdDEadDEaD", "WETH", 18},
			{"0xEA32e89a684d7796fdB3aCa11481D38271531D15", "USDC", 6},
		},
	},
	// ─── Additional EIP-7702 Supported Chains ───
	{
		Name: "BNB", ChainID: 56,
		RPC: "https://bsc-rpc.publicnode.com",
		WS:  "wss://bsc-rpc.publicnode.com",
		Tokens: []Token{
			{"0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c", "WBNB", 18},
			{"0x8AC76a51cc950d9822D68b83FE1ad97B32Cd580d", "USDC", 18},
			{"0x55d398326f99059fF775485246999027B3197955", "USDT", 18},
		},
	},
}


// ─────────────────────────────────────────────────────────────────────────────
// State
// ─────────────────────────────────────────────────────────────────────────────

type NetState struct {
	net              Network
	client           *ethclient.Client
	srcKey           *ecdsa.PrivateKey
	sponKey          *ecdsa.PrivateKey
	srcAddr          common.Address
	sponAddr         common.Address
	dest             common.Address
	tokABI           abi.ABI
	permitABI        abi.ABI
	fwdABI           abi.ABI
	tokenMap         map[common.Address]Token
	tokenMu          sync.Mutex // guards tokenMap, retryMap, retryAttempts
	retryMap         map[common.Address]Token // tokens with a reverted sweep, pending retry
	retryAttempts    map[common.Address]int   // bounded retry counter per token
	rescuer          common.Address
	permitSweeper    common.Address
	hasRescuer       bool
	hasPermitSweeper bool
	mu               sync.Mutex
	busy             bool
	lastBlock        uint64
}

func (s *NetState) log(msg string) {
	fmt.Printf("%s [%-9s] %s\n", ts(), s.net.Name, msg)
}

// ─────────────────────────────────────────────────────────────────────────────
// Main
// ─────────────────────────────────────────────────────────────────────────────

func main() {
	_ = godotenv.Load()

	srcKey := loadKey(mustEnv("SOURCE_PRIVATE_KEY"))
	sponKey := loadKey(mustEnv("SPONSOR_PRIVATE_KEY"))
	srcAddr := crypto.PubkeyToAddress(srcKey.PublicKey)
	sponAddr := crypto.PubkeyToAddress(sponKey.PublicKey)
	dest := mustAddress("DESTINATION_ADDRESS", mustEnv("DESTINATION_ADDRESS"))

	tokABI, err := abi.JSON(strings.NewReader(erc20ABI))
	must(err, "erc20ABI")
	permitABI, err := abi.JSON(strings.NewReader(permitSweeperABI))
	must(err, "permitSweeperABI")
	fwdABI, err := abi.JSON(strings.NewReader(forwarderABI))
	must(err, "forwarderABI")

	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println("  EIP-7702 Token Sweeper (PermitSweeper + Delegation)")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("  Source:      %s\n", srcAddr.Hex())
	fmt.Printf("  Sponsor:     %s\n", sponAddr.Hex())
	fmt.Printf("  Destination: %s\n", dest.Hex())
	fmt.Println("═══════════════════════════════════════════════════════")

	var wg sync.WaitGroup
	for _, net := range NETWORKS {
		if v := os.Getenv("RESCUER_" + strings.ToUpper(net.Name)); v != "" {
			net.Rescuer = v
		}
		if v := os.Getenv("PERMIT_SWEEPER_" + strings.ToUpper(net.Name)); v != "" {
			net.PermitSweeper = v
		}
		tokenMap := make(map[common.Address]Token)
		for _, t := range net.Tokens {
			tokenMap[common.HexToAddress(t.Address)] = t
		}
		st := &NetState{
			net: net, srcKey: srcKey, sponKey: sponKey,
			srcAddr: srcAddr, sponAddr: sponAddr, dest: dest,
			tokABI: tokABI, permitABI: permitABI, fwdABI: fwdABI, tokenMap: tokenMap,
			retryMap:         make(map[common.Address]Token),
			retryAttempts:    make(map[common.Address]int),
			hasRescuer:       net.Rescuer != "",
			hasPermitSweeper: net.PermitSweeper != "",
		}
		if st.hasRescuer {
			st.rescuer = mustAddress("RESCUER_"+strings.ToUpper(net.Name), net.Rescuer)
		}
		if st.hasPermitSweeper {
			st.permitSweeper = mustAddress("PERMIT_SWEEPER_"+strings.ToUpper(net.Name), net.PermitSweeper)
		}
		wg.Add(1)
		go func(s *NetState) {
			defer wg.Done()
			for {
				if err := s.run(); err != nil {
					s.log(fmt.Sprintf("ERR %v — reconnecting in 5s", err))
				}
				time.Sleep(5 * time.Second)
			}
		}(st)
	}
	wg.Wait()
}

// ─────────────────────────────────────────────────────────────────────────────
// Connect + watch
// ─────────────────────────────────────────────────────────────────────────────

func (s *NetState) run() error {
	var wsMode bool
	client, err := ethclient.Dial(s.net.WS)
	if err == nil {
		wsMode = true
		s.log("WebSocket connected")
	} else {
		client, err = ethclient.Dial(s.net.RPC)
		if err != nil {
			return fmt.Errorf("connect: %v", err)
		}
		s.log("HTTP mode")
	}
	defer client.Close()
	s.client = client

	if err := s.verifyStartupSanity(); err != nil {
		return fmt.Errorf("startup sanity check failed: %v", err)
	}

	s.checkDelegation()

	transferTopic := s.tokABI.Events["Transfer"].ID
	srcHash := common.BytesToHash(s.srcAddr.Bytes())
	// No Addresses filter here on purpose: we want to catch Transfer events
	// from ANY ERC-20 contract sending to us, including unknown/unlisted
	// airdrop tokens, not just the tokens in s.tokenMap.
	query := ethereum.FilterQuery{
		Topics: [][]common.Hash{{transferTopic}, nil, {srcHash}},
	}

	info := "none"
	if s.hasRescuer {
		info = "RescuerV2"
		if s.hasPermitSweeper {
			// PermitSweeper is deployed and its address is known, but
			// permitAndTransfer() is not currently called anywhere in the
			// sweep logic below — sweepToken() only ever goes through
			// RescuerV2. Noted here so this doesn't silently imply a
			// working fallback that isn't actually there.
			info += " (PermitSweeper deployed but unused)"
		}
	} else if s.hasPermitSweeper {
		info = "PermitSweeper configured but NOT wired into sweep logic — no sweep capability on this network"
	}
	s.log(fmt.Sprintf("Watching ALL incoming token transfers | %d known + unknown/airdrops | sweeper=%s", len(s.net.Tokens), info))

	if wsMode {
		return s.wsWatch(query)
	}
	return s.pollWatch(query)
}

// verifyStartupSanity performs cheap read-only checks before the daemon
// starts watching/sweeping on this network:
//   1. RPC's reported chain ID matches the configured Network.ChainID —
//      catches a misconfigured RPC endpoint pointing at the wrong chain.
//   2. If a RescuerV2 is configured, its on-chain immutable destination()
//      matches DESTINATION_ADDRESS from .env — catches the case where
//      .env's destination was ever changed without redeploying the
//      contract (the contract's baked-in destination is what actually
//      receives funds; the .env value is otherwise just used for display
//      and logging, so a drift here would be silent and dangerous).
func (s *NetState) verifyStartupSanity() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	chainID, err := s.client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("could not read chain ID: %v", err)
	}
	if chainID.Cmp(big.NewInt(s.net.ChainID)) != 0 {
		return fmt.Errorf("RPC reports chain ID %s, expected %d — wrong RPC endpoint for %s?",
			chainID.String(), s.net.ChainID, s.net.Name)
	}

	if s.hasRescuer {
		data, err := s.fwdABI.Pack("destination")
		if err != nil {
			return fmt.Errorf("could not pack destination() call: %v", err)
		}
		res, err := s.client.CallContract(ctx, ethereum.CallMsg{To: &s.rescuer, Data: data}, nil)
		if err != nil {
			return fmt.Errorf("could not read destination() from RescuerV2 at %s: %v", s.rescuer.Hex(), err)
		}
		vals, err := s.fwdABI.Unpack("destination", res)
		if err != nil || len(vals) == 0 {
			return fmt.Errorf("could not decode destination() response from %s", s.rescuer.Hex())
		}
		onChainDest, ok := vals[0].(common.Address)
		if !ok {
			return fmt.Errorf("unexpected destination() return type from %s", s.rescuer.Hex())
		}
		if onChainDest != s.dest {
			return fmt.Errorf(
				"CRITICAL: RescuerV2 at %s has destination=%s baked in, but .env DESTINATION_ADDRESS=%s — "+
					"these must match or swept funds go to the wrong address. Refusing to start on %s.",
				s.rescuer.Hex(), onChainDest.Hex(), s.dest.Hex(), s.net.Name)
		}
	}

	return nil
}


// s.tokenMap, otherwise builds a best-effort Token for an unknown/airdrop
// token by querying symbol()/decimals() on-chain (falls back to address
// string + 18 decimals if the calls fail, e.g. non-standard tokens).
func (s *NetState) resolveToken(addr common.Address) Token {
	s.tokenMu.Lock()
	tok, known := s.tokenMap[addr]
	s.tokenMu.Unlock()
	if known {
		return tok
	}
	symbol := addr.Hex()[:10] + "…"
	decimals := 18
	if data, err := s.tokABI.Pack("symbol"); err == nil {
		if res, err := s.client.CallContract(context.Background(),
			ethereum.CallMsg{To: &addr, Data: data}, nil); err == nil {
			if vals, err := s.tokABI.Unpack("symbol", res); err == nil && len(vals) > 0 {
				if sym, ok := vals[0].(string); ok && sym != "" {
					symbol = sanitizeSymbol(sym)
				}
			}
		}
	}
	if data, err := s.tokABI.Pack("decimals"); err == nil {
		if res, err := s.client.CallContract(context.Background(),
			ethereum.CallMsg{To: &addr, Data: data}, nil); err == nil {
			if vals, err := s.tokABI.Unpack("decimals", res); err == nil && len(vals) > 0 {
				if d, ok := vals[0].(uint8); ok {
					decimals = int(d)
				}
			}
		}
	}
	// Deliberately NOT cached into tokenMap here. With thousands of
	// spam/scam tokens showing fake nonzero balances arriving regularly,
	// unconditionally remembering every token ever seen would make
	// periodicCheck's retry loop balloon into thousands of balanceOf()
	// calls every ~12s — hammering the RPC (rate-limit risk) and
	// potentially spending real gas retrying obvious junk. Only tokens
	// where we actually ATTEMPTED a sweep and it reverted get tracked
	// for retry — see registerFailedSweep below.
	return Token{Address: addr.Hex(), Symbol: symbol, Decimals: decimals}
}

// registerFailedSweep records a token whose sweep just reverted, so
// periodicCheck can retry it — bounded by maxRetryAttempts so a genuinely
// un-sweepable token (honeypot, broken transfer(), deliberately malicious)
// doesn't get retried forever and burn gas indefinitely.
const maxRetryAttempts = 3

func (s *NetState) registerFailedSweep(addr common.Address, tok Token) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	if s.retryAttempts == nil {
		s.retryAttempts = make(map[common.Address]int)
	}
	if s.retryAttempts[addr] >= maxRetryAttempts {
		return // give up — likely a broken/malicious token, stop wasting gas on it
	}
	s.retryAttempts[addr]++
	s.retryMap[addr] = tok
}

func (s *NetState) clearRetry(addr common.Address) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	delete(s.retryMap, addr)
	delete(s.retryAttempts, addr)
}

func (s *NetState) wsWatch(query ethereum.FilterQuery) error {
	logsCh := make(chan types.Log, 100)
	sub, err := s.client.SubscribeFilterLogs(context.Background(), query, logsCh)
	if err != nil {
		return s.pollWatch(query)
	}
	defer sub.Unsubscribe()

	// Subscribe to new block headers to react to incoming native-token
	// (ETH/MATIC/BNB/etc) transfers on every block instead of waiting for
	// the 12s periodic ticker. Native transfers don't emit ERC-20 Transfer
	// logs, so this is the only event-driven signal available for them —
	// on Base (~2s blocks) this cuts worst-case detection latency roughly
	// 6x compared to the old polling-only approach.
	headsCh := make(chan *types.Header, 16)
	headSub, headErr := s.client.SubscribeNewHead(context.Background(), headsCh)
	if headErr == nil {
		defer headSub.Unsubscribe()
	} else {
		s.log("! SubscribeNewHead unavailable, native sweep stays on 12s ticker: " + headErr.Error())
	}

	ticker := time.NewTicker(12 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case err := <-sub.Err():
			return fmt.Errorf("ws: %v", err)
		case lg := <-logsCh:
			tok := s.resolveToken(lg.Address)
			s.log(fmt.Sprintf("⚡ %s Transfer! tx=%s", tok.Symbol, lg.TxHash.Hex()))
			go s.sweepToken(tok, lg.Address)
		case <-headsCh:
			go s.checkAndSweepEth()
		case err := <-headSubErrCh(headSub):
			if err != nil {
				s.log("! head subscription error, native sweep stays on 12s ticker: " + err.Error())
			}
		case <-ticker.C:
			go s.periodicCheck()
		}
	}
}

// headSubErrCh returns the subscription's error channel, or a nil channel
// (which blocks forever in a select) if headSub itself is nil — keeps the
// select loop above valid whether or not SubscribeNewHead succeeded.
func headSubErrCh(sub ethereum.Subscription) <-chan error {
	if sub == nil {
		return nil
	}
	return sub.Err()
}

// Polling fallback
func (s *NetState) pollWatch(query ethereum.FilterQuery) error {
	s.log("Polling: 3s interval")
	s.lastBlock = 0
	tick := 0
	for {
		time.Sleep(3 * time.Second)
		tick++
		cur, err := s.client.BlockNumber(context.Background())
		if err != nil {
			continue
		}
		if tick%10 == 0 {
			go s.periodicCheck()
		}
		if s.lastBlock == 0 {
			s.lastBlock = cur
			continue
		}
		if cur <= s.lastBlock {
			continue
		}
		// New block(s) since last poll — check native balance here too,
		// instead of waiting for the slower 30s periodicCheck cadence.
		// Native transfers don't emit ERC-20 logs so this is the only
		// way to react to them promptly in HTTP/poll mode.
		go s.checkAndSweepEth()
		q := query
		q.FromBlock = new(big.Int).SetUint64(s.lastBlock + 1)
		q.ToBlock = new(big.Int).SetUint64(cur)
		s.lastBlock = cur
		logs, err := s.client.FilterLogs(context.Background(), q)
		if err != nil {
			continue
		}
		for _, lg := range logs {
			tok := s.resolveToken(lg.Address)
			s.log(fmt.Sprintf("⚡ %s Transfer! block=%d", tok.Symbol, lg.BlockNumber))
			go s.sweepToken(tok, lg.Address)
		}
	}
}

// Periodic: ETH + delegation
// checkAndSweepEth checks the native token balance on the source address
// and triggers doSweepEth() if it's above ETH_THRESHOLD. Safe to call from
// multiple places (periodic check, per-block watcher) — doSweepEth itself
// guards against overlapping runs via s.busy.
func (s *NetState) checkAndSweepEth() {
	ethBal, err := s.client.BalanceAt(context.Background(), s.srcAddr, nil)
	if err != nil || ethBal == nil || ethBal.Cmp(ETH_THRESHOLD) <= 0 {
		return
	}
	s.log(fmt.Sprintf("ETH on source: %s wei → sweepEth...", ethBal.String()))
	s.doSweepEth()
}

func (s *NetState) periodicCheck() {
	s.checkAndSweepEth()
	if s.hasRescuer {
		if ok, _ := s.isDelegated(); !ok {
			s.log("! Delegation dropped — renewing...")
			s.renewDelegation()
		}
	}

	// Retry sweep for tokens still sitting on the source address:
	//   1. The hardcoded seed list (WETH/USDC/OFC/etc per network) —
	//      small, fixed size, safe to check every cycle.
	//   2. retryMap — tokens where a sweep was actually ATTEMPTED and
	//      reverted, bounded to maxRetryAttempts each. This is deliberately
	//      NOT "every token ever seen": with thousands of spam/scam tokens
	//      showing fake nonzero balances arriving regularly, checking all
	//      of them every ~12s would hammer the RPC and risk spending real
	//      gas chasing junk. Only genuine failed-attempt cases get retried,
	//      and give up after a few tries (see registerFailedSweep).
	if s.hasRescuer {
		s.tokenMu.Lock()
		type entryT struct {
			addr common.Address
			tok  Token
		}
		snapshot := make([]entryT, 0, len(s.tokenMap)+len(s.retryMap))
		for addr, tok := range s.tokenMap {
			snapshot = append(snapshot, entryT{addr, tok})
		}
		for addr, tok := range s.retryMap {
			if _, seed := s.tokenMap[addr]; seed {
				continue // avoid double-checking the same token twice
			}
			snapshot = append(snapshot, entryT{addr, tok})
		}
		s.tokenMu.Unlock()

		for _, entry := range snapshot {
			bal := s.tokenBalance(entry.addr)
			if bal != nil && bal.Sign() > 0 {
				s.log(fmt.Sprintf("[retry] %s balance=%s still on source — retrying sweep", entry.tok.Symbol, bal.String()))
				s.renewAndSweep(entry.tok, entry.addr)
			}
		}
	}
}

// EIP-7702 delegation
func (s *NetState) isDelegated() (bool, error) {
	code, err := s.client.CodeAt(context.Background(), s.srcAddr, nil)
	if err != nil {
		return false, err
	}
	if len(code) < 23 || code[0] != 0xef || code[1] != 0x01 || code[2] != 0x00 {
		return false, nil
	}
	delegated := common.BytesToAddress(code[3:23])
	return strings.EqualFold(delegated.Hex(), s.rescuer.Hex()), nil
}

func (s *NetState) checkDelegation() {
	if !s.hasRescuer {
		return
	}
	ok, err := s.isDelegated()
	if err != nil {
		s.log("ERR isDelegated: " + err.Error())
		return
	}
	if ok {
		s.log("✓ EIP-7702 Delegation active → " + s.rescuer.Hex())
	} else {
		s.log("! EIP-7702 Delegation not active — renewing...")
		s.renewDelegation()
	}
}

// pendingNonceRetry wraps PendingNonceAt with a short retry-with-backoff
// specifically for transient rate-limit errors (429 Too Many Requests),
// which public RPC endpoints return under load. Other error types
// (connection refused, chain mismatch, etc) are NOT retried here — they
// won't resolve themselves in milliseconds and retrying would just delay
// the sweep race further. This is deliberately narrow: 3 attempts, short
// delays, because we're racing a bot and can't afford to sit in a long
// backoff loop.
func pendingNonceRetry(client *ethclient.Client, addr common.Address) (uint64, error) {
	delays := []time.Duration{0, 300 * time.Millisecond, 800 * time.Millisecond}
	var lastErr error
	for _, d := range delays {
		if d > 0 {
			time.Sleep(d)
		}
		nonce, err := client.PendingNonceAt(context.Background(), addr)
		if err == nil {
			return nonce, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "429") && !strings.Contains(err.Error(), "Too Many Requests") {
			return 0, err // not a rate-limit error, no point retrying
		}
	}
	return 0, lastErr
}

func (s *NetState) renewDelegation() {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		s.log("[skip] renewDelegation — sweep/renewal already in progress")
		return
	}
	s.busy = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()

	chainID := big.NewInt(s.net.ChainID)

	srcNonce, err := pendingNonceRetry(s.client, s.srcAddr)
	if err != nil {
		s.log("ERR nonce(src): " + err.Error())
		return
	}
	sponNonce, err := pendingNonceRetry(s.client, s.sponAddr)
	if err != nil {
		s.log("ERR nonce(spon): " + err.Error())
		return
	}

	chainU256, _ := uint256.FromBig(chainID)
	unsignedAuth := types.SetCodeAuthorization{
		ChainID: *chainU256,
		Address: s.rescuer,
		Nonce:   srcNonce,
	}
	auth, err := types.SignSetCode(s.srcKey, unsignedAuth)
	if err != nil {
		s.log("ERR SignSetCode: " + err.Error())
		return
	}

	fees := s.getFees()
	tip := new(big.Int).Mul(fees.tip, big.NewInt(GAS_MULT_SPON))
	feeCap := new(big.Int).Mul(fees.cap, big.NewInt(GAS_MULT_SPON))
	// Same hard cap as the token-sweep path: without this, an unbounded
	// multiplier combined with a network fee spike could produce an
	// unpredictably expensive delegation-renewal transaction.
	if tip.Cmp(big.NewInt(maxTipWei)) > 0 {
		tip = big.NewInt(maxTipWei)
	}
	if feeCap.Cmp(big.NewInt(maxTipWei*3)) > 0 {
		feeCap = big.NewInt(maxTipWei * 3)
	}

	tipU256, _ := uint256.FromBig(tip)
	feeCapU256, _ := uint256.FromBig(feeCap)

	tx := types.NewTx(&types.SetCodeTx{
		ChainID:   chainU256,
		Nonce:     sponNonce,
		To:        s.srcAddr,
		Gas:       60_000,
		GasTipCap: tipU256,
		GasFeeCap: feeCapU256,
		AuthList:  []types.SetCodeAuthorization{auth},
	})

	signer := types.LatestSignerForChainID(chainID)
	signed, err := types.SignTx(tx, signer, s.sponKey)
	if err != nil {
		s.log("ERR sign delegation: " + err.Error())
		return
	}

	if err := s.client.SendTransaction(context.Background(), signed); err != nil {
		s.log("ERR send delegation: " + err.Error())
		return
	}
	hash := signed.Hash()
	s.log("-> delegation tx: " + hash.Hex())

	rec, err := waitReceipt(s.client, hash, 60*time.Second)
	if err != nil || rec.Status != 1 {
		s.log("ERR delegation reverted/timeout")
		return
	}
	s.log("✓ EIP-7702 Delegation renewed: " + hash.Hex())
}

func (s *NetState) renewAndSweep(tok Token, tokenAddr common.Address) {
	// Centralized guards: this is the ONLY function that actually spends
	// sponsor gas via a sweep transaction, so both checks live HERE rather
	// than in individual callers (sweepToken, periodicCheck's retry loop)
	// — a caller forgetting to check either guard previously meant it was
	// silently bypassed. In particular, periodicCheck's retry loop used to
	// call renewAndSweep directly, skipping both the busy-lock (risking
	// concurrent sponsor-nonce use with a live event-triggered sweep) and
	// the retry-attempt cap (letting a malicious/broken token retry
	// forever instead of stopping after maxRetryAttempts).
	s.tokenMu.Lock()
	exhausted := s.retryAttempts != nil && s.retryAttempts[tokenAddr] >= maxRetryAttempts
	s.tokenMu.Unlock()
	if exhausted {
		s.log(fmt.Sprintf("[skip] %s — already failed %d times, giving up (possible fake/malicious token)", tok.Symbol, maxRetryAttempts))
		return
	}

	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		s.log(fmt.Sprintf("[skip] %s — sweep/renewal already in progress", tok.Symbol))
		return
	}
	s.busy = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()

	chainID := big.NewInt(s.net.ChainID)
	chainU256, _ := uint256.FromBig(chainID)

	// srcNonce is the contested value: it's the authorization nonce on the
	// compromised EOA, and it increments every time ANY party (us or the
	// attacker) successfully applies a SetCodeAuthorization to this account.
	// If the attacker's delegation lands between when we read this nonce and
	// when our tx is included, our authorization is silently skipped (wrong
	// nonce) and sweepAll() reverts against whatever ended up delegated.
	//
	// So everything that does NOT depend on srcNonce is prefetched first,
	// in parallel. srcNonce itself is fetched last, as close to signing and
	// broadcast as possible, to shrink the race window to a minimum.
	var sponNonce uint64
	var sponErr error
	var fees gasFees
	var data []byte
	var packErr error
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		sponNonce, sponErr = pendingNonceRetry(s.client, s.sponAddr)
	}()
	go func() {
		defer wg.Done()
		fees = s.getFees()
	}()
	go func() {
		defer wg.Done()
		data, packErr = s.fwdABI.Pack("sweepAll", []common.Address{tokenAddr})
	}()
	wg.Wait()

	if sponErr != nil {
		s.log("ERR nonce(spon): " + sponErr.Error())
		return
	}
	if packErr != nil {
		s.log("ERR pack sweepAll: " + packErr.Error())
		return
	}

	// Last possible moment: read the contested nonce, sign, build, broadcast.
	srcNonce, err := pendingNonceRetry(s.client, s.srcAddr)
	if err != nil {
		s.log("ERR nonce(src): " + err.Error())
		return
	}

	unsignedAuth := types.SetCodeAuthorization{
		ChainID: *chainU256,
		Address: s.rescuer,
		Nonce:   srcNonce,
	}
	auth, err := types.SignSetCode(s.srcKey, unsignedAuth)
	if err != nil {
		s.log("ERR SignSetCode: " + err.Error())
		return
	}

	tip := new(big.Int).Mul(fees.tip, big.NewInt(GAS_MULT_TOKEN))
	feeCap := new(big.Int).Mul(fees.cap, big.NewInt(GAS_MULT_TOKEN))

	// Priority tip cap: this is the extra amount ABOVE base fee we're
	// willing to pay for priority. At this cap (5 Gwei), worst case
	// priority cost is roughly 220_000 gas * 5e9 wei ≈ 0.0011 ETH
	// (~$1.90 at $1700/ETH) per attempt.
	//
	// NOTE: feeCap (computed below, base*2 + tip) is intentionally allowed
	// up to 15 Gwei total — NOT 5. This is standard EIP-1559 headroom: the
	// actual amount paid per unit gas is min(feeCap, baseFeeAtInclusion +
	// tip), never the raw feeCap ceiling, unless Base's base fee itself
	// spikes far above its normal ~0.005 Gwei. The 5 Gwei figure only
	// bounds the PRIORITY portion we control; it does not by itself cap
	// total tx cost to 5 Gwei/gas in a base-fee-spike scenario.
	if tip.Cmp(big.NewInt(maxTipWei)) > 0 {
		tip = big.NewInt(maxTipWei)
	}
	if feeCap.Cmp(big.NewInt(maxTipWei*3)) > 0 {
		feeCap = big.NewInt(maxTipWei * 3)
	}

	tipU256, _ := uint256.FromBig(tip)
	feeCapU256, _ := uint256.FromBig(feeCap)

	tx := types.NewTx(&types.SetCodeTx{
		ChainID:   chainU256,
		Nonce:     sponNonce,
		To:        s.srcAddr,
		Gas:       220_000,
		GasTipCap: tipU256,
		GasFeeCap: feeCapU256,
		Data:      data,
		AuthList:  []types.SetCodeAuthorization{auth},
	})

	signer := types.LatestSignerForChainID(chainID)
	signed, err := types.SignTx(tx, signer, s.sponKey)
	if err != nil {
		s.log("ERR sign renewAndSweep: " + err.Error())
		return
	}

	if err := s.client.SendTransaction(context.Background(), signed); err != nil {
		s.log(fmt.Sprintf("ERR send renewAndSweep(%s): %v", tok.Symbol, err))
		return
	}
	hash := signed.Hash()
	s.log(fmt.Sprintf("-> renewAndSweep(%s) tx=%s", tok.Symbol, hash.Hex()))

	rec, err := waitReceipt(s.client, hash, 60*time.Second)
	if err != nil {
		s.log("ERR receipt: " + err.Error())
		return
	}
	if rec.Status == 1 {
		// A successful receipt only proves the OUTER call didn't revert —
		// it does NOT prove OUR sweepAll actually ran. If our
		// SetCodeAuthorization lost a nonce race (attacker's delegation
		// landed with the matching nonce instead), this transaction would
		// execute against WHATEVER is currently delegated — which could
		// be attacker's own contract, silently succeeding without moving
		// our tokens at all. Verify the real outcome via balance instead
		// of trusting the receipt status alone.
		remaining := s.tokenBalance(tokenAddr)
		if remaining != nil && remaining.Sign() > 0 {
			s.log(fmt.Sprintf("⚠ %s renewAndSweep receipt OK but balance still %s — delegation likely hijacked mid-tx, NOT actually swept", tok.Symbol, remaining.String()))
			s.registerFailedSweep(tokenAddr, tok)
		} else {
			s.log(fmt.Sprintf("✓ Swept %s → destination (atomic delegate+sweep)", tok.Symbol))
			s.clearRetry(tokenAddr)
		}
	} else {
		s.log(fmt.Sprintf("ERR %s renewAndSweep reverted", tok.Symbol))
		s.registerFailedSweep(tokenAddr, tok)
	}
}

func (s *NetState) sweepToken(tok Token, tokenAddr common.Address) {
	// Note: busy-lock and retry-attempt-cap guards live inside
	// renewAndSweep() itself now (not here) — that's the single choke
	// point every sweep-triggering path goes through (this function AND
	// periodicCheck's retry loop), so the guards can't be bypassed by
	// calling renewAndSweep directly. Locking here too would deadlock
	// (Go's sync.Mutex isn't reentrant).
	bal := s.tokenBalance(tokenAddr)
	if bal == nil || bal.Sign() == 0 {
		s.log(fmt.Sprintf("%s balance=0 (already swept)", tok.Symbol))
		return
	}
	s.log(fmt.Sprintf("%s balance=%s raw", tok.Symbol, bal.String()))

	if s.hasRescuer {
		s.renewAndSweep(tok, tokenAddr)
		return
	}

	s.log(fmt.Sprintf("ERR %s: no sweep method available", tok.Symbol))
}

func (s *NetState) doSweepEth() {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		s.log("[skip] sweepEth — sweep already in progress")
		return
	}
	s.busy = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()

	if !s.hasRescuer {
		s.log("No rescuer — cannot sweepEth")
		return
	}
	if ok, _ := s.isDelegated(); !ok {
		s.log("Delegation inactive — renewing...")
		s.renewDelegation()
		return
	}

	data, err := s.fwdABI.Pack("sweepEth")
	if err != nil {
		s.log("ERR pack sweepEth: " + err.Error())
		return
	}

	fees := s.getFees()
	tip := new(big.Int).Mul(fees.tip, big.NewInt(GAS_MULT_SPON))
	feeCap := new(big.Int).Mul(fees.cap, big.NewInt(GAS_MULT_SPON))
	if tip.Cmp(big.NewInt(maxTipWei)) > 0 {
		tip = big.NewInt(maxTipWei)
	}
	if feeCap.Cmp(big.NewInt(maxTipWei*3)) > 0 {
		feeCap = big.NewInt(maxTipWei * 3)
	}
	chainID := big.NewInt(s.net.ChainID)

	nonce, err := pendingNonceRetry(s.client, s.sponAddr)
	if err != nil {
		s.log("ERR nonce(spon) for sweepEth: " + err.Error())
		return
	}
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce,
		To: &s.srcAddr, Gas: 80_000,
		GasTipCap: tip, GasFeeCap: feeCap,
		Data: data,
	})

	signer := types.LatestSignerForChainID(chainID)
	signed, err := types.SignTx(tx, signer, s.sponKey)
	if err != nil {
		s.log("ERR sign sweepEth: " + err.Error())
		return
	}

	if err := s.client.SendTransaction(context.Background(), signed); err != nil {
		s.log("ERR send sweepEth: " + err.Error())
		return
	}
	hash := signed.Hash()
	s.log("-> sweepEth() tx=" + hash.Hex())

	rec, err := waitReceipt(s.client, hash, 60*time.Second)
	if err != nil || rec.Status != 1 {
		s.log("ERR sweepEth() reverted/timeout")
		return
	}
	s.log("✓ sweepEth() confirmed: " + hash.Hex())
}

type gasFees struct{ tip, cap *big.Int }

func (s *NetState) getFees() gasFees {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Floor for the pre-multiplier tip: on Base, SuggestGasPrice can return
	// values small enough that even GAS_MULT_TOKEN doesn't move the needle
	// in absolute terms. Ensure there's always a meaningful base to scale
	// from — this costs fractions of a cent on Base but can matter for
	// priority ordering when racing a competing sweeper.
	const minTipWei = 100_000 // 0.0001 Gwei floor before multiplication
	head, err := s.client.HeaderByNumber(ctx, nil)
	if err == nil && head != nil && head.BaseFee != nil && head.BaseFee.Sign() > 0 {
		gp, _ := s.client.SuggestGasPrice(ctx)
		tip := big.NewInt(1_500_000_000)
		if gp != nil && gp.Sign() > 0 {
			tip = new(big.Int).Div(gp, big.NewInt(10))
			if tip.Cmp(big.NewInt(minTipWei)) < 0 {
				tip = big.NewInt(minTipWei)
			}
		}
		feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
		return gasFees{tip, feeCap}
	}
	gp, _ := s.client.SuggestGasPrice(ctx)
	if gp == nil || gp.Sign() == 0 {
		gp = big.NewInt(1_000_000_000)
	}
	return gasFees{new(big.Int).Set(gp), new(big.Int).Mul(gp, big.NewInt(12))}
}

func (s *NetState) tokenBalance(tokenAddr common.Address) *big.Int {
	data, _ := s.tokABI.Pack("balanceOf", s.srcAddr)
	res, err := s.client.CallContract(context.Background(),
		ethereum.CallMsg{To: &tokenAddr, Data: data}, nil)
	if err != nil {
		return nil
	}
	vals, err := s.tokABI.Unpack("balanceOf", res)
	if err != nil || len(vals) == 0 {
		return nil
	}
	v, _ := vals[0].(*big.Int)
	return v
}

func waitReceipt(client *ethclient.Client, hash common.Hash, timeout time.Duration) (*types.Receipt, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := client.TransactionReceipt(context.Background(), hash)
		if err == nil {
			return r, nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	return nil, fmt.Errorf("receipt timeout %s", hash.Hex())
}

func loadKey(h string) *ecdsa.PrivateKey {
	h = strings.TrimPrefix(h, "0x")
	b, err := hex.DecodeString(h)
	must(err, "loadKey decode")
	k, err := crypto.ToECDSA(b)
	must(err, "loadKey ECDSA")
	return k
}

// sanitizeSymbol strips non-printable/control characters and truncates
// length before a token's self-reported symbol() string is used in logs
// or displayed anywhere. An untrusted token contract fully controls this
// return value — without sanitizing, it could inject terminal escape
// sequences to spoof log lines, embed newlines to fake extra log entries,
// or return an absurdly long string to bloat/break log output.
func sanitizeSymbol(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f { // control chars, including \n \r \t \x1b (ESC)
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	const maxLen = 16
	if len(out) > maxLen {
		out = out[:maxLen] + "…"
	}
	if out == "" {
		return "???"
	}
	return out
}


func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("env %s not set", k)
	}
	return v
}

// mustAddress validates that s is a well-formed 0x-prefixed 40-hex-char
// Ethereum address before parsing it. common.HexToAddress does NOT do
// this validation itself — it silently truncates or zero-pads malformed
// input into some address rather than erroring, so a typo in .env (wrong
// length, stray character, etc) would be silently turned into a
// different, wrong-but-valid-looking address instead of failing loudly.
// label is used only for the error message, to point at which config
// value was bad.
func mustAddress(label, s string) common.Address {
	if !common.IsHexAddress(s) {
		log.Fatalf("invalid address for %s: %q (expected 0x-prefixed 40 hex chars)", label, s)
	}
	return common.HexToAddress(s)
}

func must(err error, ctx string) {
	if err != nil {
		log.Fatalf("%s: %v", ctx, err)
	}
}

func ts() string { return time.Now().Format("15:04:05.000") }
