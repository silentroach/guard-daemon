# Резервное копирование и восстановление состояния

Единицей резервного копирования является весь каталог
`/var/lib/guard-daemon`, снятый только после успешной штатной остановки. Нельзя
копировать отдельные базы независимо друг от друга: очередь, инциденты, точки
чтения, бюджет, допуск и состояние предупреждений должны относиться к одному
моменту.

В резервную копию не входят:

- `/etc/guard-daemon/guard-daemon.env` и любые другие файлы окружения;
- приватные ключи и данные доступа;
- `/var/tmp/guard-daemon-leases-v1` и файлы межпроцессной блокировки;
- весь `/var/lib/guard-daemon-operator`, включая production recovery registry и
  постоянный restore marker;
- `/var/lib/guard-daemon/diagnostics.sock`;
- исполняемый файл, контрактный артефакт и манифесты выпуска.

Файлы блокировки не являются состоянием: право владения принадлежит работающему
процессу и ядру. Unix-сокет также создаётся заново при запуске.

**Важно:** устойчивое состояние может содержать точное двоичное представление
подписанной транзакции, сохранённое перед отправкой для безопасного повтора после
сбоя. Такая транзакция не является приватным ключом, но до окончательного исхода
может быть пригодна для отправки. Архив и карантинные копии считаются
чувствительными, хранятся с режимом `0600`, шифруются средствами оператора и не
публикуются в задачах, журналах или системе контроля версий.

## Создание согласованного снимка

Команды рассчитаны на GNU tar и выполняются от `root`:

```bash
set -euo pipefail
systemctl stop guard-daemon.service
test "$(systemctl show guard-daemon.service --property=MainPID --value)" = '0'
test "$(systemctl show guard-daemon.service --property=ActiveState --value)" = 'inactive'
test "$(systemctl show guard-daemon.service --property=Result --value)" = 'success'

BACKUP_DIRECTORY=/srv/guard-daemon-backup
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
ARCHIVE_NAME="guard-daemon-state-$STAMP.tar"
ARCHIVE="$BACKUP_DIRECTORY/$ARCHIVE_NAME"
install -d -o root -g root -m 0700 "$BACKUP_DIRECTORY"
umask 077
tar --create --file="$ARCHIVE" --numeric-owner --acls --xattrs --selinux \
  --directory=/var/lib --exclude='guard-daemon/diagnostics.sock' guard-daemon
chmod 0600 "$ARCHIVE"
(cd "$BACKUP_DIRECTORY" && sha256sum "$ARCHIVE_NAME" > "$ARCHIVE_NAME.sha256")
chmod 0600 "$ARCHIVE.sha256"

ARCHIVE_LIST="$ARCHIVE.list"
trap 'rm -f "$ARCHIVE_LIST"' EXIT
tar --list --file="$ARCHIVE" > "$ARCHIVE_LIST"
MATCH_STATUS=0
grep -Eq '(^|/)diagnostics\.sock$|(^|/).*\.env$' "$ARCHIVE_LIST" || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
MATCH_STATUS=0
grep -Evq '^guard-daemon(/|$)' "$ARCHIVE_LIST" || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
(cd "$BACKUP_DIRECTORY" && sha256sum --check "$ARCHIVE_NAME.sha256")
```

Не продолжайте, если `systemctl stop` завершился ошибкой, по истечении 120 секунд
процесс был принудительно убит или `MainPID` не равен нулю. Такой каталог нельзя
называть согласованным после остановки.

Команды сохраняют список рядом с архивом только на время проверки, проверяют
его без вывода содержимого файлов и удаляют при выходе. Архив должен содержать
только один верхний каталог `guard-daemon/`.

Не добавляйте к нему `/etc`, `/var/lib/guard-daemon-operator`, домашние каталоги,
файлы ключей или `/var/tmp`. Архив охватывает только `/var/lib/guard-daemon`;
operator registry управляется отдельно и никогда не попадает под рекурсивный
`chown` восстановленного state. Управление секретами и несекретной конфигурацией
выполняется отдельно от резервной копии состояния.

## Восстановление

Восстанавливайте только доверенный архив, контрольная сумма которого получена
по независимому доверенному каналу. Целевой выпуск, роли, манифесты и
экономическая политика должны соответствовать снимку; иначе проверка привязки
состояния обязана остановить запуск.

Восстановленное состояние в этом выпуске никогда не возвращается в signing.
До quarantine, извлечения или публикации восстановленного state процедура
durable и атомарно создаёт root-owned marker
`/var/lib/guard-daemon-operator/live-disabled-after-restore`. Dry-run и
`EMERGENCY_STOP=true` могут открыть восстановленное дерево для наблюдения, но
обычный live startup fail-closed останавливается до создания signer.
Процедуры удаления marker в этом выпуске нет.

До извлечения определите, выполнялась ли после снимка любая live-активность хотя
бы на одной сети этого `sponsor`. Если более новый процесс мог подписать,
отправить, согласовать или записать результат рабочей операции либо отсутствие
этого нельзя доказать, снимок устарел. Устаревший снимок запрещено извлекать в
`/var/lib/guard-daemon` или указывать как `STATE_DIRECTORY`. Он пригоден только
для автономного forensic-исследования на изолированном offline-узле вне рабочего
контура; daemon с этим деревом не запускают. В canonical path сохраняют наиболее
новое состояние.

Сначала остановите службу и только после подтверждения `MainPID=0` измените
конфигурацию: удалите обе строки приватных ключей, установите `DRY_RUN=false` и
`EMERGENCY_STOP=true`, сохранив обязательные `RPC_BROADCAST_HTTP_<N>`. Следующая
процедура выполняется целиком от `root`; любое несовпадение до извлечения
оставляет текущее состояние на месте, а любой отказ после него оставляет службу
остановленной:

```bash
set -euo pipefail
systemctl stop guard-daemon.service
test "$(systemctl show guard-daemon.service --property=MainPID --value)" = '0'
test "$(systemctl show guard-daemon.service --property=ActiveState --value)" = 'inactive'
test "$(systemctl show guard-daemon.service --property=Result --value)" = 'success'

sudoedit /etc/guard-daemon/guard-daemon.env
test "$(grep -c '^DRY_RUN=false$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -c '^EMERGENCY_STOP=true$' /etc/guard-daemon/guard-daemon.env)" -eq 1
test "$(grep -Ec '^RPC_BROADCAST_HTTP_[A-Z][A-Z0-9_]*=' /etc/guard-daemon/guard-daemon.env)" -gt 0
MATCH_STATUS=0
grep -Eq '^(SOURCE_PRIVATE_KEY|SPONSOR_PRIVATE_KEY|STATE_DIRECTORY)=' /etc/guard-daemon/guard-daemon.env || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
test "$(cat /usr/lib/guard-daemon/guard-daemon.state.env)" = 'STATE_DIRECTORY=/var/lib/guard-daemon'

ARCHIVE='<путь-к-доверенному-снимку.tar>'
ARCHIVE_DIRECTORY="$(dirname "$ARCHIVE")"
ARCHIVE_NAME="$(basename "$ARCHIVE")"
test -f "$ARCHIVE"
test -f "$ARCHIVE.sha256"
(cd "$ARCHIVE_DIRECTORY" && sha256sum --check "$ARCHIVE_NAME.sha256")

umask 077
ARCHIVE_LIST="$(mktemp /run/guard-daemon-restore-list.XXXXXX)"
RESTORE_MARKER_TMP=''
trap 'rm -f -- "$ARCHIVE_LIST"; if test -n "$RESTORE_MARKER_TMP"; then rm -f -- "$RESTORE_MARKER_TMP"; fi' EXIT
tar --list --file="$ARCHIVE" > "$ARCHIVE_LIST"
test "$(grep -Ec '^guard-daemon(/|$)' "$ARCHIVE_LIST")" -gt 0
MATCH_STATUS=0
grep -Eq '(^|/)diagnostics\.sock$|(^|/).*\.env$|^/|(^|/)\.\.(/|$)' "$ARCHIVE_LIST" || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1
MATCH_STATUS=0
grep -Evq '^guard-daemon(/|$)' "$ARCHIVE_LIST" || MATCH_STATUS=$?
test "$MATCH_STATUS" -eq 1

OPERATOR_DIRECTORY=/var/lib/guard-daemon-operator
RECOVERY_DIRECTORY="$OPERATOR_DIRECTORY/deployment-recovery"
MANIFEST_DIRECTORY="$OPERATOR_DIRECTORY/deployment-manifests"
RESTORE_MARKER="$OPERATOR_DIRECTORY/live-disabled-after-restore"
test -d "$OPERATOR_DIRECTORY"
test ! -L "$OPERATOR_DIRECTORY"
test -d "$RECOVERY_DIRECTORY"
test ! -L "$RECOVERY_DIRECTORY"
test -d "$MANIFEST_DIRECTORY"
test ! -L "$MANIFEST_DIRECTORY"
test "$(stat -c '%U:%G %a' "$OPERATOR_DIRECTORY")" = 'root:root 755'
test "$(stat -c '%U:%G %a' "$RECOVERY_DIRECTORY")" = 'root:root 700'
test "$(stat -c '%U:%G %a' "$MANIFEST_DIRECTORY")" = 'root:root 700'
if test -e "$RESTORE_MARKER" || test -L "$RESTORE_MARKER"; then exit 1; fi
RESTORE_MARKER_TMP="$(mktemp "$OPERATOR_DIRECTORY/.live-disabled-after-restore.tmp.XXXXXX")"
chown root:root "$RESTORE_MARKER_TMP"
chmod 0444 "$RESTORE_MARKER_TMP"
test "$(stat -c '%U:%G %a' "$RESTORE_MARKER_TMP")" = 'root:root 444'
python3 - "$RESTORE_MARKER_TMP" <<'PY'
import os
import sys

descriptor = os.open(sys.argv[1], os.O_RDONLY)
try:
    os.fsync(descriptor)
finally:
    os.close(descriptor)
PY
mv -T -- "$RESTORE_MARKER_TMP" "$RESTORE_MARKER"
RESTORE_MARKER_TMP=''
python3 - "$OPERATOR_DIRECTORY" <<'PY'
import os
import sys

descriptor = os.open(sys.argv[1], os.O_RDONLY | os.O_DIRECTORY)
try:
    os.fsync(descriptor)
finally:
    os.close(descriptor)
PY
test -f "$RESTORE_MARKER"
test ! -L "$RESTORE_MARKER"
test "$(stat -c '%U:%G %a' "$RESTORE_MARKER")" = 'root:root 444'

QUARANTINE="/var/lib/guard-daemon.failed.$(date -u +%Y%m%dT%H%M%SZ)"
test ! -e "$QUARANTINE"
test ! -L "$QUARANTINE"
if test -e /var/lib/guard-daemon; then mv -T -- /var/lib/guard-daemon "$QUARANTINE"; fi
tar --extract --file="$ARCHIVE" --acls --xattrs --selinux --directory=/var/lib
chown -R guard-daemon:guard-daemon /var/lib/guard-daemon
chmod 0700 /var/lib/guard-daemon
test ! -S /var/lib/guard-daemon/diagnostics.sock
test "$(stat -c '%U:%G %a' /var/lib/guard-daemon)" = 'guard-daemon:guard-daemon 700'
test "$(stat -c '%U:%G %a' "$OPERATOR_DIRECTORY")" = 'root:root 755'
test "$(stat -c '%U:%G %a' "$RECOVERY_DIRECTORY")" = 'root:root 700'
test "$(stat -c '%U:%G %a' "$MANIFEST_DIRECTORY")" = 'root:root 700'
test "$(stat -c '%U:%G %a' "$RESTORE_MARKER")" = 'root:root 444'
test -x /usr/lib/guard-daemon/current/guard-daemon
test -r /usr/lib/guard-daemon/current/artifacts/contracts/RescuerV2.json

systemctl start guard-daemon.service
systemctl is-active guard-daemon.service
curl --silent --show-error --unix-socket /var/lib/guard-daemon/diagnostics.sock --write-out '\nHTTP %{http_code}\n' http://localhost/healthz
```

Ожидается активная служба и `503/stopped`. Такой первый запуск не создаёт
подписывающие компоненты и не повторяет сохранённую подписанную транзакцию.
Проверьте `/metrics`, предупреждения, соответствие выпуска и доступность чтения.
Не добавляйте приватные ключи, не устанавливайте `EMERGENCY_STOP=false` и не
пытайтесь удалить, заменить или переименовать внешний marker: восстановленное
дерево остаётся только мониторинговым и forensic-состоянием на весь срок
поддержки этого выпуска.

Не объединяйте файлы из карантина и восстановленного дерева. Карантин храните с
теми же ограничениями, что и архив, до завершения расследования, затем удаляйте
по утверждённой политике хранения.
