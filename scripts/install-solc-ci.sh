#!/usr/bin/env bash

set -euo pipefail

: "${GITHUB_PATH:?This script may only run in GitHub Actions}"
: "${GITHUB_ENV:?GitHub Actions environment file is not set}"
: "${RUNNER_TEMP:?GitHub Actions temporary directory is not set}"
: "${SOLC_COMMIT:?solc commit is not set}"
: "${SOLC_SHA256:?solc SHA-256 is not set}"
: "${SOLC_VERSION:?solc version is not set}"

install_dir="${RUNNER_TEMP}/solc"
binary="${install_dir}/solc"
asset="solc-linux-amd64-v${SOLC_VERSION}+commit.${SOLC_COMMIT}"

mkdir -p "${install_dir}"
curl --fail --location --silent --show-error \
  --output "${binary}" \
  "https://binaries.soliditylang.org/linux-amd64/${asset}"
printf '%s  %s\n' "${SOLC_SHA256}" "${binary}" | sha256sum --check --strict
chmod 755 "${binary}"

version_output="$("${binary}" --version)"
case "${version_output}" in
  *"Version: ${SOLC_VERSION}+commit.${SOLC_COMMIT}"*) ;;
  *)
    printf '%s\n' "Unexpected solc version: ${version_output}" >&2
    exit 1
    ;;
esac

printf '%s\n' "${install_dir}" >> "${GITHUB_PATH}"
printf 'FOUNDRY_SOLC=%s\n' "${binary}" >> "${GITHUB_ENV}"
