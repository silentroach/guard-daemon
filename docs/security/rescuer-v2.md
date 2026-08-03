# Модель безопасности RescuerV2

## Назначение

`RescuerV2` исполняется как код EIP-7702 delegated EOA и переводит токены или
ETH только на неизменяемый `destination`. Контракт не делает
скомпрометированный source безопасным: владелец source-ключа может заменить
делегацию, подписать конкурирующую авторизацию или опередить rescue-транзакцию.

## Контекст исполнения EIP-7702

Конструктор выполняется по адресу реализации и сохраняет его в immutable
`self`. При последующем delegated execution `address(this)` равен source EOA,
а не адресу реализации. `_verifyDelegation` читает код source, требует точный
23-байтовый designator `0xef0100 || implementation` и сравнивает implementation
с `self`.

Проверка предотвращает продолжение операции через этот код при ошибочной
делегации. Она не способна остановить произвольный код другой реализации,
которую атакующий назначил с помощью скомпрометированного source-ключа.

## Роли и получатель

- `destination` получает все активы из `sweepAll` и `sweepEth` и не задаётся
  caller.
- `sponsor` оплачивает транзакции и является единственным caller для
  `executeAndSweep`.
- Конструктор отклоняет нулевые роли и совпадение `destination == sponsor`.

`sweepAll` и `sweepEth` permissionless: посторонний caller может только раньше
перевести активы на тот же фиксированный `destination`. `executeAndSweep`
намеренно принимает target и calldata от доверенного sponsor, потому что
claim-интерфейсы различаются. Списки запрещённых селекторов здесь не дают
защиты из-за fallback, прокси и совпадений селекторов. Компрометация sponsor
поэтому требует остановки daemon и смены реализации с новым sponsor.

## Недоверенные токены и callback

`balanceOf` принимается только при успешном `staticcall` с результатом ровно
32 байта. `transfer` использует optional-return правило:

- пустой результат означает совместимый успех;
- ровно 32 байта со значением `1` означают успех;
- `false`, неканонический `bool`, другая длина и revert означают ошибку всей
  операции.

Такой результат остаётся только заявлением недоверенного token contract и не
доказывает экономическую стоимость. Поэтому daemon классифицирует результат
любого ERC-20 только как `token-reported`, даже если адрес известен и имеет
доверенную операторскую оценку ценности. Callback токена может повторно вызвать
permissionless sweep, но получатель останется тем же. Callback не может войти
в `executeAndSweep`, поскольку его `msg.sender` не равен sponsor. Storage-based
reentrancy guard намеренно не используется: при EIP-7702 storage принадлежит
source EOA, и новый служебный slot создал бы риск коллизии с другой реализацией.

## События и поверхность кода

`Swept(source, token, destination, amount)` фиксирует source, token,
destination и заявленный amount. Для ETH поле token равно нулевому адресу.
Событие не является доказательством экономической ценности токена.

Permit-based production contract и его deployment path удалены. Единственный
CLI `scripts/deployRescuerV2.ts` использует canonical artifact и без явного
`--broadcast` только строит локальный план без RPC, ключа и отправки. Локальная
проверка включает повторную pinned-компиляцию и побайтовое сравнение artifact.
Она описана в `docs/deployment/local-verification.md`; production-активация
остаётся отдельной задачей эксплуатации.

## Воспроизводимость и измерения

Контракт компилируется `solc 0.8.36` для Prague с optimizer `runs=200`, без
CBOR metadata и metadata hash. `foundry.toml` и TypeScript compiler используют
одинаковые параметры. Размер creation/runtime bytecode и gas measurements
фиксируются результатами `forge build --sizes` и `forge test --gas-report`;
это эксплуатационные измерения, а не доказательство безопасности.

Для проверенного снимка Task 03 `forge build --sizes` сообщил 2 790 байт
creation bytecode и 2 431 байт runtime bytecode. `forge test --gas-report`
сообщил 581 683 gas для тестового deployment и диапазон 1 221–22 715 gas для
распознанных reporter-вызовов `sweepAll`. Reporter не учитывает полную стоимость
каждого delegated EIP-7702 сценария, поэтому эти числа нельзя использовать как
fee policy без simulation конкретной транзакции.

`Slither 0.11.6` анализирует production source отдельно от adversarial mocks.
Critical и High findings отсутствуют. Оставшиеся сообщения относятся к
необходимым low-level ERC-20 вызовам и assembly-декодированию, вызовам токенов
в цикле и событию после успешного внешнего вызова. Цикл ограничивается
операционной policy и budget в последующих задачах; callback-модель покрыта
тестами и не позволяет изменить фиксированный получатель.
