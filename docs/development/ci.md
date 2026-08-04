# Обязательные проверки разработки

GitHub Actions не устанавливает и не вызывает Nix. CI работает на
`ubuntu-24.04`. Текущие точные версии основных инструментов:

- Go `1.26.5`;
- Node.js `24.18.1`;
- npm `11.16.0`;
- Python `3.14.6`;
- actionlint `1.7.12`;
- ShellCheck `0.11.0`;
- govulncheck `1.1.4`;
- gitleaks `8.30.1`;
- Foundry `1.7.1`;
- Slither `0.11.6` и crytic-compile `0.4.2`;
- solc `0.8.36`, commit `8a079791`;
- PyYAML `6.0.3` для repository policy.

Node.js-инструменты также закреплены в `package-lock.json`: TypeScript `6.0.3`,
tsx `4.23.1`, ESLint `10.8.0`, typescript-eslint `8.65.0` и Prettier `3.9.6`.

Официальные actions закреплены одновременно версией и полным commit SHA:

- `actions/checkout` `v7.0.1`:
  `3d3c42e5aac5ba805825da76410c181273ba90b1`;
- `actions/setup-go` `v7.0.0`:
  `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e`;
- `actions/setup-node` `v7.0.0`:
  `820762786026740c76f36085b0efc47a31fe5020`;
- `actions/setup-python` `v7.0.0`:
  `5fda3b95a4ea91299a34e894583c3862153e4b97`;
- `actions/dependency-review-action` `v4.9.0`:
  `2031cfc080254a8a887f58cffee85186f0e49e48`.

Архивы ShellCheck, Foundry и отдельный solc проверяются по закреплённым SHA-256.
Foundry устанавливается напрямую из release archive без `foundryup`.
Python-зависимости устанавливаются только из hash-locked wheels. Контрактные
тесты используют Prague EVM и усиленный профиль `ci`: 10 000 прогонов каждого
fuzz-теста и 1 000 invariant-прогонов глубиной 256 вызовов.

Локально разрешено войти в необязательное окружение `nix develop`, но все
приведённые ниже команды являются обычными repository commands и не зависят от
Nix.

Установите Node.js-зависимости без изменения lock-файла:

```sh
npm ci
```

Ни одна из следующих команд не выполняет production-развёртывание, не использует
операторские ключи и не обращается к внешнему RPC. Deployment tests запускают
только тестовый Anvil на loopback-интерфейсе и отправляют в него детерминированные
локальные транзакции.

## Команды

Базовая локальная проверка:

```sh
make check
```

Полная проверка безопасности Task 09, включая статический анализ, атакующие
интеграционные тесты, ограниченный фаззинг, repository policy и две чистые
сборки артефактов:

```sh
make security-validation
```

Для `ci-policy` заранее установите hash-locked зависимости:

```sh
python3 -m pip install --only-binary=:all: --require-hashes \
  --requirement test/ci/requirements.txt
```

Отдельные группы проверок:

```sh
make format-check
make workflow-lint
make go-ci
make node-ci
make vuln
make audit
make secret-scan
make contracts-static
make adversarial-test
make fuzz-test
make ci-policy
make reproducibility-check
```

## Проверки эксплуатации и выпуска

Интерфейс репозитория включает следующие цели:

```sh
make documentation-check
make operations-check
make operations-rehearsal
make release-candidate RELEASE_COMMIT=<40hex>
```

`documentation-check` проверяет ссылки, русский язык и соответствие
документации schema. `operations-check` проверяет эксплуатационную
политику, сгенерированные метаданные, схемы и контрольные суммы.
`operations-rehearsal` выполняет только локальный сценарий без
отправки транзакций в публичную сеть. `release-candidate`
принимает только полный immutable commit и создаёт детерминированный
`linux/amd64`, `CGO_ENABLED=0` каталог по процедуре
[../release/process.md](../release/process.md).

Workflow воспроизводимости дважды создаёт release candidate из точного commit,
сравнивает результаты и сканирует оба каталога и вложенные tar-файлы на секреты.
Workflows имеют только `contents: read`, не получают OIDC token и не подписывают артефакты. До Task 11 кандидат не
разрешён к публикации; tag, `push`, GitHub Release и mainnet-действия этими
целями не выполняются.

Исправление форматирования Go, TypeScript и Solidity:

```sh
make format
```

Локальная компиляция Solidity создаёт игнорируемые артефакты в `build/contracts`:

```sh
make contracts-build
make contracts-test
```

## Обязательные проверки ветки

Для защищённой основной ветки следует требовать следующие status checks:

- `Проверки / Форматирование`;
- `Проверки / Проверки Go`;
- `Проверки / Проверки TypeScript и контрактов`;
- `Проверки / Атакующая интеграция Go`;
- `Проверки / Ограниченный фаззинг Go`;
- `Проверки / Изолированные тесты локального развёртывания`;
- `Безопасность / Уязвимости`;
- `Безопасность / Проверка новых зависимостей` для pull request;
- `Безопасность / Секреты`;
- `Безопасность / Политика репозитория`;
- `Безопасность / Воспроизводимые артефакты`;
- `Безопасность / Статический анализ Solidity`.

Рекомендуемые правила branch protection/ruleset:

- запрет прямого push и force-push в основную ветку;
- минимум одно одобрение pull request от участника, не являющегося автором;
- сброс одобрений после изменения diff;
- обязательное разрешение всех review threads;
- запрет merge при незавершённых или неуспешных обязательных проверках;
- запрет автоматического merge dependency major updates без успешных проверок и ручного security review;
- ограничение прав обхода ruleset минимальным списком операторов.

Включение ruleset выполняет оператор в GitHub. Само наличие этого документа не является доказательством включения: перед Task 11 требуется проверить правила через GitHub API и сохранить публичное доказательство без токенов и приватных данных.

## Кеши CI

CI использует встроенные caches `actions/setup-go` и `actions/setup-node` только
для Go modules/build и npm downloads. Cache Foundry отключён, тесты не
обращаются к внешнему RPC. Локальный deployment suite использует только Anvil
на `127.0.0.1`. `.env`, key files, generated deployment manifests и иное
локальное состояние в cache paths не входят. Workflows имеют только
`contents: read`, не используют GitHub environments и не получают production
secrets.
