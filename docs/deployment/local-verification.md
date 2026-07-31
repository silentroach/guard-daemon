# Локальная проверка deployment tooling

## Граница применения

Эта инструкция проверяет canonical artifact, безопасный режим планирования,
локальный deployment в Anvil и Go API аттестации. Она не описывает активацию в
production или mainnet. До отдельной эксплуатационной процедуры Task 10 нельзя
переносить локальные команды на публичную сеть.

В репозитории нет operator-specific manifests и адресов deployment по умолчанию.
Локальные manifests должны оставаться в игнорируемом каталоге
`deployments/local/`.

## Закреплённые инструменты

- Node.js `24.18.1`;
- npm `11.16.0`;
- `solc` `0.8.36+commit.8a079791.Emscripten.clang` из `package-lock.json`;
- Foundry `1.7.1` для локального Anvil-теста;
- Go `1.26.5`.

Nix можно использовать как необязательное локальное окружение. Команды
репозитория не зависят от Nix и в CI запускаются после установки тех же
закреплённых версий инструментов.

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
результат с `artifacts/contracts/RescuerV2.json`. Deployment-тест запускает
только локальный Anvil, проверяет фактический chain ID, runtime, immutable
getters, manifest и атомарное обновление тестовой конфигурации.

## План без отправки

Без флага `--broadcast` CLI только строит deployment data из canonical artifact.
Он не обращается к RPC, не читает приватный ключ, не подписывает и не отправляет
транзакцию. Перед построением плана CLI сам повторно компилирует pinned source и
побайтно сверяет результат с единственным canonical artifact:

```bash
npx tsx scripts/deployRescuerV2.ts \
  --chain-id 31337 \
  --destination 0x1000000000000000000000000000000000000001 \
  --sponsor 0x2000000000000000000000000000000000000002
```

Адреса в примере являются детерминированными локальными фикстурами и не должны
использоваться оператором.

## Результат проверки

Manifest версии `1` фиксирует chain ID, роль контракта, deployment transaction и
block, compiler/settings, происхождение source, SHA-256 artifact, Keccak-256
связанного runtime и immutable `destination`, `self`, `sponsor`. Схема находится
в `deployments/schema/rescuer-manifest.schema.json`.

Go API `internal/contracts.LoadTrustedManifest` повторно связывает runtime из
canonical artifact и immutable values. `AttestDeployment` требует согласия как
минимум двух независимо настроенных read providers по chain ID, одному
finalized block hash, deployment transaction/receipt, runtime и всем трём
getters. Providers должны иметь разные endpoint fingerprints, trust domains и
экземпляры read client. Любая ошибка или расхождение блокирует аттестацию.

Startup ещё не вызывает этот API: интеграция принадлежит Task 05. Успешная
локальная проверка Task 04 не разрешает запуск daemon с production-ключами.
