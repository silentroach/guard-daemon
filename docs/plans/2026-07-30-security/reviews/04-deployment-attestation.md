# Отчёт независимого ревью Task 04

Задача: `Task 04`

Статус ревью: `ПРОЙДЕНО`

## Проверенный снимок

- Base commit: `80eeff6d77abf12f0b47bb88a9dab0014c1c816f`.
- Проверенные пути: deployment scripts и tests, canonical artifact и schema,
  `internal/contracts/attestation*`, package/Makefile integration и изменённая
  публичная документация.
- SHA-256 digest diff проверенных production/test/doc файлов:
  `c6c3b0394e0a171ae85115af46caaaa864f229ddaf2e09e52154f7fb91c78a5a`.
- Из digest исключены только этот отчёт, `findings.md`, task-файл и task index.
- Digest построен из binary/full-index tracked diff от base и отсортированных
  SHA-256 содержимого untracked files; включены 20 путей.
- Ревьюер не участвовал в реализации: `да`.

## Проверенные инварианты

- CLI непосредственно перед планированием и отправкой повторно компилирует
  pinned source точным `solc` и побайтно сверяет единственный canonical artifact.
- Без `--broadcast` CLI не обращается к RPC, не читает ключ, не подписывает и не
  отправляет транзакцию.
- Broadcast проверяет output paths, artifact и фактический chain ID до чтения
  ключа; signer обязан совпадать со sponsor.
- Manifest связывает artifact, source tree, compiler settings, signed CREATE
  transaction, receipt, deployment block, linked runtime и immutable values.
- Аттестация требует минимум два read provider с разными endpoint fingerprint,
  trust domain и экземплярами client и единогласие на общем finalized block
  hash.
- Code и getters читаются по block hash; legacy runtime, отсутствующий sponsor
  getter, Byzantine divergence и недекодируемые ответы блокируют результат.
- Manifest и operator config публикуются только после exact runtime/getter
  readback; path, symlink и hardlink conflicts отклоняются до RPC.
- Permit deployment path удалён; operator-specific manifests, RPC credentials и
  ключи в repository defaults отсутствуют.

## Findings

| ID | Критичность | Путь/строка | Описание | Требуемое исправление | Статус |
|---|---|---|---|---|---|
| REV-04-001 | High | `scripts/deployRescuerV2.ts`, `scripts/verifyArtifacts.ts` | Первичный вариант доверял выбранному artifact и непроверенному source commit | Оставить один artifact, повторно собирать его из pinned source и удалить неподтверждённый commit provenance | ЗАКРЫТ |
| REV-04-002 | High | `internal/contracts/attestation.go` | Уникальные display ID не доказывали независимость providers | Проверять endpoint fingerprint, trust domain и reader identity | ЗАКРЫТ |
| REV-04-003 | Medium | `internal/contracts/attestation.go`, `scripts/deployRescuerV2.ts` | Manifest не был связан с deployment transaction/receipt, TS readback использовал block number | Проверить exact signed CREATE transaction/receipt у каждого provider и читать state по block hash | ЗАКРЫТ |
| REV-04-004 | Medium | `scripts/deployRescuerV2.ts` | Совпадающие output paths могли повредить manifest с успешным exit status | Отклонять canonical path и inode aliases до RPC | ЗАКРЫТ |

Новых findings в closure pass нет.

## Проверка исправлений

- Closure pass выполнен для каждого изменения после первичного ревью.
- Digest обновлён после последнего изменения проверяемых файлов и независимо
  воспроизведён координирующим агентом.
- `make node-ci`: успешно; 31 Forge test, 2 artifact test и 7 deployment test,
  включая полный локальный Anvil deployment.
- `make go-ci`: успешно.
- `make format-check`: успешно.
- `go test -race -mod=readonly ./internal/contracts`: успешно.
- `go vet -mod=readonly ./internal/contracts`: успешно.
- `make secret-scan`: успешно, утечки не найдены.
- `npm run audit`: успешно, уязвимости не найдены.
- Mainnet deployment и отправка реальных транзакций не выполнялись.

## Остаточные риски

| ID | Критичность | Обоснование | Владелец | Срок пересмотра |
|---|---|---|---|---|
| Нет | — | Нет в рамках Task 04 | — | — |

Граница следующей задачи: Task 05 должна формировать provider identities из
нормализованной доверенной конфигурации и вызывать аттестацию до создания signer.
До этого production use запрещён; это dependency, а не принятый остаточный риск
Task 04.

## Решение

`ПРОЙДЕНО`. Critical и High findings закрыты, актуальный digest воспроизведён,
критерии Task 04 подтверждены. Статус задачи меняется на `DONE` только после
разрешённого atomic task commit.
