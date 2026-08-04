# Отчёт независимого ревью Task 10

Задача: `Task 10`

Статус ревью: `ЧЕРНОВИК`

## Проверенный снимок

- Base commit: `c14b723d145c96e31f3afd12d02a1d3ea3888bd6`.
- Проверенные пути: весь implementation/test/documentation diff Task 10,
  включая CLI/config, deployment tooling, release metadata, systemd packaging,
  operations/migration/release runbooks и связанные CI gates.
- SHA-256 digest diff проверенных production/test/doc файлов:
  `4bfadce34f7dd1c0ec1e8fe92633f813a1f8e9f89e3c6740c6acd71e1d1b6960`.
- Из digest исключены только этот отчёт, task-файл Task 10 и task index.
- Digest построен при `LC_ALL=C` из `--no-ext-diff --binary --full-index`
  tracked diff от base и отсортированных SHA-256 содержимого untracked files;
  включены 32 tracked и 26 untracked paths.
- Ревьюеры не участвовали в реализации финальных изменений: `да`.
- Первичные passes требовали исправлений; два финальных независимых closure pass
  не обнаружили открытых Critical, High, Medium или Low findings.

## Проверенные инварианты

| № | Результат | Evidence |
|---|---|---|
| 1 | `PASS` | Публичная документация русскоязычна, ссылки и документированные config-поля автоматически проверяются. |
| 2 | `PASS` | Quick start и CLI без `--broadcast` не читают ключи, RPC отправки или release candidate и не могут отправить транзакцию. |
| 3 | `PASS` | Production deployment выполняет только оператор из аутентифицированных `root:root 0700` candidate/tooling; mutable checkout и `node_modules` запрещены. |
| 4 | `PASS` | Secret-bearing deployment child получает allowlist environment, absolute Node, `RLIMIT_CORE=0` и non-dumpable policy; credentials отсутствуют в argv. |
| 5 | `PASS` | Recovery record с точной подписанной транзакцией durable публикуется до broadcast в фиксированном root-only registry. |
| 6 | `PASS` | Production manifest публикуется только в фиксированном root-only staging; path identity и ownership перепроверяются. |
| 7 | `PASS` | Daemon работает как `guard-daemon` без capabilities; operator recovery/manifest paths скрыты, а restore marker нельзя удалить service UID. |
| 8 | `PASS` | Restore marker durable создаётся до изменения state; восстановленное состояние допускается только в dry-run/emergency и никогда не возвращается в signing. |
| 9 | `PASS` | Update staging не меняет unit/current; production service persistently disabled и masked до backup, emergency configuration и контролируемого переключения. |
| 10 | `PASS` | Release builder требует exact clean commit, отклоняет hidden Git attributes/index flags и создаёт deterministic `linux/amd64`, CycloneDX 1.6, provenance и checksums. |
| 11 | `PASS` | SBOM сохраняет выбранный MVS graph, replacements и транзитивные Go/npm зависимости, включая служебные Go 1.26 toolchain edges без ложных компонентов. |
| 12 | `PASS` | Release/install scripts не выполняют signing, tag, push, GitHub Release, production deployment или mainnet action. |

## Findings

| ID | Критичность | Путь | Описание | Требуемое исправление | Статус |
|---|---|---|---|---|---|
| REV-10-001 | High | `internal/config/**`, `internal/buildinfo/**`, `cmd/guard-daemon/**` | Binary release identity ошибочно могла подменять provenance неизменённого deployment manifest | Разделить binary identity и `RESCUER_RELEASE_COMMIT_<N>`, встроить exact binary commit | ЗАКРЫТО |
| REV-10-002 | High | `scripts/build-release-candidate.sh`, `scripts/deployment.ts` | Dirty checkout, local attributes и hidden index flags могли нарушить связь candidate с commit | Exact `HEAD`, clean status, запрет `info/attributes`, `assume-unchanged` и `skip-worktree` | ЗАКРЫТО |
| REV-10-003 | High | `packaging/systemd/**`, `cmd/guard-daemon/daemon.go` | Service UID мог удалить restore fence или подменить recovery registry внутри writable state | Вынести operator paths под `root:root`, скрыть registry/manifest dirs и проверять фиксированный marker до signer | ЗАКРЫТО |
| REV-10-004 | High | `docs/operations/runbook.md`, `docs/deployment/operator-activation.md` | Candidate binary, verifier, packaging или mutable npm tooling могли исполняться от root до аутентификации | Обязательный внешний digest `SHA256SUMS`, root-owned staging и tooling только из аутентифицированного source tar | ЗАКРЫТО |
| REV-10-005 | High | `scripts/deployRescuerV2.ts`, `docs/deployment/**` | Secrets могли попасть в mutable TypeScript/npm code или credential-bearing RPC argv | Fixed immutable tooling, secret-free preflight, direct absolute Node и новый allowlist environment | ЗАКРЫТО |
| REV-10-006 | Medium | `docs/operations/backup-restore.md` | Restore передавал registry daemon UID и имел crash window до marker | Исключить operator tree из backup/chown и fsync-публиковать внешний marker до state mutation | ЗАКРЫТО |
| REV-10-007 | Medium | `scripts/release_metadata.py`, `test/ci/test_release_metadata.py` | SBOM ошибочно обрабатывал MVS и реальный `go -> toolchain` edge Go 1.26 | Учитывать только selected module versions и игнорировать служебные nodes до known-module checks | ЗАКРЫТО |
| REV-10-008 | Medium | `scripts/deployRescuerV2.ts`, `packaging/systemd/**` | Manifest/recovery pathname TOCTOU допускал writable ancestor или замену directory | Fixed canonical roots, exact modes/owners, device/inode rechecks и exclusive durable writes | ЗАКРЫТО |
| REV-10-009 | Medium | `docs/deployment/operator-activation.md` | Deployment child мог наследовать preload/Node hooks или записать key в core dump | Absolute Node, manager environment allowlist, `RLIMIT_CORE=0`, `PR_SET_DUMPABLE=0`, no crash collector | ЗАКРЫТО |
| REV-10-010 | Medium | `docs/operations/runbook.md` | Update мог переключить current до backup или запуститься live после reboot во время maintenance | `ACTIVATE_RELEASE=false`, persistent disable/mask и delayed autostart после emergency health-check | ЗАКРЫТО |
| REV-10-011 | Medium | `scripts/build-release-candidate.sh`, `test/ci/compare-artifacts.sh` | Existing release directory мог быть перезаписан, а recipe/digest не полностью отражали build inputs | Unique staging, no-replace publication, exact `GOAMD64`, ldflags и builder digest | ЗАКРЫТО |
| REV-10-012 | Low | operations/deployment runbooks | Negative grep/find checks и npm symlink audit могли завершаться fail-open или ложно отклонять valid tooling | Явно проверять exit status, исключить symlink mode и проверить containment каждого target | ЗАКРЫТО |
| REV-10-013 | Low | release/process documentation | Rehearsal ошибочно описывался как полностью no-sign/no-send | Точно указать local test-key signing и broadcast только в одноразовый Anvil | ЗАКРЫТО |
| REV-10-014 | Low | `internal/watcher/service_test.go` | Fake block и logs публиковались раздельно, создавая flaky gap test | Публиковать block+logs под одним lock; regression прошёл `-count=100` | ЗАКРЫТО |

## Проверка исправлений

- Два независимых финальных closure pass завершились решением `ПРОЙДЕНО` для
  текущего implementation/test/doc diff.
- `nix develop -c make format-check` — PASS: gofmt, Prettier, Forge format,
  actionlint и ShellCheck.
- `nix develop -c make go-ci` — PASS: module verification, build, все Go tests,
  vet и race.
- `nix develop -c make node-ci` — PASS: TypeScript, ESLint, 33 Foundry tests,
  artifact verification и deployment `19/19` с локальным Anvil.
- Repository policy — PASS, `37/37`; documentation — `5/5`; operations — `9/9`;
  release metadata — `11/11`.
- `operations-rehearsal` — PASS: targeted Go startup tests и deployment `19/19`.
- `govulncheck v1.1.4`, `npm audit`, gitleaks working tree/history — PASS.
- Slither `0.11.6` с solc `0.8.36` и `--fail-high` — PASS.
- Adversarial Go suite и две bounded fuzz campaigns по 20 секунд — PASS.
- Реальный `go list -m -json all` + `go mod graph` создаёт SBOM — PASS.
- Isolated `npm ci --ignore-scripts` и `artifacts:verify` для deployment tooling — PASS.
- Watcher gap regression `-count=100`, Bash syntax operator blocks и `git diff
  --check` — PASS.
- `make reproducibility-check` не запускался: текущий Task 10 diff ещё не является
  immutable commit, а builder намеренно отклоняет такой вход.

## Остаточные риски

| ID | Критичность | Обоснование | Владелец | Срок пересмотра |
|---|---|---|---|---|
| Нет | — | Открытых repository implementation findings нет | — | — |

Critical и High residual risks не приняты.

## Внешнее evidence и blockers

| Проверка | Владелец | Срок | Статус |
|---|---|---|---|
| Создать immutable Task 10 commit и выполнить две clean exact-commit сборки с побайтовым сравнением candidate | Координирующий агент и оператор | До `DONE` Task 10 | ОЖИДАЕТСЯ |
| Выполнить на целевом Linux/systemd install/start/health/stop/backup/restore/update/reboot/rollback, `systemd-analyze verify` и core-dump policy | Оператор эксплуатации | До `DONE` Task 10 | ОЖИДАЕТСЯ |
| Подтвердить GitHub ruleset, required checks и Dependabot | Оператор репозитория | До Task 11 | ОЖИДАЕТСЯ |
| Аутентифицировать и подписать candidate допустимой keyless identity либо оператором вне агента | Оператор выпуска | После решения Task 11 | НЕ ВЫПОЛНЯЛОСЬ |

Mainnet deployment, tag, push и GitHub Release не выполнялись.

## Решение

`ЧЕРНОВИК`: repository implementation и независимый code/doc closure review
пройдены без открытых findings, но критерии Task 10 требуют immutable commit,
exact-commit reproducibility и реальную Linux/systemd rehearsal. До получения
этих evidence Task 10 остаётся `IN PROGRESS`, а отчёт нельзя перевести в
`ПРОЙДЕНО`.
