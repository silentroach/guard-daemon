# Обязательные проверки разработки

GitHub Actions не устанавливает и не вызывает Nix. CI на `ubuntu-24.04`
настраивает Go `1.26.5`, Node.js `24.18.1` и npm `11.16.0` через официальные
setup actions, закреплённые полными commit SHA. `actionlint`, ShellCheck,
`govulncheck`, `gitleaks`, Foundry `1.7.1` и Slither `0.11.6` также
устанавливаются по точным версиям; архив ShellCheck проверяется по SHA-256.
Foundry устанавливается напрямую из release archive после проверки SHA-256,
без `foundryup`. Статический анализ использует Python `3.14.6`, полный
hash-locked набор Python wheels и отдельный checksummed `solc 0.8.36`.
Контрактные тесты используют Prague EVM и усиленный профиль `ci`: 10 000
прогонов каждого fuzz-теста и 1 000 invariant-прогонов глубиной 256 вызовов.

Локально разрешено войти в необязательное окружение `nix develop`, но все
приведённые ниже команды являются обычными repository commands и не зависят от
Nix.

Установите Node.js-зависимости без изменения lock-файла:

```sh
npm ci
```

Ни одна из следующих команд не выполняет deployment, не подписывает транзакции и не обращается к RPC.

## Команды

Полная локальная проверка:

```sh
make check
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
```

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

- `CI / Форматирование`;
- `CI / Go`;
- `CI / TypeScript и контракты`;
- `Безопасность / Уязвимости`;
- `Безопасность / Секреты`;
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
обращаются к RPC. `.env`, key files, generated deployment manifests и иное
локальное состояние в cache paths не входят. Workflows имеют только
`contents: read`, не используют GitHub environments и не получают production
secrets.
