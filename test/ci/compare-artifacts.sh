#!/usr/bin/env bash

set -euo pipefail

root=$(git rev-parse --show-toplevel)
temporary_directory=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/guard-reproducibility.XXXXXX")
trap 'rm -rf "${temporary_directory}"' EXIT

if [[ -n "${GITHUB_SHA:-}" ]]; then
  if [[ ! "${GITHUB_SHA}" =~ ^[0-9a-f]{40}$ ]]; then
    printf 'GITHUB_SHA должен быть полным 40-символьным SHA commit.\n' >&2
    exit 1
  fi
  source_ref=${GITHUB_SHA}
else
  source_ref=HEAD
fi
if [[ "${source_ref}" == -* ]]; then
  printf 'Ссылка снимка не может начинаться с дефиса.\n' >&2
  exit 1
fi
if ! commit=$(git -C "${root}" rev-parse --verify "${source_ref}^{commit}" 2>/dev/null); then
  printf 'Ссылка снимка не разрешается в commit: %s\n' "${source_ref}" >&2
  exit 1
fi
tree=$(git -C "${root}" rev-parse --verify "${commit}^{tree}")
printf 'Commit снимка: %s\nХеш дерева исходников: %s\n' "${commit}" "${tree}"

copy_commit_snapshot() {
  local destination=$1

  mkdir -p "${destination}"
  git -C "${root}" archive --format=tar "${commit}" | tar -xf - -C "${destination}"
}

build_snapshot() {
  local directory=$1

  (
    cd "${directory}"
    npm ci
    npm run contracts:build
    npm run artifacts:verify
    mkdir -p build/bin
    CGO_ENABLED=0 GOCACHE="${directory}/.go-build-cache" \
      go build -trimpath -buildvcs=false -mod=readonly \
      -o build/bin/guard-daemon ./cmd/guard-daemon
  )
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  else
    shasum -a 256 "$1" | cut -d ' ' -f 1
  fi
}

first="${temporary_directory}/first"
second="${temporary_directory}/second"
copy_commit_snapshot "${first}"
copy_commit_snapshot "${second}"
build_snapshot "${first}"
build_snapshot "${second}"

shopt -s nullglob
contract_artifacts=("${first}"/build/contracts/*.json)
shopt -u nullglob
if ((${#contract_artifacts[@]} != 1)) ||
  [[ "${contract_artifacts[0]:-}" != "${first}/build/contracts/RescuerV2.json" ]]; then
  printf 'Чистая сборка создала неожиданный набор артефактов контрактов.\n' >&2
  exit 1
fi

while IFS= read -r binary_string; do
  if [[ "${binary_string}" =~ (PermitSweeper|permitAndTransfer|permitAndSweep) ]]; then
    printf 'Удалённый путь permit найден в рабочем исполняемом файле.\n' >&2
    exit 1
  fi
done < <(strings "${first}/build/bin/guard-daemon")

artifacts=(
  artifacts/contracts/RescuerV2.json
  build/contracts/RescuerV2.json
  build/bin/guard-daemon
)

for artifact in "${artifacts[@]}"; do
  cmp "${first}/${artifact}" "${second}/${artifact}"
  printf '%s  %s\n' "$(sha256_file "${first}/${artifact}")" "${artifact}"
done
