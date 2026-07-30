# Отчёт независимого ревью Task 02

Задача: `Task 02`

Статус ревью: `ПРОЙДЕНО`

## Проверенный снимок

- Base commit: `ca6ecce`
- Проверенные пути: `.gitignore`, `README.md`, `guard-daemon-HELP_RU.txt`,
  удалённый `main.go`, `cmd/guard-daemon/**`, `internal/**`,
  `docs/architecture/go-daemon.md`.
- SHA-256 digest diff проверенных production/test/documentation файлов:
  `705476e0b3fe69d21dd11352eb5ddfca27fb07596cdd5a869160ad1d1fb53f26`.
- Из digest исключены только этот отчёт, status-поля task index/task-файла и
  служебное обновление путей, evidence и статусов матрицы findings.
- Ревьюер не участвовал в реализации: `да`.

## Проверенные инварианты

- Единственная production entry point находится в `cmd/guard-daemon`; startup,
  dependency wiring и shutdown отделены от watcher и rescue.
- Watcher не получает signer или broadcaster, а rescue не управляет
  subscription lifecycle.
- RPC client неизменяем в пределах reconnect-generation; generation отменяет и
  дожидается watcher и consumer до закрытия client.
- Source authorization nonce читается последним RPC-read перед подписью token
  authorization; target, gas limits и fee caps соответствуют сохраняемому пути.
- Signer identity проверяется до signing; fake signer и broadcaster фиксируют
  attempted operation без приватного ключа.
- Initial WebSocket failure допускает HTTP/polling fallback, а разрыв активной
  subscription завершает generation и создаёт новый client.
- Retryable и ambiguous candidate не подтверждается: `Nack` сохраняет incident,
  переносит работу в delayed heap с backoff и не блокирует новые candidates.
- Stable candidate ID не зависит от process generation; duplicate `Put`, replay,
  incident persistence, `Ack` и crash boundaries проверены тестами.
- Startup и fee reads имеют ограниченные сроки; hung RPC отменяется и освобождает
  operation lock.
- Go production surface не содержит Permit ABI, config, state или fallback.
- Typed observability не принимает raw error, RPC URL, signature или signed
  transaction; production console дополнительно отбрасывает неразрешённые поля.
- Package graph не содержит import cycles и mutable global runtime state.

## Findings

| ID | Критичность | Путь/строка | Описание | Требуемое исправление | Статус |
|---|---|---|---|---|---|
| REV-02-001 | High | `cmd/guard-daemon/daemon.go`, `internal/store` | Ошибка обработки подтверждала и теряла candidate | Сохранять incident и replayable work до успешного `Ack` | ЗАКРЫТО |
| REV-02-002 | High | `internal/watcher/service.go` | Разрыв активной subscription переходил в polling на повреждённом client | Завершать generation и переподключаться | ЗАКРЫТО |
| REV-02-003 | High | `cmd/guard-daemon/handoff.go` | Очередь и tombstones росли без границы, `Next` сканировал историю | Добавить backpressure, O(1) ready queue и ограниченное окно tombstones | ЗАКРЫТО |
| REV-02-004 | High | `internal/rescue` | При переносе потеряны startup и fee RPC deadlines | Вернуть настраиваемые deadlines и hang tests | ЗАКРЫТО |
| REV-02-005 | Medium | `internal/domain/model.go`, `internal/watcher/service.go` | Periodic ID зависел от process generation | Использовать process-independent observation slot | ЗАКРЫТО |
| REV-02-006 | Medium | `internal/store/interfaces_test.go` | Crash test повторно использовал тот же heap state и расходился по `CreatedAt` | Reopen снимка и явная incident idempotence | ЗАКРЫТО |
| REV-02-007 | Medium | `internal/budget/interfaces.go` | Budget request не связывал сеть, sponsor, incident и attempt | Зафиксировать immutable value-typed reservation request | ЗАКРЫТО |
| REV-02-008 | Medium | `README.md`, `guard-daemon-HELP_RU.txt` | Документация заявляла загружаемый Permit ABI/fallback | Удалить Go claims и отделить последующее удаление artifacts | ЗАКРЫТО |
| REV-02-009 | High | `cmd/guard-daemon/daemon.go`, `cmd/guard-daemon/handoff.go` | Poison candidate мог блокировать ready queue или постоянно завершать generation | Ввести per-candidate delayed `Nack` без остановки watcher | ЗАКРЫТО |

Critical findings отсутствовали. После финального closure pass новых findings не
обнаружено.

## Проверка исправлений

- Closure pass выполнен после каждого изменения production/test файлов; reviewer
  каждый раз повторно проверял полный staged diff от base commit.
- Digest обновлён после последнего изменения и подтверждён reviewer.
- `go test -mod=readonly -count=1 ./...`: успешно.
- `CGO_ENABLED=1 go test -race -mod=readonly -count=1 ./...`: успешно.
- `go test -mod=readonly -count=20 ./cmd/guard-daemon`: успешно.
- `go vet -mod=readonly ./...`: успешно.
- `CGO_ENABLED=0 go build -mod=readonly ./...`: успешно.
- `make go-ci`: успешно.
- `go mod verify` и `go mod tidy -diff`: успешно.
- `make secret-scan`: утечки в рабочем дереве и Git history не обнаружены.
- Targeted tests ABI packing, fee caps, address derivation, delegation parsing,
  network loop, disconnect/reconnect, handoff crash/replay, backpressure,
  poison-candidate scheduling и fake signer/broadcaster: успешно.
- `git diff --cached --check ca6ecce`: успешно.

Команды проверки выполнены через локальное закреплённое окружение
`nix develop`; daemon не запускался, реальные RPC и ключи не использовались.

## Остаточные риски

| ID | Критичность | Обоснование | Владелец | Срок пересмотра |
|---|---|---|---|---|
| Нет | — | Нет в рамках задачи | — | — |

Persistent queue/checkpoint, checkpoint-aware tombstone GC, backfill/reorg,
durable retry outcomes, nonce lease, RPC quorum, dry run и global budget остаются
явным scope Tasks 04-08 и не считаются закрытыми этим отчётом.

## Решение

`ПРОЙДЕНО`: все findings закрыты, closure pass выполнен, digest соответствует
последнему проверенному diff, обязательные build/test/race/vet/security checks
успешны.
