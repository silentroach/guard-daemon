# Отчёт независимого ревью Task 10

Задача: `Task 10`

Статус ревью: `ЧЕРНОВИК`

## Проверенный снимок

- Base commit: `c14b723d145c96e31f3afd12d02a1d3ea3888bd6`.
- Проверенный implementation commit:
  `9f752cbe63fafa9706ac1de199f9681af738aec5`.
- Проверенные пути: весь implementation/test/documentation diff Task 10,
  включая CLI/config, deployment tooling, release metadata, systemd packaging,
  operations/migration/release runbooks и связанные CI gates.
- SHA-256 digest diff проверенных production/test/doc файлов:
  `4f880b8e588c03de2e2dddd30765f266c1afa74f5e541465cc862223a5fd04d3`.
- Из digest исключены только этот отчёт, task-файл Task 10 и task index.
- Digest построен при `LC_ALL=C` из `--no-ext-diff --binary --full-index`
  tracked diff от base; включены 58 tracked paths, untracked implementation
  files отсутствуют.
- Ревьюеры не участвовали в реализации финальных изменений: `да`.
- Первичные passes и exact-commit проверки требовали исправлений; два финальных
  независимых closure pass на implementation commit не обнаружили открытых
  Critical, High, Medium или Low findings.

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
| REV-10-015 | Medium | `scripts/build-release-candidate.sh` | Неверный разбор `go version`, совпадающие npm config paths и read-only Go cache блокировали exact build | Исправить parsing, разделить npm config paths и возвращать write bit перед cleanup | ЗАКРЫТО |
| REV-10-016 | Medium | `scripts/build-release-candidate.sh`, `docs/release/**` | Случайный путь Go module cache попадал в assembly metadata и менял binary между clean builds | Эксклюзивный стабильный cache path, `-s -w` и документированный canonical recipe | ЗАКРЫТО |
| REV-10-017 | High | `scripts/build-release-candidate.sh`, `test/ci/compare-artifacts.sh` | Унаследованные npm config и Node hooks позволяли пропустить verifier или выполнить посторонний код одинаково в обеих сборках | `env -i` allowlist, отсутствующие fixed config paths и hostile outer-env regression | ЗАКРЫТО |
| REV-10-018 | High | `scripts/build-release-candidate.sh`, `test/ci/compare-artifacts.sh` | `GOFIPS140`, `GOCACHEPROG`, `GOWORK` и другие Go inputs могли менять candidate и давать common-mode reproducibility pass | Полный Go subprocess allowlist с canonical values и различающиеся hostile environments | ЗАКРЫТО |
| REV-10-019 | Medium | `scripts/build-release-candidate.sh` | Race между target absence check и `mv` мог сообщить успех для чужого каталога | Сравнивать device/inode staging и опубликованного каталога до success | ЗАКРЫТО |
| REV-10-020 | Low | `test/ci/compare-artifacts.sh` | Ошибка `strings` в process substitution не передавалась циклу и давала fail-open проверку удалённого permit surface | Сначала сохранить output с явной проверкой exit status | ЗАКРЫТО |
| REV-10-021 | Medium | `scripts/build-release-candidate.sh`, `test/ci/compare-artifacts.sh` | Exported shell function `env` могла перехватить `env -i` до очистки окружения | Проверенный абсолютный `/usr/bin/env` и adversarial exported-function regression | ЗАКРЫТО |
| REV-10-022 | Medium | `scripts/build-release-candidate.sh`, `docs/release/**` | Nix-patched Go с той же строкой версии встраивал `/nix/store` runtime defaults и создавал другой binary | Требовать upstream Go 1.26.5 и fail-closed отклонять host-specific/toolchain paths | ЗАКРЫТО |
| REV-10-023 | Medium | `scripts/build-release-candidate.sh`, `test/ci/compare-artifacts.sh` | Exported `strings` function могла скрыть path scan; короткие roots и sibling prefixes создавали false pass/false reject | Абсолютный `/usr/bin/strings`, hostile nonzero stub, suffix/descendant matcher и запрет ambiguous roots | ЗАКРЫТО |
| REV-10-024 | Medium | `scripts/build-release-candidate.sh`, `docs/release/artifacts.md` | Запрет symlink ложно отклонял Ubuntu `/usr/bin/strings`, а exact directory в конце printable record не обнаруживался | Разрешить package-managed executable symlink и проверять directory suffix отдельно | ЗАКРЫТО |

## Проверка исправлений

- Два независимых финальных closure pass завершились решением `ПРОЙДЕНО` для
  implementation/test/doc diff commit
  `9f752cbe63fafa9706ac1de199f9681af738aec5`.
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
- Полный `make security-validation` с upstream Go 1.26.5 и pinned
  policy/Slither/solc tools — PASS, включая exact implementation commit: два
  clean checkout, конфликтующие hostile Go/npm/Node/Python environments,
  exported `env`/`strings` functions, побайтовое сравнение всех candidate files,
  metadata verification и два gitleaks scan.
- Два новых независимых `--no-local` clone без Nix как обязательного условия
  собраны upstream Go 1.26.5, Node.js 24.18.1, npm 11.16.0 и Python 3.14.6 —
  PASS; каталоги candidate и `SHA256SUMS` совпали побайтово с local exact check.
- Redacted evidence без локальных путей:

```text
Commit снимка: 9f752cbe63fafa9706ac1de199f9681af738aec5
7afb79969d3d3b180004239d0352593c9f6b89f00e8ee160824f326f3de106b9  LICENSE
96c6c0d358e44980814b40553358c53ce5231b7c2ce4ef9bf99add3b9a96a4af  RescuerV2.json
f4f25e757d31f335244fd8532b858712d09f5e6f59ee2b277ebd2671be62881d  guard-daemon-linux-amd64
c61d37d0f628d1b9f9380ec8a8576319a99ac7bea9a660c72a1f8427563f852d  guard-daemon-source.tar
f228b23b73b3148116b9b35825482b5340cb40d86c0d39fc07055871d272a8a7  guard-daemon.cdx.json
542bba39d6b99f1c7c652ba99c6989b1267dc7d16b3b689ad9fb351e17659b9b  guard-daemon.intoto.jsonl
a6aaf1f9b9ec722e66e447c629efbcb09ea44b82518877d83271e5784e304c25  release-candidate.json
a16dec6b9a809d9768815dda2e831e792bccdb10d8d583431716f94fb83bbc98  rescuer-manifest.schema.json
```

## Остаточные риски

| ID | Критичность | Обоснование | Владелец | Срок пересмотра |
|---|---|---|---|---|
| Нет | — | Открытых repository implementation findings нет | — | — |

Critical и High residual risks не приняты.

## Внешнее evidence и blockers

| Проверка | Владелец | Срок | Статус |
|---|---|---|---|
| Выполнить локальную adversarial exact-commit проверку двух clean checkout с побайтовым сравнением candidate | Координирующий агент | До `DONE` Task 10 | ВЫПОЛНЕНО на `9f752cbe63fafa9706ac1de199f9681af738aec5` |
| Повторить candidate build в двух новых независимых clone с native pinned tools без Nix как обязательного условия | Координирующий агент | До `DONE` Task 10 | ВЫПОЛНЕНО на `9f752cbe63fafa9706ac1de199f9681af738aec5` |
| Выполнить на целевом Linux/systemd install/start/health/stop/backup/restore/update/reboot/rollback, `systemd-analyze verify` и core-dump policy | Оператор эксплуатации | До `DONE` Task 10 | ОЖИДАЕТСЯ |
| Подтвердить GitHub ruleset, required checks и Dependabot | Оператор репозитория | До Task 11 | ОЖИДАЕТСЯ |
| Аутентифицировать и подписать candidate допустимой keyless identity либо оператором вне агента | Оператор выпуска | После решения Task 11 | НЕ ВЫПОЛНЯЛОСЬ |

Mainnet deployment, tag, push и GitHub Release не выполнялись.

## Решение

`ЧЕРНОВИК`: repository implementation, независимый code/doc closure review,
локальная adversarial exact-commit reproducibility и две независимые native-clone
сборки пройдены без открытых findings. До реальной Linux/systemd rehearsal Task
10 остаётся `IN PROGRESS`, а отчёт нельзя перевести в `ПРОЙДЕНО`.
