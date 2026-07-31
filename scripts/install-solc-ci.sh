#!/usr/bin/env bash

set -euo pipefail

: "${GITHUB_PATH:?Скрипт разрешено запускать только в GitHub Actions}"
: "${GITHUB_ENV:?Не задан файл окружения GitHub Actions}"
: "${RUNNER_TEMP:?Не задан временный каталог GitHub Actions}"
: "${SOLC_COMMIT:?Не задан commit solc}"
: "${SOLC_SHA256:?Не задан SHA-256 solc}"
: "${SOLC_VERSION:?Не задана версия solc}"

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
    printf '%s\n' "Получена неожиданная версия solc: ${version_output}" >&2
    exit 1
    ;;
esac

printf '%s\n' "${install_dir}" >> "${GITHUB_PATH}"
printf 'FOUNDRY_SOLC=%s\n' "${binary}" >> "${GITHUB_ENV}"
