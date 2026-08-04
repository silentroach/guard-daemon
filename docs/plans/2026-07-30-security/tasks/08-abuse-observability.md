# Task 08: Защита от злоупотребления gas и наблюдаемость

Статус: `DONE`

Исходный commit: `5a611cb`.

Реализация, тесты злоупотребления и нагрузки и независимое ревью завершены.
Отчёт: [`../reviews/08-abuse-observability.md`](../reviews/08-abuse-observability.md),
digest проверенного diff:
`b3686ec031b4ba88a40f20d79ac26340938f015f4baf34bdf6ee20cd6ede6077`.
Результат включён в atomic task commit Task 08.

Зависимости: Tasks 05, 06 и 07.

## Цель

Ограничить экономический ущерб от malicious/бесполезных tokens и сделать состояние системы наблюдаемым без утечки секретов. Per-transaction cap дополняется суммарным budget, rate limiting и безопасной token policy.

## Границы задачи

- Fee и abuse policy в `internal/rescue`.
- `internal/observability`.
- Persistent budget counters и health state.
- Русскоязычная operator reference для limits/alerts.

Не менять contract ABI, watcher checkpoint protocol и deployment manifests.

## Обязательные изменения

1. Безопасный default обрабатывает только configured known/allowlisted tokens. Режим `all` требует явного opt-in и предупреждения.
2. Ограничить attempts на token/incident/source event и число новых unknown token addresses за временное окно.
3. Ввести per-network и global sponsor budgets: per transaction, per hour/day и emergency reserve.
4. Единственный persistent `BudgetLedger` атомарно обслуживает все network coordinators.
5. До signing ledger резервирует максимальную стоимость `gasLimit * maxFee` плюс chain-specific overhead. После finalized receipt списывает фактическую стоимость; при ambiguous outcome reservation сохраняется до reconciliation.
6. Budget state и reservations сохраняются между restart либо восстанавливаются консервативно; restart не обнуляет защиту.
7. До submission выполнять bounded simulation/estimate; failure или чрезмерный gas отклоняется без live spend.
8. Min-value policy для native и известных tokens не должна тратить больше ожидаемой ценности. Unknown token без доверенной оценки не считается ценным автоматически.
9. Chain-specific fee caps учитывают base fee и реальную возможность inclusion; Base-specific значения не применяются ко всем сетям вслепую.
10. При исчерпании budget coordinator останавливает paid actions, сохраняет candidates и поднимает alert.
11. Проверять sponsor balance и reserved minimum до каждой попытки.
12. Structured logs содержат chain, incident ID, state, public tx hash после broadcast и классификацию результата, но не keys, signatures, raw signed tx и private RPC URL.
13. Metrics: queue depth, candidates, attempts, spent/remaining budget, RPC errors, reconnects, lost races, ambiguous outcomes, delegation state и last successful reconciliation.
14. Health check основан на state/metrics, а не на «свежести последней строки лога».
15. Alert deduplication и cooldown предотвращают flood.
16. Emergency stop запрещает signing/broadcast, но оставляет read-only monitoring и diagnostics.

## Параллельные направления

- **Budget ledger/fee policy:** только `/internal/budget/**`, `/internal/store/budget*` и их tests.
- **Token abuse policy:** только `/internal/rescue/policy*`, `/internal/rescue/simulation*` и их tests.
- **Observability:** только `/internal/observability/log*`, `/internal/observability/metrics*` и их tests.
- **Alerts/emergency stop:** только `/internal/observability/alerts*`, `/internal/observability/health*` и их tests.

Координирующий агент один подключает policy к coordinator и watcher interfaces.

## Критерии приёмки

- Генерация неограниченного числа fake token events не может превысить configured cumulative budget.
- Restart не позволяет обойти budget.
- Concurrent reservations из разных сетей не превышают global budget.
- Crash до send освобождает безопасно доказанную неиспользованную reservation; crash после send/timeout сохраняет её до reconciliation.
- Default configuration не отправляет paid transaction для unknown token.
- Budget exhaustion и emergency stop дают ноль новых signatures/broadcasts.
- Sponsor reserve никогда не расходуется ниже configured threshold самим daemon.
- Metrics и logs не содержат secret fixtures и private URL.
- Health различает healthy idle, degraded RPC, blocked budget, ambiguous rescue и stopped mode.
- Все policy decisions покрыты deterministic tests с fake clock.

## Проверка

- Property tests cumulative spend invariant.
- Multi-network concurrent reservation, crash-before-send, crash-after-send и ambiguous-timeout tests.
- Load test с тысячами distinct fake token addresses без unbounded memory/goroutines.
- Restart/persistence tests.
- Log/metric redaction tests.
- Chain fee scenarios: low base fee, spike, underpriced cap и estimate failure.
- `go test -race ./...`.

## Независимое ревью

Ревьюер выступает экономическим атакующим: ищет способы обнулить counters, дробить incidents, менять token addresses, провоцировать retries и обходить emergency stop.

Все обходы закрыть и повторить property/load tests.

## Завершение и коммит

После ревью сохранить отчёт в `../reviews/08-abuse-observability.md`, выполнить closure pass и изменить статус на `DONE` здесь и в `../tasks.md`.

Рекомендуемый subject:

```text
security(policy): ограничить расходы sponsor и добавить безопасную наблюдаемость
```

Тело коммита содержит budget invariants, safe defaults, alert semantics, redaction guarantees и результаты abuse tests.
