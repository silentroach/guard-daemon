# Task 05: Типизированная конфигурация и настоящий dry run

Статус: `DONE`

Исходный commit: `86f7d7d`.

Implementation, `make go-ci`, secret scan и независимое review завершены.
Отчёт: [`../reviews/05-config-dry-run.md`](../reviews/05-config-dry-run.md), digest
проверенного diff:
`39eec1a4f8d183228b5fe83b341235521e2d7c20da4f09844f18686dce7c2f8f`.
Результат включён в разрешённый atomic task commit Task 05.

Зависимости: Tasks 01, 02 и 04.

## Цель

Сделать configuration surface явной, строго проверяемой и соответствующей русскоязычной документации. Реализовать dry run, который физически не способен подписать или отправить транзакцию.

## Границы задачи

- `internal/config` и startup wiring.
- Разделение публичных адресов и private signer.
- Network enablement и RPC overrides.
- Token policy configuration.
- Dry-run signer/broadcaster implementations.
- `.env.example` и минимальная русскоязычная config reference.

Не реализовывать watcher reliability, retry engine и cumulative gas accounting; определить для них typed policy fields и interfaces.

## Обязательные изменения

1. Все env/config fields описаны typed schema с defaults, required/optional semantics и validation.
2. Поддержать explicit `ENABLED_NETWORKS`; daemon не подключается ко всем сетям неявно.
3. Поддержать отдельные HTTP/WS RPC overrides на каждую enabled network.
4. Поддержать token modes: `known-only`/`allowlist` как безопасный default и явный opt-in `all` для unknown tokens.
5. Реализовать и протестировать `RESCUE_TOKENS` либо заменить его однозначной typed настройкой.
6. Добавить fields для per-transaction/cumulative budget, rate limit и sponsor minimum balance, используемые Task 08.
7. Проверять zero address, `source != sponsor`, `source != destination`, required rescuer manifest и chain-specific constraints.
8. Проверять `sponsor != destination`: hot sponsor не может быть safe storage.
9. Dry run использует публичные source/sponsor/destination addresses и не требует private keys.
10. Dry-run dependency graph не содержит production signer/broadcaster; случайный вызов submission возвращает typed error и учитывается test spy.
11. Live mode создаёт signer только после успешного config и RPC quorum attestation.
12. `Signer.Address()` обязан совпасть с configured source/sponsor до первой подписи; mismatch завершает startup.
13. Требовать минимум два независимо настроенных read RPC для live mode; broadcast RPC настраивается отдельно. Providers согласуют finalized block/hash и critical state.
14. Не логировать env values, keys, signatures, raw authorization и private RPC credentials.
15. Удалить или пометить unsupported `CLAIM_*`/другие поля вместо притворной поддержки.

## Параллельные направления

- **Schema/validation:** только `/internal/config/schema.go`, `/internal/config/validate.go` и их tests.
- **Dry run:** только `/internal/config/mode.go`, `/internal/rescue/dryrun/**` и их tests.
- **Network/RPC/token policy:** только `/internal/config/networks.go`, `/internal/config/policy.go` и их tests.
- **Публичные примеры:** только `/.env.example` и `/docs/configuration.md` без реальных адресов.

Координирующий агент один интегрирует `/cmd/guard-daemon/**` и вызов attestation API Task 04.

## Критерии приёмки

- Invalid/malformed/zero/conflicting addresses завершают startup до network connection и signing.
- Все равенства между source, sponsor и destination отклоняются.
- Empty/unknown network name, missing RPC pair и missing trusted deployment дают понятную русскую ошибку.
- `DRY_RUN=true` с fake public addresses проходит planning/simulation и создаёт ноль signatures и ноль broadcasts.
- Live mode без keys не запускается; dry run без keys запускается.
- Fake signer с неправильным source или sponsor address не получает ни одного вызова signing.
- Расхождение RPC quorum блокирует signer construction.
- RPC override и enabled network реально используются, что подтверждают unit tests.
- Token allowlist реально ограничивает watcher query/policy input.
- `.env.example` содержит только placeholders и безопасные defaults.
- Каждый документированный config field покрыт test; phantom settings отсутствуют.

## Проверка

- Table-driven config tests.
- Spy signer/broadcaster tests с assertion `calls == 0` для dry run.
- Integration startup test без network access.
- Secret-redaction tests для validation errors/log fields.
- `go test -race ./...` и build.

## Независимое ревью

Ревьюер пытается обойти dry run через альтернативный путь, malformed env, неявную активацию сети и косвенное создание signer. Отдельно сверяет каждое поле `.env.example` с parser и тестом.

После fixes повторить no-sign/no-broadcast tests и race suite.

## Завершение и commit

После ревью сохранить отчёт в `../reviews/05-config-dry-run.md`, выполнить closure pass и изменить status на `DONE` здесь и в `../tasks.md`.

Рекомендуемый subject:

```text
security(config): реализовать fail-closed настройки и настоящий dry run
```

Commit body перечисляет config contract, безопасные defaults, удалённые phantom options и доказательства отсутствия side effects.
