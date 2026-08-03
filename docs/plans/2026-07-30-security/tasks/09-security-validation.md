# Task 09: Атакующие интеграционные тесты и проверки безопасности

Статус: `DONE`

Исходный commit: `2613a55`.

Implementation, adversarial tests, repository security gates и независимое
review завершены. Отчёт:
[`../reviews/09-security-validation.md`](../reviews/09-security-validation.md),
digest проверенного diff:
`f0c0e09963b45001c23b95a54791776e8a460b7df411efacbd2b763f03a53c0e`.
Результат включён в atomic task commit Task 09.

Зависимости: Tasks 01-08.

## Цель

Проверить всю систему как единое целое в hostile environment и превратить найденные ранее классы ошибок в постоянные regression gates. GitHub Actions должен блокировать merge и release при нарушении security invariants.

## Границы задачи

- End-to-end local-chain tests.
- Adversarial Go/Solidity tests и fuzzing.
- `.github/workflows` release/security gates.
- Static, dependency, race, secret и artifact verification.
- Исправления обнаруженных integration defects в соответствующих packages.

Не выполнять mainnet transactions, не использовать production RPC и keys.

## Обязательные сценарии

1. Competing EIP-7702 authorizations и пропущенный authorization tuple.
2. Attacker-controlled active delegation, возвращающий success без rescue.
3. Старый contract с правильным destination, но без sponsor/runtime identity.
4. Malicious token: fake balance, revert, gas burn, false return, no return, malformed return, callback/reentrancy и fake `Transfer` logs.
5. Production binaries, ABI, artifacts и canonical deployment не содержат удалённый PermitSweeper/permit path.
6. Simultaneous token/native candidates и sponsor nonce contention.
7. RPC timeout, wrong chain, stale response, disconnect gap, duplicate logs и switch WebSocket/polling.
8. Один Byzantine read provider расходится со вторым по finalized block, runtime, receipt или balances; signing блокируется.
9. Reorg до/после receipt и removed logs.
10. Queue saturation, restart, checkpoint restore и incident restore.
11. Budget exhaustion, emergency stop и restart bypass attempts.
12. Dry run с assertion: ноль signatures, ноль broadcasts, ноль live side effects.
13. Secret/log redaction и отсутствие `.env`/key artifacts в Git diff.

## GitHub Actions

Обязательные workflows должны запускаться для pull request и protected branch:

- Go format/build/test/race/vet.
- Contract format/build/unit/fuzz/static analysis.
- TypeScript format/lint/typecheck и deployment tool tests.
- End-to-end local-chain adversarial suite.
- `govulncheck` и dependency audit.
- Secret scan.
- Reproducible artifact/hash comparison.
- Проверка соответствия `.env.example` typed config schema.
- Проверка русскоязычной документации и отсутствия запрещённых operational values.

Workflows используют минимальные permissions, pinned action revisions, concurrency cancellation и caches без secrets. Никакой job не имеет deployment credentials.

## Dependabot и dependency review

- Проверить `.github/dependabot.yml` из Task 01 для Go, npm и GitHub Actions.
- Dependabot pull requests проходят полный обязательный CI.
- Добавить dependency review для новых transitive dependencies и лицензий.
- Major security-sensitive updates требуют ручного ревью; auto-merge не настраивать.
- Vulnerability exception имеет владельца, обоснование, expiry и issue reference без чувствительных данных.

## Параллельные направления

- **Local-chain adversarial suite:** только `/test/integration/chain/**`.
- **Watcher/RPC fault suite:** только `/test/integration/watcher/**`.
- **Policy/load suite:** только `/test/integration/policy/**`.
- **GitHub Actions/security tooling:** только `/.github/workflows/**`, `/.github/dependabot.yml`, `/test/ci/**`.

Каждый subagent владеет отдельным test directory/workflow. Координирующий агент распределяет исправления в production paths последовательно, чтобы не создавать конфликтов.

## Критерии приёмки

- Все mandatory security invariants из `context.md` имеют автоматический test или machine-checkable gate.
- `go test -race ./...`, contract fuzz/static analysis и end-to-end suite проходят многократно без flaky behavior.
- Repository workflows проходят `actionlint`/локальную проверку и описывают обязательный набор checks.
- Dependabot config валиден для трёх ecosystems и не содержит auto-merge.
- Фактический GitHub ruleset/branch protection и создание Dependabot PR проверяет оператор через API evidence перед Task 11. Без evidence release остаётся `BLOCKED`, но repository implementation Task 09 может быть `DONE`.
- Dependency/vulnerability scan не содержит нерешённых Critical/High findings.
- Reproducible artifacts совпадают между clean builds.
- Нет production network access в tests и workflows.

## Независимое ревью

Security-review subagent Task 09 не участвует в реализации. Он получает diff этой задачи, threat model, CI runs и artifacts и проверяет качество regression gates. Целостное финальное ревью Tasks 01-10 выполняется отдельно в Task 11.

Все findings устраняются в этой задаче с повторным полным CI. После существенных исправлений обязателен второй review pass.

## Завершение и коммит

После чистого ревью сохранить отчёт в `../reviews/09-security-validation.md`, выполнить closure pass и изменить статус на `DONE` здесь и в `../tasks.md`.

Рекомендуемый subject:

```text
test(security): закрепить adversarial сценарии и обязательный GitHub CI
```

Тело коммита подробно перечисляет threat scenarios, GitHub Actions gates, Dependabot policy, scan results, исправленные findings и остаточные риски.
