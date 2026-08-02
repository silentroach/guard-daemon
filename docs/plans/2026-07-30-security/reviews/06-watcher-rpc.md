# Отчёт независимого ревью Task 06

Задача: `Task 06`

Статус ревью: `ПРОЙДЕНО`

## Проверенный снимок

- Base commit: `729af6a740c3dcc865a45c9afdb8b4fbb87646bf`.
- Проверенные пути: runtime wiring и handoff, typed watcher config, RPC deadline и
  quorum, watcher scanner/subscriptions/reconciliation, bbolt state, tests и
  публичная документация Task 06.
- SHA-256 digest diff проверенных production/test/doc файлов:
  `b8674a01a05cfddccb81c884b6f5577c8557511f42a006f636d70dc5bb678718`.
- Из digest исключены только этот отчёт, `findings.md`, task-файл и task index.
- Digest построен при `LC_ALL=C` из binary/full-index tracked diff от base и
  отсортированных SHA-256 содержимого untracked files; в финальном atomic index
  включены 37 tracked paths, untracked implementation files отсутствуют.
- Digest после последнего изменения независимо воспроизведён reviewer и
  координирующим агентом.
- Ревьюер не участвовал в реализации: `да`.

## Проверенные инварианты

- WebSocket является только provisional hint. Полнота обеспечивается scanner,
  который читает одинаковый finalized block hash у независимых провайдеров,
  запрашивает logs по этому hash и сохраняет блок атомарно.
- Ошибка, timeout, disconnect, несовпадение quorum или parent hash не продвигают
  scan cursor. После reconnect следующий generation повторяет backfill от
  сохранённого cursor; первый запуск использует bounded lookback.
- Exact duplicate log безопасно объединяется по block hash, transaction hash и
  log index; конфликтующий duplicate, malformed response, oversized payload и
  нестрогая форма `Transfer` отклоняются до durable commit.
- Canonical journal, provisional observations, ready FIFO, delayed retries,
  block history, tombstones, discovery registry и metadata cache ограничены.
  Saturation provisional hints не блокирует canonical scanner.
- Порядок handoff сохраняется атомарно в bbolt: candidate, incident, `Ack` или
  delayed `Nack`, затем confirmed checkpoint. Crash/reopen повторяет
  незавершённую работу с тем же candidate ID.
- Ready FIFO и round-robin staged/delayed promotion сохраняются после restart.
  Reopen проверяет все двунаправленные индексы, sequence и покрытие каждого
  acknowledged candidate retained canonical block либо tombstone.
- Существующий пустой, усечённый, schema-less, повреждённый или иначе
  привязанный state не переинициализируется. Новый state создаётся только через
  эксклюзивное создание файла и привязывается к network/source/policy.
- HTTP и WebSocket RPC responses имеют transport limit; logs дополнительно
  ограничены по количеству, topics и data до нормализации и формирования
  candidates. Все dial/read/setup операции ограничены context deadline.
- Primary read client неизменяем в пределах generation. Runtime quorum,
  subscriptions, workers, clients и stores закрываются в управляемом порядке.
  Broadcast capability не выдаётся watcher/quorum facade.
- Private RPC URL, state path и backend cause не попадают в публичные ошибки;
  repository secret scans не нашли утечек.

## Findings

| ID | Критичность | Путь/строка | Описание | Требуемое исправление | Статус |
|---|---|---|---|---|---|
| REV-06-001 | High | `internal/rpc/quorum.go`, `internal/watcher/service.go` | Canonical scan мог зависеть от неприкреплённого к hash ответа одного RPC | Ввести independent finalized quorum и hash-pinned logs | ЗАКРЫТ |
| REV-06-002 | High | `internal/store`, `internal/watcher/service.go` | Queue capacity могла связать canonical persistence с немедленной выдачей consumer | Разделить bounded ready queue и durable staged journal | ЗАКРЫТ |
| REV-06-003 | High | `internal/store`, `cmd/guard-daemon/daemon.go` | Production lifecycle не имел crash-consistent persistent handoff | Добавить configuration-bound bbolt store и открыть его до signer | ЗАКРЫТ |
| REV-06-004 | High | `internal/store/bolt_store.go` | Same-height provisional non-member мог пережить canonical seal | Атомарно удалять orphaned observations при полном canonical commit | ЗАКРЫТ |
| REV-06-005 | Medium | `internal/rpc/quorum.go` | Duplicate normalization не отличала точную повторную доставку от конфликта payload | Coalesce exact duplicates и fail closed при конфликте | ЗАКРЫТ |
| REV-06-006 | Medium | `internal/store/bolt_store.go` | Provisional unknown token мог попасть в confirmed discovery registry | Подтверждать discovery только canonical commit | ЗАКРЫТ |
| REV-06-007 | Medium | `internal/store/bolt_store.go`, `internal/watcher/service.go` | Насыщение provisional observations могло остановить authoritative scanner | Возвращать отдельный bounded hint outcome без блокировки scan | ЗАКРЫТ |
| REV-06-008 | Medium | `internal/store/bolt_store.go` | Journal, canonical history и dedup tombstones могли расти без доказанной границы | Ввести согласованные limits и reopen invariants | ЗАКРЫТ |
| REV-06-009 | Medium | `internal/store/bolt_store.go` | Discovery overflow мог откатить canonical commit и терять остальную работу блока | Сохранять durable overflow marker, не блокируя canonical seal | ЗАКРЫТ |
| REV-06-010 | Medium | `internal/store/bolt_store.go` | Due delayed или staged work могли голодать после restart | Сохранять round-robin promotion turn | ЗАКРЫТ |
| REV-06-011 | Medium | `internal/store/bolt_store.go` | Несколько освободившихся slots выдавались не в persisted FIFO order | Добавить monotonic `ready-order` и двунаправленный индекс | ЗАКРЫТ |
| REV-06-012 | Medium | `internal/store/bolt_store.go` | Rollback sequence `ready-order` принимался при reopen | Проверять sequence относительно максимального persisted order | ЗАКРЫТ |
| REV-06-013 | High | `internal/store/bolt_store.go` | Существующий усечённый или полностью очищенный state принимался как новая база | Разрешать schema initialization только для атомарно созданного нового файла | ЗАКРЫТ |
| REV-06-014 | Medium | `internal/store/bolt_store.go` | Sequence exhaustion/rollback tombstones и acknowledged record без покрытия могли нарушить bounds/dedup | Проверять sequence, collisions и полное acknowledgement coverage | ЗАКРЫТ |
| REV-06-015 | Medium | `internal/rpc/dialer.go`, `internal/rpc/quorum.go`, `internal/watcher/service.go` | Недоверенный RPC мог заставить декодировать и копировать неограниченный payload | Ограничить HTTP/WS response и application log shape/count/size | ЗАКРЫТ |

Новых findings в финальном closure pass нет.

## Проверка исправлений

- Closure pass выполнен после каждого изменения state machine, persistence и
  RPC transport; финальный reviewer повторно проверил весь снимок.
- `make go-ci` вне Nix: успешно на официальном Go `1.26.5`; module
  verification, build, tests, vet и race suite прошли.
- `nix develop -c go test -race -mod=readonly -count=1 ./...`: успешно.
- `nix develop -c go test -mod=readonly -count=20 ./internal/store ./internal/rpc ./internal/watcher ./cmd/guard-daemon`:
  успешно.
- Reviewer дополнительно выполнил race stress `-count=3` для store, RPC,
  watcher и daemon, а также новые regression tests `-count=10`: успешно.
- `nix develop -c go vet -mod=readonly ./...`: успешно.
- `nix develop -c env CGO_ENABLED=0 go build -mod=readonly ./...`: успешно.
- `nix develop -c go mod tidy -diff`: изменений нет.
- Оба fuzz target выполнены минимум по `5000x`; reviewer дополнительно запускал
  каждый по пять секунд: успешно.
- Полный `make format-check` с Forge `1.7.1` из Nix: успешно; отдельно проверен
  `gofmt` всех tracked и untracked Go-файлов.
- `git diff --check 729af6a`: успешно.
- `nix develop -c make vuln`: уязвимости не обнаружены.
- `nix develop -c make secret-scan`: directory и Git-history scans не нашли
  утечек.
- Mainnet actions, реальные RPC, ключи, подписи и отправка транзакций не
  выполнялись.

## Остаточные риски

| ID | Критичность | Обоснование | Владелец | Срок пересмотра |
|---|---|---|---|---|
| RISK-06-001 | Low | 2-of-2 quorum безопасно останавливается при отказе одного provider, но снижает доступность | Task 10 | До release rehearsal |
| RISK-06-002 | Low | Произвольный rollback валидного state snapshot и power-loss fault injection не обнаруживаются одной внутренней schema | Tasks 09, 10 | До release candidate |
| RISK-06-003 | Low | Oversized WebSocket проверен wiring review и upstream test, но не отдельным project end-to-end test | Task 09 | При adversarial integration suite |

Эти риски не приняты для production release и переданы следующим задачам.
Transaction-time quorum, nonce lease, ambiguous submission reconciliation и
global budget принадлежат Tasks 07-08 и не являются остаточным риском реализации
Task 06.

## Решение

`ПРОЙДЕНО`. Critical и High findings закрыты, все Medium findings исправлены,
актуальный digest воспроизведён независимо и критерии Task 06 подтверждены.
Статус задачи меняется на `DONE` только после разрешённого atomic task commit.
