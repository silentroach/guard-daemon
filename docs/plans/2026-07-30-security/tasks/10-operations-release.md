# Task 10: Эксплуатация, миграция и документация выпуска

Статус: `IN PROGRESS`

Зависимости: Tasks 01-09.

## Цель

Подготовить полностью русскоязычную, точную и безопасную документацию для сборки, локальной проверки, изолированного запуска, обновления, emergency stop и rollback. Зафиксировать fail-closed границу неподдерживаемой in-place migration. Подготовить release candidate без выполнения mainnet deployment, tag или push.

## Границы задачи

- Полный перевод и переработка `README.md`.
- Русскоязычная справка CLI/config.
- Operator runbook, systemd/container hardening и monitoring guide.
- Deployment activation/migration/rollback checklist.
- Release checklist, SBOM, checksums и provenance instructions.
- Удаление устаревшей и дублирующей документации.

## Обязательные изменения

1. Вся пользовательская и эксплуатационная документация написана по-русски; английский только для принятых технических терминов, команд и идентификаторов.
2. Каждая задокументированная env/CLI option существует, тестируется и имеет точный безопасный default.
3. Удалить ложные claims: гарантированная atomic success, неподдерживаемые сети, phantom dry run/RPC/filter settings, неверные confirmations и устаревшие constructor examples.
4. Repository examples не содержат реальные incident addresses и legacy deployments.
5. Quick start по умолчанию запускает dry run/read-only на local chain либо требует явного подтверждения live mode.
6. Отдельно описать: source key остаётся compromised, победа не гарантирована, legacy EOA нельзя повторно использовать.
7. Deployment activation runbook: clean build, artifact verification, chain/destination/sponsor/runtime checks, operator confirmation, ограниченное funding и post-deploy readback.
8. Mainnet action выполняет только оператор. Agent instructions явно запрещают автоматический broadcast.
9. In-place смена rescuer не заявляется без crash-consistent переноса всех bindings, incidents, nonce и budget. Пока такого механизма нет, migration runbook обязан блокировать смену manifest/state, запрещать сброс постоянных данных и направлять оператора к emergency stop и отдельному forward fix.
10. Rollback/emergency runbook включает stop signing, сохранение monitoring, quarantine sponsor funds и incident evidence без публикации secrets.
11. Service запускается не от `root`, с отдельным пользователем, read-only filesystem где возможно, `NoNewPrivileges`, ограниченными capabilities, защищённым environment file и outbound policy.
12. Health/alerts основаны на state/metrics Task 08, а не на свежести логов.
13. Описать backup/permissions для checkpoints и budget state без хранения keys в backup.
14. Release process создаёт SBOM, checksums и evidence воспроизводимой сборки. Подпись artifacts выполняется только keyless CI с проверенной identity policy либо оператором вне агента; private signing key не передаётся в repository/agent environment.
15. Документировать `origin` как рабочий fork и `upstream` как источник синхронизации без автоматического push/tag.
16. Mainnet activation commands проходят только статическую проверку и local no-broadcast rehearsal; агент никогда не исполняет их.
17. Точно определить состав release candidate: source tree, pinned lock files, contract artifacts, schemas, SBOM, checksums, русскоязычные runbooks и review reports.
18. Official deployment manifest с release commit создаётся оператором или keyless CI только после существования неизменяемого release commit.

## Параллельные направления

- **README и CLI/config:** только `/README.md`, `/docs/configuration.md`, `/docs/cli.md`.
- **Эксплуатация:** только `/docs/operations/**` и `/packaging/systemd/**`.
- **Deployment/migration:** только `/docs/deployment/**`, `/docs/migration/**`.
- **Release:** только `/docs/release/**`, scripts создания SBOM/checksums без signing secret.

Каждый subagent редактирует отдельные документы. Координирующий агент выполняет терминологическую и фактическую сверку всего набора.

## Критерии приёмки

- В публичных документах нет обычных англоязычных абзацев, персональных данных, реальных keys/private URLs и incident-specific defaults.
- Все команды проверены на clean clone с документированными native tool
  versions без зависимости от Nix; дополнительный локальный запуск через
  `nix develop` разрешён, но не заменяет CI-compatible evidence.
- Каждая настройка автоматически сверяется с typed config schema.
- Quick start не способен случайно выполнить live transaction.
- Service hardening соответствует фактическим файлам установки и не требует `root` для daemon.
- Deployment и rollback проверены локальной репетицией; migration runbook точно описывает проверяемый fail-closed отказ неподдерживаемой смены rescuer.
- Release checklist требует успешные GitHub Actions checks, operator evidence branch protection/Dependabot и отсутствие Critical/High findings.
- Release candidate подготовлен, но tag/push/mainnet deployment не выполнены без отдельного запроса.

## Проверка

- Documentation command tests либо проверяемые shell examples без live network.
- Link/config-schema validation.
- Secret scan всех документов и generated release metadata.
- Local rehearsal install/start/health/stop/rollback и тесты отсутствия отдельного proactive renewal.
- Проверка SBOM/checksum reproducibility на двух clean builds.

## Независимое ревью

Отдельный ревьюер читает документацию как новый оператор и как атакующий. Он проверяет точность, опасные двусмысленности, язык, secret hygiene, privilege boundaries и готовность к последующему целостному ревью Task 11.

Все findings исправляются. После значимого изменения команды или runbook провести повторное rehearsal и review pass.

## Завершение и коммит

После ревью сохранить отчёт в `../reviews/10-operations-release.md`, выполнить closure pass и изменить статус на `DONE` здесь и в `../tasks.md`.

Рекомендуемый subject:

```text
docs(release): подготовить безопасную русскоязычную эксплуатацию и выпуск
```

Тело коммита подробно описывает перевод, удалённые unsafe claims, operator safeguards, проверенные runbooks, release evidence и residual risks.
