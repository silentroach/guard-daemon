# Task 02: Архитектура Go и тестируемые границы

Статус: `PENDING`

Зависимости: Task 01.

## Цель

Разделить монолитный Go daemon на небольшие тестируемые packages без намеренного изменения поведения. Создать стабильные interfaces, чтобы watcher/RPC и rescue coordinator можно было безопасно реализовывать параллельно.

## Границы задачи

- Перенос startup/wiring в `cmd/guard-daemon`.
- Создание package boundaries из `context.md`.
- Interfaces для RPC reads/subscriptions, signer, broadcaster, clock, checkpoint store, candidate queue и observability.
- Typed domain models для network, token, rescue candidate, transaction outcome и retry state.
- Crash-consistent handoff contract: stable candidate ID, durable `Put`, incident persistence, `Ack`, replay и idempotent coalescing.
- Interfaces для межпроцессного lease и единого global budget ledger, реализация которых принадлежит Tasks 07 и 08.
- Dependency injection вместо скрытого mutable global state.
- Characterization tests для сохраняемого поведения.

Не исправлять business/security behavior, кроме минимального fail-safe изменения, без которого refactor нельзя проверить. Такое изменение должно быть отдельно отмечено и протестировано.

## Design requirements

1. `main` только загружает config, создаёт dependencies, запускает service и обрабатывает shutdown.
2. Packages не читают environment напрямую, кроме `internal/config` в следующей задаче; на этом этапе допускается совместимый adapter.
3. RPC client не хранится в поле, которое независимо заменяют background goroutines.
4. Transaction submission доступен только через interface, который поддерживает fake implementation и dry-run implementation.
5. Watcher производит candidates, но не подписывает транзакции.
6. Rescue package потребляет candidates, но не управляет subscription lifecycle.
7. Time, retry и confirmation logic используют injectable clock.
8. Ошибки имеют typed/classified representation, пригодную для retry и operator alerts.
9. Logging interface не принимает secrets и raw signed transactions.
10. Новый package layout документирован на русском языке.
11. Удалить incident-specific identifiers из Go comments/constants, не цитируя их в документации или commit message.
12. До параллельного выполнения Tasks 06/07 зафиксировать порядок: log persisted/enqueued → incident persisted → queue acknowledgement → checkpoint advancement.
13. Удалить из Go daemon неиспользуемые PermitSweeper ABI, state и заявления о fallback; production path остаётся только EIP-7702 Rescuer.

## Последовательность и параллельность

Coordinator сначала создаёт и фиксирует interfaces/domain types. После этого параллельно:

- **Watcher extraction:** только `/internal/watcher/**` и `/internal/watcher/**/*_test.go`.
- **Rescue extraction:** только `/internal/rescue/**` и `/internal/rescue/**/*_test.go`.
- **RPC/contracts extraction:** только `/internal/rpc/**`, `/internal/contracts/**` и их tests.
- **Persistence contracts:** только `/internal/store/interfaces.go`, `/internal/store/model.go` и interface tests.
- **Process/config adapter:** только `/cmd/guard-daemon/**` и временный `/internal/config/legacy.go`.

Только coordinator редактирует исходный `main.go` во время интеграции. Subagents не должны параллельно удалять или переносить одни и те же функции.

## Критерии приёмки

- Production entry point собирается и сохраняет текущий набор поддерживаемых действий.
- Core packages тестируются без public RPC и без private keys.
- Fake signer/broadcaster позволяет доказать количество и содержание attempted operations без подписи.
- `go test ./...`, `go test -race ./...`, `go vet ./...` проходят.
- Нет package import cycle и mutable globals для runtime state.
- Дальнейшие Tasks 05-07 могут владеть разными directories без изменения общих interfaces.
- Crash tests для handoff interface доказывают replay без потери и idempotent duplicate handling.
- Русскоязычная architecture note соответствует фактической структуре.

## Проверка

```text
go test ./...
go test -race ./...
go vet ./...
go build -mod=readonly ./...
```

Добавить characterization tests как минимум для ABI packing, fee cap application, address derivation, delegation parsing и существующей network loop wiring.

## Независимое ревью

Ревьюер сравнивает поведение до/после, проверяет владение состоянием, жизненный цикл goroutines, interfaces на утечки chain-specific details и пригодность для adversarial tests.

После исправлений повторить race tests. Material interface changes требуют второго review pass.

## Завершение и commit

После ревью сохранить отчёт в `../reviews/02-go-architecture.md`, выполнить closure pass и изменить статус на `DONE` здесь и в `../tasks.md`.

Рекомендуемый subject:

```text
refactor(go): разделить daemon на безопасные тестируемые компоненты
```

Commit body описывает старые coupling problems, новые boundaries, сохранённое поведение, migration notes и результаты race review.
