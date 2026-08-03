# Отчёт независимого ревью Task 07

Задача: `Task 07`

Статус ревью: `ПРОЙДЕНО`

## Проверенный снимок

- Base commit: `9385864d049853a2129ada2476b4248a9b735e8d`.
- Проверенные пути: `cmd/guard-daemon/**`, `internal/domain/model.go`,
  `internal/rescue/**`, `internal/rpc/**`, `internal/store/**`,
  `internal/watcher/service_test.go`, `go.mod`, `go.sum`.
- SHA-256 digest diff проверенных production/test файлов:
  `11d42ceba8175981287eef9b88816329b85f7fc2116dfb98ea70a9660a64f0ac`.
- Digest включает tracked diff и новые untracked production/test files. Из него
  исключены только этот отчёт, `findings.md` и status-поля task index/task-файла.
- Ревьюер не участвовал в реализации: `да`.
- Первичный review: `ТРЕБУЮТСЯ ИСПРАВЛЕНИЯ`; выполнены два независимых closure
  pass после изменений state machine, success definition и process fencing.

## Проверенные инварианты

- Один coordinator сериализует nonce allocation, signing и submission; durable
  nonce floor не уменьшается после restart или pruning.
- Persistent lease и host-wide process fence для `chain+sponsor` приобретаются
  до signer construction. Fence не зависит от `STATE_DIRECTORY`, `HOME` или
  cache environment и проверяется после signer и перед каждым broadcast.
- Signed payload сохраняется в bbolt-файле с режимом `0600`, строго сверяется с
  source/sponsor/nonce/hash и повторно отправляется только с тем же hash.
- Proactive delegation renewal отсутствует. Token и native rescue являются
  атомарными EIP-7702 `SetCodeTx` с непустым asset call и authorization.
- Receipt принимается только после finalized canonical quorum. Snapshot,
  receipt block и postcondition state повторно проверяются до terminal outcome.
- Trusted success требует нулевой source balance и достаточный destination
  delta. Unknown token получает только `token-reported` outcome.
- Ошибки prestate сохраняются одним атомарным переходом, signing retries
  ограничены incident policy, а поздний receipt обрабатывает отдельный sparse
  reconciliation worker без повторной подписи.
- Schema v1 атомарно мигрируется в v2 и привязывается к
  source/sponsor/destination/rescuer; corruption и binding mismatch блокируют
  startup.
- Raw transaction и signatures не попадают в observer или operator errors.

## Findings

| ID | Критичность | Путь/строка | Описание | Требуемое исправление | Статус |
|---|---|---|---|---|---|
| REV-07-001 | High | `internal/rescue/operations.go`, `internal/store/rescue_state.go` | Crash после signing терял exact payload и создавал nonce gap | Durable signed payload, exact rebroadcast и nonce-gap fence | CLOSED |
| REV-07-002 | High | `internal/store/process_fence*.go`, `cmd/guard-daemon/daemon.go` | Lease можно было обойти другим state/cache path; guard имел окно перед send | Stable host-wide fence, inode validation и pre-send guard | CLOSED |
| REV-07-003 | High | `internal/rescue/operations.go` | Finalized receipt ошибочно исчерпывался за короткий timeout | Durable finality horizon и sparse reconciliation | CLOSED |
| REV-07-004 | High | `internal/rescue/operations.go` | Prestate и poststate не были повторно связаны с canonical chain | Двойная проверка snapshot/receipt/finalized refs | CLOSED |
| REV-07-005 | High | `internal/rescue/operations.go`, `internal/store/rescue_state.go` | Crash между двумя writes мог превратить RPC error в no-paid-action | Один атомарный preparation-failure transition | CLOSED |
| REV-07-006 | High | `internal/store/bolt_store.go` | Rescue state не был связан со sponsor/destination/rescuer | Строгие schema bindings и mismatch tests | CLOSED |
| REV-07-007 | High | `internal/rescue/coordinator.go` | Expired ambiguity снималась только после restart | Autonomous receipt-only reconciliation worker | CLOSED |
| REV-07-008 | Medium | `internal/rescue/operations.go` | Destination delta без source-zero мог дать ложный success | Обязательный source-zero и destination delta | CLOSED |
| REV-07-009 | Medium | `internal/rescue/operations.go` | Не проверялась криптографическая authority signer | Recover authority/sender до persistence и send | CLOSED |
| REV-07-010 | Medium | `internal/store/rescue_state.go` | Terminal incidents росли без границы | Bounded pruning с отдельным nonce floor | CLOSED |
| REV-07-011 | Medium | `internal/rescue/local_chain_test.go` | Не было protocol-level hostile authorization evidence | Local Prague-chain same-nonce и malicious delegation tests | CLOSED |
| REV-07-012 | Low | `cmd/guard-daemon/daemon.go` | Startup rollback скрывал release failure | Redacted aggregate cleanup error | CLOSED |

## Проверка исправлений

- Closure pass выполнен для каждого изменения после первичного ревью.
- Digest обновлён после последнего изменения проверяемых файлов.
- `nix develop -c go test -mod=readonly ./...` — PASS.
- `nix develop -c go test -race -mod=readonly ./...` — PASS.
- `nix develop -c go vet -mod=readonly ./...` — PASS.
- `nix develop -c go build -mod=readonly ./...` — PASS.
- `nix develop -c go mod verify` — PASS.
- Hostile local-chain tests для malicious delegation, competing same-nonce
  authorization и atomic native rescue — PASS.
- `npm run typecheck`, `npm run lint`, contract reproducibility tests — PASS.
- `git diff --check` — PASS.
- Локальный `forge` отсутствовал; Solidity source и contract tests в Task 07 не
  изменялись.

## Остаточные риски

| ID | Критичность | Обоснование | Владелец | Срок пересмотра |
|---|---|---|---|---|
| Нет | — | Нет в рамках задачи | — | — |

Global cumulative sponsor budget и default-deny paid action для unknown tokens
остаются незавершёнными зависимостями Task 08, а не принятым остаточным риском
Task 07. Multi-host deployment запрещается до явной эксплуатационной процедуры
Task 10; реализованный fence гарантирует exclusivity одного хоста.

## Решение

`ПРОЙДЕНО`: открытые Critical, High, Medium и Low findings отсутствуют; digest
соответствует последнему проверенному production/test diff.
