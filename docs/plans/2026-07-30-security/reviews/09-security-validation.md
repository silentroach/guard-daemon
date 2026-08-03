# Отчёт независимого ревью Task 09

Задача: `Task 09`

Статус ревью: `ПРОЙДЕНО`

## Проверенный снимок

- Base commit: `2613a5503218f1e711ba5c6b5c4dc775f1359085`.
- Проверенные пути: GitHub workflows, Make targets и CI tooling; adversarial
  Go/Solidity/deployment suites; ERC-20 outcome и bbolt migration; связанная
  русскоязычная документация Task 09.
- SHA-256 digest diff проверенных production/test/doc файлов:
  `f0c0e09963b45001c23b95a54791776e8a460b7df411efacbd2b763f03a53c0e`.
- Из digest исключены только этот отчёт, `findings.md`, task-файл и task index.
- Digest построен при `LC_ALL=C` из standard `--no-ext-diff --binary
  --full-index` tracked diff от base и отсортированных SHA-256 содержимого
  untracked files; включены 20 tracked и 12 untracked paths.
- Digest независимо воспроизведён reviewer и координирующим агентом.
- Ревьюер не участвовал в реализации: `да`.
- Первичный review и closure passes требовали исправлений; финальный pass не
  обнаружил открытых findings.

## Проверенные инварианты

| № | Результат | Evidence |
|---|---|---|
| 1 | `PASS` | Competing EIP-7702 authorization приводит к пропуску проигравшего tuple и `LostRace`; malformed или отсутствующий tuple отклоняется. |
| 2 | `PASS` | Attacker-controlled delegation не классифицируется как rescue success по одному успешному outer receipt. |
| 3 | `PASS` | Legacy implementation с правильным destination, но неверными runtime/sponsor getters, блокируется attestation до создания signer. |
| 4 | `PASS` | Покрыты fake balance, revert, gas burn, false/no/malformed return, callback/reentrancy и fake `Transfer` hint без реального баланса. |
| 5 | `PASS` | Production source, ABI, artifact и binary проверяются на отсутствие PermitSweeper/permit path; неожиданные artifacts запрещены. |
| 6 | `PASS` | Одновременные native/token candidates сериализуют coordinator state и используют разные sponsor nonces. |
| 7 | `PASS` | Покрыты RPC timeout, wrong chain, stale fork, disconnect gap, duplicate logs и переключение WebSocket/polling. |
| 8 | `PASS` | Расхождения Byzantine provider по finalized block, runtime, receipt, call output и balances блокируют startup или signing. |
| 9 | `PASS` | Canonical rechecks обрабатывают reorg до/после receipt; removed и orphaned logs не становятся готовой работой. |
| 10 | `PASS` | Покрыты queue saturation, checkpoint/incident restore, restart, v1→v3 и точная v2→v3 migration с атомарным rollback при corruption. |
| 11 | `PASS` | Persistent budget нельзя обойти restart; emergency stop не создаёт signatures, broadcasts или новые incidents. |
| 12 | `PASS` | Dry-run graph не создаёт live signer/submission capabilities, не требует private key и выполняет только read simulation. |
| 13 | `PASS` | Typed redaction canaries не выходят в operator surface; `.env`, key/state/generated paths запрещены policy и secret scan. |

Workflows запускаются для pull request и защищаемой основной ветки, используют
только `contents: read`, pinned action SHA и concurrency cancellation. Отдельные
gates покрывают Go format/build/test/race/vet, TypeScript, Foundry
unit/fuzz/invariant/static analysis, local-chain deployment, adversarial Go,
bounded Go fuzz, dependencies, licenses, secrets, repository policy и
reproducible artifacts. Production RPC, deployment credentials и production
network access не используются.

## Findings

| ID | Критичность | Путь | Описание | Требуемое исправление | Статус |
|---|---|---|---|---|---|
| REV-09-001 | High | `internal/rescue/operations.go`, `internal/store/**` | Configured ERC-20 мог получить trusted success, хотя token способен сфабриковать результат | Любой ERC-20 получает только `token-reported`; запретить token trusted-success в current schema | ЗАКРЫТО |
| REV-09-002 | High | `.github/workflows/*.yml`, `test/ci/repository_policy.py` | Line-based repository policy допускала semantic YAML bypass | Структурный parser с запретом duplicate/merge keys, semantic checks и negative fixtures | ЗАКРЫТО |
| REV-09-003 | Medium | `test/contracts/**`, `internal/rescue/session_test.go` | Malicious-token suite неполно закрепляла gas exhaustion, fake balances и fake logs | Добавить gas-burn/lying-balance contracts и pre-signing fake-hint regression | ЗАКРЫТО |
| REV-09-004 | Medium | `internal/rescue/**`, `cmd/guard-daemon/startup_test.go` | Часть signer/dry-run assertions использовала несвязанные synthetic counters | Проверять signer identity и отсутствие production capabilities в реальном startup graph | ЗАКРЫТО |
| REV-09-005 | Medium | `test/ci/compare-artifacts.sh` | Reproducibility check мог собирать mutable dirty worktree | Дважды собирать один commit snapshot через `git archive` | ЗАКРЫТО |
| REV-09-006 | Medium | `.github/workflows/security.yml`, `test/ci/dependency_licenses.py` | Dependency gate мог пропустить отсутствующую или неизвестную лицензию | Fail-closed dependency review и строгая проверка license JSON | ЗАКРЫТО |
| REV-09-007 | Medium | `.github/workflows/*.yml`, `docs/development/ci.md` | Список обязательных GitHub checks был неполным | Зафиксировать точные workflow/job names и требование API evidence | ЗАКРЫТО |
| REV-09-008 | Medium | `test/deploy/integration/local-chain.test.ts` | Anvil harness имел port/startup/shutdown races | Bounded startup retries и TERM→KILL cleanup | ЗАКРЫТО |
| REV-09-009 | Low | `internal/rescue/local_chain_test.go` | Новый local-chain harness дублировал canonical EIP-7702 suite | Удалить дубликат и запускать существующий harness | ЗАКРЫТО |
| REV-09-010 | Low | `Makefile`, `test/ci/*.sh` | Новые CI scripts не входили в ShellCheck gate | Добавить `test/ci/*.sh` в `workflow-lint` | ЗАКРЫТО |
| REV-09-011 | Low | workflows, help и deployment diagnostics | Operator-facing сообщения без необходимости смешивали русский и английский | Привести интерфейс Task 09 к русскоязычному виду | ЗАКРЫТО |
| REV-09-012 | Medium | `internal/store/**`, `docs/operations/limits-alerts.md` | Новый outcome invariant делал валидную schema v2 нечитаемой; первый decoder принимал запрещённый v2 state | Атомарная schema v3 migration с точной legacy validation и rollback tests | ЗАКРЫТО |

## Проверка исправлений

- Closure pass выполнен после каждого изменения production/test файлов.
- Финальный closure pass подтвердил закрытие `REV-09-001`–`REV-09-012` и всех
  обязательных сценариев Task 09.
- Digest обновлён после последнего изменения и независимо воспроизведён.
- `nix develop --command make check` — PASS на финальном implementation diff:
  Go build/test/vet/race, TypeScript checks, 33 Foundry tests, 7 deployment
  tests, artifact verification, `govulncheck`, npm audit и gitleaks.
- `python -B -m unittest discover -s test/ci -p 'test_*.py'` — PASS, 12 tests.
- `CI_BASE_SHA=2613a5503218f1e711ba5c6b5c4dc775f1359085 bash
  test/ci/repository-policy.sh` — PASS.
- `bash test/ci/adversarial-go.sh` — PASS.
- `bash test/ci/go-fuzz.sh` — PASS: две bounded fuzz campaigns по 20 секунд.
- `go test -count=10 ./internal/store` и `go test -race -count=3
  ./internal/store` — PASS после финального migration fix.
- `bash test/ci/contracts-test.sh` — PASS: unit, fuzz и invariant suites
  обнаружены до запуска, 33 tests без failures.
- Slither `0.11.6` с solc `0.8.36` и `--fail-high` — PASS.
- Dependency/license negative fixtures, `actionlint`, ShellCheck и `git diff
  --check` — PASS.
- Две чистые копии точного candidate snapshot реконструированы из base archive,
  standard full-index patch и untracked files. В обеих прошли `npm ci`, contract
  build/artifact verification и deterministic Go build; canonical artifact,
  generated artifact и binary совпали byte-for-byte.

## Остаточные риски

| ID | Критичность | Обоснование | Владелец | Срок пересмотра |
|---|---|---|---|---|
| Нет | — | Нет в рамках repository implementation Task 09 | — | — |

## Внешнее release evidence

| Проверка | Владелец | Срок | Статус |
|---|---|---|---|
| Подтвердить через GitHub API фактический ruleset/branch protection, required checks и создание Dependabot PR | Оператор репозитория | До Task 11 | ОЖИДАЕТСЯ; release заблокирован |

Это внешнее доказательство не является accepted residual risk. Критерии Task 09
явно разрешают завершить repository implementation без него, но запрещают
release до операторской проверки.

## Решение

`ПРОЙДЕНО`: открытые Critical, High, Medium и Low findings отсутствуют; digest
соответствует последнему implementation/test/doc diff, обязательные сценарии и
repository acceptance criteria Task 09 подтверждены. Статус `DONE`, отчёт и
implementation входят в один atomic task commit.
