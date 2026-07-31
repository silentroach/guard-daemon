#!/usr/bin/env bash

set -euo pipefail

: "${FOUNDRY_VERSION:?Не задана версия Foundry}"
: "${FOUNDRY_SHA256:?Не задан SHA-256 Foundry}"
: "${GITHUB_PATH:?Скрипт разрешено запускать только в GitHub Actions}"
: "${RUNNER_TEMP:?Не задан временный каталог GitHub Actions}"

archive="${RUNNER_TEMP}/foundry.tar.gz"
install_dir="${RUNNER_TEMP}/foundry"
asset="foundry_${FOUNDRY_VERSION}_linux_amd64.tar.gz"

curl --fail --location --silent --show-error \
  --output "${archive}" \
  "https://github.com/foundry-rs/foundry/releases/download/${FOUNDRY_VERSION}/${asset}"
printf '%s  %s\n' "${FOUNDRY_SHA256}" "${archive}" | sha256sum --check --strict

mkdir -p "${install_dir}"
tar -xzf "${archive}" -C "${install_dir}"
for binary in anvil cast chisel forge; do
  test -x "${install_dir}/${binary}"
done

version_output="$("${install_dir}/forge" --version)"
case "${version_output}" in
  *"Version: ${FOUNDRY_VERSION#v}"*) ;;
  *)
    printf '%s\n' "Получена неожиданная версия Forge: ${version_output}" >&2
    exit 1
    ;;
esac

printf '%s\n' "${install_dir}" >> "${GITHUB_PATH}"
