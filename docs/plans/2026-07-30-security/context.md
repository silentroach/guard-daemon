# Контекст устранения недостатков guard-daemon

## Назначение

Это общий контекст для каждой задачи из этой папки. Репозиторий публичный. Любое описание задачи, тестовые данные, фрагмент лога, замечание ревью и сообщение коммита следует считать публичной информацией.

Проект наблюдает за скомпрометированным EVM-аккаунтом (EOA) и пытается переводить поступающие активы на безопасный адрес. Отдельный sponsor-аккаунт оплачивает комиссии. EIP-7702 позволяет source-аккаунту делегировать выполнение rescue-контракту, но source-ключ остаётся скомпрометированным, поэтому атакующий способен подписывать конкурирующие авторизации.

Цель плана не ограничивается успешной сборкой. Система должна безопасно отказывать, ограничивать финансовый ущерб, подтверждать каждый заявленный результат и воспроизводимо выпускаться.

## Язык документации

- Вся публичная пользовательская и эксплуатационная документация должна быть на русском языке.
- `README.md`, справка, runbook, release checklist, task-файлы и сообщения об ошибках для оператора должны быть русскоязычными.
- Основной текст, заголовки, критерии и пояснения пишутся по-русски. Не использовать обычные английские слова там, где есть точный и естественный русский эквивалент.
- Английский допустим только для идентификаторов кода, имён переменных, команд, названий протоколов, инструментов и общепринятых технических терминов, которые обычно не переводят.
- При изменении поведения документация обновляется в том же task-коммите. Нельзя оставлять англоязычное или устаревшее описание рядом с новым поведением.

## Правила публичной информации

- Никогда не добавлять приватные ключи, seed-фразы, API-ключи, credentials, приватные RPC URL, access token, персональные имена, личную переписку и непубличные детали инцидента.
- Никогда не копировать реальный `.env` в исходники, тесты, артефакты, задачи, ревью и коммиты.
- Не добавлять связанные с конкретным инцидентом адреса кошельков в remediation-документацию и фикстуры. Для тестов использовать явно обозначенные детерминированные локальные адреса.
- Публичные адреса официальных деплоев разрешено фиксировать только в специально спроектированном и проверенном deployment manifest. Операторские адреса не должны становиться repository defaults.
- Не логировать секреты и authorization signatures. Реальная подписанная транзакция до включения в блок может быть чувствительной и не должна становиться фикстурой.
- Агентам запрещено выполнять mainnet-деплои, отправлять реальные транзакции, пополнять счета, ротировать ключи и запрашивать секреты. Активация в mainnet выполняется оператором по runbook.
- При обнаружении чувствительных данных в Git history остановить задачу, не цитировать значение и сообщить только путь и необходимое действие.

## Модель доверия

### Доверенные компоненты

- Проверенный исходный код на зафиксированном commit.
- Безопасный destination, управляемый независимо от скомпрометированного source.
- Sponsor/operator key с учётом того, что это hot key с намеренно ограниченным балансом.
- Воспроизводимо собранные contract artifacts и deployment manifests, прошедшие локальную проверку.
- Результат security-critical чтения только после согласования независимого RPC quorum на одном finalized block.

### Недоверенные компоненты

- Source private key. Считать, что атакующий всегда может подписывать транзакции и EIP-7702 authorization.
- Публичный mempool, порядок транзакций, builders и конкурирующие relayers.
- Token contracts, metadata, event logs, return data, callbacks и соответствие ERC-20.
- Публичные RPC, WebSocket, ответы, доступность, reorg и rate limits.
- Существующие on-chain реализации, пока не проверены runtime identity и immutable-параметры.
- Environment input, примеры документации, устаревшие адреса деплоев и частичный результат deployment script.

### Возможности атакующего

- Заменять EIP-7702 delegation source-аккаунта с помощью скомпрометированного ключа.
- Выигрывать гонку nonce или ordering и добиваться пропуска authorization tuple.
- Заставлять outer transaction вызывать код текущей вредоносной делегации.
- Наблюдать и front-run публичные транзакции и EIP-2612 signatures.
- Разворачивать произвольные token contracts, генерировать правдоподобные `Transfer` logs, возвращать фиктивный баланс, тратить gas, делать revert, re-enter и возвращать нестандартные данные.
- Создавать множество событий с разных адресов и увеличивать расходы sponsor.
- Провоцировать RPC errors, disconnects, duplicates, delayed receipts и короткие reorg.

## Не-цели

- Восстановление секретности скомпрометированного source key.
- Гарантия победы rescue-транзакции в каждой гонке.
- Признание legacy EOA безопасным после rescue.
- Автоматическая оценка стоимости любого неизвестного токена.
- Необратимые production/mainnet-действия из автоматизированной задачи.

## Обязательные инварианты безопасности

1. Каждый путь перемещения активов имеет фиксированный или криптографически связанный destination.
2. Произвольный внешний вызов от имени delegated EOA разрешён только ожидаемому sponsor/operator и недоступен произвольному caller или callback.
3. Sponsor и destination являются разными ролями и разными адресами: sponsor — ограниченный hot wallet, destination — независимо управляемое безопасное хранилище.
4. Startup отклоняет implementation, если chain, runtime identity, destination, sponsor или происхождение artifact не совпадают с доверенной конфигурацией.
5. Security-critical chain state подтверждается минимум двумя независимо настроенными read providers на одном finalized block. Расхождение блокирует создание signer и signing.
6. Адрес, возвращаемый signer, до первой подписи совпадает с явно настроенной ролью source/sponsor.
7. Proactive delegation renewal не вызывает source EOA. Предпочтительный вариант — удалить отдельный renewal и прикладывать authorization только к конкретной rescue operation. Если отдельный renewal всё же обоснован, outer transaction направляется на заранее аттестованный безопасный sink, а не на source.
8. Dry run не подписывает и не отправляет транзакции и не требует приватных ключей, когда достаточно публичных адресов.
9. Успешный receipt не считается успешным rescue без fail-closed postcondition. Для недоверенного token contract результат называется только `token-reported outcome` и не считается доказанной экономической ценностью.
10. Отсутствующий, испорченный, просроченный или недекодируемый RPC response никогда не интерпретируется как успех.
11. Расходы sponsor ограничены на одну транзакцию и суммарно. Единый persistent budget ledger атомарно резервирует максимальную стоимость до signing для всех сетей.
12. Автоматическая работа с unknown tokens только opt-in и всегда ограничена; по умолчанию paid action запрещён.
13. Между watcher и rescue используется crash-consistent протокол: stable candidate ID, durable put, incident persistence, acknowledgement и idempotent replay.
14. Работа ставится в queue и дедуплицируется. Contention, reconnect, receipt timeout и временные RPC errors не теряют rescue candidate молча.
15. Retry limit относится к конкретному rescue incident, а не навсегда к token address. Новое поступление после cooldown/state transition создаёт новую работу.
16. Один coordinator на сеть владеет sponsor nonce allocation и transaction submission. Межпроцессный lock/lease запрещает второй daemon для той же пары chain+sponsor.
17. Polling checkpoint продвигается только после успешной обработки. После reconnect выполняется backfill, reorg обрабатывается явно.
18. Каждый network call отменяем и имеет подходящий deadline.
19. Любая задокументированная настройка реализована и протестирована. Неподдерживаемые настройки удаляются из публичной документации.
20. Секреты не коммитятся и не логируются; generated artifacts не содержат operational secrets.
21. Builds, tests, contract bytecode и deployment records воспроизводимы из pinned dependencies.

## Исходный класс рисков

Каждая задача должна сначала повторно проверить актуальный код. Следующие пункты описывают классы рисков, а не разрешают считать реализацию неизменной:

- Безопасный contract source может не совпадать с configured или active on-chain implementation.
- Startup может не подтверждать полную runtime identity и immutable operator configuration.
- Заявленные dry-run, custom RPC, token filtering и network selection могут отсутствовать.
- Unknown token events могут неограниченно увеличивать gas spend sponsor.
- Event gaps, skipped work, retry exhaustion и fail-open outcome checks могут пропускать или неверно классифицировать rescue.
- Optional-return ERC-20 требует явной поддержки; неиспользуемый permit path должен быть полностью удалён из production surface.
- Deployment scripts, Node dependencies, compiler versions и manifests могут быть невоспроизводимы.
- Automated tests и release gates недостаточны для программы, подписывающей финансовые транзакции.

## Целевая архитектура

Task 02 может уточнить имена, но должен сохранить границы для безопасной параллельной работы:

- `cmd/guard-daemon`: запуск процесса, dependency wiring, shutdown и выбор режима.
- `internal/config`: typed config, validation, enablement сетей, dry-run policy и публичные адреса.
- `internal/contracts`: ABI, artifacts/manifests, runtime attestation и read helpers.
- `internal/watcher`: subscriptions, polling, backfill, reorg, deduplication и формирование rescue candidates.
- `internal/rescue`: transaction coordination, nonce ownership, simulation, retries, fee policy, submission, receipts и postconditions.
- `internal/store`: crash-consistent candidate/incident/checkpoint persistence и межпроцессные leases.
- `internal/budget`: единый persistent ledger резервов и фактических расходов для всех сетей.
- `internal/rpc`: bounded clients, deadlines, reconnect lifecycle и тестируемые interfaces.
- `internal/observability`: structured redacted logs, metrics, alerts и health state.
- `contracts` и `test/contracts`: Solidity contracts и adversarial tests.
- `scripts` или `tools/deploy`: воспроизводимый deployment и verification из pinned artifacts.

Не выполнять большую framework migration без доказанной необходимости. Предпочитать маленькие interfaces и детерминированные state transitions.

## Протокол выполнения задачи агентом

### 1. Определить scope

- Полностью прочитать этот файл, `tasks.md` и назначенный task-файл.
- Проверить `git status`, последние commits и актуальную реализацию до изменений.
- Зафиксировать starting commit в рабочих заметках без чувствительной информации.
- Соблюдать dependencies. Нельзя завершать задачу при незавершённой зависимости.

### 2. Безопасно распараллелить

- Запускать субагентов реализации параллельно только для направлений с непересекающимися путями из task-файла.
- Каждому субагенту передавать правила публичной информации, разрешённые пути, критерии приёмки и команды проверки.
- Два субагента не редактируют один файл. Общие interfaces фиксируются до начала параллельной реализации.
- Координирующий агент интегрирует результаты и отвечает за взаимодействие всех направлений.

### 3. Проверить

- Выполнить task-specific unit/integration tests, static analysis, formatting и build.
- Успешная сборка не заменяет behavioral tests.
- Не запускать daemon с production keys и не отправлять live transactions.
- Для transaction tests использовать deterministic local chain и test keys.

### 4. Независимое ревью

- После реализации и первичной проверки запустить отдельного review subagent, который не писал этот код.
- Передать reviewer этот context, task-файл, полный diff задачи и результаты тестов.
- Требовать security-oriented review: regressions, нарушения threat model, пропущенные adversarial tests, утечки публичных данных и точность русскоязычной документации.
- Сохранить публичный отчёт по шаблону `review-template.md`: base commit, digest проверяемого diff без самого отчёта/status-файлов, независимость reviewer, findings, evidence и решение.
- Исправить каждое замечание в границах задачи и повторить нужные tests.
- Любое изменение проверенных production/test файлов после review требует closure pass по изменённому diff и обновления отчёта. Формулировка «изменение несущественно» не отменяет closure pass.
- Нельзя завершать задачу с открытыми Critical или High findings. Остальные исправить либо явно занести как accepted residual risk в task и index.

### 5. Завершить и закоммитить

- Обновить status задачи и checkbox в `tasks.md` только после implementation, verification и independent review.
- До staging проверить `git status`, `git diff` и недавний log.
- Stage только файлы задачи, не включая чужие изменения worktree.
- Создать один atomic commit, если задача явно не требует небольшой упорядоченной серии.
- Не делать amend, force-push, skip hooks и push без явной команды оператора.

Подробное сообщение коммита:

```text
security(<area>): краткий результат

Контекст:
- Какой риск или сломанное поведение потребовали изменения.

Изменения:
- Что изменено и какие design decisions приняты.

Обоснование безопасности:
- Какие инварианты теперь соблюдаются и как обрабатывается отказ.

Проверка:
- Точные команды и результаты build, tests, analysis и review.

Остаточные риски:
- Оставшиеся ограничения или «Нет в рамках задачи».

Отчёт ревью:
- Путь к committed review report и digest проверенного diff.
```

Commit message может быть русскоязычным. Не включать адреса кошельков, transaction payloads, секреты, персональную информацию и детали приватной инфраструктуры.

## Критичность и правила завершения

- **Critical:** прямой asset/key compromise, unsafe live default или safety mode, выполняющий live action. Блокирует любое production use.
- **High:** практическая потеря, неограниченный sponsor spend, silent missed rescue, false success или ошибка deployment identity. Блокирует release.
- **Medium:** defense-in-depth gap, broken optional path, reproducibility issue или operational fragility. Устранить до final release либо явно принять.
- **Low:** maintainability или clarity issue с ограниченным непосредственным эффектом.

Critical/High нельзя принимать как исключение. Advisory разрешено переклассифицировать только после доказательства недостижимости уязвимого кода, независимого ревью, назначения владельца и срока повторной проверки.

`DONE` означает завершённые code, tests, русскоязычную публичную документацию, независимое ревью, committed review report и task commit. Это не означает «написан первый вариант».

## Общие release gates

- Repository defaults не содержат ссылок на непроверенный legacy deployment.
- Contract и Go tests покрывают adversarial EIP-7702 ordering, malicious tokens, non-standard ERC-20, RPC failures, reconnect gaps и reorg.
- Тесты и документация подтверждают полное отсутствие удалённого PermitSweeper/permit production path.
- RPC quorum tests покрывают Byzantine provider, расходящиеся finalized blocks, runtime, receipt и balances.
- Dry-run tests доказывают отсутствие signing и broadcasting.
- Dependency, secret, static-analysis, race и vulnerability scans проходят на pinned toolchains.
- Deployment tooling создаёт verifiable manifest и возвращает non-zero exit status при security-critical partial failure.
- Вся пользовательская и эксплуатационная документация написана на русском и содержит только реализованные команды.
- Operator runbook явно отделяет local verification от mainnet activation.
- После документационной и эксплуатационной задачи выполнено отдельное финальное независимое ревью всего release candidate; оно не содержит нерешённых Critical и High findings.
