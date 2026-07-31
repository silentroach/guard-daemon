.PHONY: audit build check contracts-build contracts-lint contracts-static contracts-test format format-check go-ci lint mod-verify node-ci race secret-scan test typecheck vet vuln workflow-lint

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
	shellcheck scripts/*.sh

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
	forge test

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

node-ci: typecheck lint contracts-build contracts-test contracts-lint

check: format-check go-ci node-ci vuln audit secret-scan
