# Отчёт независимого ревью Task 03

Задача: `Task 03`

Статус ревью: `ПРОЙДЕНО`

## Проверенный снимок

- Base commit: `b54b90f`.
- Проверенные пути: `contracts/{RescuerV2.sol,PermitSweeper.sol}`,
  `test/contracts/**`, `foundry.toml`, `scripts/{compileContracts.ts,install-foundry-ci.sh,install-solc-ci.sh}`,
  `tools/slither-requirements.*`, `Makefile`, `.github/workflows/{ci.yml,security.yml}`,
  `docs/{security/rescuer-v2.md,development/ci.md}`.
- SHA-256 digest diff проверенных production/test/configuration/documentation файлов:
  `88c3da4ff0617a73df04214f06ba0680884498a5b07d87a0283bdee2a88b77f4`.
- Из digest исключены только этот отчёт, `findings.md` и status-поля task index и task-файла.
- Ревьюер не участвовал в реализации: `да`.

## Проверенные инварианты

- `executeAndSweep` достигает внешнего вызова только для immutable `sponsor`; ERC-20 approve/transfer,
  ERC-721 operator/transfer, fallback и callback атаки постороннего caller отклоняются до target call.
- Permissionless `sweepAll` и `sweepEth` не принимают recipient и переводят активы только на immutable
  `destination`; конструктор запрещает нулевые роли и совпадение `destination == sponsor`.
- EIP-7702 designator имеет точную длину и prefix, а implementation совпадает с immutable `self`;
  прямой вызов и делегация через другую implementation завершаются ошибкой.
- Optional-return ERC-20 принимает только пустой ответ или точный canonical `true`; `false`, revert,
  malformed return и недекодируемый `balanceOf` обрабатываются fail-closed.
- Callback может повторить только permissionless sweep на тот же destination и не получает sponsor-доступ.
- Событие `Swept` содержит source, token, destination и amount; для ETH token равен нулевому адресу.
- Production contract не содержит upgradeability, `delegatecall`, `selfdestruct`, административного вывода
  и caller-controlled sweep recipient.
- Permit contract, `permitAndTransfer`, production ABI и собираемый artifact удалены; deployment scripts
  следующей задачи не запускались и не считались доказательством выпуска.
- Foundry, Python, Slither и solc закреплены версиями и hashes; проверенный solc передаётся Forge/Slither
  через `FOUNDRY_SOLC`, поэтому `solc-select` не скачивает другой compiler.

## Findings

| ID | Критичность | Путь/строка | Описание | Требуемое исправление | Статус |
|---|---|---|---|---|---|
| REV-03-001 | Medium | `.github/workflows/ci.yml`, `.github/workflows/security.yml` | Первоначальная установка Foundry выполняла незакреплённый `foundryup` | Скачивать immutable release archive и fail-closed проверять repository-pinned SHA-256 и версию | ЗАКРЫТО |
| REV-03-002 | Medium | `.github/workflows/security.yml`, `scripts/install-solc-ci.sh`, `tools/slither-requirements.txt` | Первоначально Python dependency closure и фактически используемый Slither compiler не были полностью воспроизводимы | Закрепить Python и все wheels с hashes, запретить sdist, проверять solc SHA-256 и передавать его через `FOUNDRY_SOLC` | ЗАКРЫТО |
| REV-03-003 | Medium | `.github/workflows/ci.yml`, `foundry.toml` | Обязательный contract job первоначально запускал сокращённый default fuzz/invariant profile | Явно задать `FOUNDRY_PROFILE=ci` для обязательного job | ЗАКРЫТО |

Critical и High findings отсутствовали. После финального closure pass новых findings не обнаружено.

## Проверка исправлений

- Closure pass выполнен тем же независимым reviewer после каждого изменения production/test/CI файлов.
- Digest обновлён после последнего изменения проверяемых файлов.
- `make check`: успешно пройдены format checks, `actionlint`, ShellCheck, Go build/tests/vet/race,
  TypeScript typecheck/ESLint, solc-js artifact tests, Forge tests/lint, `govulncheck`, `npm audit` и оба
  `gitleaks` scan.
- `FOUNDRY_PROFILE=ci forge test --force`: 31 test пройден, 0 ошибок; четыре fuzz-теста выполнили по
  10 000 запусков, invariant выполнил 1 000 прогонов и 256 000 вызовов без revert.
- Fresh venv `pip install --only-binary=:all: --require-hashes -r tools/slither-requirements.txt`:
  hash-locked Slither dependency closure установлен успешно.
- `make contracts-static` с чистым `HOME`: Slither `0.11.6` использовал repository-verified
  `solc 0.8.36`, не создавал `.solc-select` и не выявил Critical/High findings.
- `forge build --sizes`: runtime `RescuerV2` — 2 431 байт, initcode — 2 790 байт.
- Linux archives Foundry и solc повторно скачаны по CI URL; SHA-256 совпали с repository values.
- Production scan `contracts`, `internal` и свежего `build/contracts` не обнаружил Permit surface.
- `git diff --check`: успешно.

## Остаточные риски

| ID | Критичность | Обоснование | Владелец | Срок пересмотра |
|---|---|---|---|---|
| Нет | — | Нет в рамках задачи | — | — |

Полная type-4 EIP-7702 ordering/invalid-authorization модель, gas budget и unknown-token policy относятся к
последующим задачам и не считаются закрытыми этим отчётом. Исполнение Linux installers окончательно
подтверждается обязательным GitHub Ubuntu job; локально проверены URL, SHA-256, состав и архитектура архивов.

## Решение

`ПРОЙДЕНО`: все findings закрыты, финальный closure pass не обнаружил новых замечаний, digest соответствует
последнему проверенному diff, обязательные проверки Task 03 успешны.
