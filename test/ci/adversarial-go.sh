#!/usr/bin/env bash

set -euo pipefail

packages=(
  ./cmd/guard-daemon
  ./internal/budget
  ./internal/config
  ./internal/contracts
  ./internal/domain
  ./internal/observability
  ./internal/rescue
  ./internal/rescue/dryrun
  ./internal/rpc
  ./internal/store
  ./internal/watcher
  ./test/integration/policy
  ./test/integration/watcher
)

go test -count=1 -mod=readonly "${packages[@]}"
