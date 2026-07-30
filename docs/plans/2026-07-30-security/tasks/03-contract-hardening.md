# Task 03: Усиление смарт-контрактов

Статус: `PENDING`

Зависимости: Task 01.

## Цель

Сделать Solidity contracts безопасными при EIP-7702 delegated execution, front-running, permit pre-consumption, reentrancy и нестандартном ERC-20 behavior. Все свойства подтверждаются adversarial tests.

## Границы задачи

- `contracts/RescuerV2.sol`.
- Contract test harness, mocks и fuzz/invariant tests.
- Contract-specific русскоязычная design note.

Не изменять mainnet deployment, `.env` и TypeScript deployment scripts.

## Обязательные изменения

1. Сохранить immutable `destination`, `sponsor/operator` и `self` с zero-address validation.
2. Требовать `destination != sponsor` в constructor и tests: hot sponsor не может быть safe destination.
3. Оставить `sweepAll`/`sweepEth` permissionless только если они физически могут отправлять активы исключительно на fixed destination.
4. Ограничить `executeAndSweep` ожидаемым sponsor/operator; проверить callback/reentrancy paths и обосновать выбранную защиту.
5. Не допускать произвольного recipient во всех asset-moving functions.
6. Реализовать optional-return ERC-20 operations по модели `SafeERC20`: empty return — success, `false` — failure, malformed return — failure.
7. Удалить неиспользуемый PermitSweeper из production scope: contract, Go ABI, canonical deployment и claims документации удаляются в Tasks 02-04. Возврат permit flow возможен только отдельным будущим security design/task.
8. События должны позволять проверить source, token, destination и amount без чувствительных данных.
9. Не использовать upgradeability, `delegatecall`, `selfdestruct` и скрытый административный вывод.
10. Удалить incident-specific identifiers из Solidity comments/tests.

## Параллельные направления

- **Rescuer implementation:** только `/contracts/RescuerV2.sol`.
- **Contract tests:** только `/test/contracts/RescuerV2*.t.sol`.
- **Adversarial mocks:** только `/test/contracts/mocks/**` и `/test/contracts/invariants/**`.
- **Permit removal:** только удаление `/contracts/PermitSweeper.sol`; связанные Go/deployment/docs paths принадлежат Tasks 02, 04 и 10.

Общий test helper и final integration принадлежат coordinator.

## Обязательные tests

- Посторонний caller не может вызвать arbitrary call через delegated EOA.
- Sponsor может выполнить разрешённый claim и sweep.
- Callback от malicious token не обходит `onlySponsor`.
- Permissionless sweep всегда отправляет только fixed destination.
- Authorization на другой implementation вызывает revert.
- Standard, no-return, false-return, reverting и reentrant tokens.
- Production ABI/artifacts не содержат PermitSweeper и `permitAndTransfer`.
- Fuzz для token arrays, amount, return data и arbitrary calldata boundaries.

## Критерии приёмки

- Contract tests и fuzz/invariant suite проходят на pinned compiler/toolchain.
- ABI явно содержит destination и sponsor/operator getters для runtime attestation.
- Runtime не имеет caller-controlled asset destination.
- Slither/static analysis не содержит нерешённых Critical/High findings.
- Contract size и gas measurements задокументированы без необоснованных security claims.
- Design note на русском объясняет EIP-7702 execution context и access-control assumptions.

## Независимое ревью

Отдельный reviewer анализирует только Solidity diff и tests как hostile contract auditor. Он обязан построить calldata для `approve`, `transfer`, NFT/operator и callback attacks, проверить role separation и optional-return behavior, а также отсутствие permit production surface.

Исправить все findings и повторить fuzz/static analysis. Изменение ABI требует второго review pass.

## Завершение и commit

После ревью сохранить отчёт в `../reviews/03-contract-hardening.md`, выполнить closure pass и изменить status на `DONE` здесь и в `../tasks.md`.

Рекомендуемый subject:

```text
security(contracts): закрыть delegated-call и permit риски
```

Commit body подробно описывает threat scenarios, ABI changes, compatibility behavior, tests и residual assumptions.
