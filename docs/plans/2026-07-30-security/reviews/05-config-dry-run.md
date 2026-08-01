# Отчёт независимого ревью Task 05

Задача: `Task 05`

Статус ревью: `ПРОЙДЕНО`

## Проверенный снимок

- Base commit: `86f7d7df55a6cf71e2121882c698f4fc884e2463`.
- Проверенные пути: typed config и tests, startup wiring, dry-run session и
  guards, RPC capability facades, token policy в watcher/session, публичные
  config-примеры и точечные исправления README/справки.
- SHA-256 digest diff проверенных production/test/doc файлов:
  `39eec1a4f8d183228b5fe83b341235521e2d7c20da4f09844f18686dce7c2f8f`.
- Из digest исключены только этот отчёт, `findings.md`, task-файл и task index.
- Digest построен из binary/full-index tracked diff от base и отсортированных
  SHA-256 содержимого untracked files; включены 30 путей.
- Digest после последнего изменения независимо воспроизведён reviewer и
  координирующим агентом.
- Ревьюер не участвовал в реализации: `да`.

## Проверенные инварианты

- `DRY_RUN=true` является безопасным default, не запрашивает и не сохраняет
  private keys, не создаёт production signer, submission client или attestation
  dependency.
- Read/subscription RPC выдаются только через facade-типы, dynamic method set
  которых не содержит `SendTransaction`; dry branch выполняется раньше любой
  injected session factory.
- Обычный dry-run путь выполняет только bounded `ChainID` и `EstimateGas` и не
  вызывает signing/broadcast guards. Прямой вызов guard возвращает typed error
  и учитывает попытку, не создавая подпись и не отправляя транзакцию.
- Локальная config validation отклоняет malformed, zero и совпадающие role
  addresses, пустые/неизвестные/повторные сети, неполные и зависимые RPC,
  отсутствующий manifest и удалённые или зарезервированные phantom fields до
  network connection и signer construction.
- Все trusted manifests загружаются, затем все включённые сети проходят
  finalized quorum attestation, и только после этого единожды создаются signers.
  Их адреса сверяются с source/sponsor до первой подписи.
- Два HTTP и два WebSocket override имеют разные endpoint identity и trust
  domain и участвуют в startup/fallback. Broadcast URL задаётся отдельно,
  отличается от read HTTP и доступен только в live mode.
- `known-only` и `allowlist` ограничивают watcher query, log-to-candidate путь и
  session/replay input. Неизвестный token разрешён только после явного `all`.
- Строгий `.env` принимает только `NAME=value`; альтернативный синтаксис не
  обходит фильтрацию private keys и зарезервированных полей. Live keys
  принимаются только из окружения процесса.
- Ошибки и форматирование не раскрывают ключи, RPC credentials, URL, manifest
  paths, authorization или raw RPC cause.

## Findings

| ID | Критичность | Путь/строка | Описание | Требуемое исправление | Статус |
|---|---|---|---|---|---|
| REV-05-001 | Critical | `internal/rpc/dialer.go`, `cmd/guard-daemon/daemon.go` | Первичный read client сохранял dynamic broadcast capability, а injected factory мог предшествовать dry branch | Ввести read-only facade-типы и сделать dry branch структурно приоритетной | ЗАКРЫТ |
| REV-05-002 | High | `cmd/guard-daemon/daemon.go`, session paths | Token policy ограничивала watcher, но не injected/durable candidate replay | Обернуть все sessions единым immutable token policy guard до RPC/signing | ЗАКРЫТ |
| REV-05-003 | Medium | `internal/config/schema.go` | Первичная загрузка `.env` materialized private key values до выбора режима | Пропускать secret fields до parser, использовать process-only live keys и строгую grammar | ЗАКРЫТ |
| REV-05-004 | Medium | `internal/config/networks.go`, `cmd/guard-daemon/daemon.go` | Второй WS override не использовался, broadcast/read separation была неполной | Проверять независимость HTTP/WS и задействовать оба provider fallback; отделить broadcast | ЗАКРЫТ |
| REV-05-005 | Medium | `internal/config/validate.go`, `.env` loader | Конечный список legacy names и альтернативная dotenv grammar позволяли молча игнорировать reserved prefixes | Проверять зарезервированные namespaces и отклонять альтернативную grammar до разбора | ЗАКРЫТ |
| REV-05-006 | Medium | `README.md`, `guard-daemon-HELP_RU.txt` | Публичные sections содержали phantom config и безусловные claims об unknown tokens/dry run | Синхронизировать Task 05 sections с mode, manifest и token policy | ЗАКРЫТ |
| REV-05-007 | Medium | startup/config/dry-run tests | Первичное evidence слабо связывало production composition | Проверить facade capabilities, provider wiring, startup order и integrated dry path; использовать исчерпывающие Task 04 tests реального attestation API | ЗАКРЫТ |

Новых findings в финальном closure pass нет.

## Проверка исправлений

- Closure pass выполнен после каждого изменения production/test/doc файлов.
- `nix develop -c make go-ci`: успешно; module verification, build, unit tests,
  `go vet` и полный race suite прошли.
- `nix develop -c go test -race -mod=readonly ./...`: успешно.
- `nix develop -c go vet -mod=readonly ./...`: успешно.
- `CGO_ENABLED=0 go build -mod=readonly ./...`: успешно.
- `nix develop -c npm run format:check`: успешно.
- `git diff --check 86f7d7df55a6cf71e2121882c698f4fc884e2463`:
  успешно.
- `nix develop -c make secret-scan`: успешно; directory и Git-history scans не
  обнаружили утечек.
- Полный `make format-check` локально не завершён только из-за отсутствующего
  `forge`; изменённых Solidity-файлов нет, Go formatting и npm format gate
  проверены отдельно.
- Mainnet actions, реальные подписи и отправка транзакций не выполнялись.

## Остаточные риски

| ID | Критичность | Обоснование | Владелец | Срок пересмотра |
|---|---|---|---|---|
| Нет | — | Нет в рамках Task 05 | — | — |

Надёжный RPC lifecycle/checkpoints, transaction retry/nonce/lease и persistent
budget enforcement остаются dependencies Tasks 06-08 и не считаются принятыми
остаточными рисками Task 05. До завершения release-плана production use запрещён.

## Решение

`ПРОЙДЕНО`. Critical и High findings закрыты, все Medium findings исправлены,
актуальный digest воспроизведён независимо и критерии Task 05 подтверждены.
Статус задачи меняется на `DONE` только после разрешённого atomic task commit.
