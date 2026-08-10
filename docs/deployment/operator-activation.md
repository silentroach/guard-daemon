# Операторская активация RescuerV2

## Полномочия и запрет автоматизации

Все разделы с командами публичной сети имеют статус **ТОЛЬКО ОПЕРАТОР**. Агент
не выполняет их, не запрашивает приватные ключи, не отправляет транзакции, не
пополняет `sponsor` и не изменяет делегацию в сети.

Процедура не является утверждением готовности к основной сети. Репозиторий не
содержит официальных манифестов и не разрешает автоматическую активацию. Смена
уже используемой реализации дополнительно заблокирована ограничениями
постоянного состояния.

## Входные данные

Оператор до любого действия фиксирует вне журнала команд:

- полный 40-символьный коммит неизменяемого выпуска;
- аутентифицированные кандидат выпуска
  `/usr/lib/guard-daemon/candidates/<commit>/release-candidate.json` и средства
  развёртывания `/usr/lib/guard-daemon/deployment/<commit>`, принадлежащие root и
  подготовленные процедурой
  [`../operations/runbook.md`](../operations/runbook.md) до передачи секретов;
- проверенные и попарно различные адреса `destination` и `sponsor`;
- ожидаемый идентификатор сети;
- два независимых поставщика чтения с разными конечными точками HTTP/WebSocket и
  доменами доверия;
- отдельный RPC отправки;
- операторские пределы `gasLimit`, `maxFeePerGas`, `maxPriorityFeePerGas` и
  полной стоимости газа;
- имя нового манифеста непосредственно в фиксированном каталоге
  `/var/lib/guard-daemon-operator/deployment-manifests` с владельцем root и
  режимом `0700`;
- заранее созданный канонический реестр восстановления
  `/var/lib/guard-daemon-operator/deployment-recovery` с режимом `0700` и
  владельцем `root:root`.

Коммит должен существовать до создания официального манифеста. Сокращённый SHA,
изменяемая ветка или тег без проверки полного коммита не подходят.

## Проверка выпуска

До установки оператор выполняет локальные команды из
[`local-verification.md`](local-verification.md). До передачи секретов он
отдельно устанавливает и сверяет кандидата и средства развёртывания,
принадлежащие root, по инструкции systemd:

1. Доверенный хеш самого `SHA256SUMS` проверен по независимому каналу.
2. SHA-256 артефакта совпадает с `release-candidate.json`.
3. Кандидат выпуска содержит утверждённый полный коммит, а средства развёртывания
   извлечены из аутентифицированного `guard-daemon-source.tar` этого кандидата.
4. `destination` принадлежит безопасному хранилищу, независимо управляемому от
   `source` и `sponsor`.
5. Роль `sponsor` использует отдельный горячий кошелёк с ограниченным балансом; `source`,
   `sponsor` и `destination` попарно различны.
6. Сеть поддерживает EIP-7702 и имеет ограничиваемую транзакцией модель полной
   стоимости газа. Дополнительная комиссия без верхнего предела блокирует
   активацию.

План без отправки безопасно проверяется локально командой из
`local-verification.md`. Детерминированные адреса оттуда нельзя переносить в
публичную сеть.

## Пределы транзакции

Перед отправкой должны одновременно выполняться условия:

- `gas-limit` является положительным десятичным целым и явно записывается в
  транзакцию;
- `max-priority-fee-per-gas-wei <= max-fee-per-gas-wei`;
- `gas-limit * max-fee-per-gas-wei <= max-total-cost-wei`;
- для баланса `sponsor` независимо установлен утверждённый предел, который не
  используется как замена верхней границе транзакции;
- сформированная и подписанная транзакция побайтно соответствует проверенным
  данным конструктора и заданным полям комиссии.

`max-total-cost-wei` является проверкой CLI. Протокол EVM ограничивает расход
полями `gasLimit` и `maxFeePerGas`; поэтому CLI обязан отказать, если локальная
граница не согласуется с их произведением. Сеть с дополнительной комиссией, для
которой нет предела, обеспеченного полями транзакции, не подходит для этой
процедуры.

## Отправка в публичную сеть

**ТОЛЬКО ОПЕРАТОР. Агент эту команду не выполняет.** Сначала в процессе без
`DEPLOYMENT_RPC_URL` и `DEPLOYER_PRIVATE_KEY` оператор проверяет неизменность
установленного набора файлов. Обычная рабочая копия и её игнорируемый
`node_modules` запрещены для развёртывания в публичной сети. Предварительную
проверку доверенный диспетчер процессов с правами root запускает напрямую через
`/bin/bash --noprofile --norc` в новом окружении, содержащем только перечисленные переменные:
`PATH=/usr/bin:/bin`, `LANG=C.UTF-8`, `TZ=UTC`; он не наследует `BASH_ENV`,
`ENV`, `NODE_*`, `LD_*`, `DYLD_*`, `HOME` или `npm_config_*`:

```bash
set -euo pipefail
test "$(id -u)" -eq 0
test "$PATH" = '/usr/bin:/bin'
for FORBIDDEN_ENV in BASH_ENV ENV NODE_OPTIONS NODE_PATH LD_PRELOAD LD_LIBRARY_PATH DYLD_INSERT_LIBRARIES HOME; do
  if /usr/bin/printenv "$FORBIDDEN_ENV" >/dev/null; then exit 1; fi
done
RELEASE_COMMIT='<проверенный-полный-40hex-commit>'
test "${#RELEASE_COMMIT}" -eq 40
case "$RELEASE_COMMIT" in *[!0-9a-f]*) exit 1 ;; esac
TOOLING_DIRECTORY="/usr/lib/guard-daemon/deployment/$RELEASE_COMMIT"
RELEASE_CANDIDATE_PATH="/usr/lib/guard-daemon/candidates/$RELEASE_COMMIT/release-candidate.json"
MANIFEST_DIRECTORY=/var/lib/guard-daemon-operator/deployment-manifests
RECOVERY_DIRECTORY=/var/lib/guard-daemon-operator/deployment-recovery
NODE_BINARY=/usr/bin/node
CANDIDATE_DIRECTORY="$(dirname "$RELEASE_CANDIDATE_PATH")"
for TRUSTED_DIRECTORY in \
  /usr \
  /usr/bin \
  /usr/lib \
  /usr/lib/guard-daemon \
  /usr/lib/guard-daemon/deployment \
  /usr/lib/guard-daemon/candidates \
  "$TOOLING_DIRECTORY" \
  "$CANDIDATE_DIRECTORY" \
  /var \
  /var/lib \
  /var/lib/guard-daemon-operator \
  "$MANIFEST_DIRECTORY" \
  "$RECOVERY_DIRECTORY"; do
  test -d "$TRUSTED_DIRECTORY"
  test ! -L "$TRUSTED_DIRECTORY"
  test "$(stat -c '%U:%G' "$TRUSTED_DIRECTORY")" = 'root:root'
  DIRECTORY_MODE="$(stat -c '%a' "$TRUSTED_DIRECTORY")"
  if (( (8#$DIRECTORY_MODE & 0022) != 0 )); then exit 1; fi
done
test "$(stat -c '%a' "$TOOLING_DIRECTORY")" = '700'
test "$(stat -c '%a' "$CANDIDATE_DIRECTORY")" = '700'
test "$(stat -c '%a' "$MANIFEST_DIRECTORY")" = '700'
test "$(stat -c '%a' "$RECOVERY_DIRECTORY")" = '700'
test -f "$NODE_BINARY"
test ! -L "$NODE_BINARY"
test "$(stat -c '%U:%G' "$NODE_BINARY")" = 'root:root'
NODE_MODE="$(stat -c '%a' "$NODE_BINARY")"
if (( (8#$NODE_MODE & 0022) != 0 )); then exit 1; fi
test "$("$NODE_BINARY" --version)" = 'v24.18.1'
test -f "$TOOLING_DIRECTORY/node_modules/tsx/dist/cli.mjs"
test -f "$TOOLING_DIRECTORY/scripts/deployRescuerV2.ts"
test -f "$RELEASE_CANDIDATE_PATH"
OWNERSHIP_ERROR="$(find "$TOOLING_DIRECTORY" "$CANDIDATE_DIRECTORY" -xdev \( ! -user root -o ! -group root \) -print -quit)" || exit 1
test -z "$OWNERSHIP_ERROR"
WRITABLE_ERROR="$(find "$TOOLING_DIRECTORY" "$CANDIDATE_DIRECTORY" -xdev ! -type l -perm /022 -print -quit)" || exit 1
test -z "$WRITABLE_ERROR"
LINK_LIST="$(mktemp /run/guard-daemon-deployment-links.XXXXXX)"
trap 'rm -f -- "$LINK_LIST"' EXIT
find "$TOOLING_DIRECTORY" "$CANDIDATE_DIRECTORY" -xdev -type l -print0 > "$LINK_LIST" || exit 1
while IFS= read -r -d '' LINK_PATH; do
  LINK_TARGET="$(readlink -f -- "$LINK_PATH")" || exit 1
  case "$LINK_PATH" in
    "$TOOLING_DIRECTORY"/*) LINK_ROOT="$TOOLING_DIRECTORY" ;;
    "$CANDIDATE_DIRECTORY"/*) LINK_ROOT="$CANDIDATE_DIRECTORY" ;;
    *) exit 1 ;;
  esac
  case "$LINK_TARGET" in "$LINK_ROOT"/*) ;; *) exit 1 ;; esac
done < "$LINK_LIST"
if /usr/bin/printenv DEPLOYMENT_RPC_URL >/dev/null; then exit 1; fi
if /usr/bin/printenv DEPLOYER_PRIVATE_KEY >/dev/null; then exit 1; fi
```

Только после успешной проверки защищённый диспетчер секретов запускает новый
процесс с рабочим каталогом
`/usr/lib/guard-daemon/deployment/<releaseCommit>`. Диспетчер обязан напрямую,
без командной оболочки, `env` и обёртки npm, запустить `/usr/bin/node` по
абсолютному пути. Окружение дочернего процесса создаётся заново и содержит только
`DEPLOYMENT_RPC_URL`,
`DEPLOYER_PRIVATE_KEY`, `LANG=C.UTF-8` и `TZ=UTC`; `PATH`, `NODE_OPTIONS`,
`NODE_PATH`, `LD_PRELOAD`, `LD_LIBRARY_PATH`, `BASH_ENV`, `ENV`, `HOME` и
`npm_config_*` не наследуются. Непосредственно перед `exec` диспетчер
устанавливает мягкое и жёсткое значения `RLIMIT_CORE` дочернего процесса равными нулю, Linux
`PR_SET_DUMPABLE=0` и запрещает подключение внешнего сборщика аварийных дампов.
Отсутствие дампов памяти является обязательным условием допуска: развёртывание со
стандартной политикой дампов не запускают.

Ниже показан точный argv отдельного процесса. Значения в угловых скобках
подставляет API диспетчера секретов, а не командная оболочка; сами секреты
находятся только в окружении и не входят в argv:

```text
/usr/bin/node
/usr/lib/guard-daemon/deployment/<releaseCommit>/node_modules/tsx/dist/cli.mjs
/usr/lib/guard-daemon/deployment/<releaseCommit>/scripts/deployRescuerV2.ts
--chain-id <chainId>
--destination <destinationAddress>
--sponsor <sponsorAddress>
--manifest /var/lib/guard-daemon-operator/deployment-manifests/rescuer-v2-<chainId>-<releaseCommit>.json
--release-candidate /usr/lib/guard-daemon/candidates/<releaseCommit>/release-candidate.json
--gas-limit <gasLimit>
--max-fee-per-gas-wei <maxFeePerGasWei>
--max-priority-fee-per-gas-wei <maxPriorityFeePerGasWei>
--max-total-cost-wei <maxTotalCostWei>
--broadcast
```

Ключ подписанта транзакции развёртывания и RPC URL с данными доступа передаются
процессу только через защищённый операторский канал и не записываются в
репозиторий, `.env`, кандидат выпуска, манифест, запись восстановления, argv или
журнал. Адрес подписанта обязан совпадать с `sponsor`. При отправке в публичную
сеть CLI отклоняет `--rpc-url`; переменная `DEPLOYMENT_RPC_URL` обязательна.

`--config` и `--config-key` не поддерживаются. Оператор отдельно просматривает
созданный манифест, устанавливает проверенную копию как `root:guard-daemon 0640`
в `/etc/guard-daemon/manifests/` и только затем явно указывает её в конфигурации
демона. CLI развёртывания не изменяет `/etc`.

Для идентификаторов публичных сетей `1`, `56` и `137` CLI всегда использует
`/var/lib/guard-daemon-operator/deployment-recovery`; параметра для
переопределения нет. `--recovery-directory` разрешён и обязателен только для
локального идентификатора сети `31337`. Назначенный оператор с правами root
запускает CLI для публичной сети; реестр заранее создаётся как `0700 root:root`,
а пользователь службы `guard-daemon` не должен получать к нему доступ. CLI не
создаёт реестр или родительский каталог манифеста. Манифест публичной сети
создаётся только непосредственно в
`/var/lib/guard-daemon-operator/deployment-manifests`. Непосредственно перед
блокировкой и каждой публикацией CLI повторно проверяет канонический путь,
идентификатор устройства, inode каталогов и режим `0700 root:root`.

Для публичной сети CLI требует точные пути кандидата и средств развёртывания с
владельцем root для выбранного коммита и не полагается на изменяемую рабочую
копию Git. Проверки рабочей копии Git,
`info/attributes`, `assume-unchanged` и `skip-worktree` сохраняются только для
локального идентификатора сети `31337`. До обращения к RPC и ключу CLI блокирует
запуск при любой незавершённой записи той же пары «сеть + `sponsor`», независимо
от коммита выпуска. После подписи он атомарно создаёт запись восстановления с
правами `0600` и детерминированным именем
`rescuer-v2-<chain>-<lowercase-sponsor>-<releaseCommit>.json`. Эта запись
содержит пригодную к отправке подписанную транзакцию, и только затем CLI передаёт
те же байты RPC. Этот файл содержит конфиденциальные данные: его нельзя публиковать или
включать в кандидат выпуска. Если отправка, получение квитанции или
предварительное чтение
завершились неоднозначно, манифест может отсутствовать, но запись восстановления
сохраняется. Повторное развёртывание запрещено до операторской сверки хеша, nonce
и квитанции и отдельного решения по точному повтору тех же байтов.

CLI развёртывания и автоматизированный агент не помечают запись восстановления
как завершённую, не удаляют и не архивируют запись или оставшуюся запись блокировки.
После независимой сверки сети оператор вручную переносит разрешённую запись в
защищённый архив вне реестра либо оставляет её на месте как блокировку. Это
отдельное операторское действие; очистка ради повторного развёртывания без
сохранённого свидетельства запрещена.

## Предварительное чтение

Сценарий развёртывания должен завершаться с ошибкой при несовпадении
идентификатора сети, квитанции, исполняемого кода, `destination`, `sponsor` или
`self`. Положительный результат этого шага основан на единственном RPC и означает
только, что манифест можно передать следующей проверке. Называть его окончательной
аттестацией нельзя.

Оператор до продолжения сверяет, что:

- `manifest.source.kind` равен `git-commit`;
- `manifest.source.value` равен полному коммиту из кандидата выпуска;
- `manifest.artifact.sha256` равен SHA-256 канонического артефакта из кандидата
  выпуска;
- транзакция и квитанция соответствуют манифесту;
- фактический расход не превышает `max-total-cost-wei`.

## Окончательная эксплуатационная аттестация

**ТОЛЬКО ОПЕРАТОР. Агент не запускает демон с конфигурацией публичной сети.**

Перед передачей ключей демон обязан полностью пройти процедуру запуска в рабочей
конфигурации с остановленными платными действиями:

```dotenv
DRY_RUN=false
EMERGENCY_STOP=true
```

При этом:

- `SOURCE_PRIVATE_KEY` и `SPONSOR_PRIVATE_KEY` отсутствуют в окружении процесса;
- учётные данные systemd `source-private-key` и `sponsor-private-key` пусты;
- настроены два независимых поставщика чтения и отдельный RPC отправки;
- указан новый проверенный манифест и точный канонический артефакт;
- демон получает общий финализированный блок от обоих поставщиков и сверяет
  идентификатор сети, транзакцию и квитанцию развёртывания, исполняемый код и все
  неизменяемые роли;
- открываются существующие журнал бюджета и состояние наблюдателя без удаления,
  переименования, подмены или сброса;
- диагностика показывает состояние `stopped`, ожидаемое при
  `EMERGENCY_STOP=true`; свежесть журнала не используется как критерий здоровья.

Отсутствие ключей является обязательной проверкой границы: подписант и рабочий
компонент отправки не должны создаваться. Любая ошибка запуска, расхождение RPC,
несовместимость состояния или журнала бюджета блокирует дальнейшую активацию.

Только после сохранения свидетельств этого запуска оператор может отдельно
утвердить передачу ключей через диспетчер секретов и изменение
`EMERGENCY_STOP=false`. Это новое решение о риске работы в публичной сети, а не
автоматическое продолжение сценария развёртывания. Ключ `source` всё равно
считается скомпрометированным, а победа транзакции восстановления не гарантируется.

## Свидетельства и отказ

Сохраняются полный коммит, кандидат выпуска, SHA-256 артефакта и манифеста,
публичный хеш транзакции, квитанция, согласованный результат аттестации при
запуске и состояние диагностики. Не сохраняются и не публикуются ключи, подписи,
набор параметров авторизации и RPC URL с данными доступа. Запись восстановления
с подписанной транзакцией хранится отдельно как закрытое операторское
свидетельство и никогда не публикуется. Её разбор и архивирование выполняет
только оператор вне
автоматизированного агента.

При любой неоднозначности оператор оставляет `EMERGENCY_STOP=true`, не повторяет
отправку вслепую и переходит к
[`../migration/rollback-emergency.md`](../migration/rollback-emergency.md).
