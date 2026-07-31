# Task 04: Воспроизводимый деплой и аттестация runtime

Статус: `DONE`

Starting commit: `80eeff6d77abf12f0b47bb88a9dab0014c1c816f`.

Реализация, проверки и независимое ревью завершены. Отчёт:
[`reviews/04-deployment-attestation.md`](../reviews/04-deployment-attestation.md).

Зависимости: Tasks 01, 02 и 03.

## Цель

Исключить использование legacy или подменённого implementation. Deployment tooling должно работать из pinned artifacts, fail closed при partial failure и создавать проверяемое описание результата. Daemon должен подтверждать runtime identity и immutable configuration до любой подписи.

## Границы задачи

- `scripts`/`tools/deploy` и package scripts.
- Contract artifacts и schemas deployment manifest.
- `internal/contracts` runtime attestation.
- `internal/contracts` API для обязательной attestation без изменения startup wiring, которое принадлежит Task 05.
- Local-chain deployment tests.

Mainnet deployment не входит в scope и запрещён агентам.

## Обязательные изменения

1. Deployment tools используют contract artifacts Task 03 и pinned compiler Task 01, а не компилируют случайной локальной версией.
2. Удалить или исправить дублирующиеся broken deploy scripts; один canonical path на contract type.
3. Перед deployment сверять фактический chain ID с expected chain ID.
4. Проверять constructor arguments, deployed runtime, destination, sponsor/operator и artifact provenance.
5. Генерировать machine-readable manifest: chain ID, contract role, address, transaction hash для публичного official deployment, block, compiler/settings, source commit, artifact hash, runtime hash и immutable values.
6. Operator-specific manifests с адресами не коммитятся автоматически. Repository defaults остаются пустыми либо с reviewed official manifests.
7. `.env.example` не содержит непроверенных legacy addresses.
8. Обновлять operator config атомарно только после успешной проверки critical contract.
9. Любой critical network failure даёт non-zero exit status. Успех вспомогательного contract не маскирует failure Rescuer.
10. Attestation API загружает trusted manifest и сверяет chain, runtime bytecode/hash, destination и sponsor getter; startup integration выполняется в Task 05.
11. Attestation API сверяет минимум два независимых read providers на одном finalized block/hash: chain ID, runtime, destination и sponsor. Любое расхождение возвращает blocking error.
12. Contract, возвращающий ожидаемый `destination()`, но имеющий другой runtime, должен быть отклонён.
13. Legacy contract без sponsor getter должен быть отклонён до signing/broadcasting.
14. Deployment tool ничего не отправляет без explicit `--broadcast`; default mode строит/проверяет plan локально.
15. Manifest до task commit использует digest подготовленного source tree/artifact. Official manifest с release commit создаёт operator/CI после появления неизменяемого commit.

## Параллельные направления

- **Deployment CLI:** только существующие `/scripts/**`, новые `/tools/deploy/**` и `/test/deploy/cli/**`; устаревшие/permit scripts удаляются этим владельцем.
- **Manifest/artifacts:** только `/deployments/schema/**`, `/artifacts/**` и `/test/deploy/fixtures/**`.
- **Go attestation API:** только `/internal/contracts/attestation*` и соответствующие tests.
- **Local-chain integration:** только `/test/deploy/integration/**`.

Startup wiring в этой задаче не меняется.

## Критерии приёмки

- Local deployment из clean clone воспроизводится pinned commands.
- Runtime attestation принимает только artifact с ожидаемыми immutable values.
- Старый ABI/runtime с тем же destination отклоняется.
- Wrong chain, missing code, wrong sponsor, wrong destination, partial deployment и stale manifest дают явную ошибку/non-zero status.
- Один Byzantine provider при правильном втором provider блокирует attestation и signing path.
- No-broadcast mode доказуемо не вызывает transaction submission.
- Публичный репозиторий не содержит ключи оператора, RPC credentials и incident-specific defaults.
- Русскоязычный deployment guide описывает только local verification; production activation остаётся Task 10.

## Проверка

- Unit tests manifest parsing/hash comparison.
- Local-chain deploy + readback + tampered manifest tests.
- Typecheck/lint deployment tools.
- Contract artifact reproducibility check из двух clean builds.
- Go tests для fake contract с правильным getter и неправильным runtime.

## Независимое ревью

Ревьюер проверяет supply chain, соответствие artifact/runtime, immutable linking, поведение при частичной ошибке, отсутствие неявной отправки и утечек в generated files.

После fixes повторить clean local deploy и tamper tests.

## Завершение и commit

После ревью сохранить отчёт в `../reviews/04-deployment-attestation.md`, выполнить closure pass и изменить status на `DONE` здесь и в `../tasks.md`.

Рекомендуемый subject:

```text
security(deploy): аттестовать runtime и исключить legacy deployments
```

Commit body включает manifest design, fail-closed behavior, local verification и явное подтверждение отсутствия live deployment.
