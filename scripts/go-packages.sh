#!/usr/bin/env bash

set -euo pipefail

root=$(pwd -P)
directories=$(go list -mod=readonly -f '{{.Dir}}' ./...)

while IFS= read -r directory; do
  case "$directory" in
    "$root")
      printf '.\n'
      ;;
    "$root"/node_modules/*)
      ;;
    "$root"/*)
      printf './%s\n' "${directory#"$root"/}"
      ;;
  esac
done <<<"$directories"
