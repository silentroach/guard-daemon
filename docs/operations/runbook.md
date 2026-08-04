# Руководство оператора systemd

Это руководство относится к системной службе из `packaging/systemd`. Установка,
изменение `/etc`, переключение выпуска и управление службой требуют прав `root`.
Сам демон запускается отдельным непривилегированным пользователем
`guard-daemon`; запуск демона от `root` не поддерживается.

`source` считается скомпрометированным всегда. Демон не восстанавливает
секретность ключа, не гарантирует победу в гонке и не делает прежний EOA
безопасным для повторного использования.

## Установка

Поддерживаемая раскладка одного выпуска использует в качестве
`<release-commit>` полный 40-символьный commit из проверенного
`release-candidate.json`:

```text
/usr/lib/guard-daemon/releases/<release-commit>/guard-daemon
/usr/lib/guard-daemon/releases/<release-commit>/artifacts/contracts/RescuerV2.json
/usr/lib/guard-daemon/candidates/<release-commit>/release-candidate.json
/usr/lib/guard-daemon/deployment/<release-commit>/scripts/deployRescuerV2.ts
/usr/lib/guard-daemon/deployment/<release-commit>/node_modules/
/usr/lib/guard-daemon/current -> releases/<release-commit>
/usr/lib/guard-daemon/guard-daemon.state.env
/etc/guard-daemon/guard-daemon.env
/etc/guard-daemon/manifests/
/var/lib/guard-daemon/
/var/lib/guard-daemon-operator/deployment-recovery/
/var/lib/guard-daemon-operator/deployment-manifests/
/var/lib/guard-daemon-operator/live-disabled-after-restore
/var/tmp/guard-daemon-leases-v1/
```

Путь `live-disabled-after-restore` отсутствует до первого восстановления и
создаётся только процедурой восстановления. Каталоги под `/var/lib` и `/etc`
создаёт `systemd-tmpfiles`; release, candidate, tooling и ссылку `current`
создаёт приведённая ниже процедура установки.

Самосогласованные `SHA256SUMS` и неподписанный provenance не аутентифицируют
издателя. До первого исполнения candidate binary оператор обязан получить по
независимому доверенному каналу полный release commit и SHA-256 самого файла
`SHA256SUMS`. Допустимый источник digest — проверенная подпись keyless CI с
точной identity policy, подпись оператора вне агента либо результат независимой
воспроизводимой сборки того же commit. Без такого значения установка
блокируется.

`RELEASE_SOURCE` указывает на полученный каталог кандидата. Установка не
исполняет код и не читает Git metadata из исходного checkout: candidate сначала
копируется в root-owned staging, `SHA256SUMS` аутентифицируется доверенным
digest, а tooling извлекается из уже проверенного source archive. Только этот
root-owned tooling запускает verifier и поставляет packaging-файлы. Оператор
сохраняет проверенный блок как `root:root 0700` вне candidate и checkout, после
чего trusted process manager напрямую запускает его через
`/bin/bash --noprofile --norc` с новым allowlist-окружением
`PATH=/usr/bin:/bin`, `LANG=C.UTF-8`, `TZ=UTC`. `BASH_ENV`, exported functions,
`ENV`, `NODE_*`, `LD_*`, `DYLD_*`, `HOME` и `npm_config_*` не наследуются;
разрешённые npm config ниже задаются заново:

```bash
set -euo pipefail
test "$(id -u)" -eq 0
test "$PATH" = '/usr/bin:/bin'
for FORBIDDEN_ENV in BASH_ENV ENV NODE_OPTIONS NODE_PATH LD_PRELOAD LD_LIBRARY_PATH DYLD_INSERT_LIBRARIES HOME npm_config_script_shell; do
  if /usr/bin/printenv "$FORBIDDEN_ENV" >/dev/null; then exit 1; fi
done
umask 077
RELEASE_SOURCE='<путь-к-полученному-каталогу-выпуска>'
TRUSTED_RELEASE_COMMIT='<проверенный-полный-40hex-commit>'
TRUSTED_SHA256SUMS_SHA256='<проверенный-64hex-sha256-файла-SHA256SUMS>'
ACTIVATE_RELEASE='<true-только-для-первой-установки-или-false-для-update-stage>'
test "${#TRUSTED_RELEASE_COMMIT}" -eq 40
case "$TRUSTED_RELEASE_COMMIT" in *[!0-9a-f]*) exit 1 ;; esac
test "${#TRUSTED_SHA256SUMS_SHA256}" -eq 64
case "$TRUSTED_SHA256SUMS_SHA256" in *[!0-9a-f]*) exit 1 ;; esac
case "$ACTIVATE_RELEASE" in true|false) ;; *) exit 1 ;; esac
NPM_GLOBAL_CONFIG=/var/empty/guard-daemon-npm-globalconfig
NPM_USER_CONFIG=/var/empty/guard-daemon-npm-userconfig
for NPM_CONFIG_PATH in "$NPM_GLOBAL_CONFIG" "$NPM_USER_CONFIG"; do
  test ! -e "$NPM_CONFIG_PATH"
  test ! -L "$NPM_CONFIG_PATH"
done
RELEASE_SOURCE="$(CDPATH='' cd -- "$RELEASE_SOURCE" && pwd -P)" || exit 1

GUARD_ROOT=/usr/lib/guard-daemon
RELEASE_PARENT="$GUARD_ROOT/releases"
CANDIDATE_PARENT="$GUARD_ROOT/candidates"
TOOLING_PARENT="$GUARD_ROOT/deployment"
install -d -o root -g root -m 0755 \
  "$GUARD_ROOT" "$RELEASE_PARENT" "$CANDIDATE_PARENT" "$TOOLING_PARENT"
CANDIDATE_STAGING="$(mktemp -d "$CANDIDATE_PARENT/.candidate.tmp.XXXXXX")"
TOOLING_STAGING="$(mktemp -d "$TOOLING_PARENT/.deployment.tmp.XXXXXX")"
RELEASE_STAGING=''
NPM_CACHE="$(mktemp -d /var/tmp/guard-daemon-npm.XXXXXX)"
SOURCE_LIST="$(mktemp /var/tmp/guard-daemon-source-list.XXXXXX)"
TOOLING_LINKS="$(mktemp /var/tmp/guard-daemon-tooling-links.XXXXXX)"
trap 'if test -n "$RELEASE_STAGING"; then rm -rf -- "$RELEASE_STAGING"; fi; if test -n "$TOOLING_STAGING"; then rm -rf -- "$TOOLING_STAGING"; fi; if test -n "$CANDIDATE_STAGING"; then rm -rf -- "$CANDIDATE_STAGING"; fi; rm -rf -- "$NPM_CACHE"; rm -f -- "$SOURCE_LIST" "$TOOLING_LINKS"' EXIT
for NAME in \
  LICENSE \
  RescuerV2.json \
  SHA256SUMS \
  guard-daemon-linux-amd64 \
  guard-daemon-source.tar \
  guard-daemon.cdx.json \
  guard-daemon.intoto.jsonl \
  release-candidate.json \
  rescuer-manifest.schema.json; do
  test -f "$RELEASE_SOURCE/$NAME"
  test ! -L "$RELEASE_SOURCE/$NAME"
  install -o root -g root -m 0644 "$RELEASE_SOURCE/$NAME" "$CANDIDATE_STAGING/$NAME"
done
chmod 0755 "$CANDIDATE_STAGING/guard-daemon-linux-amd64"
test "$(stat -c '%U:%G %a' "$CANDIDATE_STAGING")" = 'root:root 700'
(
  cd "$CANDIDATE_STAGING"
  printf '%s  SHA256SUMS\n' "$TRUSTED_SHA256SUMS_SHA256" | sha256sum --check --strict -
  sha256sum --check --strict SHA256SUMS
)

SOURCE_PREFIX="guard-daemon-$TRUSTED_RELEASE_COMMIT"
tar --list --file="$CANDIDATE_STAGING/guard-daemon-source.tar" > "$SOURCE_LIST"
test "$(grep -Ec "^$SOURCE_PREFIX(/|$)" "$SOURCE_LIST")" -gt 0
MATCH_STATUS=0
grep -Eq '(^|/)\.\.(/|$)|^/' "$SOURCE_LIST" || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
MATCH_STATUS=0
grep -Evq "^$SOURCE_PREFIX(/|$)" "$SOURCE_LIST" || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
tar --extract --file="$CANDIDATE_STAGING/guard-daemon-source.tar" \
  --directory="$TOOLING_STAGING" --strip-components=1 \
  --no-same-owner --no-same-permissions
for SOURCE_PATH in \
  LICENSE \
  artifacts/contracts/RescuerV2.json \
  deployments/schema/rescuer-manifest.schema.json \
  package.json \
  package-lock.json \
  packaging/systemd/guard-daemon.service \
  packaging/systemd/guard-daemon.state.env \
  packaging/systemd/guard-daemon.sysusers \
  packaging/systemd/guard-daemon.tmpfiles \
  scripts/deployRescuerV2.ts \
  scripts/deployment.ts \
  scripts/release_metadata.py; do
  test -f "$TOOLING_STAGING/$SOURCE_PATH"
  test ! -L "$TOOLING_STAGING/$SOURCE_PATH"
done
cmp "$TOOLING_STAGING/LICENSE" "$CANDIDATE_STAGING/LICENSE"
cmp "$TOOLING_STAGING/artifacts/contracts/RescuerV2.json" "$CANDIDATE_STAGING/RescuerV2.json"
cmp "$TOOLING_STAGING/deployments/schema/rescuer-manifest.schema.json" "$CANDIDATE_STAGING/rescuer-manifest.schema.json"
NODE_BINARY=/usr/bin/node
NPM_BINARY=/usr/bin/npm
PYTHON_BINARY=/usr/bin/python3
for SYSTEM_TOOL in "$NODE_BINARY" "$NPM_BINARY" "$PYTHON_BINARY"; do
  test -e "$SYSTEM_TOOL"
  test "$(stat -c '%U:%G' "$SYSTEM_TOOL")" = 'root:root'
done
test ! -L "$NODE_BINARY"
test -f "$NODE_BINARY"
test "$(env -i PATH=/usr/bin:/bin LANG=C.UTF-8 TZ=UTC "$NODE_BINARY" --version)" = 'v24.18.1'
test "$(env -i PATH=/usr/bin:/bin LANG=C.UTF-8 TZ=UTC npm_config_globalconfig="$NPM_GLOBAL_CONFIG" npm_config_registry=https://registry.npmjs.org/ npm_config_userconfig="$NPM_USER_CONFIG" "$NPM_BINARY" --version)" = '11.16.0'
test "$(env -i PATH=/usr/bin:/bin LANG=C.UTF-8 TZ=UTC "$PYTHON_BINARY" --version)" = 'Python 3.14.6'
"$PYTHON_BINARY" -I -B "$TOOLING_STAGING/scripts/release_metadata.py" verify --directory "$CANDIDATE_STAGING"
RELEASE_ID="$("$PYTHON_BINARY" -I -B -c 'import json, pathlib, sys; print(json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))["releaseCommit"])' "$CANDIDATE_STAGING/release-candidate.json")"
RELEASE_TREE="$("$PYTHON_BINARY" -I -B -c 'import json, pathlib, sys; print(json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))["releaseTree"])' "$CANDIDATE_STAGING/release-candidate.json")"
test "${#RELEASE_ID}" -eq 40
case "$RELEASE_ID" in *[!0-9a-f]*) exit 1 ;; esac
test "$RELEASE_ID" = "$TRUSTED_RELEASE_COMMIT"
test "${#RELEASE_TREE}" -eq 40
case "$RELEASE_TREE" in *[!0-9a-f]*) exit 1 ;; esac

(
  cd "$TOOLING_STAGING"
  env -i PATH=/usr/bin:/bin LANG=C.UTF-8 TZ=UTC NODE_OPTIONS='' npm_config_cache="$NPM_CACHE" npm_config_globalconfig="$NPM_GLOBAL_CONFIG" npm_config_registry=https://registry.npmjs.org/ npm_config_userconfig="$NPM_USER_CONFIG" "$NPM_BINARY" ci --ignore-scripts --no-audit --no-fund
  env -i PATH=/usr/bin:/bin LANG=C.UTF-8 TZ=UTC NODE_OPTIONS='' npm_config_cache="$NPM_CACHE" npm_config_globalconfig="$NPM_GLOBAL_CONFIG" npm_config_registry=https://registry.npmjs.org/ npm_config_userconfig="$NPM_USER_CONFIG" "$NPM_BINARY" run artifacts:verify
)
chown -R root:root "$TOOLING_STAGING"
chmod -R go-w "$TOOLING_STAGING"
chmod 0700 "$TOOLING_STAGING"
TOOLING_OWNERSHIP="$(find "$TOOLING_STAGING" -xdev \( ! -user root -o ! -group root \) -print -quit)" || exit 1
test -z "$TOOLING_OWNERSHIP"
TOOLING_WRITABLE="$(find "$TOOLING_STAGING" -xdev ! -type l -perm /022 -print -quit)" || exit 1
test -z "$TOOLING_WRITABLE"
find "$TOOLING_STAGING" -xdev -type l -print0 > "$TOOLING_LINKS" || exit 1
while IFS= read -r -d '' LINK_PATH; do
  LINK_TARGET="$(readlink -f -- "$LINK_PATH")" || exit 1
  case "$LINK_TARGET" in "$TOOLING_STAGING"/*) ;; *) exit 1 ;; esac
done < "$TOOLING_LINKS"
test "$("$CANDIDATE_STAGING/guard-daemon-linux-amd64" --version)" = "$RELEASE_ID"

RELEASE_TARGET="$RELEASE_PARENT/$RELEASE_ID"
CANDIDATE_TARGET="$CANDIDATE_PARENT/$RELEASE_ID"
TOOLING_TARGET="$TOOLING_PARENT/$RELEASE_ID"
test ! -e "$RELEASE_TARGET"
test ! -L "$RELEASE_TARGET"
test ! -e "$CANDIDATE_TARGET"
test ! -L "$CANDIDATE_TARGET"
test ! -e "$TOOLING_TARGET"
test ! -L "$TOOLING_TARGET"
mv -Tn -- "$CANDIDATE_STAGING" "$CANDIDATE_TARGET"
test ! -e "$CANDIDATE_STAGING"
CANDIDATE_STAGING=''
mv -Tn -- "$TOOLING_STAGING" "$TOOLING_TARGET"
test ! -e "$TOOLING_STAGING"
TOOLING_STAGING=''

RELEASE_STAGING="$(mktemp -d "$RELEASE_PARENT/.${RELEASE_ID}.tmp.XXXXXX")"
install -d -o root -g root -m 0755 "$RELEASE_STAGING"
install -d -o root -g root -m 0755 "$RELEASE_STAGING/artifacts/contracts"
install -o root -g root -m 0755 "$CANDIDATE_TARGET/guard-daemon-linux-amd64" "$RELEASE_STAGING/guard-daemon"
install -o root -g root -m 0644 "$CANDIDATE_TARGET/RescuerV2.json" "$RELEASE_STAGING/artifacts/contracts/RescuerV2.json"
test ! -e "$RELEASE_TARGET"
test ! -L "$RELEASE_TARGET"
mv -Tn -- "$RELEASE_STAGING" "$RELEASE_TARGET"
test ! -e "$RELEASE_STAGING"
test ! -L "$RELEASE_STAGING"
RELEASE_STAGING=''
test "$("$RELEASE_TARGET/guard-daemon" --version)" = "$RELEASE_ID"

if test "$ACTIVATE_RELEASE" = true; then
  test ! -e /usr/lib/guard-daemon/current
  test ! -L /usr/lib/guard-daemon/current
  install -D -o root -g root -m 0644 "$TOOLING_TARGET/packaging/systemd/guard-daemon.service" /usr/lib/systemd/system/guard-daemon.service
  install -D -o root -g root -m 0644 "$TOOLING_TARGET/packaging/systemd/guard-daemon.sysusers" /usr/lib/sysusers.d/guard-daemon.conf
  install -D -o root -g root -m 0644 "$TOOLING_TARGET/packaging/systemd/guard-daemon.tmpfiles" /usr/lib/tmpfiles.d/guard-daemon.conf
  install -D -o root -g root -m 0644 "$TOOLING_TARGET/packaging/systemd/guard-daemon.state.env" /usr/lib/guard-daemon/guard-daemon.state.env
  systemd-sysusers /usr/lib/sysusers.d/guard-daemon.conf
  systemd-tmpfiles --create /usr/lib/tmpfiles.d/guard-daemon.conf
  ln -s "releases/$RELEASE_ID" /usr/lib/guard-daemon/current.new
  mv -Tn /usr/lib/guard-daemon/current.new /usr/lib/guard-daemon/current
  test ! -e /usr/lib/guard-daemon/current.new
  systemctl daemon-reload
fi
```

Каталоги `RELEASE_TARGET`, `CANDIDATE_TARGET` и `TOOLING_TARGET` обязаны
отсутствовать. Команды никогда не дописывают существующий release или прежний
rollback-каталог: каждый результат сначала создаётся в root-owned staging на том
же файловом разделе, а `mv -Tn` публикует целый каталог без перезаписи. Candidate
не исполняет ни одного байта до проверки доверенного digest; verifier,
`node_modules` и packaging читаются только из tooling, извлечённого из его
аутентифицированного source archive. Trap удаляет только не опубликованные
staging-каталоги и npm cache. Опубликованный immutable каталог после более
позднего отказа не переиспользуют и не исправляют на месте.

Для первой установки, когда `current` гарантированно отсутствует, задайте
`ACTIVATE_RELEASE=true`: процедура установит unit и создаст initial symlink, но
не включит и не запустит службу. Для подготовки обновления обязательно задайте
`ACTIVATE_RELEASE=false`; в этом режиме публикуются только candidate, tooling и
release, а действующие unit и `current` не меняются.

Установите отдельно проверенный манифест каждой включённой сети в
`/etc/guard-daemon/manifests/`: владелец `root`, группа `guard-daemon`, режим
`0640`. Заполните `/etc/guard-daemon/guard-daemon.env` через `sudoedit`. Файл
обязан принадлежать `root:guard-daemon`, иметь режим `0640` и не должен быть
доступен на запись пользователю службы. Полный перечень полей находится в
[`docs/configuration.md`](../configuration.md).

Не создавайте `.env` в каталоге выпуска. `STATE_DIRECTORY` запрещено задавать в
операторском `guard-daemon.env`. Unit читает этот файл первым, а package-owned
`/usr/lib/guard-daemon/guard-daemon.state.env` вторым, поэтому последнее значение
всегда принудительно равно `/var/lib/guard-daemon`.

## Предварительная проверка

Выполните проверки до первого запуска и после изменения unit-файла:

```bash
set -euo pipefail
systemd-analyze verify /usr/lib/systemd/system/guard-daemon.service
test "$(stat -c '%U:%G %a' /etc/guard-daemon/guard-daemon.env)" = 'root:guard-daemon 640'
test "$(stat -c '%U:%G %a' /usr/lib/guard-daemon/guard-daemon.state.env)" = 'root:root 644'
test "$(cat /usr/lib/guard-daemon/guard-daemon.state.env)" = 'STATE_DIRECTORY=/var/lib/guard-daemon'
test "$(stat -c '%U:%G %a' /var/lib/guard-daemon)" = 'guard-daemon:guard-daemon 700'
test "$(stat -c '%U:%G %a' /var/lib/guard-daemon-operator)" = 'root:root 755'
test "$(stat -c '%U:%G %a' /var/lib/guard-daemon-operator/deployment-recovery)" = 'root:root 700'
test "$(stat -c '%U:%G %a' /var/lib/guard-daemon-operator/deployment-manifests)" = 'root:root 700'
test "$(stat -c '%U:%G %a' /var/tmp/guard-daemon-leases-v1)" = 'guard-daemon:guard-daemon 700'
test "$(stat -c '%U:%G' /usr/lib/guard-daemon/current/guard-daemon)" = 'root:root'
test -x /usr/lib/guard-daemon/current/guard-daemon
test -r /usr/lib/guard-daemon/current/artifacts/contracts/RescuerV2.json
MATCH_STATUS=0
grep -q '^STATE_DIRECTORY=' /etc/guard-daemon/guard-daemon.env || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
systemctl cat guard-daemon.service
```

Проверьте, что для каждой сети настроены два независимых источника чтения HTTP и
WebSocket, отдельный URL отправки для рабочего режима и доверенный манифест.
Для официального manifest задайте `RESCUER_RELEASE_COMMIT_<N>` равным полному
release commit именно этого manifest из проверенных неизменяемых метаданных.
Commit текущего бинарного файла, который выводит `guard-daemon --version`, не
подставляется вместо deployment identity автоматически.
Производственная эксплуатация требует `https://` и `wss://`; политика исходящих
соединений описана в [отдельном документе](outbound-policy.md).

Межпроцессная блокировка защищает только один хост. До рабочего запуска
оператор обязан подтвердить, что другой хост не использует ту же пару
`chain+sponsor`.

У демона нет отдельной команды проверки конфигурации. Полная проверка включает
открытие состояния и сетевые чтения и выполняется только при запуске. Поэтому
первый запуск делается без ключей и без отправки.

## Первый запуск без отправки

В `guard-daemon.env` установите `DRY_RUN=true`, `EMERGENCY_STOP=false`, удалите
строки `SOURCE_PRIVATE_KEY`, `SPONSOR_PRIVATE_KEY` и все
`RPC_BROADCAST_HTTP_<N>`. Затем:

```bash
set -euo pipefail
test "$(grep -c '^DRY_RUN=true$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -c '^EMERGENCY_STOP=false$' /etc/guard-daemon/guard-daemon.env)" -eq 1
MATCH_STATUS=0
grep -Eq '^(SOURCE_PRIVATE_KEY|SPONSOR_PRIVATE_KEY|RPC_BROADCAST_HTTP_[A-Z][A-Z0-9_]*)=' /etc/guard-daemon/guard-daemon.env || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
MATCH_STATUS=0
grep -q '^STATE_DIRECTORY=' /etc/guard-daemon/guard-daemon.env || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
test "$(cat /usr/lib/guard-daemon/guard-daemon.state.env)" = 'STATE_DIRECTORY=/var/lib/guard-daemon'
test -x /usr/lib/guard-daemon/current/guard-daemon
test -r /usr/lib/guard-daemon/current/artifacts/contracts/RescuerV2.json
systemctl start guard-daemon.service
systemctl is-active guard-daemon.service
curl --silent --show-error --unix-socket /var/lib/guard-daemon/diagnostics.sock --write-out '\nHTTP %{http_code}\n' http://localhost/healthz
```

Сразу после запуска допустим `503` с `degraded_rpc`, пока все сетевые поколения
не открыли рабочую сессию. Для готового режима без отправки ожидается `200` и
`healthy_idle`. `DRY_RUN=true` не создаёт подписывающие компоненты и не
отправляет транзакции.

## Аттестация перед рабочим запуском

Остановите проверочный процесс и подтвердите `MainPID=0`:

```bash
set -euo pipefail
systemctl stop guard-daemon.service
test "$(systemctl show guard-daemon.service --property=MainPID --value)" = '0'
test "$(systemctl show guard-daemon.service --property=ActiveState --value)" = 'inactive'
systemctl show guard-daemon.service --property=ActiveState --property=SubState --property=Result --property=MainPID
```

В защищённом файле окружения установите `DRY_RUN=false` и
`EMERGENCY_STOP=true`, добавьте отдельный `RPC_BROADCAST_HTTP_<N>` для каждой
сети и убедитесь, что оба приватных ключа отсутствуют. После независимой
операторской проверки ролей, лимитов, манифестов и отсутствия второго активного
хоста выполните:

```bash
set -euo pipefail
test "$(grep -c '^DRY_RUN=false$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -c '^EMERGENCY_STOP=true$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -Ec '^RPC_BROADCAST_HTTP_[A-Z][A-Z0-9_]*=' /etc/guard-daemon/guard-daemon.env)" -gt 0
MATCH_STATUS=0
grep -Eq '^(SOURCE_PRIVATE_KEY|SPONSOR_PRIVATE_KEY|STATE_DIRECTORY)=' /etc/guard-daemon/guard-daemon.env || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
test "$(cat /usr/lib/guard-daemon/guard-daemon.state.env)" = 'STATE_DIRECTORY=/var/lib/guard-daemon'
test -x /usr/lib/guard-daemon/current/guard-daemon
test -r /usr/lib/guard-daemon/current/artifacts/contracts/RescuerV2.json
systemctl start guard-daemon.service
systemctl is-active guard-daemon.service
curl --silent --show-error --unix-socket /var/lib/guard-daemon/diagnostics.sock --write-out '\nHTTP %{http_code}\n' http://localhost/healthz
```

Ожидаются активная служба, HTTP `503`, состояние `stopped` и предупреждение
`paid_actions_stopped`. Такой запуск проверяет манифест и аттестацию двумя RPC
чтения, открывает состояние и блокировки, но не создаёт подписывающие компоненты
и не подключается к RPC отправки. Любой отказ должен блокировать продолжение.

## Рабочий запуск

Только после сохранения результатов аварийной аттестации остановите службу. В
защищённом файле окружения установите `EMERGENCY_STOP=false`, добавьте оба ключа
и не меняйте остальные проверенные поля. Ограничьте баланс горячего `sponsor`
утверждённым объёмом, затем запустите службу заново:

```bash
set -euo pipefail
systemctl stop guard-daemon.service
test "$(systemctl show guard-daemon.service --property=MainPID --value)" = '0'
test "$(systemctl show guard-daemon.service --property=ActiveState --value)" = 'inactive'
sudoedit /etc/guard-daemon/guard-daemon.env
test "$(grep -c '^DRY_RUN=false$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -c '^EMERGENCY_STOP=false$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -c '^SOURCE_PRIVATE_KEY=' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -c '^SPONSOR_PRIVATE_KEY=' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -Ec '^RPC_BROADCAST_HTTP_[A-Z][A-Z0-9_]*=' /etc/guard-daemon/guard-daemon.env)" -gt 0
MATCH_STATUS=0
grep -q '^STATE_DIRECTORY=' /etc/guard-daemon/guard-daemon.env || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
test "$(cat /usr/lib/guard-daemon/guard-daemon.state.env)" = 'STATE_DIRECTORY=/var/lib/guard-daemon'
test -x /usr/lib/guard-daemon/current/guard-daemon
test -r /usr/lib/guard-daemon/current/artifacts/contracts/RescuerV2.json
test ! -e /var/lib/guard-daemon-operator/live-disabled-after-restore
test ! -L /var/lib/guard-daemon-operator/live-disabled-after-restore
systemctl start guard-daemon.service
systemctl is-active guard-daemon.service
curl --silent --show-error --unix-socket /var/lib/guard-daemon/diagnostics.sock --write-out '\nHTTP %{http_code}\n' http://localhost/healthz
```

Live startup повторно проверяет аттестацию, состояние и блокировки до создания
signer, затем сверяет адреса подписывающих ролей до первой подписи.

## Проверка состояния

```bash
set -euo pipefail
systemctl is-active guard-daemon.service
curl --silent --show-error --unix-socket /var/lib/guard-daemon/diagnostics.sock --write-out '\nHTTP %{http_code}\n' http://localhost/healthz
curl --silent --show-error --unix-socket /var/lib/guard-daemon/diagnostics.sock http://localhost/metrics
```

Точная схема ответов и действия для каждого состояния приведены в
[`monitoring.md`](monitoring.md). `503` не означает, что процесс следует
перезапускать. В частности, `503` со `stopped` является ожидаемым результатом
аварийной остановки платных действий. Не настраивайте цикл перезапуска по коду
`/healthz`; сначала разберите поле `status` и активные предупреждения.

## Штатная остановка

```bash
set -euo pipefail
systemctl stop guard-daemon.service
test "$(systemctl show guard-daemon.service --property=MainPID --value)" = '0'
test "$(systemctl show guard-daemon.service --property=ActiveState --value)" = 'inactive'
test "$(systemctl show guard-daemon.service --property=Result --value)" = 'success'
systemctl show guard-daemon.service --property=ActiveState --property=SubState --property=Result --property=MainPID
```

Ожидаются `inactive`, `MainPID=0` и успешный результат. systemd посылает
`SIGTERM` и ждёт не более 120 секунд, пока демон закроет хранилища и снимет
блокировки. Если systemd принудительно завершил процесс по истечении срока,
состояние нельзя считать согласованным после остановки; не создавайте резервную
копию до расследования.

## Аварийная остановка платных действий

Эта процедура сохраняет наблюдение, но запрещает новые подписи и отправку.
Порядок обязателен: сначала остановить процесс, затем убрать ключи, затем
запустить аварийный режим.

```bash
set -euo pipefail
systemctl stop guard-daemon.service
test "$(systemctl show guard-daemon.service --property=MainPID --value)" = '0'
test "$(systemctl show guard-daemon.service --property=ActiveState --value)" = 'inactive'
sudoedit /etc/guard-daemon/guard-daemon.env
test "$(grep -c '^DRY_RUN=false$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -c '^EMERGENCY_STOP=true$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -Ec '^RPC_BROADCAST_HTTP_[A-Z][A-Z0-9_]*=' /etc/guard-daemon/guard-daemon.env)" -gt 0
MATCH_STATUS=0
grep -Eq '^(SOURCE_PRIVATE_KEY|SPONSOR_PRIVATE_KEY|STATE_DIRECTORY)=' /etc/guard-daemon/guard-daemon.env || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
test "$(cat /usr/lib/guard-daemon/guard-daemon.state.env)" = 'STATE_DIRECTORY=/var/lib/guard-daemon'
test -x /usr/lib/guard-daemon/current/guard-daemon
test -r /usr/lib/guard-daemon/current/artifacts/contracts/RescuerV2.json
systemctl start guard-daemon.service
systemctl is-active guard-daemon.service
curl --silent --show-error --unix-socket /var/lib/guard-daemon/diagnostics.sock --write-out '\nHTTP %{http_code}\n' http://localhost/healthz
curl --silent --show-error --unix-socket /var/lib/guard-daemon/diagnostics.sock http://localhost/metrics
```

Перед `start` удалите из файла обе строки приватных ключей и установите ровно
`DRY_RUN=false` и `EMERGENCY_STOP=true`. Оставьте
`RPC_BROADCAST_HTTP_<N>`: рабочая схема конфигурации требует это поле, хотя в
аварийном режиме компонент отправки не создаётся.

Ожидаемый результат: служба активна, `/healthz` возвращает HTTP `503` и
`{"status":"stopped",...}`, а `/metrics` содержит активное предупреждение
`paid_actions_stopped` для каждой сети. Не перезапускайте службу из-за этого
`503` и отключите такой перезапуск во внешней системе надзора.

После остановки платных действий отдельно ограничьте или изолируйте средства
`sponsor`, сохранив сведения об инциденте без ключей и подписей. Удаление ключа
`source` из службы не делает `source` безопасным: атакующий по-прежнему может
иметь его копию и создавать конкурирующие транзакции и авторизации.

## Обновление

1. Проверьте новый выпуск и выполните процедуру выше с
   `ACTIVATE_RELEASE=false`. Она публикует новый release, candidate и tooling, но
   не меняет `current` или установленный unit. Не переиспользуйте каталог уже
   установленного или rollback-выпуска.
2. До снимка штатно остановите службу, отключите автозапуск и установите
   постоянную mask. Затем удалите ключи, установите `DRY_RUN=false` и
   `EMERGENCY_STOP=true`; отдельные URL отправки оставьте. Mask сохраняется при
   reboot и не снимается до emergency-запуска нового выпуска:

```bash
set -euo pipefail
test "$(systemctl is-enabled guard-daemon.service)" = 'enabled'
systemctl stop guard-daemon.service
test "$(systemctl show guard-daemon.service --property=MainPID --value)" = '0'
test "$(systemctl show guard-daemon.service --property=ActiveState --value)" = 'inactive'
systemctl disable guard-daemon.service
DISABLED_STATE=''
DISABLED_STATE="$(systemctl is-enabled guard-daemon.service 2>/dev/null)" || true
test "$DISABLED_STATE" = 'disabled'
systemctl mask guard-daemon.service
MASKED_STATE=''
MASKED_STATE="$(systemctl is-enabled guard-daemon.service 2>/dev/null)" || true
test "$MASKED_STATE" = 'masked'
sudoedit /etc/guard-daemon/guard-daemon.env
test "$(grep -c '^DRY_RUN=false$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -c '^EMERGENCY_STOP=true$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -Ec '^RPC_BROADCAST_HTTP_[A-Z][A-Z0-9_]*=' /etc/guard-daemon/guard-daemon.env)" -gt 0
MATCH_STATUS=0
grep -Eq '^(SOURCE_PRIVATE_KEY|SPONSOR_PRIVATE_KEY|STATE_DIRECTORY)=' /etc/guard-daemon/guard-daemon.env || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
```

3. Пока service masked, создайте единый снимок состояния по
   [`backup-restore.md`](backup-restore.md). Этот снимок обязателен до первого
   запуска нового исполняемого файла, потому что открытие может мигрировать
   схему состояния. Сбой или reboot оставляет service masked.
4. Не снимая mask, установите unit, атомарно переключите ссылку и перечитайте
   systemd. Только затем снимите mask, вручную запустите emergency-режим и
   восстановите автозапуск после успешной проверки:

```bash
set -euo pipefail
RELEASE_ID='<полный-40hex-release-commit-нового-выпуска>'
test "${#RELEASE_ID}" -eq 40
case "$RELEASE_ID" in *[!0-9a-f]*) exit 1 ;; esac
MASKED_STATE=''
MASKED_STATE="$(systemctl is-enabled guard-daemon.service 2>/dev/null)" || true
test "$MASKED_STATE" = 'masked'
test "$(systemctl show guard-daemon.service --property=MainPID --value)" = '0'
test "$(systemctl show guard-daemon.service --property=ActiveState --value)" = 'inactive'
test "$(grep -c '^DRY_RUN=false$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -c '^EMERGENCY_STOP=true$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -Ec '^RPC_BROADCAST_HTTP_[A-Z][A-Z0-9_]*=' /etc/guard-daemon/guard-daemon.env)" -gt 0
MATCH_STATUS=0
grep -Eq '^(SOURCE_PRIVATE_KEY|SPONSOR_PRIVATE_KEY|STATE_DIRECTORY)=' /etc/guard-daemon/guard-daemon.env || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
test -x "/usr/lib/guard-daemon/releases/$RELEASE_ID/guard-daemon"
test -r "/usr/lib/guard-daemon/releases/$RELEASE_ID/artifacts/contracts/RescuerV2.json"
test "$("/usr/lib/guard-daemon/releases/$RELEASE_ID/guard-daemon" --version)" = "$RELEASE_ID"
TOOLING_TARGET="/usr/lib/guard-daemon/deployment/$RELEASE_ID"
test "$(stat -c '%U:%G %a' "$TOOLING_TARGET")" = 'root:root 700'
install -D -o root -g root -m 0644 "$TOOLING_TARGET/packaging/systemd/guard-daemon.service" /usr/lib/systemd/system/guard-daemon.service
install -D -o root -g root -m 0644 "$TOOLING_TARGET/packaging/systemd/guard-daemon.sysusers" /usr/lib/sysusers.d/guard-daemon.conf
install -D -o root -g root -m 0644 "$TOOLING_TARGET/packaging/systemd/guard-daemon.tmpfiles" /usr/lib/tmpfiles.d/guard-daemon.conf
install -D -o root -g root -m 0644 "$TOOLING_TARGET/packaging/systemd/guard-daemon.state.env" /usr/lib/guard-daemon/guard-daemon.state.env
systemd-sysusers /usr/lib/sysusers.d/guard-daemon.conf
systemd-tmpfiles --create /usr/lib/tmpfiles.d/guard-daemon.conf
test "$(cat /usr/lib/guard-daemon/guard-daemon.state.env)" = 'STATE_DIRECTORY=/var/lib/guard-daemon'
test ! -e /usr/lib/guard-daemon/current.new
test ! -L /usr/lib/guard-daemon/current.new
ln -s "releases/$RELEASE_ID" /usr/lib/guard-daemon/current.new
mv -Tf /usr/lib/guard-daemon/current.new /usr/lib/guard-daemon/current
systemctl daemon-reload
test "$(readlink -f /usr/lib/guard-daemon/current)" = "/usr/lib/guard-daemon/releases/$RELEASE_ID"
systemctl unmask guard-daemon.service
DISABLED_STATE=''
DISABLED_STATE="$(systemctl is-enabled guard-daemon.service 2>/dev/null)" || true
test "$DISABLED_STATE" = 'disabled'
systemctl start guard-daemon.service
systemctl is-active guard-daemon.service
curl --silent --show-error --unix-socket /var/lib/guard-daemon/diagnostics.sock --write-out '\nHTTP %{http_code}\n' http://localhost/healthz
```

5. Подтвердите активную службу, ожидаемый `503/stopped`, активное предупреждение
   `paid_actions_stopped`, доступность чтения и правильный выпуск через
   `readlink -f /usr/lib/guard-daemon/current`.
   Если deployment не менялся, оставьте manifest R1 и
   `RESCUER_RELEASE_COMMIT_<N>=R1` при переходе binary R1 на R2. Подмена поля на
   R2 сделает неизменённый официальный manifest недоверенным и заблокирует
   startup. Только после этой проверки восстановите автозапуск:

```bash
set -euo pipefail
systemctl is-active guard-daemon.service
systemctl enable guard-daemon.service
test "$(systemctl is-enabled guard-daemon.service)" = 'enabled'
```

6. Только после проверки снова остановите службу и выполните отдельную процедуру
   рабочего запуска из раздела выше. Не включайте рабочий режим автоматически
   как часть установки пакета. Этот шаг неприменим к восстановленному состоянию.

## Откат

Сначала определите, выполнялась ли хотя бы одна live-активность после
предобновительного снимка. Если новый процесс мог подписать, отправить,
согласовать или записать результат рабочей операции либо это нельзя
доказательно исключить, снимок устарел. Его запрещено извлекать в
`/var/lib/guard-daemon` или использовать как `STATE_DIRECTORY`: он забудет
nonce, расходы, резервы или незавершённую сверку. Такой снимок разрешено
исследовать только автономно вне рабочего узла. Сохраните наиболее новое
состояние и перезапустите выбранный совместимый выпуск с
`EMERGENCY_STOP=true`.

Предыдущий двоичный файл разрешено вернуть поверх наиболее нового состояния
только после доказанной обратной совместимости его схемы и bindings. Если такой
совместимости нет, откат блокируется и требуется новое исправление вперёд.

Восстановление снимка не является способом вернуться к signing. Даже когда
доказано отсутствие любой live-активности после снимка, процедура из
`backup-restore.md` создаёт постоянный marker и разрешает открывать это дерево
только для наблюдения в dry-run или emergency. В этом выпуске процедуры удаления
marker нет.

Следующая процедура снимок не восстанавливает: она переключает только двоичный
файл поверх наиболее нового, никогда не восстановленного состояния после
доказанной совместимости. Сначала служба останавливается, затем оператор удаляет
ключи и включает аварийный режим:

```bash
set -euo pipefail
RELEASE_ID='<полный-40hex-release-commit-прежнего-выпуска>'
test "${#RELEASE_ID}" -eq 40
case "$RELEASE_ID" in *[!0-9a-f]*) exit 1 ;; esac
systemctl stop guard-daemon.service
test "$(systemctl show guard-daemon.service --property=MainPID --value)" = '0'
test "$(systemctl show guard-daemon.service --property=ActiveState --value)" = 'inactive'
sudoedit /etc/guard-daemon/guard-daemon.env
test "$(grep -c '^DRY_RUN=false$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -c '^EMERGENCY_STOP=true$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -Ec '^RPC_BROADCAST_HTTP_[A-Z][A-Z0-9_]*=' /etc/guard-daemon/guard-daemon.env)" -gt 0
MATCH_STATUS=0
grep -Eq '^(SOURCE_PRIVATE_KEY|SPONSOR_PRIVATE_KEY|STATE_DIRECTORY)=' /etc/guard-daemon/guard-daemon.env || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
test -x "/usr/lib/guard-daemon/releases/$RELEASE_ID/guard-daemon"
test -r "/usr/lib/guard-daemon/releases/$RELEASE_ID/artifacts/contracts/RescuerV2.json"
test "$("/usr/lib/guard-daemon/releases/$RELEASE_ID/guard-daemon" --version)" = "$RELEASE_ID"
test "$(cat /usr/lib/guard-daemon/guard-daemon.state.env)" = 'STATE_DIRECTORY=/var/lib/guard-daemon'
ln -sfn "releases/$RELEASE_ID" /usr/lib/guard-daemon/current.new
mv -Tf /usr/lib/guard-daemon/current.new /usr/lib/guard-daemon/current
systemctl daemon-reload
test "$(readlink -f /usr/lib/guard-daemon/current)" = "/usr/lib/guard-daemon/releases/$RELEASE_ID"
systemctl start guard-daemon.service
systemctl is-active guard-daemon.service
curl --silent --show-error --unix-socket /var/lib/guard-daemon/diagnostics.sock --write-out '\nHTTP %{http_code}\n' http://localhost/healthz
```

Подтвердите `503/stopped` и чтение показателей. Дальнейший рабочий запуск
рассматривается отдельно и допустим только для наиболее нового состояния, которое
никогда не восстанавливалось и не содержит внешний marker
`/var/lib/guard-daemon-operator/live-disabled-after-restore`.
Восстановленное состояние навсегда остаётся вне signing.

Файл окружения не входит в снимок состояния. Предыдущую несекретную
конфигурацию и секреты восстанавливают из раздельной защищённой системы
управления конфигурацией, а не из архива `/var/lib/guard-daemon`.

## Статус проверки процедуры

Команды и unit-файлы требуют интеграционной репетиции на целевом Linux с
systemd. Наличие этой инструкции не является свидетельством выполненной
репетиции установки, запуска, остановки, обновления или отката.
