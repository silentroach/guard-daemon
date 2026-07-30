# Архитектура Go daemon

## Назначение границ

Go daemon разделён на небольшие пакеты с направленными зависимостями. Точка
входа `cmd/guard-daemon` загружает совместимую конфигурацию, создаёт зависимости,
запускает по одному процессу координации на сеть и обрабатывает остановку.
Прикладная логика не читает переменные окружения и не владеет жизненным циклом
процесса.

Основные пакеты:

- `internal/config` — временный типизированный адаптер существующих переменных
  окружения и совместимых сетевых настроек;
- `internal/domain` — network, token, candidate, incident, outcome, retry и
  классифицированные ошибки;
- `internal/rpc` — узкие интерфейсы чтения, subscriptions и broadcasting, а
  также неизменяемый client одного reconnect-generation;
- `internal/contracts` — минимальные ABI-кодеки ERC-20 и Rescuer и разбор
  EIP-7702 delegation indicator;
- `internal/watcher` — получение событий и создание rescue candidates без
  доступа к signer и отправке транзакций;
- `internal/rescue` — проверка startup state и обработка candidates без
  управления subscription lifecycle;
- `internal/store` — contracts очереди, incidents, checkpoints и lease;
- `internal/budget` — contract единого persistent budget ledger;
- `internal/observability` — типизированные события без приватных ключей,
  signatures, RPC URL и signed transaction payloads.

Watcher и rescue не импортируют друг друга. Они обмениваются только
`domain.RescueCandidate` через `store.CandidateQueue`. Transaction submission
доступен rescue только через отдельный `rpc.Broadcaster`. Signer представлены
интерфейсами с проверяемым адресом, поэтому тесты фиксируют attempted operations
без приватных ключей и реальной подписи.

## Жизненный цикл RPC

Каждая сеть имеет один долгоживущий coordinator с retry state и очередью. На
каждом reconnect создаётся новый immutable `rpc.GenerationClient`. Watcher и
rescue session получают интерфейсы этого client, завершаются по общему context,
после чего process ждёт обе goroutine и только затем закрывает client. Фоновая
goroutine не заменяет RPC client в поле runtime state.

Существующая совместимость сохраняет попытку WebSocket-соединения до HTTP
fallback. Изменения backfill, reorg и checkpoint policy принадлежат отдельной
задаче watcher/RPC.

## Протокол handoff

Порядок durable transitions зафиксирован интерфейсами и crash tests:

1. `CandidateQueue.Put` сохраняет candidate со stable ID.
2. `IncidentStore.PutIncident` идемпотентно связывает candidate и incident.
3. После обработки `CandidateQueue.Ack` подтверждает эту связь.
4. Checkpoint разрешено продвигать только после подтверждения всей работы блока.

Повторный `Put` одного stable ID объединяется как pending или acknowledged и не
создаёт новый incident. Crash между перечисленными переходами должен приводить
к replay той же работы, а не к потере или созданию новой операции.
Retryable или ambiguous ошибка вызывает `Nack`: candidate остаётся pending, но
переходит в delayed-очередь с собственным backoff, чтобы неисправный token
contract не блокировал native и token work после него. RPC generation при этом
продолжает принимать новые candidates.

Текущий `cmd/guard-daemon` использует явно обозначенный in-memory адаптер только
для совместимости переходного этапа. Он сохраняет pending work и tombstones
между reconnect одного процесса, применяет backpressure после 1024 pending
записей и хранит ограниченное окно из 4096 acknowledged tombstones. После
вытеснения старого tombstone его повторная доставка снова считается работой,
поэтому это окно не заменяет checkpoint-aware сборку мусора. Адаптер не
переживает остановку процесса. Persistent реализация queue, incidents,
checkpoints и межпроцессный lease вводится в задачах watcher и rescue
coordinator; временный адаптер не считается доказательством process-crash
durability.

## Сохранённое и отложенное поведение

Рефакторинг сохраняет наблюдение известных и неизвестных ERC-20, periodic
reconciliation, native balance check, EIP-7702 authorization, atomic token
sweep, native sweep, fee multipliers/caps, receipt polling и ограниченный retry.
Недекодируемое postcondition после успешного receipt теперь обрабатывается
fail-closed как неоднозначная ошибка, а не как доказанный успех.

Permit production path в Go отсутствует: нет ABI, настройки, runtime state и
заявления о fallback. Производственный путь использует только EIP-7702 Rescuer.

Политики RPC quorum, finalized state, dry run, persistent queue, nonce lease,
новые retry generations, postconditions, global sponsor budget и защита от
unknown-token gas abuse намеренно остаются в последующих задачах плана.
