# Task 06: Надёжное получение событий и жизненный цикл RPC

Статус: `DONE`

Исходный commit: `729af6a`.

Зависимости: Tasks 02 и 05.

## Цель

Гарантировать, что временная занятость, RPC error, WebSocket reconnect, переход на polling и короткий reorg не приводят к молчаливой потере rescue candidate. Ограничить время каждого сетевого вызова и исключить гонки при замене клиента.

## Границы задачи

- `internal/watcher`.
- `internal/rpc`.
- Хранилище checkpoints и deduplication state.
- Очередь передачи candidates в `internal/rescue` через interface Task 02.
- Unit и integration tests с fake RPC/subscription.

Не реализовывать signing, nonce allocation, retry transaction и gas budget.

## Обязательные изменения

1. Один управляемый lifecycle владеет RPC client, subscriptions и дочерними goroutines для каждой сети.
2. RPC layer различает независимые read providers и broadcast provider. Для critical reads предоставляет quorum API на одном finalized block/hash.
3. Все вызовы используют context с deadline/cancellation; shutdown отменяет работу и дожидается завершения.
4. После WebSocket disconnect выполняется backfill от последнего подтверждённого checkpoint до текущего безопасного блока, затем создаётся новая subscription.
5. В polling-режиме checkpoint обновляется только после успешного получения, проверки и постановки logs в очередь.
6. Первый запуск использует явную policy: configured start block либо bounded lookback. Нельзя молча начинать «с текущего блока» без описания.
7. Logs дедуплицируются по chain/block hash/transaction hash/log index; duplicate delivery безопасна.
8. Обрабатывать `Removed` logs и короткие reorg. Candidate до требуемой finality имеет соответствующий статус.
9. В очередь попадает структурированный candidate; watcher не запускает отдельную transaction goroutine на каждый event.
10. Handoff следует interface Task 02: stable candidate ID, durable `Put`, incident persistence, `Ack`, replay. Checkpoint не опережает durable acknowledgement.
11. Если очередь занята, применяется bounded backpressure или durable spill/checkpoint policy, но candidate не теряется молча.
12. Обнаруженные unknown tokens сохраняются в ограниченном состоянии согласно token policy Task 05; память и диск не растут бесконечно.
13. Periodic reconciliation проверяет configured tokens и незавершённые discovered tokens, чтобы восстановиться после пропущенного event.
14. Token metadata считается недоверенной: timeout, size limits, sanitization и cache limits обязательны.
15. RPC override из config действительно используется; private URL не попадает в logs/errors.
16. Нельзя менять `s.client` под работающими goroutines без синхронизированной смены поколения.

## Параллельные направления

- **RPC lifecycle/quorum:** только `/internal/rpc/**` и `/internal/rpc/**/*_test.go`.
- **Subscription/polling:** только `/internal/watcher/subscription*`, `/internal/watcher/polling*` и их tests.
- **Очередь/deduplication:** только `/internal/watcher/queue*`, `/internal/store/candidates*` и их tests.
- **Metadata/reconciliation:** только `/internal/watcher/metadata*`, `/internal/watcher/reconcile*` и их tests.

Координирующий агент заранее фиксирует interfaces и один выполняет итоговую сборку `watcher.Service`.

## Критерии приёмки

- Искусственный disconnect с событиями в паузе завершается backfill без потерь и duplicates.
- Ошибка `FilterLogs` не двигает checkpoint.
- Reconnect не создаёт data race и не оставляет goroutine со старым закрытым client.
- Hung RPC отменяется по deadline; test не зависит от wall-clock sleep.
- Queue saturation имеет измеримое и протестированное поведение без silent drop.
- Crash в каждой точке Put/incident/Ack/checkpoint после restart не теряет candidate и не создаёт новый ID.
- Critical read quorum отклоняет расходящиеся finalized block/runtime/balance ответы.
- Restart восстанавливает checkpoint и незавершённые candidates из безопасного локального state.
- Reorg/removed log не приводит к ложному final candidate.
- `go test -race ./...` проходит.

## Проверка

- Deterministic fake RPC с последовательностями success/error/hang/disconnect/reorg.
- Property tests для checkpoint monotonicity только по успешно обработанной canonical chain.
- Leak test для goroutines и cancellation.
- Fuzz для malformed logs и metadata return data.
- Race suite и integration test WebSocket-to-polling fallback.

## Независимое ревью

Отдельный ревьюер моделирует disconnect в каждом переходе состояния, проверяет порядок checkpoint/queue acknowledgement, bounded memory/disk и отсутствие private RPC URL в logs.

Все замечания исправляются; изменения state machine требуют повторного ревью.

Финальный closure pass: `ПРОЙДЕНО`. Все `REV-06-001` — `REV-06-015`
закрыты; новых findings нет. Отчёт: [review 06](../reviews/06-watcher-rpc.md).

## Evidence

- Реализованы hash-pinned finalized quorum scanner, bounded backfill,
  subscription hints, polling fallback, reconnect/reorg validation и deadlines.
- Production bbolt store атомарно сохраняет canonical journal, FIFO ready queue,
  delayed retry, incidents, scan cursor и confirmed checkpoint; reopen fail
  closed проверяет bindings, bounds, indexes, sequences и acknowledgement
  coverage.
- Queue/discovery/metadata/history ограничены; saturation и overflow не создают
  silent drop и не блокируют authoritative scanner.
- Официальный Go `1.26.5`: `make go-ci` успешно. Полный race suite, stress
  `-count=20`, оба fuzz target, `go vet`, static build, tidy diff, formatting,
  vulnerability и secret gates прошли.
- Проверенный digest 37 implementation paths:
  `b8674a01a05cfddccb81c884b6f5577c8557511f42a006f636d70dc5bb678718`;
  независимо воспроизведён reviewer.
- Статус `DONE`, evidence и отчёт фиксируются тем же atomic task commit.

## Завершение и коммит

После ревью сохранить отчёт в `../reviews/06-watcher-rpc.md`, выполнить closure pass и изменить статус на `DONE` здесь и в `../tasks.md`.

Рекомендуемый subject:

```text
security(watcher): исключить потери событий при RPC сбоях и reorg
```

Тело коммита описывает state machine, checkpoint guarantees, backpressure, восстановление после reconnect и результаты race tests.
