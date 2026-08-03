# Задачи по устранению недостатков

Перед началом любой задачи агент обязан прочитать [context.md](context.md). Статус меняется здесь и в task-файле только после реализации, проверки, независимого ревью и коммита.

## Значения статуса

- `PENDING`: не начата.
- `IN PROGRESS`: назначена и выполняется.
- `BLOCKED`: зависимость или внешнее решение не позволяет продолжить.
- `DONE`: критерии приёмки и ревью пройдены, задача закоммичена.

## Волны выполнения

Задачи одной волны разрешено выполнять параллельно только при непересекающемся path ownership. Следующая волна начинается после статуса `DONE` у всех её dependencies.

| Волна | Задачи | Параллельность |
|---|---|---|
| 0 | Task 01 | Последовательная подготовка репозитория |
| 1 | Tasks 02 и 03 | Архитектура Go и Solidity contracts параллельно |
| 2 | Task 04 | Схема artifacts/deployment и API аттестации |
| 3 | Task 05 | Конфигурация и startup integration поверх manifest schema |
| 4 | Tasks 06 и 07 | Watcher/RPC и rescue coordinator на crash-consistent interfaces Task 02 |
| 5 | Task 08 | Интеграция защиты, global budget ledger и наблюдаемости |
| 6 | Task 09 | Атакующие интеграционные тесты и repository security gates |
| 7 | Task 10 | Русскоязычная документация эксплуатации и release candidate |
| 8 | Task 11 | Финальное независимое ревью release candidate |

## Индекс задач

- [x] [`DONE` Task 01: Воспроизводимая основа и безопасность репозитория](tasks/01-foundation.md)
- [x] [`DONE` Task 02: Архитектура Go и тестируемые границы](tasks/02-go-architecture.md)
- [x] [`DONE` Task 03: Усиление смарт-контрактов](tasks/03-contract-hardening.md)
- [x] [`DONE` Task 04: Воспроизводимый деплой и аттестация runtime](tasks/04-deployment-attestation.md)
- [x] [`DONE` Task 05: Типизированная конфигурация и настоящий dry run](tasks/05-config-dry-run.md)
- [x] [`DONE` Task 06: Надёжное получение событий и жизненный цикл RPC](tasks/06-watcher-rpc.md)
- [x] [`DONE` Task 07: Координатор транзакций, повторы и постусловия](tasks/07-rescue-coordinator.md)
- [x] [`DONE` Task 08: Защита от злоупотребления gas и наблюдаемость](tasks/08-abuse-observability.md)
- [x] [`DONE` Task 09: Атакующие интеграционные тесты и проверки безопасности](tasks/09-security-validation.md)
- [ ] [`PENDING` Task 10: Эксплуатация, миграция и документация выпуска](tasks/10-operations-release.md)
- [ ] [`PENDING` Task 11: Финальное независимое ревью и решение о выпуске](tasks/11-final-review.md)

Матрица исходных findings и доказательств закрытия: [findings.md](findings.md).

Шаблон обязательного отчёта независимого ревью: [review-template.md](review-template.md).

## Соответствие рисков задачам

| Риск | Ответственная задача |
|---|---|
| Случайный commit секретов; незакреплённые toolchains/dependencies; нет GitHub Actions и Dependabot | Task 01 |
| Монолитный Go-код мешает безопасной параллельной работе и изолированным тестам | Task 02 |
| Arbitrary delegated calls, permit surface и no-return ERC-20 | Task 03 |
| Устаревшие deployment addresses, слабая runtime identity и partial success | Task 04 |
| Фальшивый dry run, игнорируемые RPC/filter/network settings и unsafe defaults | Task 05 |
| Пробелы polling/reconnect, потерянные события, reorg и зависающие RPC calls | Task 06 |
| Nonce races, потерянная работа, вечная блокировка повторов и false success | Task 07 |
| Gas griefing, суммарные потери sponsor, отсутствие alerts и health state | Task 08 |
| Нет атакующих тестов, dependency scans, race tests и CI gates | Task 09 |
| Неточная документация, unsafe activation, слабая изоляция service и отсутствие rollback | Task 10 |
| Целостное ревью после всех изменений кода, документации и эксплуатации | Task 11 |

## Общие правила задач

- Задача может добавить узкий interface в package зависимости, но не должна реализовывать чужое поведение.
- Если двум задачам нужен один файл, сначала завершить dependency либо назначить одного владельца интеграции. Не объединять параллельные правки вслепую.
- Каждый task-файл задаёт точные glob-пути владельцев. Координирующий агент проверяет `git diff --name-only`; shared files меняются только в последовательной integration phase.
- Contract source относится к Task 03. Deployment tooling — к Task 04. Поведение Go runtime — к Tasks 05-08.
- Вся новая и изменяемая документация должна быть на русском языке.
- Ранние задачи могут минимально исправлять публичную документацию только если старый текст предлагает опасное действие. Полный перевод и сверка принадлежат Task 10.
- Mainnet deployment никогда не является acceptance criterion автоматизированной задачи. Результат — проверенный software, deterministic artifacts и operator runbook.

## Реестр принятых остаточных рисков

| ID | Критичность после мер | Владелец | Обоснование | Срок пересмотра | Статус |
|---|---|---|---|---|---|
| Нет | — | — | Остаточные риски пока не приняты | — | — |

Critical и High findings в этот реестр добавлять запрещено.

## Финальное завершение

После Task 11 release candidate допускается к выпуску только при отмеченных checkbox, заполненной матрице `findings.md`, проверенных остаточных рисках и выполненных global release gates из `context.md`.
