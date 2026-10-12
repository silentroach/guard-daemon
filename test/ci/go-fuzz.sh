#!/usr/bin/env bash

set -euo pipefail

readonly fuzz_time=20s
readonly fuzz_timeout=90s
readonly fuzz_parallelism=2
readonly targets=(
  "./internal/watcher:FuzzMalformedLog"
  "./internal/watcher:FuzzMetadataReturnData"
)

export GOMAXPROCS="${fuzz_parallelism}"

for specification in "${targets[@]}"; do
  package=${specification%%:*}
  target=${specification#*:}
  printf 'Bounded fuzzing of %s in %s\n' "${target}" "${package}"
  go test -mod=readonly "${package}" \
    -run '^$' \
    -fuzz "^${target}$" \
    -fuzztime "${fuzz_time}" \
    -parallel "${fuzz_parallelism}" \
    -timeout "${fuzz_timeout}"
done
