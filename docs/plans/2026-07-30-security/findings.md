# Матрица findings и доказательств закрытия

Исходная база аудита: публичный Git commit `b60199e`.

Матрица не содержит incident-specific identifiers. Каждая строка закрывается только ссылкой на regression test/gate и публичный review report соответствующей задачи. После изменения исходных путей задача сохраняет ссылку на новый test path, а не удаляет строку.

| ID | Критичность | Класс риска | Исходные пути | Задача | Обязательное доказательство | Статус |
|---|---|---|---|---|---|---|
| GD-001 | Critical | Configured deployment может быть legacy и не соответствовать secure source | `.env.example`, `main.go`, `contracts/RescuerV2.sol` | Task 04 | Runtime+immutable attestation test и manifest verification | OPEN |
| GD-002 | Critical | Заявленный dry run способен выполнить live action | `README.md`, `guard-daemon-HELP_RU.txt`, `main.go` | Task 05 | No-sign/no-broadcast integration test | OPEN |
| GD-003 | Critical | Один недоверенный RPC является единственным источником attestation/state | `main.go` RPC reads/startup checks | Tasks 04, 05, 07 | Independent-provider quorum Byzantine tests | OPEN |
| GD-004 | Critical | Proactive renewal может вызвать active malicious delegation | `main.go` delegation renewal | Task 07 | Test skipped authorization без вызова source fallback | OPEN |
| GD-005 | High | Unknown token events усиливают sponsor gas spend | `main.go` event filter и fee paths | Task 08 | Cumulative budget/property/load tests | OPEN |
| GD-006 | High | Receipt/source-zero допускают false success | `main.go` receipt и balance postcheck | Task 07 | Destination outcome и fail-closed RPC tests | OPEN |
| GD-007 | High | Retry exhaustion навсегда блокирует будущие deposits | `main.go` retry maps/limits | Task 07 | New-generation-after-exhaustion test | OPEN |
| GD-008 | High | Contention/transient error теряет unknown candidate | `main.go` busy/error returns | Tasks 06, 07 | Durable handoff crash/replay tests | OPEN |
| GD-009 | High | Polling/reconnect/reorg пропускают события | `main.go` watcher/polling | Task 06 | Backfill/checkpoint/reorg tests | OPEN |
| GD-010 | High | Нет crash-consistent watcher-to-rescue handoff | `main.go` in-memory goroutine dispatch | Tasks 02, 06, 07 | Durable Put/Ack/incident persistence tests | OPEN |
| GD-011 | High | Нет атомарного global sponsor budget между сетями | `main.go` per-network state/fee paths | Task 08 | Concurrent ledger reservation/crash tests | OPEN |
| GD-012 | High | Второй процесс может конфликтовать по sponsor nonce | `main.go` nonce/submission paths | Task 07 | Exclusive lease и two-process test | OPEN |
| GD-013 | High | Sponsor hot wallet может совпасть с safe destination | `main.go`, `.env.example`, contract constructors | Tasks 03, 05 | Constructor/config/deployment negative tests | OPEN |
| GD-014 | High | Signer identity не сверяется с configured role | `main.go` key loading/startup | Task 05 | Fake signer mismatch tests | OPEN |
| GD-015 | High | Non-standard ERC-20 return data ломает rescue | `contracts/RescuerV2.sol` | Task 03 | No-return/false/malformed token tests | OPEN |
| GD-016 | High | Permit path неиспользуем, сложен и создаёт дополнительный риск | `contracts/PermitSweeper.sol`, `main.go`, `scripts/*Permit*` | Tasks 02, 03, 04, 10 | Полное отсутствие PermitSweeper в production ABI/artifacts/docs | OPEN |
| GD-017 | Medium | Deployment scripts допускают partial success/stale config | `scripts/*.ts` | Task 04 | Non-zero failure и atomic update tests | OPEN |
| GD-018 | Medium | Deployment/compiler/Node build невоспроизводим | `scripts/*.ts`, отсутствующие Node manifests | Tasks 01, 04 | `flake.lock`, `package-lock.json`, `test/contracts/reproducibility.test.ts`, [review 01](reviews/01-foundation.md); deployment manifests остаются Task 04 | PARTIAL |
| GD-019 | Medium | Устаревшие dependencies и advisories | `go.mod`, `go.sum` | Tasks 01, 09 | `make vuln`, `make audit`, [review 01](reviews/01-foundation.md); финальный dependency gate остаётся Task 09 | PARTIAL |
| GD-020 | Medium | Нет tests, GitHub Actions и Dependabot | repository test/`.github` baseline | Tasks 01, 09 | `.github/workflows/*.yml`, `.github/dependabot.yml`, [review 01](reviews/01-foundation.md); полный security test gate остаётся Task 09 | PARTIAL |
| GD-021 | Medium | Документированные config fields не реализованы | `README.md`, `guard-daemon-HELP_RU.txt`, `main.go` | Task 05 | Schema-to-doc parity test | OPEN |
| GD-022 | Medium | Private RPC URL и secrets могут попасть в logs/repo | отсутствие `.gitignore`, config/log paths | Tasks 01, 05, 08 | `.gitignore`, `.gitleaks.toml`, `make secret-scan`, [review 01](reviews/01-foundation.md); runtime redaction остаётся Tasks 05 и 08 | PARTIAL |
| GD-023 | Medium | Incident-specific identifiers остаются в source/docs | `main.go`, contract comments, public docs | Tasks 01, 02, 03, 10 | Repository-wide scan и очищенный `.env.example`, [review 01](reviews/01-foundation.md); удаление из owned paths остаётся Tasks 02, 03 и 10 | PARTIAL |
| GD-024 | Medium | Unknown token невозможно доказательно считать экономически спасённым | `main.go` unknown token/outcome paths | Tasks 07, 08 | Trust-tier tests и `token-reported` outcome | OPEN |
| GD-025 | Medium | Service/root/log-health/deployment operations небезопасны или неточны | public help/operations baseline | Task 10 | Local hardening/rehearsal tests | OPEN |
| GD-026 | Medium | Публичная документация не полностью русскоязычна и содержит phantom claims | `README.md`, `guard-daemon-HELP_RU.txt` | Task 10 | Language/config/command validation | OPEN |
| GD-027 | Medium | После изменений документации и эксплуатации отсутствует целостное финальное ревью | весь release candidate | Task 11 | Финальный review report и полный CI digest | OPEN |
