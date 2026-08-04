.PHONY: adversarial-test artifacts-verify audit build check ci-policy contracts-build contracts-lint contracts-static contracts-test deployment-check documentation-check format format-check fuzz-test go-ci lint mod-verify node-ci operations-check operations-rehearsal race release-candidate reproducibility-check secret-scan security-validation test typecheck vet vuln workflow-lint

format:
	gofmt -w $$(git ls-files '*.go')
	npm run format
	forge fmt

format-check:
	test -z "$$(gofmt -l $$(git ls-files '*.go'))"
	npm run format:check
	forge fmt --check
	$(MAKE) workflow-lint

workflow-lint:
	actionlint
	shellcheck scripts/*.sh test/ci/*.sh

ci-policy:
	python3 -B -m unittest discover -s test/ci -p 'test_*.py'
	bash test/ci/repository-policy.sh

documentation-check:
	python3 -B -m unittest discover -s test/ci -p 'test_documentation.py'
	go test -mod=readonly ./internal/config

operations-check:
	python3 -B -m unittest discover -s test/ci -p 'test_operations.py'
	python3 -B -m unittest discover -s test/ci -p 'test_release_metadata.py'
	bash -n scripts/build-release-candidate.sh

operations-rehearsal:
	go test -mod=readonly ./cmd/guard-daemon ./internal/config ./test/integration/policy \
		-run 'Test(CLI|Diagnostics|DryRun|EmergencyStop|DaemonGracefulShutdown)'
	npm run deploy:test

adversarial-test:
	bash test/ci/adversarial-go.sh

fuzz-test:
	bash test/ci/go-fuzz.sh

reproducibility-check:
	bash test/ci/compare-artifacts.sh

release-candidate:
	RELEASE_COMMIT="$(RELEASE_COMMIT)" bash scripts/build-release-candidate.sh

mod-verify:
	go mod verify

build:
	@packages="$$(bash scripts/go-packages.sh)" || exit 1; \
		test -n "$$packages" || { printf '%s\n' 'Список Go packages пуст' >&2; exit 1; }; \
		mkdir -p build/bin; \
		CGO_ENABLED=0 go build -o build/bin/ -mod=readonly $$packages

test:
	@packages="$$(bash scripts/go-packages.sh)" || exit 1; \
		test -n "$$packages" || { printf '%s\n' 'Список Go packages пуст' >&2; exit 1; }; \
		go test -mod=readonly $$packages

race:
	@packages="$$(bash scripts/go-packages.sh)" || exit 1; \
		test -n "$$packages" || { printf '%s\n' 'Список Go packages пуст' >&2; exit 1; }; \
		CGO_ENABLED=1 go test -race -mod=readonly $$packages

vet:
	@packages="$$(bash scripts/go-packages.sh)" || exit 1; \
		test -n "$$packages" || { printf '%s\n' 'Список Go packages пуст' >&2; exit 1; }; \
		go vet -mod=readonly $$packages

vuln:
	@packages="$$(bash scripts/go-packages.sh)" || exit 1; \
		test -n "$$packages" || { printf '%s\n' 'Список Go packages пуст' >&2; exit 1; }; \
		govulncheck $$packages

typecheck:
	npm run typecheck

lint:
	npm run lint

contracts-build:
	npm run contracts:build

contracts-test:
	npm run contracts:test
	bash test/ci/contracts-test.sh

artifacts-verify:
	npm run artifacts:verify

deployment-check: artifacts-verify
	npm run deploy:test

contracts-lint:
	forge lint --deny warnings

contracts-static: contracts-lint
	slither contracts/RescuerV2.sol --compile-force-framework solc --solc-args "--evm-version prague --optimize --optimize-runs 200" --fail-high

audit:
	npm run audit

secret-scan:
	gitleaks dir --no-banner --redact --config=.gitleaks.toml .
	gitleaks git --no-banner --redact --config=.gitleaks.toml

go-ci: mod-verify build test vet race

node-ci: typecheck lint contracts-build contracts-test contracts-lint deployment-check

check: format-check go-ci node-ci vuln audit secret-scan

security-validation: check contracts-static adversarial-test fuzz-test ci-policy documentation-check operations-check operations-rehearsal reproducibility-check
