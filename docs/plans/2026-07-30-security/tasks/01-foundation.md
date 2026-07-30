# Task 01: Воспроизводимая основа и безопасность репозитория

Статус: `PENDING`

Зависимости: отсутствуют.

## Цель

Создать воспроизводимую и безопасную основу разработки до изменения runtime behavior. Зафиксировать toolchains и dependencies, исключить случайный commit секретов, добавить единые локальные и CI-команды проверки.

## Риски

- `.env`, ключи, binaries и deployment artifacts можно случайно закоммитить.
- Go и Node dependencies устарели либо не зафиксированы.
- TypeScript deployment scripts не имеют `package.json` и lock-файла.
- Разные версии `solc` создают различающийся bytecode.
- Vulnerability scan уже выявлял известные advisories в старых версиях Go/geth.

## Границы задачи

- `.gitignore` и правила secret hygiene.
- `flake.nix`/`flake.lock` либо эквивалентный pinned Nix environment.
- `package.json`, lock-файл и TypeScript configuration.
- `go.mod`/`go.sum` и безопасное обновление dependencies.
- Базовые команды format, build, test, lint, vulnerability scan и secret scan.
- GitHub Actions без production secrets и действий в реальных сетях.
- Dependabot для Go modules, npm и GitHub Actions.

Не менять transaction, watcher и contract behavior.

## Обязательные изменения

1. Игнорировать `.env`, key files, local state, binaries, coverage, compiler output и временные deployment manifests; оставить `.env.example` tracked.
2. Добавить проверку tracked files и history-facing diff на типичные secret patterns. Любой пример использует только явные placeholders.
3. Выполнить repository-wide scan на incident-specific identifiers без публикации найденных значений. Удаление таких комментариев в Go принадлежит Task 02, в Solidity — Task 03, в документации — Task 10. При обнаружении настоящего секрета остановиться и передать оператору решение о rotation/history rewrite.
4. Зафиксировать поддерживаемую patched-версию Go и совместимые версии `go-ethereum`, `uint256`, `godotenv`.
5. Обновить `go-ethereum` минимум до версии, закрывающей актуальные advisories, и подтвердить совместимость EIP-7702 API.
6. Зафиксировать Node, `tsx`, TypeScript, `ethers`, `solc` и test tooling в lock-файле.
7. Зафиксировать точную версию `solc`, optimizer settings, `evmVersion` и metadata policy для reproducible bytecode.
8. Добавить безопасные scripts/targets: format, lint, unit tests, contract compile/tests, Go build/vet/race, vulnerability scan и secret scan.
9. Ни один default script не должен отправлять транзакции или обращаться к mainnet.
10. Добавить GitHub Actions workflows для formatting check, Go build/test/race/vet, contract build/tests, TypeScript typecheck/lint, vulnerability scan и secret scan.
11. Настроить dependency caching без сохранения secrets и generated `.env`.
12. Добавить `.github/dependabot.yml` для `gomod`, `npm` и `github-actions` с разумным расписанием, группировкой совместимых patch/minor updates и запретом автоматического merge security-critical major updates.
13. Зафиксировать обязательные CI checks и рекомендованные branch protection rules в русскоязычной developer-инструкции.
14. Ни один GitHub Actions workflow не должен иметь production environment, wallet key, write permission без необходимости или возможность отправить live transaction.
15. Добавить краткую русскоязычную developer-инструкцию только для реально работающих команд.

## Параллельные направления

После согласования версий coordinator может запустить три subagent:

- **Nix/Node:** только `/flake.nix`, `/flake.lock`, `/package.json`, `/package-lock.json`, `/tsconfig.json`.
- **Go dependencies:** только `/go.mod`, `/go.sum` и отчёт совместимости в `/docs/development/dependencies.md`.
- **Безопасность репозитория и CI:** только `/.gitignore`, `/.github/workflows/**`, `/.github/dependabot.yml`, конфигурация secret scan и `/docs/development/ci.md`.

Subagents не редактируют файлы чужого ownership. Coordinator один интегрирует developer documentation.

## Критерии приёмки

- Fresh clone входит в pinned Nix environment одной задокументированной командой.
- Go daemon собирается с `CGO_ENABLED=0` и `-mod=readonly`.
- Solidity contracts воспроизводимо компилируются pinned compiler.
- TypeScript scripts type-check без глобально установленных packages.
- `go mod verify`, `go vet`, vulnerability scan и secret scan выполняются из repository scripts.
- Нет Critical/High vulnerability с достижимым кодом. Исключения запрещены. Недостижимую advisory можно переклассифицировать только с доказательством reachability, независимым ревью, владельцем и сроком повторной проверки.
- `.env` и типичные secret/key artifacts игнорируются и блокируются scan.
- GitHub Actions не выполняет deployment и не использует реальные RPC/keys.
- Dependabot валидно настроен для Go, npm и GitHub Actions; его pull requests проходят тот же обязательный CI.
- Repository содержит проверяемое описание обязательных status checks и запрещает auto-merge без успешных тестов и ревью.
- Фактическое включение GitHub branch protection/ruleset является operator-owned release gate: его отсутствие не скрывается, а блокирует Task 11, если нет API evidence.
- Все добавленные документы на русском языке.

## Проверка

Выполнить через repository-provided commands:

```text
nix develop
go mod verify
go build -mod=readonly ./...
go vet ./...
govulncheck ./...
npm ci
npm run typecheck
npm run contracts:build
<secret-scan command>
```

Конкретные wrapper commands могут отличаться, но должны покрывать тот же набор.

## Независимое ревью

Ревьюер проверяет lock-файлы, отсутствие сетевых побочных эффектов у scripts, secret patterns, advisories, настройки воспроизводимой компиляции, permissions GitHub Actions, политику Dependabot и совместимость EIP-7702 API после обновления geth.

Все findings исправляются до завершения. После dependency changes повторить build, vet, typecheck и vulnerability scan.

## Завершение и commit

После ревью сохранить отчёт в `../reviews/01-foundation.md`, выполнить closure pass и изменить статус на `DONE` здесь и в `../tasks.md`.

Рекомендуемый subject:

```text
build(security): зафиксировать toolchains и защитить репозиторий от утечек
```

Commit body обязан подробно перечислить версии, dependency rationale, secret controls, CI commands, результаты scan и остаточные риски.
