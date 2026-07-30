# Матрица findings и доказательств закрытия

Исходная база аудита: публичный Git commit `b60199e`.

Матрица не содержит incident-specific identifiers. Каждая строка закрывается только ссылкой на regression test/gate и публичный review report соответствующей задачи. После изменения исходных путей задача сохраняет ссылку на новый test path, а не удаляет строку.

| ID | Критичность | Класс риска | Исходные пути | Задача | Обязательное доказательство | Статус |
|---|---|---|---|---|---|---|
| GD-001 | Critical | Configured deployment может быть legacy и не соответствовать secure source | `.env.example`, `internal/config/legacy.go`, `internal/rescue/coordinator.go`, `contracts/RescuerV2.sol` | Task 04 | Runtime+immutable attestation test и manifest verification | OPEN |
| GD-002 | Critical | Заявленный dry run способен выполнить live action | `README.md`, `guard-daemon-HELP_RU.txt`, `cmd/guard-daemon`, `internal/config` | Task 05 | No-sign/no-broadcast integration test | OPEN |
| GD-003 | Critical | Один недоверенный RPC является единственным источником attestation/state | `internal/rpc`, `internal/rescue` startup/state reads | Tasks 04, 05, 07 | Independent-provider quorum Byzantine tests | OPEN |
| GD-004 | Critical | Proactive renewal может вызвать active malicious delegation | `internal/rescue/operations.go` delegation renewal | Task 07 | Test skipped authorization без вызова source fallback | OPEN |
| GD-005 | High | Unknown token events усиливают sponsor gas spend | `internal/watcher/service.go`, `internal/rescue` fee paths | Task 08 | Cumulative budget/property/load tests | OPEN |
| GD-006 | High | Receipt/source-zero допускают false success | `internal/rescue/operations.go` receipt и balance postcheck | Task 07 | Fail-closed unreadable postcondition: `internal/rescue/session_test.go`, [review 02](reviews/02-go-architecture.md); destination outcome остаётся Task 07 | PARTIAL |
| GD-007 | High | Retry exhaustion навсегда блокирует будущие deposits | `internal/rescue/coordinator.go`, `internal/rescue/operations.go` | Task 07 | New-generation-after-exhaustion test | OPEN |
| GD-008 | High | Contention/transient error теряет unknown candidate | `cmd/guard-daemon/daemon.go`, `cmd/guard-daemon/handoff.go`, `internal/rescue` | Tasks 06, 07 | In-process `Nack`/replay и poison-candidate tests: `cmd/guard-daemon/handoff_test.go`, [review 02](reviews/02-go-architecture.md); durable handoff остаётся Tasks 06/07 | PARTIAL |
| GD-009 | High | Polling/reconnect/reorg пропускают события | `internal/watcher/service.go` | Task 06 | Backfill/checkpoint/reorg tests | OPEN |
| GD-010 | High | Нет crash-consistent watcher-to-rescue handoff | `internal/store`, `internal/watcher`, `cmd/guard-daemon/handoff.go` | Tasks 02, 06, 07 | Interface crash/replay tests: `internal/store/interfaces_test.go`, [review 02](reviews/02-go-architecture.md); persistent production store остаётся Tasks 06/07 | PARTIAL |
| GD-011 | High | Нет атомарного global sponsor budget между сетями | `internal/rescue`, `internal/budget/interfaces.go` | Task 08 | Concurrent ledger reservation/crash tests | OPEN |
| GD-012 | High | Второй процесс может конфликтовать по sponsor nonce | `cmd/guard-daemon`, `internal/rescue` nonce/submission paths | Task 07 | Exclusive lease и two-process test | OPEN |
| GD-013 | High | Sponsor hot wallet может совпасть с safe destination | `internal/config`, `internal/rescue`, `.env.example`, contract constructors | Tasks 03, 05 | Constructor/config/deployment negative tests | OPEN |
| GD-014 | High | Signer identity не сверяется с configured role | `internal/config/legacy.go`, `internal/rescue/signer.go`, `cmd/guard-daemon/daemon.go` | Task 05 | Fake signer mismatch tests | OPEN |
| GD-015 | High | Non-standard ERC-20 return data ломает rescue | `contracts/RescuerV2.sol` | Task 03 | No-return/false/malformed token tests | OPEN |
| GD-016 | High | Permit path неиспользуем, сложен и создаёт дополнительный риск | `contracts/PermitSweeper.sol`, `scripts/*Permit*`; Go production surface удалён | Tasks 02, 03, 04, 10 | Go scan и `internal/contracts` tests: [review 02](reviews/02-go-architecture.md); contract/deployment artifacts остаются Tasks 03/04/10 | PARTIAL |
| GD-017 | Medium | Deployment scripts допускают partial success/stale config | `scripts/*.ts` | Task 04 | Non-zero failure и atomic update tests | OPEN |
| GD-018 | Medium | Deployment/compiler/Node build невоспроизводим | `scripts/*.ts`, отсутствующие Node manifests | Tasks 01, 04 | `flake.lock`, `package-lock.json`, `test/contracts/reproducibility.test.ts`, [review 01](reviews/01-foundation.md); deployment manifests остаются Task 04 | PARTIAL |
| GD-019 | Medium | Устаревшие dependencies и advisories | `go.mod`, `go.sum` | Tasks 01, 09 | `make vuln`, `make audit`, [review 01](reviews/01-foundation.md); финальный dependency gate остаётся Task 09 | PARTIAL |
| GD-020 | Medium | Нет tests, GitHub Actions и Dependabot | repository test/`.github` baseline | Tasks 01, 09 | `.github/workflows/*.yml`, `.github/dependabot.yml`, [review 01](reviews/01-foundation.md); полный security test gate остаётся Task 09 | PARTIAL |
| GD-021 | Medium | Документированные config fields не реализованы | `README.md`, `guard-daemon-HELP_RU.txt`, `internal/config` | Task 05 | Schema-to-doc parity test | OPEN |
| GD-022 | Medium | Private RPC URL и secrets могут попасть в logs/repo | `internal/config`, `internal/observability`, `cmd/guard-daemon/observer.go` | Tasks 01, 05, 08 | `.gitignore`, `.gitleaks.toml`, `make secret-scan`, [review 01](reviews/01-foundation.md), typed/redacted observer [review 02](reviews/02-go-architecture.md); config redaction остаётся Tasks 05/08 | PARTIAL |
| GD-023 | Medium | Incident-specific identifiers остаются в source/docs | contract comments и public docs; Go owned paths очищены | Tasks 01, 02, 03, 10 | Repository scan, [review 01](reviews/01-foundation.md), Go cleanup [review 02](reviews/02-go-architecture.md); Solidity/docs остаются Tasks 03/10 | PARTIAL |
| GD-024 | Medium | Unknown token невозможно доказательно считать экономически спасённым | `internal/watcher`, `internal/rescue` unknown token/outcome paths | Tasks 07, 08 | Trust-tier tests и `token-reported` outcome | OPEN |
| GD-025 | Medium | Service/root/log-health/deployment operations небезопасны или неточны | public help/operations baseline | Task 10 | Local hardening/rehearsal tests | OPEN |
| GD-026 | Medium | Публичная документация не полностью русскоязычна и содержит phantom claims | `README.md`, `guard-daemon-HELP_RU.txt` | Task 10 | Language/config/command validation | OPEN |
| GD-027 | Medium | После изменений документации и эксплуатации отсутствует целостное финальное ревью | весь release candidate | Task 11 | Финальный review report и полный CI digest | OPEN |
