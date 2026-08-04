# Операторская активация RescuerV2

## Полномочия и запрет автоматизации

Все разделы с командами публичной сети имеют статус **ТОЛЬКО ОПЕРАТОР**. Агент
не выполняет их, не запрашивает приватные ключи, не отправляет транзакции, не
пополняет sponsor и не изменяет сетевую делегацию.

Процедура не является утверждением готовности к mainnet. Репозиторий не содержит
официальных manifests и не разрешает автоматическую активацию. Смена уже
используемой реализации дополнительно заблокирована ограничениями постоянного
состояния.

## Входные данные

Оператор до любого действия фиксирует вне журнала команд:

- полный 40-символьный commit неизменяемого выпуска;
- аутентифицированный root-owned candidate
  `/usr/lib/guard-daemon/candidates/<commit>/release-candidate.json` и tooling
  `/usr/lib/guard-daemon/deployment/<commit>`, подготовленные процедурой
  [`../operations/runbook.md`](../operations/runbook.md) до передачи секретов;
- проверенные и попарно различные адреса `destination` и `sponsor`;
- ожидаемый chain ID;
- два независимых поставщика чтения с разными HTTP/WebSocket endpoint и доменами
  доверия;
- отдельный RPC отправки;
- операторские пределы `gasLimit`, `maxFeePerGas`, `maxPriorityFeePerGas` и
  полной стоимости газа;
- имя нового манифеста непосредственно в фиксированном root-owned каталоге
  `/var/lib/guard-daemon-operator/deployment-manifests` с режимом `0700`;
- заранее созданный канонический recovery registry
  `/var/lib/guard-daemon-operator/deployment-recovery` с режимом `0700` и
  владельцем `root:root`.

Commit должен существовать до создания официального манифеста. Сокращённый SHA,
изменяемая ветка или tag без проверки полного commit не подходят.

## Проверка выпуска

До установки operator выполняет локальные команды из
[`local-verification.md`](local-verification.md). До передачи секретов он
отдельно устанавливает и сверяет root-owned candidate/tooling по systemd-runbook:

1. Доверенный digest самого `SHA256SUMS` проверен по независимому каналу.
2. SHA-256 артефакта совпадает с `release-candidate.json`.
3. Release candidate содержит утверждённый полный commit, а tooling извлечён из
   аутентифицированного `guard-daemon-source.tar` этого candidate.
4. `destination` принадлежит безопасному хранилищу, независимо управляемому от
   source и sponsor.
5. Sponsor является отдельным ограниченным hot wallet; source, sponsor и
   destination попарно различны.
6. Сеть поддерживает EIP-7702 и имеет ограничиваемую транзакцией модель полной
   стоимости газа. Некэпируемая дополнительная комиссия блокирует активацию.

План без отправки безопасно проверяется локально командой из
`local-verification.md`. Детерминированные адреса оттуда нельзя переносить в
публичную сеть.

## Пределы транзакции

Перед отправкой должны одновременно выполняться условия:

- `gas-limit` является положительным десятичным целым и явно записывается в
  транзакцию;
- `max-priority-fee-per-gas-wei <= max-fee-per-gas-wei`;
- `gas-limit * max-fee-per-gas-wei <= max-total-cost-wei`;
- баланс sponsor ограничен независимо утверждённой суммой и не используется как
  замена верхней границе транзакции;
- сформированная и подписанная транзакция побайтно соответствует проверенным
  данным конструктора и заданным fee-полям.

`max-total-cost-wei` является проверкой CLI. Протокол EVM ограничивает расход
полями `gasLimit` и `maxFeePerGas`; поэтому CLI обязан отказать, если локальная
граница не согласуется с их произведением. Сеть с дополнительной комиссией, для
которой нет предела, обеспеченного полями транзакции, не подходит для этой
процедуры.

## Отправка в публичную сеть

**ТОЛЬКО ОПЕРАТОР. Агент эту команду не выполняет.** Сначала в процессе без
`DEPLOYMENT_RPC_URL` и `DEPLOYER_PRIVATE_KEY` оператор проверяет immutable
installation. Обычный checkout и его игнорируемый `node_modules` для production
запрещены. Сам preflight запускает доверенный root process manager напрямую как
`/bin/bash --noprofile --norc` с новым allowlist-окружением
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

Только после успешной проверки защищённый менеджер секретов запускает новый
процесс с рабочим каталогом
`/usr/lib/guard-daemon/deployment/<releaseCommit>`. Менеджер обязан напрямую,
без shell, `env` и npm shim, выполнить абсолютный `/usr/bin/node`. Окружение
ребёнка строится allowlist-ом заново и содержит только `DEPLOYMENT_RPC_URL`,
`DEPLOYER_PRIVATE_KEY`, `LANG=C.UTF-8` и `TZ=UTC`; `PATH`, `NODE_OPTIONS`,
`NODE_PATH`, `LD_PRELOAD`, `LD_LIBRARY_PATH`, `BASH_ENV`, `ENV`, `HOME` и
`npm_config_*` не наследуются. Непосредственно перед `exec` manager устанавливает
для child мягкий и жёсткий `RLIMIT_CORE=0`, Linux `PR_SET_DUMPABLE=0` и запрещает
подключение внешнего crash collector. Отсутствие core dump является обязательным
gate: deployment с default dump policy не запускают.

Ниже показан точный argv отдельного процесса. Угловые значения подставляет API
менеджера секретов, а не shell; сами секреты находятся только в environment и
не входят в argv:

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

Ключ deployment signer и credential-bearing RPC URL передаются процессу только
через защищённый операторский канал и не записываются в repository, `.env`,
release candidate, манифест, recovery record, argv или журнал. Адрес signer
обязан совпадать со sponsor. Для production broadcast CLI отклоняет
`--rpc-url`; переменная `DEPLOYMENT_RPC_URL` обязательна.

`--config` и `--config-key` не поддерживаются. Оператор отдельно просматривает
созданный манифест, устанавливает проверенную копию как `root:guard-daemon 0640`
в `/etc/guard-daemon/manifests/` и только затем явно указывает её в конфигурации
демона. Deployment CLI не изменяет `/etc`.

Для production chain ID `1`, `56` и `137` CLI всегда использует
`/var/lib/guard-daemon-operator/deployment-recovery`; параметра для
переопределения нет. `--recovery-directory` разрешён и обязателен только для
локальной chain ID `31337`. Production CLI запускает назначенный root-оператор:
registry заранее создаётся как `0700 root:root`, а пользователь службы
`guard-daemon` не должен получать к нему доступ. CLI не создаёт registry или
родитель manifest. Production manifest создаётся только непосредственно в
`/var/lib/guard-daemon-operator/deployment-manifests`. Непосредственно перед lock
и каждой публикацией CLI повторно проверяет canonical path и device/inode
каталогов и режим `0700 root:root`.

Для production CLI требует точные root-owned пути candidate/tooling выбранного
commit и не полагается на mutable Git checkout. Проверки Git checkout,
`info/attributes`, `assume-unchanged` и `skip-worktree` сохраняются только для
локальной chain ID `31337`. До RPC и ключа CLI блокирует запуск при любой
незавершённой записи той же пары chain+sponsor, независимо от release commit.
После подписи он атомарно создаёт recovery record с правами `0600` и
детерминированным именем
`rescuer-v2-<chain>-<lowercase-sponsor>-<releaseCommit>.json`, содержащий
пригодную к отправке подписанную транзакцию, и только затем передаёт те же байты
RPC. Этот файл является чувствительным: его нельзя публиковать или включать в
release candidate. Если отправка, receipt или предварительное чтение завершились
неоднозначно, манифест может отсутствовать, но recovery record сохраняется.
Повторный deployment запрещён до операторской сверки hash, nonce и receipt и
отдельного решения по точному повтору тех же байтов.

Deployment CLI и автоматизированный агент не помечают recovery завершённым, не
удаляют и не архивируют record или оставшуюся lock-запись. После независимой
сверки сети оператор вручную переносит разрешённую запись в защищённый архив вне
registry либо оставляет её на месте как блокировку. Это отдельное операторское
действие; очистка ради повторного deployment без сохранённого свидетельства
запрещена.

## Предварительное чтение

Deployment script должен завершаться с ошибкой при несовпадении chain ID,
receipt, runtime, `destination`, `sponsor` или `self`. Положительный результат
этого шага основан на единственном RPC и означает только, что манифест можно
передать следующей проверке. Называть его окончательной аттестацией нельзя.

Оператор до продолжения сверяет, что:

- `manifest.source.kind` равен `git-commit`;
- `manifest.source.value` равен полному commit из release candidate;
- `manifest.artifact.sha256` равен SHA-256 канонического артефакта из release
  candidate;
- транзакция и receipt соответствуют манифесту;
- фактический расход не превышает `max-total-cost-wei`.

## Окончательная эксплуатационная аттестация

**ТОЛЬКО ОПЕРАТОР. Агент не запускает демон с конфигурацией публичной сети.**

Перед передачей ключей демон обязан полностью пройти запуск в live-конфигурации
с остановленными платными действиями:

```dotenv
DRY_RUN=false
EMERGENCY_STOP=true
```

При этом:

- `SOURCE_PRIVATE_KEY` и `SPONSOR_PRIVATE_KEY` отсутствуют в окружении процесса;
- настроены два независимых поставщика чтения и отдельный RPC отправки;
- указан новый проверенный манифест и точный канонический артефакт;
- демон получает общий финализированный блок от обоих поставщиков и сверяет
  chain ID, deployment transaction/receipt, runtime и все неизменяемые роли;
- открываются существующие watcher state и budget ledger без удаления,
  переименования, подмены или сброса;
- diagnostics показывает состояние `stopped`, ожидаемое при
  `EMERGENCY_STOP=true`; свежесть журнала не используется как критерий здоровья.

Отсутствие ключей является обязательной проверкой границы: signer и рабочий
broadcaster не должны создаваться. Любая ошибка запуска, расхождение RPC,
несовместимость state или budget ledger блокирует дальнейшую активацию.

Только после сохранения свидетельств этого запуска оператор может отдельно
утвердить передачу ключей через менеджер секретов и изменение
`EMERGENCY_STOP=false`. Это новое решение о live-риске, а не автоматическое
продолжение deployment script. Source-ключ всё равно считается
скомпрометированным, а победа rescue-транзакции не гарантируется.

## Свидетельства и отказ

Сохраняются полный commit, release candidate, SHA-256 артефакта и манифеста,
публичный hash транзакции, receipt, согласованный результат startup-аттестации и
состояние diagnostics. Не сохраняются и не публикуются ключи, подписи,
authorization tuple и RPC URL с данными доступа. Recovery record с подписанной
транзакцией хранится отдельно как закрытое операторское свидетельство и никогда
не публикуется. Его разрешение и архивирование выполняет только оператор вне
автоматизированного агента.

При любой неоднозначности оператор оставляет `EMERGENCY_STOP=true`, не повторяет
отправку вслепую и переходит к
[`../migration/rollback-emergency.md`](../migration/rollback-emergency.md).
