# Task 07: Координатор транзакций, повторы и проверка результата

Статус: `PENDING`

Зависимости: Tasks 02, 03, 04 и 05.

## Цель

Создать единственный на сеть transaction coordinator, который не теряет работу, не переиспользует sponsor nonce, ограниченно повторяет конкретный rescue incident и не объявляет успех без доказательства ожидаемого результата.

## Границы задачи

- `internal/rescue`.
- Реализации signer/broadcaster interfaces для live mode.
- Incident/retry state и подтверждение результата.
- Unit tests с fake chain и local-chain integration tests.

Не реализовывать watcher, deployment tools и полный observability backend. Fee/budget interface из Task 05 используется, а policy наполняется в Task 08.

## Обязательные изменения

1. Один coordinator последовательно владеет sponsor nonce allocation, signing и submission в рамках сети.
2. До запуска coordinator получает exclusive persistent lease для пары chain+sponsor. Второй процесс безопасно отказывается запускаться; потеря lease останавливает signing.
3. Candidate всегда enqueue/coalesce; занятый coordinator не возвращает «skip» с потерей работы.
4. Каждая попытка связана с incident: chain, asset, observed balance/event, generation и policy snapshot.
5. Ошибки nonce read, fee read, signing, broadcasting, receipt timeout и postcondition переводят incident в явное retry/ambiguous/failed state.
6. Retry limit и cooldown относятся к incident. Новое поступление или рост balance создают новую generation после policy checks.
7. Exhausted incident не остаётся в бесконечном hot polling и не блокирует будущие поступления token address.
8. EIP-7702 authorization nonce читается максимально поздно; skipped authorization считается ожидаемой hostile race.
9. Удалить proactive delegation renewal. Каждая asset rescue operation содержит собственную authorization. Если отдельный renewal когда-либо потребуется, outer transaction не может иметь `To=source` и требует отдельного security review.
10. Обязательный test доказывает: при skipped authorization никакая delegation-only transaction не вызывает current source fallback.
11. Token rescue postcondition работает по принципу fail closed: RPC/decode/quorum error — `ambiguous`, не success.
12. Нулевой source balance недостаточен. Для allowlisted trusted token проверять destination balance delta на согласованном finalized state; несовпадение классифицировать как lost/stolen/ambiguous и поднимать alert.
13. Для unknown/unattested token использовать только `token-reported outcome`, никогда не `success` или «доказанно спасено». Paid action по умолчанию запрещён Task 08.
14. Receipt считается окончательным только после configured confirmations/finality и повторной canonical-chain проверки.
15. Native rescue выполнять атомарным EIP-7702 SetCodeTx с `sweepEth` data, а не отдельным вызовом после предварительной проверки delegation.
16. Симуляция перед отправкой не заменяет postcondition и не может обходить budget.
17. Source/sponsor/destination role equality отклоняется config и contract/deployment layers.
18. Перед signing сверять `Signer.Address()` с configured role и действующий process lease.
19. Неиспользуемый PermitSweeper не интегрируется; production permit path отсутствует.
20. Raw signed transaction и signatures не логируются.
21. External sponsor transaction вызывает nonce reconciliation; deployment runbook запрещает одновременный deployment и daemon signing одним sponsor.

## Параллельные направления

- **Coordinator/nonce/lease:** только `/internal/rescue/coordinator*`, `/internal/store/lease*` и их tests.
- **Retry incidents:** только `/internal/rescue/incident*`, `/internal/store/incidents*` и их tests.
- **Postconditions:** только `/internal/rescue/outcome*`, `/internal/rescue/finality*` и их tests.
- **EIP-7702 builder:** только `/internal/rescue/eip7702*`, `/internal/rescue/txbuilder*` и их tests.

Общие domain interfaces фиксируются координирующим агентом до параллельной работы. Каждый subagent владеет отдельными файлами.

## Обязательные tests

- Два simultaneous candidates не используют одинаковый sponsor nonce.
- Contention ставит candidate в очередь, а не теряет.
- Attacker authorization с тем же source nonce приводит к lost-race/ambiguous, не false success.
- Skipped authorization в удалённом proactive-renewal scenario не вызывает malicious source fallback.
- `receipt.Status == 1` и RPC error postcondition не дают success.
- Source balance zero при отсутствии destination delta не даёт success.
- Reorg после первого receipt отменяет прежний результат.
- Три failed attempts не запрещают новую deposit generation.
- Receipt timeout и send error сохраняют retryable incident.
- Atomic native rescue включает authorization и call в одной transaction.
- Restart восстанавливает pending incident без duplicate unsafe submission.
- Второй процесс с той же chain+sponsor lease не может подписать; потеря lease останавливает coordinator.
- Unknown token никогда не получает статус доказанного economic success только по собственным balances/events.

## Критерии приёмки

- Нет пути, где financial operation запускается вне coordinator.
- Для allowlisted trusted token статус success означает доказанный destination outcome после требуемых confirmations; unknown token получает только `token-reported outcome`.
- Все неопределённые ситуации сохраняются и видимы оператору.
- Retry state bounded и не создаёт permanent token blacklist.
- `go test -race ./...` и local-chain adversarial tests проходят.
- Ни один test не обращается к mainnet и не использует реальные keys.

## Независимое ревью

Ревьюер строит таблицу всех ошибок между nonce read, signing, send, inclusion, reorg и postcondition. Для каждого перехода должен существовать безопасный state и test.

Все findings исправить. Изменение state machine или success definition требует второго review pass.

## Завершение и коммит

После ревью сохранить отчёт в `../reviews/07-rescue-coordinator.md`, выполнить closure pass и изменить статус на `DONE` здесь и в `../tasks.md`.

Рекомендуемый subject:

```text
security(rescue): сериализовать транзакции и доказуемо проверять результат
```

Тело коммита описывает incident model, nonce guarantees, success proof, retry policy и adversarial race tests.
