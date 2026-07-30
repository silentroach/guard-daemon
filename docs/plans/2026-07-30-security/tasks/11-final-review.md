# Task 11: Финальное независимое ревью и решение о выпуске

Статус: `PENDING`

Зависимости: Tasks 01-10.

## Цель

После всех изменений кода, tests, GitHub Actions и русскоязычной документации провести отдельное целостное ревью release candidate. Эта задача не предполагает mainnet deployment, tag или push.

## Границы задачи

- Весь diff от исходного remediation base до release candidate.
- Contract artifacts, manifests/schema, Go binaries, SBOM/checksums.
- Все GitHub Actions workflows, Dependabot config и operator evidence repository rules.
- Все русскоязычные runbooks и service files.
- Матрица `../findings.md` и review reports Tasks 01-10.

## Обязательная процедура

1. Назначить ревьюера, не участвовавшего в реализации Tasks 01-10.
2. Зафиксировать base commit, candidate tree digest, toolchain versions и полный список paths.
3. Проверить каждый обязательный инвариант `context.md` и каждую строку `findings.md` по evidence, regression test и task review report.
4. Повторно проанализировать contracts, EIP-7702 ordering, RPC quorum, signer identity, leases, durable handoff, budget ledger, postconditions и dry run.
5. Выполнить весь pinned CI локально в clean environment и сверить reproducible artifacts.
6. Проверить отсутствие secrets/incident identifiers и полноту русского языка.
7. Проверить operator-provided GitHub API evidence для branch protection/required checks и Dependabot. Если evidence нет, статус задачи `BLOCKED`, а не условный успех.
8. Любой finding получает ID `FINAL-*`, criticality, evidence, owner и исправление.
9. Исправления выполняются отдельными subagents с непересекающимися точными paths. После каждого изменения обязательны соответствующие tests и closure review.
10. После любых fixes повторить полный CI и обновить candidate digest.

## Параллельные направления ревью

Ревьюеры могут работать параллельно без изменения файлов:

- Contracts/deployment supply chain.
- Go concurrency/state persistence/RPC quorum.
- Financial policy/postconditions/budget ledger.
- GitHub Actions/dependencies/reproducibility/secrets.
- Русскоязычная documentation/operations/release.

Исправления начинаются только после объединения findings; path ownership назначает один координирующий агент.

## Критерии приёмки

- Все строки `findings.md` имеют статус `CLOSED` с test/gate и review report.
- Нет нерешённых Critical/High findings.
- Medium/Low либо исправлены, либо находятся в структурированном реестре с владельцем и сроком.
- Полный CI и vulnerability/secret/race/fuzz scans проходят на candidate tree.
- Reproducible artifacts и checksums совпадают между clean builds.
- GitHub repository rules подтверждены operator evidence.
- Финальный отчёт содержит актуальный candidate digest и решение `РАЗРЕШИТЬ ВЫПУСК` либо `БЛОКИРОВАТЬ`.
- Никаких tag, push, release publication и mainnet actions без отдельной явной команды оператора.

## Отчёт и коммит

Сохранить финальный отчёт в `../reviews/11-final-release.md`. Любые fixes после первичного отчёта проходят closure pass и отражаются в нём.

После решения `РАЗРЕШИТЬ ВЫПУСК` изменить статус на `DONE` здесь и в `../tasks.md`.

Рекомендуемый subject:

```text
security(release): завершить независимое ревью release candidate
```

Тело коммита перечисляет candidate digest, проверенные invariants, полный CI, закрытые FINAL findings, operator evidence и решение о выпуске.
