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
