# Архитектура Go daemon

## Назначение границ

Go daemon разделён на небольшие пакеты с направленными зависимостями. Точка
входа `cmd/guard-daemon` загружает совместимую конфигурацию, создаёт зависимости,
запускает по одному процессу координации на сеть и обрабатывает остановку.
Прикладная логика не читает переменные окружения и не владеет жизненным циклом
процесса.

Основные пакеты:

- `internal/config` — типизированная проверка переменных окружения, сетевых
  настроек, режима и политики восстановления watcher;
- `internal/domain` — network, token, candidate, incident, outcome, retry и
  классифицированные ошибки;
- `internal/rpc` — узкие интерфейсы чтения, subscriptions и broadcasting,
  deadline-обёртки, строгий finalized quorum и неизменяемый client одного
  reconnect-generation;
- `internal/contracts` — минимальные ABI-кодеки ERC-20 и Rescuer и разбор
  EIP-7702 delegation indicator;
- `internal/watcher` — получение событий и создание rescue candidates без
  доступа к signer и отправке транзакций;
- `internal/rescue` — проверка startup state и обработка candidates без
  управления subscription lifecycle;
- `internal/store` — транзакционное локальное хранилище очереди, provisional
  observations, incidents и двух уровней checkpoints, а также interface lease;
- `internal/budget` — contract единого persistent budget ledger;
- `internal/observability` — типизированные события без приватных ключей,
  signatures, RPC URL и signed transaction payloads.

Watcher и rescue не импортируют друг друга. Они обмениваются только
`domain.RescueCandidate` через `store.CandidateQueue`. Transaction submission
доступен rescue только через отдельный `rpc.Broadcaster`. Signer представлены
интерфейсами с проверяемым адресом, поэтому тесты фиксируют attempted operations
без приватных ключей и реальной подписи.

## Жизненный цикл RPC

Каждая сеть имеет один долгоживущий coordinator и одно persistent хранилище. На
каждом reconnect создаются immutable primary client и два независимых HTTP
provider для runtime quorum. Unary-вызовы primary client, dial и установка
subscriptions ограничены `RPC_READ_TIMEOUT`. Watcher и rescue session
завершаются по общему context, process дожидается обеих goroutine и только затем
закрывает clients. Фоновая goroutine не заменяет RPC client в поле runtime state.

WebSocket subscription является только источником provisional observations и
сигналом низкой задержки. Полноту обеспечивает canonical scanner: при каждом
запуске он проверяет сохранённый cursor через finalized quorum, выполняет
backfill до согласованного блока и только после этого подписывается. Повторный
scan после установки subscription закрывает окно регистрации. Initial failure
переводит generation в polling, а active disconnect завершает generation;
следующее поколение начинает с backfill от durable cursor.

Оба provider должны единогласно подтверждать finalized header, исторические
headers и нормализованный набор logs. Расхождение, отсутствующий `nil` response,
некорректный ответ и reorg уже подтверждённого cursor блокируют продвижение.
Корректный пустой массив означает, что согласованный блок не содержит подходящих
logs, и сохраняется как empty canonical seal.

## Протокол handoff

Порядок durable transitions реализован одним ACID backend и зафиксирован crash
tests:

1. `CandidateQueue.Put` сохраняет candidate со stable ID.
2. `IncidentStore.PutIncident` идемпотентно связывает candidate и incident.
3. После обработки `CandidateQueue.Ack` подтверждает эту связь.
4. Checkpoint разрешено продвигать только после подтверждения всей работы блока.

Повторный `Put` одного stable ID объединяется как pending или acknowledged и не
создаёт новый incident. Crash между перечисленными переходами должен приводить
к replay той же работы, а не к потере или созданию новой операции.
Retryable или ambiguous ошибка вызывает `Nack`: candidate остаётся в durable
journal, переходит в delayed-состояние с собственным backoff и освобождает место
в active dispatch. Staged candidates продвигаются раньше ожидающих delayed, так
что неисправный token contract не блокирует native и token work после него.

Production открывает отдельный привязанный к chain, source и token policy файл
`bbolt` для каждой сети. Provisional observations, canonical journal и active
dispatch имеют отдельные лимиты. Насыщенный provisional слой отклоняет только
недоверенный hint с типизированным событием, а scanner восстанавливает его через
backfill. Большой canonical block сначала атомарно сохраняется как staged
journal и затем выдаётся consumer ограниченными частями. Empty block также
получает durable seal. Scan cursor продвигается после сохранения полного блока,
а подтверждённый checkpoint — только после `PutIncident` и `Ack` всех его
candidates. Старые block records и acknowledged tombstones удерживаются в
ограниченных окнах; работа старше scan cursor coalesce-ится как уже учтённая.

Removed log удаляет только provisional observation. При финализации высоты
наблюдения другой ветки удаляются атомарно; ready, delayed или acknowledged
работа другой ветки считается нарушением finalized-инварианта и останавливает
generation. Unknown token registry, metadata cache, RPC return data и все слои
очереди имеют явные пределы. Переполнение confirmed registry сохраняет durable
маркер и предупреждение, но не блокирует canonical seal или native/configured
работу.

## Сохранённое и отложенное поведение

Рефакторинг сохраняет наблюдение известных и неизвестных ERC-20, periodic
reconciliation, native balance check, EIP-7702 authorization, atomic token
sweep, native sweep, fee multipliers/caps, receipt polling и ограниченный retry.
Недекодируемое postcondition после успешного receipt теперь обрабатывается
fail-closed как неоднозначная ошибка, а не как доказанный успех.

Permit production path в Go отсутствует: нет ABI, настройки, runtime state и
заявления о fallback. Производственный путь использует только EIP-7702 Rescuer.

Межпроцессный sponsor nonce lease, transaction-time quorum reads, новые retry
generations, полные destination postconditions, global sponsor budget и защита
от unknown-token gas abuse остаются в последующих задачах плана.
