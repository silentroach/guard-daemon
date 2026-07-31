# Обязательные проверки разработки

GitHub Actions не устанавливает и не вызывает Nix. CI на `ubuntu-24.04`
настраивает Go `1.26.5`, Node.js `24.18.1` и npm `11.16.0` через официальные
setup actions, закреплённые полными commit SHA. `actionlint`, ShellCheck,
`govulncheck` и `gitleaks` также устанавливаются по точным версиям; архив
ShellCheck проверяется по SHA-256.

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
```

Исправление форматирования Go и TypeScript:

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
- `Безопасность / Секреты`.

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
для Go modules/build и npm downloads. `.env`, key files, generated deployment
manifests и иное локальное состояние в cache paths не входят. Workflows имеют
только `contents: read`, не используют GitHub environments и не получают
production secrets.
