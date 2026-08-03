# guard-daemon — EIP-7702 Automatic Token Rescue

Высокопроизводительный Go-демон наблюдает за скомпрометированным EOA и переводит настроенные ERC-20 в безопасное хранилище с помощью **EIP-7702**. Неизвестные токены по умолчанию отключены и требуют явного режима `all`.

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
- **Наблюдает только за настроенными событиями Transfer**; безопасный режим по умолчанию — `known-only`
- **Сохраняет событие как provisional hint**, но разрешает sweep только после
  подтверждения блока finalized quorum
- **Verifies delegation** before sweeping (via `_verifyDelegation()` in RescuerV2)
- **Поддерживает явное включение неизвестных токенов**, считая их metadata недоверенными
- **Классифицирует результат любого ERC-20 только как `token-reported`**: отчётность token contract не доказывает экономическую ценность

## How It Works

```
Incoming Transfer (airdrop/vesting)
        ↓
[WebSocket listener сохраняет provisional hint]
        ↓
[finalized quorum scanner подтверждает canonical log]
        ↓
resolve Token (known or bounded on-chain lookup)
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
- ✅ Ограничено policy: `known-only` по умолчанию, `allowlist` или явный `all`
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

Дополнительные сети не входят в совместимый список `internal/config`. Их включение требует отдельной проверки EIP-7702, настройки Rescuer и изменения конфигурации.

Каждая включённая сеть требует проверенного deployment manifest `RescuerV2`,
двух независимых RPC чтения и отдельного ограниченного sponsor. Live startup
создаёт signers только после quorum-аттестации всех включённых сетей. Полный
выпуск всё ещё заблокирован незавершёнными Tasks 06-11.

## Quick Start

### 1. Prerequisites

- **Go 1.26.5** installed
- **Compromised EOA private key** (source)
- **Sponsor wallet** with native gas on each network (~0.01 ETH per network recommended)
- **Safe destination wallet** (where tokens will be sent)

### 2. Setup

```bash
# Clone or extract files
git clone https://github.com/Serge693/guard-daemon.git
cd guard-daemon

# Проверить и загрузить закреплённые зависимости Go
go mod download

# Собрать исполняемый файл
go build -mod=readonly -o guard-daemon.exe ./cmd/guard-daemon
# Linux/macOS: go build -mod=readonly -o guard-daemon ./cmd/guard-daemon
```

### 3. Configure

```bash
# Copy template
cp .env.example .env

# Безопасный режим включён по умолчанию
DRY_RUN=true

# Задайте публичные роли, сеть, два независимых RPC и trusted manifest
# по docs/configuration.md. Приватные ключи в dry run не требуются.
```

Проверка artifact и deployment tooling выполняется только локально по инструкции
[`docs/deployment/local-verification.md`](docs/deployment/local-verification.md).
Repository defaults не содержат адресов production deployment.

### 4. Run

```bash
./guard-daemon.exe
```

В dry run процесс наблюдает за явно включёнными сетями и выполняет только
read-only планирование и `EstimateGas`. Подписание и отправка исключены из
графа зависимостей.

## Configuration (.env)

Авторитетный справочник находится в
[`docs/configuration.md`](docs/configuration.md). Конфигурация требует публичные
`SOURCE_ADDRESS`, `SPONSOR_ADDRESS`, `DESTINATION_ADDRESS`, явный
`ENABLED_NETWORKS`, trusted manifest и два независимых RPC чтения на сеть.
Безопасный token mode по умолчанию — `known-only`; обработка unknown tokens
требует явного `all` opt-in. Старые `RESCUER_*`, `RPC_URL_*`, `RESCUE_TOKENS`,
`TOKENS_TO_SWEEP` и `CLAIM_*` не поддерживаются.
Live private keys передаются только через окружение процесса и не загружаются
из `.env`.

## Локальная проверка deployment

Единственная поддерживаемая на этом этапе процедура находится в
[`docs/deployment/local-verification.md`](docs/deployment/local-verification.md).
Она использует закреплённый canonical artifact и локальный Anvil. Ручная
компиляция в Remix, копирование непроверенного адреса в `.env` и запуск прежних
multi-network/Permit scripts не поддерживаются.

CLI без `--broadcast` не читает ключ и не обращается к RPC. Production/mainnet
активация намеренно не документируется до Task 10.

## Understanding the Flow

### What happens when a transfer arrives

1. **Daemon предварительно обнаруживает Transfer event**
   - Filters on: `to == SOURCE_ADDRESS`
   - Ограничивает адреса контрактов в режимах `known-only` и `allowlist`
   - Принимает неизвестные контракты только после явного `TOKEN_MODE_<N>=all`
   - WebSocket-событие не запускает финансовое действие до finality

2. **Canonical confirmation**
   - Два независимых provider согласуют finalized block hash и полный набор logs
   - Scanner восстанавливает разрывы через backfill и persistent cursor

3. **Token identification**
   - Checks if token is in known list (`tokenMap`)
   - If unknown: queries `symbol()` and `decimals()` on-chain
   - Falls back to address prefix if contract is non-standard

4. **Balance check**
   - Verifies token balance > 0
   - Prevents pointless gas waste on zero-balance sweeps

5. **Atomic sweep (renewAndSweep)**
   - Sponsor constructs SetCodeTx with:
     - `AuthList`: SetCode authorization (source → RescuerV2)
     - `Data`: RescuerV2.sweepAll([token_address])
   - Broadcasts to network
   - Ожидает receipt и затем проверяет fail-closed постусловия

6. **Contract execution (RescuerV2)**
   - Receives call with delegated source EOA
   - Calls `_verifyDelegation()`: confirms delegation is to this contract
   - If OK: transfers all token balance to destination
   - If failed: reverts (tokens remain safe on source)

7. **Result**
   - If success: tokens at destination
   - If failed: logged, will retry on next event

### Periodic health checks

Примерно раз в минуту независимо от режима доставки событий:
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
✅ **Неизвестные токены** поддерживаются только после явного режима `all`; метаданные остаются недоверенными
✅ **`executeAndSweep()` restricted to sponsor only** (`onlySponsor`) — without this, anyone could hijack the delegated EOA into calling `approve()` or any other state-changing function on a token it holds, bypassing the immutable `destination` entirely  
✅ **Startup-аттестация** — до создания signers каждая включённая сеть должна пройти доверенную загрузку manifest и согласование двух независимых RPC по finalized block, runtime, deployment receipt и immutable-ролям
✅ **Address validation** — malformed addresses in `.env` (wrong length, typo) fail loudly at startup instead of being silently mangled into a different, valid-looking address  
✅ **Bounded retries** — a token whose sweep fails repeatedly (e.g. a broken or malicious ERC-20) is retried up to 3 times, then given up on — enforced centrally in `renewAndSweep()` so it can't be bypassed by any calling path  
✅ **Gas cost caps** on every sponsor-paid transaction type (sweep, delegation renewal, ETH sweep) — bounds worst-case cost per attempt regardless of network fee spikes  
✅ **Post-receipt balance verification** — a successful transaction receipt alone doesn't prove tokens actually moved (if the EIP-7702 authorization lost a nonce race, the call could silently execute against a different, attacker-controlled delegation instead). The daemon re-checks balances, but every ERC-20 outcome remains only `token-reported`
**Permit artifacts не являются частью production surface** — контракт, ABI и deployment path удалены

### What this tool does NOT do

❌ **Does not fix compromised key** — this is a stopgap, not a solution  
❌ **Does not guarantee winning the race** — bot with better infrastructure may still get there first  
❌ **Does not prevent delegation replacement** — if bot knows the key, it can set its own delegation  
❌ **Does not work for claim functions with msg.sender verification** — if claim contract validates who's calling (e.g., signature covers msg.sender), atomic approach fails  
❌ **Go daemon не поддерживает Permit flow** — ABI, адрес и fallback не загружаются; production path использует только RescuerV2/EIP-7702

### Contract deployment note

`RescuerV2`'s constructor now takes **two** arguments: `(address destination, address sponsor)`. The `sponsor` address is the only one allowed to call `executeAndSweep()`. If you ever redeploy manually (not via the provided script), make sure to pass both — a contract deployed with a stale one-argument constructor call will simply fail to deploy, which is the correct, safe failure mode.

If `RescuerV2` is ever redeployed for any reason, **the daemon's own startup check will refuse to run** against a network where the on-chain `destination()` doesn't match `.env` — so a partial/failed redeploy fails loudly rather than silently using a wrong contract.

### Best practices

1. **Keep .env private** — never commit to git
2. **Use hardware wallet for sponsor** — if possible (though less critical than source)
3. **Monitor logs** — if you see `Delegation dropped` frequently, bot is actively competing
4. **Plan migration** — use this as a temporary rescue mechanism while you migrate to a new wallet with a fresh key
5. **Don't reuse the compromised key** — even after migration, for anything new
6. **После любого редеплоя RescuerV2 отдельно проверьте runtime и immutable параметры** — общий положительный вывод legacy deployment script не является доказательством безопасного деплоя

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

**The trap:** during EIP-7702 delegated execution, `address(this)` inside the delegate contract's code does **not** refer to the contract's own deployed address — it refers to the **delegator** (the compromised EOA). This is because the delegate's bytecode runs *as* the EOA: `address(this)`, `msg.sender` in the outer frame, storage, and balance are all scoped to the EOA, not to the contract whose code was borrowed.

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
Проверьте оба `RPC_READ_<1|2>_HTTP_<N>` и `RPC_READ_<1|2>_WS_<N>` для явно включённой сети. Значения URL в ошибках не выводятся.

### Ошибка аттестации deployment
Демон останавливает startup, если trusted `RESCUER_MANIFEST_<N>`, runtime,
finalized block, sponsor или destination не согласованы двумя независимыми RPC.
Проверку нельзя обходить заменой адреса контракта в окружении.

### "insufficient funds for gas"
Sponsor wallet balance is too low. Add 0.01+ ETH and restart.

### "transaction type is not supported" (zkSync)
zkSync's sequencer doesn't accept EIP-7702 Type-4 transactions yet. This is a network limitation — the daemon will keep retrying delegation harmlessly, but sweeps on zkSync won't work until the network adds support.

### Некорректный адрес роли или токена
`SOURCE_ADDRESS`, `SPONSOR_ADDRESS`, `DESTINATION_ADDRESS` и адреса allowlist
должны быть ненулевыми EVM-адресами. Роли должны попарно различаться.

## Performance Characteristics

| Metric | Value |
|--------|-------|
| Startup time | <100ms |
| Event detection latency | <500ms (WebSocket) |
| Action readiness | после согласованного finalized block; зависит от сети |
| Transaction construction | ~100ms |
| Confirmation wait | 12-60s (network dependent) |
| Memory footprint | ~50MB |
| CPU usage (idle) | <1% |

## Architecture

- **Language:** Go 1.26.5
- **Dependencies:** go-ethereum, uint256, godotenv, bbolt
- **Concurrency:** один supervisor на сеть и ограниченный набор worker goroutines
- **Event source:** WebSocket как provisional hint; полноту обеспечивает finalized quorum scanner с backfill и polling
- **Contract interaction:** Direct eth_call / eth_sendTransaction

## Known Limitations

1. **One sweep at a time per network** (mutex prevents concurrent sweeps)
   - Durable handoff и replay реализованы; reconciliation неоднозначной отправки транзакции относится к Task 07 и пока блокирует выпуск

2. **Расходы на неизвестные токены**
   - В режиме `known-only` неизвестные адреса фильтруются до metadata lookup
   - `allowlist` и особенно явный `all` могут увеличить чтения и расходы sponsor; обязательное cumulative budget enforcement относится к Task 08

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
ENABLED_NETWORKS=base
./guard-daemon.exe

# Logs will show [Base] prefix only
```

`DRY_RUN=false` явно включает live mode и требует private keys, отдельный
broadcast RPC и успешную quorum-аттестацию до создания signers. До завершения
Tasks 06-11 этот режим не является разрешением на production-запуск.

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
