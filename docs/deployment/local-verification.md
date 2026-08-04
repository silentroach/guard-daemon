# Локальная проверка средств развёртывания

## Граница применения

Эта инструкция проверяет канонический артефакт, режим построения плана без
отправки, локальное развёртывание в Anvil и Go API аттестации. Она не разрешает
развёртывание или активацию в публичной сети. Manifest и recovery registry
должны находиться в заранее созданных временных каталогах вне candidate и Git
worktree. Deployment CLI не создаёт их родительские каталоги.

Адреса из примеров ниже являются детерминированными локальными фикстурами. Они
допустимы только для локального плана без отправки и Anvil и не являются
операторскими адресами или значениями по умолчанию для публичной сети.

## Закреплённые инструменты

- Node.js `24.18.1`;
- npm `11.16.0`;
- `solc` `0.8.36+commit.8a079791.Emscripten.clang` из `package-lock.json`;
- Foundry `1.7.1` для локального теста Anvil;
- Go `1.26.5`.

Nix можно использовать как необязательное локальное окружение. Команды
репозитория не зависят от Nix и должны работать после установки закреплённых
версий инструментов.

## Чистая проверка

Из корня чистого checkout:

```bash
npm ci
npm run artifacts:verify
npm run contracts:test
npm run deploy:test
go test -mod=readonly ./internal/contracts
```

`artifacts:verify` повторно компилирует `RescuerV2` и побайтно сравнивает
результат с `artifacts/contracts/RescuerV2.json`. `deploy:test` использует
только локальный Anvil и локальные тестовые ключи. Он не является репетицией
публичного развёртывания и не доказывает готовность к mainnet.

Результаты команд следует сохранять как локальное свидетельство с полным commit
проверяемого checkout. Этот документ не утверждает, что репетиция уже выполнена
или завершилась успешно.

## План без отправки

Без `--broadcast` CLI требует только `--chain-id`, `--destination` и
`--sponsor`. Он повторно собирает закреплённый исходный код, сверяет канонический
артефакт и строит данные конструктора, но не обращается к RPC, не читает
приватный ключ, не подписывает и не отправляет транзакцию:

```bash
./node_modules/.bin/tsx scripts/deployRescuerV2.ts \
  --chain-id 31337 \
  --destination 0x1000000000000000000000000000000000000001 \
  --sponsor 0x2000000000000000000000000000000000000002
```

Наличие `--rpc-url`, `DEPLOYMENT_RPC_URL`, переменной с ключом или пути вывода не
должно менять семантику режима без `--broadcast`: parser и runner не читают
окружение, RPC или ключ. Для проверки этого инварианта используется
`npm run deploy:test`.

## Интерфейс режима отправки

Режим с отправкой дополнительно требует весь набор:

- `--manifest`;
- `--release-candidate`;
- `--gas-limit`;
- `--max-fee-per-gas-wei`;
- `--max-priority-fee-per-gas-wei`;
- `--max-total-cost-wei`;
- `--broadcast`.

Для production chain ID `1`, `56` и `137` RPC URL обязателен в переменной
`DEPLOYMENT_RPC_URL`; любой `--rpc-url` в broadcast-команде отклоняется, чтобы
URL с credentials не попадал в argv и журналы запуска. Переменная передаётся
назначенному операторскому процессу защищённым каналом и не записывается в
repository, release candidate, manifest или recovery record. Локальный
broadcast chain ID `31337` по-прежнему требует `--rpc-url`.

Для локальной chain ID `31337` дополнительно обязателен
`--recovery-directory <preexisting-temp-dir>`. Каталог должен заранее
существовать, быть обычным каталогом без symbolic link, иметь режим `0700` и
находиться под системным временным каталогом вне candidate и Git worktree. Для
production chain ID `1`, `56` и `137` этот аргумент запрещён: CLI всегда
использует `/var/lib/guard-daemon-operator/deployment-recovery` и не позволяет
переопределить путь. Production registry должен заранее существовать как
canonical каталог `0700 root:root`. Аргумент `--recovery-record` не
поддерживается.

Production CLI разрешено запускать только из
`/usr/lib/guard-daemon/deployment/<releaseCommit>` с candidate
`/usr/lib/guard-daemon/candidates/<releaseCommit>/release-candidate.json`.
Оба каталога публикуются root-owned с режимом `0700` процедурой установки после
аутентификации `SHA256SUMS`; обычный checkout и его `node_modules` не являются
доверенной production-средой. Manifest создаётся непосредственно в
`/var/lib/guard-daemon-operator/deployment-manifests` с режимом родителя
`0700 root:root`.

`--config` и `--config-key` удаляются: средство развёртывания не изменяет
операторскую конфигурацию. `release-candidate.json` должен связывать полный
40-символьный commit выпуска с SHA-256 точных байтов
`artifacts/contracts/RescuerV2.json`. Официальный манифест получает
`source.kind=git-commit` и тот же полный commit.

Транзакция развёртывания содержит явные `gasLimit`, `maxFeePerGas` и
`maxPriorityFeePerGas`. CLI отклоняет значения, при которых priority fee
выше max fee или `gasLimit * maxFeePerGas` превышает
`max-total-cost-wei`. Последний параметр является локальной верхней границей
полной стоимости газа, а не отдельным полем EVM-транзакции.

Перед публичным действием оператор обязан подтвердить этот интерфейс тестами и
использовать `release-candidate.json` из неизменяемого release commit.

`--release-candidate` указывает на файл внутри полного каталога кандидата.
Для локальной chain ID `31337` проверка требует чистый checkout с `HEAD`, равным
release commit, сверяет hash дерева, весь `SHA256SUMS` и исходный tar с
каноническим `git archive` до RPC и чтения ключа. Наличие или symlink
`info/attributes` в git-dir/common-dir блокирует проверку. CLI также проверяет
`git ls-files -v` до и после archive и fail-closed отклоняет любой
`assume-unchanged` или `skip-worktree`: такие flags способны скрыть изменение от
`git status`.

Production не читает Git metadata. Его trust boundary — сохранённые при
установке candidate и tooling из аутентифицированного source archive. До запуска
процесса с секретами оператор отдельно проверяет их владельца, режим и отсутствие
записываемых непривилегированным пользователем потомков по operator-runbook.

Manifest должен отсутствовать, а его заранее существующий родитель не должен
быть symbolic link; и manifest, и его родитель обязаны находиться вне candidate
и Git worktree. Recovery registry также должен быть вне этих границ. CLI не
создаёт output-каталоги. Для production родитель manifest не может быть доступен
для записи группе или остальным. Перед lock, recovery publication и manifest
publication CLI повторно сверяет canonical identity и device/inode исходного
каталога; для registry также повторно проверяются `0700 root:root`, а для
родителя production manifest — владелец `root:root`.

Имя recovery record определяется только как
`rescuer-v2-<chain>-<lowercase-sponsor>-<releaseCommit>.json`. До чтения ключа и
RPC CLI сканирует весь registry. Любой незавершённый record с той же парой
chain+sponsor блокирует deployment независимо от release commit. После подписи
CLI атомарно и без перезаписи сохраняет точную подписанную транзакцию с режимом
`0600`, синхронизирует каталог и только затем выполняет broadcast. Существующий
manifest также запрещает отправку.

Режим broadcast разрешён только для chain ID `1`, `56`, `137` и локального
`31337`, где используемая модель не содержит известной дополнительной комиссии
вне transaction cap. Добавление другой сети требует отдельного изменения кода и
security-review.

## Проверяемый результат

Локальная проверка манифеста должна охватывать chain ID, транзакцию и блок
развёртывания, компилятор и настройки, SHA-256 артефакта, Keccak-256 связанного
runtime и неизменяемые `destination`, `self`, `sponsor`. Схема находится в
`deployments/schema/rescuer-manifest.schema.json`.

`internal/contracts.LoadTrustedManifest` связывает манифест с точными байтами
канонического артефакта и значениями неизменяемых полей.
`internal/contracts.AttestDeployment` требует согласия как минимум двух
независимо настроенных поставщиков чтения по chain ID, общему финализированному
блоку, транзакции и receipt развёртывания, runtime и трём getter-функциям.
Любая ошибка или расхождение блокирует результат.

Одиночное RPC-чтение, выполняемое deployment script после отправки, является
только предварительной самопроверкой. Оно не является окончательной
эксплуатационной аттестацией. Эта граница описана в
[`manifest-attestation.md`](manifest-attestation.md).
