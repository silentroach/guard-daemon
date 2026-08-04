# Упаковка systemd

Эти файлы задают поддерживаемую системную службу Linux:

| Исходный файл | Путь пакета |
|---|---|
| `guard-daemon.service` | `/usr/lib/systemd/system/guard-daemon.service` |
| `guard-daemon.sysusers` | `/usr/lib/sysusers.d/guard-daemon.conf` |
| `guard-daemon.tmpfiles` | `/usr/lib/tmpfiles.d/guard-daemon.conf` |
| `guard-daemon.state.env` | `/usr/lib/guard-daemon/guard-daemon.state.env` |
| `scripts/check-systemd-health.sh` | `/usr/lib/guard-daemon/check-systemd-health.sh` |
| `scripts/verify-systemd-account.sh` | `/usr/lib/guard-daemon/verify-systemd-account.sh` |

Пакет должен установить выпуск в
`/usr/lib/guard-daemon/releases/<идентификатор>` с владельцем `root:root` и
атомарно направить `/usr/lib/guard-daemon/current` на этот каталог. В корне
выпуска обязательны исполняемый файл `guard-daemon` с режимом `0755` и
`artifacts/contracts/RescuerV2.json` с режимом `0644`. Пользователь службы не
должен иметь права записи в выпуск или ссылку `current`.

## Учётная запись и каталоги

`systemd-sysusers` создаёт постоянную заблокированную непривилегированную
учётную запись `guard-daemon`. Это не `DynamicUser`: UID назначается при
установке и сохраняется в базе пользователей хоста. Числовой UID нельзя
фиксировать в правилах упаковки; политика исходящих соединений получает его
через `id -u guard-daemon`. Поскольку `systemd-sysusers` не изменяет уже
существующую одноимённую запись, установщик обязан после вызова выполнить
`scripts/verify-systemd-account.sh` из аутентифицированного tooling и отклонить
login-capable, unlocked, несистемную или включённую в дополнительные группы
учётную запись.

`systemd-tmpfiles` создаёт:

- защищённый каталог конфигурации и обязательный пустой файл окружения с
  владельцем `root:guard-daemon`;
- устойчивый каталог состояния `/var/lib/guard-daemon` с режимом `0700`;
- операторский каталог `/var/lib/guard-daemon-operator` с режимом `0755` и
  владельцем `root:root`; tmpfiles не создаёт постоянный restore marker;
- канонический production recovery registry
  `/var/lib/guard-daemon-operator/deployment-recovery` с режимом `0700` и
  владельцем `root:root`;
- канонический staging production manifest
  `/var/lib/guard-daemon-operator/deployment-manifests` с режимом `0700` и
  владельцем `root:root`;
- общий для хоста каталог блокировок
  `/var/tmp/guard-daemon-leases-v1` с режимом `0700`.

`StateDirectory=guard-daemon` повторно обеспечивает существование и владельца
`/var/lib/guard-daemon` при запуске. Unit сначала читает операторский
`/etc/guard-daemon/guard-daemon.env`, затем принадлежащий пакету
`/usr/lib/guard-daemon/guard-daemon.state.env`. Второй файл содержит только
`STATE_DIRECTORY=/var/lib/guard-daemon`, устанавливается как `root:root` с
режимом `0644` и принудительно возвращает канонический путь, даже если
операторский файл ошибочно попытался его переопределить. Такая строка в
операторском файле всё равно запрещена предварительной проверкой.

Deployment CLI для chain ID `1`, `56` и `137` запускает назначенный
root-оператор из аутентифицированного
`/usr/lib/guard-daemon/deployment/<commit>` и всегда использует фиксированные
recovery/manifest каталоги. Полный candidate сохраняется в root-owned
`/usr/lib/guard-daemon/candidates/<commit>`. Пользователь службы `guard-daemon`
не может просматривать эти operator-каталоги, не разрешает, не удаляет и не
архивирует записи. После независимой сверки hash, nonce и receipt оператор
вручную архивирует разрешённый record вне registry; автоматизированному агенту
это действие запрещено.

Процедура восстановления до изменения state атомарно создаёт
`/var/lib/guard-daemon-operator/live-disabled-after-restore` с владельцем
`root:root` и режимом `0444`. Родитель остаётся `root:root 0755`, поэтому процесс
может выполнить `lstat` marker, но не может создать, заменить или удалить его.

## Граница привилегий

Установка пакета, заполнение `/etc/guard-daemon/guard-daemon.env`, установка
манифестов и управление выпуском выполняются от `root`. Процесс работает как
`guard-daemon` без capabilities и без новых привилегий. Файл окружения обязателен
и читается процессом, но изменяется только `root`; рекомендуемый режим `0640`,
владелец `root`, группа `guard-daemon`.

`ProtectSystem=strict` делает файловую систему доступной только для чтения, кроме
явно разрешённых `/var/lib/guard-daemon` и
`/var/tmp/guard-daemon-leases-v1`. Unit дополнительно закрепляет
`/var/lib/guard-daemon-operator` как read-only, делает recovery и manifest
children недоступными процессу и не включает operator tree в `ReadWritePaths`.
`PrivateTmp=no` задан
намеренно: приватный `/var/tmp` разрушил бы общую для хоста блокировку и позволил
бы двум процессам считать себя единственным владельцем одной роли `sponsor`.

`LimitCORE=0` является внешней границей unit. До чтения конфигурации и приватных
ключей Linux binary дополнительно устанавливает собственные hard/soft
`RLIMIT_CORE=0` и `PR_SET_DUMPABLE=0`; невозможность применить любую из этих
политик блокирует startup. Поэтому pipe-based crash collector не получает core
или окружение live-процесса даже когда глобальный `kernel.core_pattern` направлен
в такой collector.

Unit не использует `Type=notify`, `ExecReload`, `DynamicUser` или автоматический
перезапуск. Конфигурация применяется только полной остановкой и новым запуском.
Код `503` от `/healthz` не управляет systemd и не должен использоваться внешним
средством для безусловного перезапуска: `503/stopped` ожидается при
`EMERGENCY_STOP=true`.

Базовый unit не содержит `IPAddressDeny=`. Операторские RPC заранее неизвестны,
поэтому исходящий запрет внедряется на целевом хосте по UID или cgroup согласно
[`docs/operations/outbound-policy.md`](../../docs/operations/outbound-policy.md).

## Действия установщика пакета

После размещения файлов установщик выполняет в таком порядке:

```bash
set -euo pipefail
systemd-sysusers /usr/lib/sysusers.d/guard-daemon.conf
/usr/bin/env -i PATH=/usr/bin:/bin /bin/bash --noprofile --norc \
  /usr/lib/guard-daemon/verify-systemd-account.sh
systemd-tmpfiles --create /usr/lib/tmpfiles.d/guard-daemon.conf
systemctl daemon-reload
```

Установщик не должен автоматически включать или запускать рабочий режим, менять
`DRY_RUN`, добавлять ключи, пополнять `sponsor`, создавать производственный
манифест или отправлять транзакцию. Включение в автозапуск выполняет оператор
только после предварительной проверки:

```bash
set -euo pipefail
systemctl enable guard-daemon.service
```

Полная процедура установки, аварийной остановки, обновления и отката находится
в [`docs/operations/runbook.md`](../../docs/operations/runbook.md).

## Состояние интеграционной проверки

Файлы требуют проверки `systemd-analyze verify` и полной репетиции на целевой
версии systemd. Этот каталог сам по себе не подтверждает, что установка, запуск,
остановка, резервное копирование, обновление и откат уже были выполнены на Linux.
