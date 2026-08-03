# Отчёт независимого ревью Task 08

Задача: `Task 08`

Статус ревью: `ПРОЙДЕНО`

## Проверенный снимок

- Base commit: `5a611cb6a5272d6e9328430d7933f2a9c5ba4d33`.
- Проверенные пути: budget ledger и sponsor fence, fee/value/admission/simulation
  policy, coordinator integration, alerts/health/metrics/logs, diagnostics,
  typed config, tests и русскоязычная operator documentation Task 08.
- SHA-256 digest diff проверенных production/test/doc файлов:
  `b3686ec031b4ba88a40f20d79ac26340938f015f4baf34bdf6ee20cd6ede6077`.
- Из digest исключены только этот отчёт, `findings.md`, task-файл и task index.
- Digest построен при `LC_ALL=C` из binary/full-index tracked diff от base и
  отсортированных SHA-256 содержимого untracked files; включены 28 tracked и 26
  untracked paths.
- Digest независимо воспроизведён reviewer и координирующим агентом.
- Ревьюер не участвовал в реализации: `да`.
- Первичный review и четыре closure pass требовали исправлений; последний pass
  не обнаружил открытых Critical, High или Medium findings.

## Проверенные инварианты

- Один ACID ledger обслуживает все сети sponsor, сохраняется в durable
  `/var/lib/guard-daemon` независимо от `STATE_DIRECTORY` и защищён host-wide
  process fence.
- Global и network caps одной транзакции, часа, суток и cumulative atomically
  учитывают held/exposed reservations. Restart, retries, concurrent networks и
  clock skew не обнуляют counters.
- Emergency sponsor reserve повторно проверяется по finalized quorum balance
  перед authorization signature, sponsor signature и каждым send/rebroadcast.
- Live signing разрешён только для execution-only fee model с enforceable
  transaction cap. Сети с некэпируемым дополнительным L1/operator fee
  fail-closed остаются только в dry-run/read-only режиме.
- Exact EIP-7702 call с authorization tuple проходит pinned quorum simulation и
  bounded primary estimate до sponsor signature.
- Canonical finalized receipt списывается до untrusted postcondition reads по
  `gasUsed * effectiveGasPrice`; посторонние входящие/исходящие движения sponsor
  не искажают transaction accounting.
- Default token policy отклоняет unknown addresses. Явно разрешённый unknown
  token остаётся недоверенным, имеет отдельный cap и не получает trusted success.
- Admission counters persistently ограничивают global rate, token attempts,
  source events, unknown addresses и memory cardinality.
- Emergency stop запрещает signing и broadcast, сохраняя queue, monitoring,
  reconciliation и diagnostics.
- Logs, alerts и metrics используют закрытые bounded schemas без keys,
  signatures, raw transactions, token metadata и private RPC URL. Alert outbox
  crash-consistent, cooldown не скрывает повторную активацию.
- Health начинается с degraded state, восстанавливает rolling budget и
  ambiguity из durable state и очищает RPC degradation только после полностью
  успешной операции с реальными reads.

## Findings

| ID | Критичность | Путь | Описание | Требуемое исправление | Статус |
|---|---|---|---|---|---|
| REV-08-001 | High | `internal/store/budget_path.go` | Смена `STATE_DIRECTORY` могла создать независимый budget state | Canonical sponsor-bound path и fence | ЗАКРЫТО |
| REV-08-002 | High | `internal/rescue/operations.go`, `internal/budget/**` | Максимальная и фактическая стоимость не полностью связывались с sponsor debit | Полная reservation, recheck и finalized actual accounting | ЗАКРЫТО |
| REV-08-003 | High | `internal/budget/ledger.go` | Released attempt можно было небезопасно переоткрыть | Повторная полная budget/reserve проверка | ЗАКРЫТО |
| REV-08-004 | Medium | `internal/rescue/policy_persistence.go` | Admission limits не были общими и persistent для всех сетей | Единая bounded bbolt admission policy | ЗАКРЫТО |
| REV-08-005 | Medium | `internal/rescue/operations.go` | Sponsor reserve не перепроверялся перед каждой paid action | Finalized capacity check перед signatures/send | ЗАКРЫТО |
| REV-08-006 | Medium | `internal/budget/ledger.go`, `internal/rescue/policy.go` | Rolling windows можно было сдвигать недоверенным временем | Persisted monotonic clock и bounded timestamp skew | ЗАКРЫТО |
| REV-08-007 | Medium | `internal/budget/ledger.go` | Persistent records не имели жёсткой ёмкости | `MaxRecords` и fail-closed capacity | ЗАКРЫТО |
| REV-08-008 | Medium | `internal/observability/**` | Metrics и health неполно отражали RPC, ambiguity и budget | State-driven bounded telemetry | ЗАКРЫТО |
| REV-08-009 | Medium | `internal/observability/alerts.go` | Alert cooldown и active state не переживали restart | Persistent dedup state и structured sink | ЗАКРЫТО |
| REV-08-010 | Medium | `internal/rescue/coordinator.go` | Явно allowlisted unknown token не имел отдельной безопасной семантики | Untrusted outcome и отдельный cap | ЗАКРЫТО |
| REV-08-011 | High | `internal/store/budget_path.go` | `/var/tmp` мог очищаться между restart | Durable `/var/lib/guard-daemon` | ЗАКРЫТО |
| REV-08-012 | High | `internal/config/economics.go`, `internal/rescue/fees.go` | Статический L2 overhead не является enforceable fee cap | Fail-closed запрет live signing для unbounded fee model | ЗАКРЫТО |
| REV-08-013 | Medium | `internal/rescue/operations.go` | Sponsor balance delta допускал under/over-accounting | Receipt-derived exact execution fee | ЗАКРЫТО |
| REV-08-014 | Medium | `internal/budget/ledger.go`, `internal/rescue/policy.go` | Одна сеть могла перемотать shared rolling clock | Local monotonic windows и проверка chain skew | ЗАКРЫТО |
| REV-08-015 | Medium | `internal/rescue/operations.go` | Finalized spend списывался после token postconditions | Commit receipt cost до postcondition reads | ЗАКРЫТО |
| REV-08-016 | Medium | `internal/rescue/simulation.go` | Primary RPC мог ложно подтвердить simulation | Pinned quorum execution плюс bounded estimate | ЗАКРЫТО |
| REV-08-017 | Medium | `internal/observability/health.go`, `cmd/guard-daemon/daemon.go` | Uninitialized/exhausted daemon мог быть healthy | Initial degraded и restored budget conditions | ЗАКРЫТО |
| REV-08-018 | Medium | `internal/observability/observer.go`, `cmd/guard-daemon/daemon.go` | Validator отклонял alert log, delivery errors игнорировались | Разрешённый code и propagation в health/process | ЗАКРЫТО |
| REV-08-019 | Medium | `internal/observability/alerts.go` | Cooldown persistence не была crash-consistent с delivery | Durable pending outbox, fsync и replay | ЗАКРЫТО |
| REV-08-020 | Low | `internal/rescue/policy_persistence.go` | Повреждённая admission DB могла переинициализироваться | `O_EXCL` first-use и strict existing schema | ЗАКРЫТО |
| REV-08-021 | Low | `internal/budget/ledger.go` | Released records могли исчерпать capacity без расходов | Детерминированное удаление только released records | ЗАКРЫТО |
| REV-08-022 | Low | `internal/rescue/coordinator.go` | Failure logs не содержали incident/durable state | Incident-aware safe structured events | ЗАКРЫТО |
| REV-08-023 | Medium | `cmd/guard-daemon/daemon.go`, `internal/rescue/operations.go` | Health учитывал только cumulative exhaustion | Hour/day/cumulative blocked state | ЗАКРЫТО |
| REV-08-024 | Medium | `internal/rescue/coordinator.go` | Terminal replay мог очистить RPC degradation без read | Очистка только после успешной операции с RPC | ЗАКРЫТО |
| REV-08-025 | Medium | `internal/observability/alerts.go` | Failed persist оставлял mutated in-memory alert | Rollback pre-commit mutation | ЗАКРЫТО |
| REV-08-026 | Medium | `cmd/guard-daemon/daemon.go` | Stale stopped/budget alerts не reconciled на startup | Raise/resolve по восстановленному состоянию | ЗАКРЫТО |
| REV-08-027 | Medium | `internal/rescue/operations.go` | Periodic error ошибочно назначался всем child incidents | Per-asset incident attribution | ЗАКРЫТО |
| REV-08-028 | Medium | `internal/observability/alerts.go` | Ошибка после успешного rename конфликтовала с rollback | Отдельное committed-error состояние | ЗАКРЫТО |
| REV-08-029 | Medium | `internal/rescue/operations.go` | Intermediate read преждевременно очищал RPC degradation | Отложенная очистка в конце успешного `Handle` | ЗАКРЫТО |
| REV-08-030 | Medium | `internal/rescue/operations.go` | Только первый periodic error обновлял RPC telemetry | Telemetry для каждого failed child | ЗАКРЫТО |
| REV-08-031 | Medium | `internal/observability/alerts.go` | Reactivation внутри cooldown оставалась без firing event | Cooldown только для active reminders | ЗАКРЫТО |
| REV-08-032 | Medium | `internal/budget/ledger.go` | Idle snapshot не обновлял rolling health | Current-time read-only aging snapshot | ЗАКРЫТО |
| REV-08-033 | Medium | `internal/budget/ledger.go` | Diagnostic snapshot мог persist host clock jump | Read-only snapshot без изменения `lastSeen` | ЗАКРЫТО |

## Проверка исправлений

- Closure pass выполнен после каждого изменения production/test файлов.
- Digest обновлён после последнего изменения и независимо воспроизведён.
- `nix develop --command go test -mod=readonly ./...` — PASS.
- `nix develop --command go test -race -mod=readonly ./...` — PASS.
- `nix develop --command go vet -mod=readonly ./...` — PASS.
- `nix develop --command go build -mod=readonly ./...` — PASS.
- `nix develop --command go mod verify` — PASS.
- Property, concurrent multi-network, restart, crash-before/after-send,
  ambiguous-timeout, fake-token load, redaction, fee-spike и simulation tests —
  PASS в составе Go suite.
- `npm run typecheck`, `npm run lint`, `npm run contracts:test` и
  `npm run format:check` — PASS.
- `git diff --check` — PASS.

Локальный `npm run deploy:test` трижды завершился внутренним assertion pinned
Node 24.18.1 в `InternalCallbackScope::Close` после шести успешных deploy tests,
включая повтор с `--test-concurrency=1`. Task 08 не меняет TypeScript/deployment
paths; этот toolchain failure не является finding проверяемого diff и должен быть
повторно проверен repository security gates Task 09.

## Остаточные риски

| ID | Критичность | Обоснование | Владелец | Срок пересмотра |
|---|---|---|---|---|
| Нет | — | Нет в рамках задачи | — | — |

Fail-closed запрет live paid actions для сетей с некэпируемым дополнительным fee
является документированным ограничением, а не принятым риском потери средств.

## Решение

`ПРОЙДЕНО`: открытые Critical, High и Medium findings отсутствуют; все findings
закрыты, digest соответствует последнему production/test/doc diff, критерии
Task 08 подтверждены. Статус `DONE`, отчёт и implementation входят в один
atomic task commit.
