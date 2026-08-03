# Ограничения расходов и диагностика

## Бюджет sponsor

Все включённые сети используют один host-wide persistent ledger в
`/var/lib/guard-daemon`, канонически привязанный к sponsor вне сменяемого
`STATE_DIRECTORY`. Каталог должен находиться на durable filesystem, иметь режим
`0700` и принадлежать пользователю daemon. Перед первой подписью ledger атомарно резервирует верхнюю
стоимость `gasLimit * maxFeePerGas` и только enforceable chain fee. Резерв одновременно
проверяется по global и network пределам одной транзакции, скользящего часа,
скользящих суток и накопительного расхода.

После canonical finalized receipt резерв заменяется точной стоимостью
`gasUsed * effectiveGasPrice` до чтения postconditions. Ошибка RPC, timeout или неразрешённый исход
сохраняют полный резерв до reconciliation. Перезапуск не обнуляет расходы и
reservations. Host-wide sponsor fence запрещает второму процессу на том же хосте
создать независимый ledger для этого sponsor. Смена `STATE_DIRECTORY` не создаёт
новые counters; несовместимая policy требует явной проверяемой миграции.

Live paid actions разрешены только для execution-only fee model, где protocol
ограничивает полную стоимость полями transaction. Для Base, Optimism и других
сетей с некэпируемым L1 data/operator fee daemon работает в dry-run или
`EMERGENCY_STOP`, но fail-closed отклоняет live coordinator. Значение
`CHAIN_OVERHEAD_MAX_WEI_<N>` не превращает такой fee в enforceable cap.

Перед reserve проверяется finalized balance sponsor. Демон не разрешает
операцию, если после всех открытых reservations и новой верхней стоимости баланс
может стать ниже `NETWORK_SPONSOR_MIN_BALANCE_WEI_<N>`.

## Ограничение злоупотреблений

Единая persistent admission policy ограничивает общую для всех сетей частоту paid attempts за минуту, попытки
одного token и source event за `ABUSE_WINDOW`, а также число новых unknown token
addresses. Состояние имеет фиксированную ёмкость, сохраняется рядом с canonical
sponsor ledger и не создаёт отдельные goroutines для токенов. Перезапуск не
обнуляет admission counters или финансовый budget.

`TOKEN_MODE_<N>=known-only` используется по умолчанию. Unknown token требует
явного `allowlist` или `all`, остаётся недоверенным и ограничивается
`UNKNOWN_TOKEN_MAX_TRANSACTION_COST_WEI_<N>`. Для известного токена обязательна
явная операторская оценка `TOKEN_VALUE_RULES_<N>`; без неё live coordinator не
создаётся. Эта оценка ограничивает расходы, но не доверяет отчётности контракта:
результат любого ERC-20 остаётся только `token-reported`.

При первом открытии state schema v2 демон атомарно обновляет её до v3 и
переклассифицирует прежние terminal-результаты известных ERC-20 из
`trusted-success` в `token-reported`. Повреждённая запись отменяет всю миграцию
и блокирует запуск без частичного изменения базы.

## Emergency stop

`EMERGENCY_STOP=true` запрещает новые authorization signatures, sponsor
signatures, initial broadcast и exact rebroadcast. Candidates остаются в durable
queue. Read-only watcher, finalized receipt reconciliation и diagnostics не
останавливаются. Для запуска в этом режиме приватные ключи не нужны.

## Structured logs

Журнал имеет формат NDJSON. Финансовые события содержат `chain_id`, incident ID,
durable state, классификацию результата и публичный hash только после broadcast
или доказанного receipt. В журнал не передаются private RPC URL, raw error,
token metadata, private keys, signatures и raw signed transaction.

## Diagnostics

Демон создаёт Unix socket `STATE_DIRECTORY/diagnostics.sock` с правами `0600`.
Он доступен и в emergency stop:

- `GET /healthz` возвращает state-driven health; HTTP `200` означает
  `healthy_idle`, остальные состояния возвращают `503`.
- `GET /metrics` возвращает bounded JSON snapshot без token, incident и RPC URL
  labels.

Health различает `healthy_idle`, `degraded_rpc`, `blocked_budget`,
`ambiguous_rescue` и `stopped`. При одновременных условиях приоритет имеют
`stopped`, затем `ambiguous_rescue`, `blocked_budget` и `degraded_rpc`.
Отсутствие новых строк журнала не меняет health.

Metrics включают глубину queue, candidates, attempts, global и network
spent/reserved/remaining budget, RPC errors, reconnects, lost races, active и
total ambiguous outcomes, delegation state и время последней успешной
reconciliation.

Alerts дедуплицируются по `chain_id` и закрытому коду. Повтор активного условия
не отправляется чаще `ALERT_COOLDOWN`; incident и token address не входят в ключ,
поэтому поток fake token events не создаёт неограниченную cardinality.

Ledger хранит не более `100000` reservation records. При достижении предела он
детерминированно удаляет только старейшие доказанно unused released records;
held, exposed и committed записи не удаляются. Если безопасно освободить место
нельзя, новые paid actions блокируются. Смена каталога состояния этот предел не
сбрасывает.
