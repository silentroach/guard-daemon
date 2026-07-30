.PHONY: audit build check contracts-build contracts-test format format-check go-ci lint mod-verify node-ci race secret-scan test typecheck vet vuln workflow-lint

format:
	gofmt -w $$(git ls-files '*.go')
	npm run format

format-check:
	test -z "$$(gofmt -l $$(git ls-files '*.go'))"
	npm run format:check
	$(MAKE) workflow-lint

workflow-lint:
	actionlint
	shellcheck scripts/go-packages.sh

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

audit:
	npm run audit

secret-scan:
	gitleaks dir --no-banner --redact --config=.gitleaks.toml .
	gitleaks git --no-banner --redact --config=.gitleaks.toml

go-ci: mod-verify build test vet race

node-ci: typecheck lint contracts-build contracts-test

check: format-check go-ci node-ci vuln audit secret-scan
