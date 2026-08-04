# Состав кандидата выпуска

## Каталог

Планируемая команда
`make release-candidate RELEASE_COMMIT=<40hex>` создаёт
`dist/release/<commit>/`. Каталог игнорируется Git и не изменяет дерево исходного
кода.
Имена и набор файлов фиксированы:

| Файл | Назначение | Детерминированный источник |
|---|---|---|
| `guard-daemon-linux-amd64` | Двоичный файл для единственной целевой платформы выпуска `linux/amd64`, `CGO_ENABLED=0` | Пакеты Go из выбранного commit |
| `guard-daemon-source.tar` | Точный снимок отслеживаемого дерева исходного кода | `git archive` с фиксированным префиксом выпуска |
| `LICENSE` | Условия распространения | `LICENSE` из выбранного commit |
| `RescuerV2.json` | Проверенный контрактный артефакт | `artifacts/contracts/RescuerV2.json` из выбранного commit |
| `rescuer-manifest.schema.json` | Схема будущего deployment manifest | `deployments/schema/rescuer-manifest.schema.json` из выбранного commit |
| `guard-daemon.cdx.json` | Перечень компонентов ПО | Собственный Python-генератор, CycloneDX 1.6 JSON |
| `release-candidate.json` | Машиночитаемое описание commit, целевой платформы и состава | Собственный Python-генератор |
| `guard-daemon.intoto.jsonl` | Заявление о входах и результатах сборки | Неподписанная in-toto Statement, SLSA Provenance v1 |
| `SHA256SUMS` | SHA-256 всех остальных файлов | Собственный Python-генератор после создания содержимого |

Любой дополнительный или отсутствующий файл является ошибкой. Русскоязычные
runbooks, закреплённые lock-файлы и отчёты ревью Tasks 01-10 входят в
`guard-daemon-source.tar`, поскольку берутся из release commit. Они не копируются рядом
отдельным неконтролируемым набором.

## Двоичный файл

Двоичный файл строится из snapshot commit, а не из текущего checkout. Параметры
сборки фиксированы:

```text
GOOS=linux
GOARCH=amd64
GOAMD64=v1
CGO_ENABLED=0
GOFIPS140=off
GOWORK=off
GOMODCACHE=/var/tmp/guard-daemon-release-go-mod-v1
-mod=readonly
-trimpath
-buildvcs=false
-ldflags="-s -w -buildid= -X guard-daemon/internal/buildinfo.ReleaseCommit=<commit>"
```

Другие OS, архитектура или режим CGO не являются официальной целевой платформой
и требуют нового явно спроектированного профиля. Генератор не встраивает время,
имя машины, пользователя, путь checkout или значения окружения.
Отладочные секции и таблица символов исключены флагами `-w -s`: они не нужны
runtime-процессу и не входят в канонический release artifact.
Go, npm и Python запускаются через абсолютный `/usr/bin/env -i` и явный
allowlist. Поэтому exported shell function `env`, локальные `GOFIPS140`,
`GOCACHEPROG`, `GOWORK`, `NODE_OPTIONS`, `npm_config_*` и `PYTHON*` не могут
изменить candidate. Фиксированный `GOMODCACHE` создаётся
эксклюзивно с mode `0700`, проверяется через `go mod verify` и удаляется после
сборки; стабильный абсолютный путь исключает module-cache paths из различий
между бинарными файлами.
Для release build требуется upstream distribution Go 1.26.5. Совпадающей строки
версии недостаточно: patched toolchain, который встраивает локальные пути
пакетного менеджера, не является каноническим. Builder завершает сборку с
ошибкой, если binary содержит checkout, output, `GOROOT` или `/nix/store/`.
Проверка выполняется абсолютным `/usr/bin/strings` по полным directory prefixes,
поэтому exported shell function не может скрыть результат, а короткое имя
каталога не совпадает с частью module path.
Документированный fixed `GOMODCACHE` является каноническим build parameter, а
не host-specific path. Nix разрешён для остальных локальных gates, но
Nix-patched Go нельзя использовать как компилятор официального candidate.

## Архив исходного кода

`guard-daemon-source.tar` является точным результатом одной операции:

```sh
GIT_ATTR_NOSYSTEM=1 git -c core.attributesFile=/dev/null archive \
  --format=tar --prefix="guard-daemon-<commit>/" <commit>
```

Префикс является частью канонического формата. Запрещено менять его, заголовки
tar, повторно упаковывать или сжимать результат. Commit должен быть локальным
полным immutable SHA. В архив попадают только отслеживаемые пути этого commit;
неотслеживаемые, игнорируемые и локальные операторские файлы исключены.
Builder дополнительно отключает системные/global attributes и отклоняет
repository-local `info/attributes`, `assume-unchanged` и `skip-worktree`.

`LICENSE`, `RescuerV2.json` и `rescuer-manifest.schema.json` извлекаются из того
же commit. Генератор должен завершиться с ошибкой, если файл отсутствует,
является symlink вместо обычного файла или отличается от соответствующего
содержимого в `guard-daemon-source.tar`.

## SBOM

`guard-daemon.cdx.json` создаёт собственный Python-генератор репозитория без
внешнего SBOM-сервиса. Документ использует `bomFormat: CycloneDX`,
`specVersion: 1.6` и включает основной компонент, модули Go из
`go.mod`/`go.sum` и `go mod graph`, пакеты npm из `package-lock.json`, их версии
и граф зависимостей. Порядок `components`,
`dependencies`, `properties` и ключей стабилен.

Необязательные поля, создающие случайность, запрещены: `serialNumber`,
`metadata.timestamp`, имена узла и пользователя, абсолютный путь и значения
окружения. SBOM описывает закреплённые входы, но сам по себе не доказывает, кто
его создал.

## Метаданные кандидата

`release-candidate.json` содержит только устойчивые данные:

- версию собственной схемы метаданных;
- полный release commit;
- полный hash дерева release commit;
- целевую платформу `linux/amd64` и `cgoEnabled: false`;
- фиксированные пути и SHA-256 двоичного файла, исходного архива, контрактного
  артефакта, схемы манифеста и SBOM.

Файл не содержит URL remotes, branch/tag, времени, сведений об узле или
пользователе, текущего каталога, окружения или адресов развёртывания. Его
собственный SHA-256 находится только в `SHA256SUMS`, чтобы не создавать цикл.

## Сведения о происхождении

`guard-daemon.intoto.jsonl` содержит ровно одну JSON-строку. Это in-toto
Statement v1 с predicate SLSA Provenance v1. Поле `subject` перечисляет
созданные до provenance файлы и их SHA-256. `buildDefinition` фиксирует полный
commit, целевую платформу выпуска и стабильный тип сборки репозитория.
`runDetails` не включает метки времени, runner, имена узла и пользователя, дамп
окружения или персональный URL репозитория.
Список разрешённых входов также содержит SHA-256 фактически выполненного
`scripts/build-release-candidate.sh`, совпадающего с файлом в source archive.

Statement намеренно не подписана. Она позволяет сверить заявленные входы и
выходы, но не подтверждает identity издателя или сборщика. Текущий workflow не
выдаёт OIDC token и не подписывает provenance.

## Контрольные суммы

`SHA256SUMS` содержит SHA-256 каждого другого файла каталога, по одному пути на
строку, в байтовой сортировке по имени. Сам `SHA256SUMS` не перечисляется, чтобы
не создавать рекурсию. Проверка на Linux выполняется из каталога кандидата:

```sh
sha256sum --check --strict SHA256SUMS
```

Совпавшая сумма доказывает только отсутствие изменения относительно полученного
`SHA256SUMS`. Если атакующий заменил и файл, и список сумм, эта проверка не
обнаружит подмену издателя. Для аутентификации нужна отдельная допустимая
подпись: keyless CI с проверенной OIDC identity policy либо подпись оператора
вне агента.

## Что не входит в кандидат

В каталог запрещено включать:

- официальный deployment manifest или production addresses;
- закрытые ключи, seed-фразы, API token, RPC credentials и `.env`;
- ключ или секрет подписи, OIDC token или автоматически созданную подпись;
- логи, метки времени, имена узла и пользователя, абсолютные локальные пути и
  дамп окружения;
- Git tag, данные GitHub Release или результат remote API;
- state database, checkpoints, budget ledger и operator backup.

Официальный deployment manifest создаётся только после существования этого
неизменяемого кандидата. До завершения Task 11 ни один файл каталога не разрешён
к публикации.
