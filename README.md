# guard-daemon — EIP-7702 Automatic Token Rescue

A high-performance Go daemon that monitors a compromised EOA wallet and **atomically forwards all incoming ERC-20 transfers** (including unknown airdrops) to a safe destination using **EIP-7702**.

## The Problem

When a wallet's private key is compromised, sweeper bots can intercept incoming tokens before the legitimate owner. This is especially critical for:
- **Vesting unlocks** (time-locked, predictable)
- **Airdrops** tied to old addresses with known key weakness (e.g., MyEtherWallet's weak entropy from 2015-2017)
- **Layer3/Galxe rewards** sent to snapshot addresses

Traditional approach (claim → transfer in separate txs) gives bots a 6-12 second window to drain funds.

## The Solution: Atomic EIP-7702

`guard-daemon` uses **EIP-7702 SetCodeTx** to bundle delegation + sweep in a single transaction:

```
One atomic transaction:
  1. SetCode authorization (temporarily delegate EOA to RescuerV2 contract)
  2. Call sweepAll([token]) — executed immediately in same tx
  
Result: No window for bot interception. Either both succeed or both fail.
```

The daemon:
- **Monitors all Transfer events** on-chain (known tokens + unknown airdrops)
- **Instantly triggers atomic sweep** when new tokens arrive
- **Verifies delegation** before sweeping (via `_verifyDelegation()` in RescuerV2)
- **Catches unknown airdrops** by querying symbol/decimals on-chain if needed

## How It Works

```
Incoming Transfer (airdrop/vesting)
        ↓
[WebSocket listener catches event]
        ↓
resolve Token (known or on-chain lookup)
        ↓
renewAndSweep() — atomic EIP-7702 tx
  ├─ AuthList: [SetCode → RescuerV2]
  └─ Data: sweepAll([token_address])
        ↓
[RescuerV2 verifies delegation, transfers tokens]
        ↓
Funds arrive at DESTINATION_ADDRESS
```

**Key properties:**
- ✅ One transaction (no race window)
- ✅ Automatic (daemon runs 24/7)
- ✅ Universal (catches any ERC-20, even unknown tokens)
- ✅ Verified (contract checks delegation is active)

## Supported Networks

| Network | Chain ID | Status |
|---------|----------|--------|
| Base | 8453 | ✅ Primary |
| Ethereum | 1 | ✅ Supported |
| Arbitrum | 42161 | ✅ Supported |
| Optimism | 10 | ✅ Supported |
| Polygon | 137 | ✅ Supported |
| BNB | 56 | ✅ Supported |
| Ink | 57073 | ✅ Supported |
| Linea | 59144 | ✅ Supported |
| Scroll | 534352 | ✅ Supported |
| Metis | 1088 | ✅ Supported |

zkSync Era was removed permanently — its sequencer rejects EIP-7702 `SetCodeTx` (Type-4) transactions entirely at the protocol level (`transaction type is not supported`), so delegation can never work there. This isn't a daemon bug or an RPC issue; there's nothing to fix.

Soneium, 0G, DACC, and X1ecochain are commented out in `main.go`'s network list — either not yet deployed, or (in Soneium's case) EIP-7702 delegation reliably fails there in practice despite being listed as supported in some docs. Uncomment and redeploy if/when there's a concrete need to watch them again.

Each network requires:
- Deployed `RescuerV2.sol` contract (address in `.env`)
- Sufficient gas in sponsor wallet

The daemon verifies at startup (before watching/sweeping on a network) that the RPC's chain ID matches config and that the deployed contract's on-chain `destination()` matches `.env`. It refuses to start on that network if either check fails, rather than proceeding with a misconfiguration.

## Quick Start

### 1. Prerequisites

- **Go 1.22+** installed
- **Compromised EOA private key** (source)
- **Sponsor wallet** with native gas on each network (~0.01 ETH per network recommended)
- **Safe destination wallet** (where tokens will be sent)

### 2. Setup

```bash
# Clone or extract files
git clone https://github.com/Serge693/guard-daemon.git
cd guard-daemon

# Install Go dependencies
go mod tidy

# Build binary
go build -o guard-daemon.exe main.go
# (On Linux/Mac: go build -o guard-daemon main.go)
```

### 3. Configure

```bash
# Copy template
cp .env.example .env

# Edit .env with your values
SOURCE_PRIVATE_KEY=0x<your_compromised_wallet_private_key>
SPONSOR_PRIVATE_KEY=0x<your_sponsor_wallet_private_key>
DESTINATION_ADDRESS=0x<your_safe_wallet_address>
```

**Optional:** Deploy RescuerV2 to new networks or override existing addresses:

```bash
RESCUER_BASE=0x63635ac6b448965d08c03e9dab3066bebf050c09
RESCUER_ETHEREUM=0x...
RPC_URL_BASE=https://base-mainnet.g.alchemy.com/v2/YOUR_KEY
```

### 4. Run

```bash
./guard-daemon.exe
```

Output:
```
08:22:15.874 [Base     ] ⚡ OFC Transfer! tx=0xe42a...
08:22:16.637 [Base     ] -> renewAndSweep(OFC) tx=0xea5...
08:22:17.741 [Base     ] ✓ Swept OFC → destination (atomic delegate+sweep)
```

## Configuration (.env)

### Required

```bash
# Private key of the compromised EOA (source of transfers)
# Used only to sign SetCode authorizations locally
# Never needs ETH for gas (sponsor pays)
SOURCE_PRIVATE_KEY=0x...

# Private key of the sponsor wallet
# Pays gas for all delegation + sweep transactions
# Needs ~0.01 ETH per network for regular operation
SPONSOR_PRIVATE_KEY=0x...

# Safe wallet where rescued tokens are sent
# Can be a cold wallet, multisig, hardware wallet address
DESTINATION_ADDRESS=0x...
```

### Network Configuration

```bash
# RescuerV2 contract addresses (one per network)
# Auto-populated by deployment, or set manually
RESCUER_BASE=0x...
RESCUER_ETHEREUM=0x...
RESCUER_ARBITRUM=0x...
RESCUER_OPTIMISM=0x...
RESCUER_POLYGON=0x...
RESCUER_INK=0x...

# Custom RPC endpoints (optional, recommended for reliability)
# Uses public endpoints if not set, but can be rate-limited
RPC_URL_BASE=https://base-mainnet.g.alchemy.com/v2/KEY
RPC_URL_ETHEREUM=https://eth-mainnet.g.alchemy.com/v2/KEY
RPC_URL_POLYGON=https://polygon-mainnet.g.alchemy.com/v2/KEY
# ... etc for each network
```

### Optional Tuning

```bash
# Known tokens to prioritize (comma-separated addresses)
# If set, daemon will listen only to these addresses
# If empty, listens to ALL incoming transfers
# RESCUE_TOKENS=0xToken1,0xToken2

# Sponsor minimum balance before warning (default: 0.005 ETH)
# SPONSOR_MIN_BALANCE=0.01
```

## How to Deploy RescuerV2

If deploying to a new network or updating the contract:

**Constructor takes two arguments:** `constructor(address destination, address sponsor)`. `sponsor` is the only address allowed to call `executeAndSweep()` — this is a required security control, not optional. A deploy attempt with only one argument will fail outright (correct, safe behavior — not a bug).

### Option A: Using the provided TypeScript script

```bash
npm install
npx tsx scripts/deployRescuerV2.ts
# Outputs: RESCUER_<NETWORK>=0x...
# Saved to .env automatically
```

The script reports RescuerV2 and PermitSweeper deployment results **separately**, and loudly warns if RescuerV2 (the security-critical contract) failed on any network — a successful PermitSweeper deployment elsewhere can't mask that. Read the summary output; don't assume "it printed something positive" means the daemon is safe to run.

### Option B: Manual deployment

1. Open `contracts/RescuerV2.sol` in Remix (remix.ethereum.org)
2. Compile with Solidity 0.8.23
3. Deploy on your target network
4. Pass **both** `DESTINATION_ADDRESS` and the sponsor wallet's address as constructor arguments, in that order
5. Copy deployed address to `.env`

### Verifying Deployment

```bash
# Check that contract is at the address
cast code 0x<RESCUER_ADDRESS> --rpc-url <RPC_URL>

# Should output bytecode starting with 0x...
```

## Understanding the Flow

### What happens when a transfer arrives

1. **Daemon detects Transfer event**
   - Filters on: `to == SOURCE_ADDRESS`
   - Works for any ERC-20, including unknown contracts

2. **Token identification**
   - Checks if token is in known list (`tokenMap`)
   - If unknown: queries `symbol()` and `decimals()` on-chain
   - Falls back to address prefix if contract is non-standard

3. **Balance check**
   - Verifies token balance > 0
   - Prevents pointless gas waste on zero-balance sweeps

4. **Atomic sweep (renewAndSweep)**
   - Sponsor constructs SetCodeTx with:
     - `AuthList`: SetCode authorization (source → RescuerV2)
     - `Data`: RescuerV2.sweepAll([token_address])
   - Broadcasts to network
   - Waits for receipt (2 block confirmations)

5. **Contract execution (RescuerV2)**
   - Receives call with delegated source EOA
   - Calls `_verifyDelegation()`: confirms delegation is to this contract
   - If OK: transfers all token balance to destination
   - If failed: reverts (tokens remain safe on source)

6. **Result**
   - If success: tokens at destination
   - If failed: logged, will retry on next event

### Periodic health checks

Every ~12 seconds (WebSocket mode) or per polling interval (HTTP mode):
- Verifies delegation is still active
- If delegation was overwritten: calls `renewDelegation()` to restore it
- Sweeps small native ETH if balance > threshold
- **Retries sweep for any known token still sitting on the source address** — this covers the case where a previous `renewAndSweep()` reverted (e.g. an authorization-nonce race, or a transient RPC error) and the tokens are still there. Without this, tokens that survived a failed sweep attempt would just sit unswept until the next external Transfer event, which may never come.

## Security Considerations

### What this tool does well

✅ **Atomic transactions** minimize bot race window  
✅ **Verification** via `_verifyDelegation()` in contract (checks against an `immutable self`, not the caller — see architecture notes below)  
✅ **No custody** — funds go straight to destination, not held anywhere  
✅ **Automatic delegation renewal** if overwritten  
✅ **Unknown token support** with on-chain metadata lookup  
✅ **`executeAndSweep()` restricted to sponsor only** (`onlySponsor`) — without this, anyone could hijack the delegated EOA into calling `approve()` or any other state-changing function on a token it holds, bypassing the immutable `destination` entirely  
✅ **Startup sanity checks** — before watching/sweeping on a network, the daemon verifies the RPC's chain ID matches config, and that the deployed contract's on-chain `destination()` matches `.env`'s `DESTINATION_ADDRESS`. Refuses to start on mismatch, rather than silently sending funds to the wrong place  
✅ **Address validation** — malformed addresses in `.env` (wrong length, typo) fail loudly at startup instead of being silently mangled into a different, valid-looking address  
✅ **Bounded retries** — a token whose sweep fails repeatedly (e.g. a broken or malicious ERC-20) is retried up to 3 times, then given up on — enforced centrally in `renewAndSweep()` so it can't be bypassed by any calling path  
✅ **Gas cost caps** on every sponsor-paid transaction type (sweep, delegation renewal, ETH sweep) — bounds worst-case cost per attempt regardless of network fee spikes  
✅ **Post-receipt balance verification** — a successful transaction receipt alone doesn't prove tokens actually moved (if the EIP-7702 authorization lost a nonce race, the call could silently execute against a different, attacker-controlled delegation instead). The daemon re-checks the real balance before logging success  
✅ **PermitSweeper has no caller-suppliable recipient** — the EIP-2612 permit signature it relies on authorizes *(owner, spender, value, deadline)* only; it says nothing about where funds end up. An earlier version took `to` as a parameter, which meant anyone who observed a valid permit signature (e.g. in the mempool) could front-run it with their own address. Destination is now hardcoded to the immutable `owner`

### What this tool does NOT do

❌ **Does not fix compromised key** — this is a stopgap, not a solution  
❌ **Does not guarantee winning the race** — bot with better infrastructure may still get there first  
❌ **Does not prevent delegation replacement** — if bot knows the key, it can set its own delegation  
❌ **Does not work for claim functions with msg.sender verification** — if claim contract validates who's calling (e.g., signature covers msg.sender), atomic approach fails  
❌ **`PermitSweeper.permitAndTransfer()` is deployed but not wired into the daemon's sweep logic** — the ABI and address are loaded (shown in startup logs), but nothing currently calls it. RescuerV2/EIP-7702 handles sweeping on its own; treat PermitSweeper as inactive unless you specifically build something that calls it

### Contract deployment note

`RescuerV2`'s constructor now takes **two** arguments: `(address destination, address sponsor)`. The `sponsor` address is the only one allowed to call `executeAndSweep()`. If you ever redeploy manually (not via the provided script), make sure to pass both — a contract deployed with a stale one-argument constructor call will simply fail to deploy, which is the correct, safe failure mode.

If `RescuerV2` is ever redeployed for any reason, **the daemon's own startup check will refuse to run** against a network where the on-chain `destination()` doesn't match `.env` — so a partial/failed redeploy fails loudly rather than silently using a wrong contract.

### Best practices

1. **Keep .env private** — never commit to git
2. **Use hardware wallet for sponsor** — if possible (though less critical than source)
3. **Monitor logs** — if you see `Delegation dropped` frequently, bot is actively competing
4. **Plan migration** — use this as a temporary rescue mechanism while you migrate to a new wallet with a fresh key
5. **Don't reuse the compromised key** — even after migration, for anything new
6. **After any RescuerV2/PermitSweeper redeploy, verify the deploy script's summary explicitly** — it reports RescuerV2 and PermitSweeper success/failure separately, and warns loudly if the critical access-control fix didn't actually deploy anywhere, rather than reporting generic success from an unrelated contract

## Troubleshooting

### "Delegation dropped — renewing..."
Normal behavior if a scam bot is also attempting sweeps. The daemon will keep renewing until it wins or you migrate the wallet.

### "ERR renewAndSweep reverted" — how to actually diagnose it
Gas usage alone won't tell you why. Don't guess — simulate the exact call with a read-only `eth_call` (no private key, no gas spent) and read the returned error selector:

```go
// eth_call simulating sponsor -> source, data = sweepAll([token])
client.CallContract(ctx, ethereum.CallMsg{From: sponsorAddr, To: &srcAddr, Data: sweepAllCalldata}, nil)
```

If the error implements `rpc.DataError`, call `.ErrorData()` to get the raw revert bytes. The first 4 bytes are the custom error selector — compare against your contract's own `error` definitions (e.g. `DelegationNotActive()`, `TransferFailed()`) to know exactly which check failed, instead of guessing from gas-usage patterns or transaction position in block.

Common causes, roughly in order of likelihood:
- **`DelegationNotActive()` firing even though delegation looks correct on-chain** — see the dedicated section below; this was a real bug in earlier versions of this contract, not a delegation problem.
- Sponsor doesn't have enough gas on that specific network
- A scammer's competing `SetCodeAuthorization` landed in the same block with a lower nonce than yours (genuine race condition — rare in practice, but possible)
- Token contract has unusual transfer logic that legitimately fails (blacklist, pause, insufficient transferable balance vs. `balanceOf`)

### ⚠️ `DelegationNotActive()` reverting despite correct-looking delegation

This bit us hard during development and is worth documenting precisely, because it's a very easy mistake to reintroduce if you ever touch `_verifyDelegation()`.

**The trap:** during EIP-7702 delegated execution, `address(this)` inside the delegate contract's code does **not** refer to the contract's own deployed address — it refers to the **delegator** (the compromised EOA, e.g. `anaxine.eth`). This is because the delegate's bytecode runs *as* the EOA: `address(this)`, `msg.sender` in the outer frame, storage, and balance are all scoped to the EOA, not to the contract whose code was borrowed.

Two broken versions we actually shipped, in order:

1. **Checking `msg.sender`** — this is the sponsor wallet (whoever broadcasts the transaction), a plain EOA that never carries a delegation designator. Always reverts.
2. **Checking `address(this)`** — seems intuitive ("verify my own designator"), but `address(this)` at call time equals the *delegator*, not this contract. So the check became "does the designator's embedded address equal the delegator's own address" — which can never be true, since the designator always points at RescuerV2's real address, never at the EOA itself. Always reverts, silently, with no useful signal that the bug was in the check itself rather than in the delegation.

**The fix:** capture the contract's own address in an `immutable`, set in the constructor — the *only* place where `address(this)` is guaranteed to mean this contract's own address, because constructors always run in the deploying contract's own context, never via delegation:

```solidity
address public immutable self;

constructor(address _destination) {
    destination = _destination;
    self = address(this);   // correctly captured here, at deploy time
}

function _verifyDelegation() internal view {
    bytes memory code = address(this).code;   // correct: read the DELEGATOR's designator
    // ... parse 0xef0100 + 20-byte address ...
    if (delegatedTo != self) revert DelegationNotActive();  // compare against immutable, not address(this)
}
```

If you ever redeploy or modify this contract, keep this straight: **read the designator from `address(this)` (the delegator, at call time), but compare it against `self` (this contract, captured at deploy time)** — never compare `address(this)` against itself, that's always going to be trivially true/false in the wrong way.

### "RPC chain ID mismatch"
You're pointing at the wrong network RPC. Verify `RPC_URL_<NETWORK>` matches the network you're trying to use.

### "startup sanity check failed" / "CRITICAL: RescuerV2 at ... has destination=... baked in, but .env DESTINATION_ADDRESS=..."
The daemon refused to start on this network because the deployed contract's immutable `destination` doesn't match what's in `.env`. This usually means either: (a) `DESTINATION_ADDRESS` was changed in `.env` after the contract was deployed without redeploying, or (b) `RESCUER_<NETWORK>` in `.env` points at an old/wrong contract address. Fix `.env` to match the actual deployed contract, or redeploy `RescuerV2` with the correct destination and update `.env` with the new address. Do not bypass this check — it exists specifically to prevent swept funds from silently going to the wrong address.

### "insufficient funds for gas"
Sponsor wallet balance is too low. Add 0.01+ ETH and restart.

### "transaction type is not supported" (zkSync)
zkSync's sequencer doesn't accept EIP-7702 Type-4 transactions yet. This is a network limitation — the daemon will keep retrying delegation harmlessly, but sweeps on zkSync won't work until the network adds support.

### "invalid address for RESCUER_BASE" / similar at startup
An address in `.env` isn't a well-formed `0x`-prefixed 40-hex-character address — likely a typo (missing/extra character, stray whitespace). The daemon fails loudly here on purpose: silently accepting a malformed address could otherwise turn a typo into a different, valid-looking address that funds get sent to instead.

## Performance Characteristics

| Metric | Value |
|--------|-------|
| Startup time | <100ms |
| Event detection latency | <500ms (WebSocket) |
| Transaction construction | ~100ms |
| Confirmation wait | 12-60s (network dependent) |
| Memory footprint | ~50MB |
| CPU usage (idle) | <1% |

## Architecture

- **Language:** Go 1.22+
- **Dependencies:** go-ethereum, uint256, godotenv
- **Concurrency:** One goroutine per network
- **Event source:** WebSocket (fallback to polling)
- **Contract interaction:** Direct eth_call / eth_sendTransaction

## Known Limitations

1. **One sweep at a time per network** (mutex prevents concurrent sweeps)
   - Multiple tokens in same block → first one sweeps, others retry in 5 min

2. **Gas costs for spam tokens**
   - Every unknown transfer = gas spent on on-chain lookups
   - Worthless tokens still get swept (just don't have value)

3. **No guarantee against superior bot**
   - If bot has better RPC, gas price, or builder relationships → it may still win
   - This improves odds but doesn't guarantee victory

4. **EIP-7702 dependent**
   - Won't work on chains that don't support EIP-7702
   - Requires Ethereum/Base/L2 with SetCode support

## What Happens After Successful Rescue

1. Tokens are at `DESTINATION_ADDRESS`
2. Delegation is still active on source EOA (will renew periodically)
3. **Next step:** Migrate to a new wallet
   - Create new EOA with Ledger/Trezor/MetaMask (cryptographically secure generation)
   - For future airdrops: use the new address in snapshots/registrations
   - Keep old address only for airdrop claims tied to its history

## Testing Before Production

### Dry run (simulation without sending)

```bash
# Edit .env
DRY_RUN=true

./guard-daemon.exe

# Will show what WOULD happen without sending actual transactions
```

### Single network

```bash
# Start only on Base
./guard-daemon.exe

# Logs will show [Base] prefix only
```

### Send test transaction manually

```bash
# Once confident, remove DRY_RUN
DRY_RUN=false
./guard-daemon.exe
```

## Related Work

- **[eip7702-rescue](https://github.com/Serge693/eip7702-rescue)** — Original TypeScript tool (slower, but same concept)
- **[Flashbots Protect](https://protect.flashbots.net/)** — Private RPC (complementary, doesn't solve key compromise race)
- **[MEV-Share](https://docs.flashbots.net/flashbots-mev-share/overview)** — Bundle ordering (useful for known claims, not unknown airdrops)

## Educational Context

This tool is built to address a **real, documented problem**: weak entropy in older wallet generators (MyEtherWallet 2015-2017) led to predicable keys that bots can brute-force and monitor.

References:
- [When Revoking Isn't Enough: Building a Tool to Outrun the Bot](https://medium.com/@skartanenkov/when-revoking-isnt-enough-building-a-tool-to-outrun-the-bot-bd8b3f07c023)
- EIP-7702: [ethereum.org EIPs](https://eips.ethereum.org/EIPS/eip-7702)

## License

MIT

## Author

Serge Kartanenkov

---

**⚠️ Important:** This tool is a stopgap for rescuing funds from a compromised wallet. It does not fix the root cause (key compromise). Plan a migration to a new wallet with a fresh, securely-generated key as soon as possible.
