#!/usr/bin/env bash

set -euo pipefail

root=$(git rev-parse --show-toplevel)
temporary_directory=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/guard-reproducibility.XXXXXX")
attributes_checkout=""
index_flags_checkout=""
flagged_path=""
cleanup() {
  if [[ -n "${index_flags_checkout}" ]]; then
    if [[ -n "${flagged_path}" ]]; then
      git -C "${index_flags_checkout}" update-index --no-assume-unchanged \
        -- "${flagged_path}" >/dev/null 2>&1 || true
      git -C "${index_flags_checkout}" update-index --no-skip-worktree \
        -- "${flagged_path}" >/dev/null 2>&1 || true
    fi
    git -C "${root}" worktree remove --force "${index_flags_checkout}" \
      >/dev/null 2>&1 || true
  fi
  if [[ -n "${attributes_checkout}" ]]; then
    git -C "${root}" worktree remove --force "${attributes_checkout}" >/dev/null 2>&1 || true
  fi
  rm -rf "${temporary_directory}"
}
trap cleanup EXIT

fail() {
  printf 'Ошибка проверки воспроизводимости: %s\n' "$1" >&2
  exit 1
}

check_rejected_index_flag() {
  local set_option=$1
  local unset_option=$2
  local flag_name=$3
  local error

  git -C "${index_flags_checkout}" update-index "${set_option}" -- "${flagged_path}"
  if RELEASE_COMMIT="${commit}" \
    RELEASE_OUTPUT_ROOT="${temporary_directory}/${flag_name}-output" \
    bash "${index_flags_checkout}/scripts/build-release-candidate.sh" \
    >"${temporary_directory}/${flag_name}.stdout" \
    2>"${temporary_directory}/${flag_name}.stderr"; then
    fail "builder принял Git index flag ${flag_name}"
  fi
  error=$(<"${temporary_directory}/${flag_name}.stderr")
  [[ "${error}" == *"${flag_name}"* ]] || \
    fail "builder завершился не из-за Git index flag ${flag_name}"
  git -C "${index_flags_checkout}" update-index "${unset_option}" -- "${flagged_path}"
}

if [[ -n "${GITHUB_SHA:-}" ]]; then
  commit=${GITHUB_SHA}
else
  commit=$(git -C "${root}" rev-parse --verify HEAD)
fi
if [[ ! "${commit}" =~ ^[0-9a-f]{40}$ ]] ||
  [[ "$(git -C "${root}" rev-parse --verify "${commit}^{commit}")" != "${commit}" ]]; then
  printf 'Снимок воспроизводимости должен быть полным существующим SHA commit.\n' >&2
  exit 1
fi

attributes_checkout="${temporary_directory}/attributes-checkout"
git -C "${root}" worktree add --detach "${attributes_checkout}" "${commit}" >/dev/null
attributes_git_dir=$(git -C "${attributes_checkout}" rev-parse --absolute-git-dir)
mkdir -p "${attributes_git_dir}/info"
attributes_path="${attributes_git_dir}/info/attributes"
printf '* export-ignore\n' >"${attributes_path}"
if RELEASE_COMMIT="${commit}" RELEASE_OUTPUT_ROOT="${temporary_directory}/attributes-output" \
  bash "${attributes_checkout}/scripts/build-release-candidate.sh" \
  >"${temporary_directory}/attributes-file.stdout" \
  2>"${temporary_directory}/attributes-file.stderr"; then
  fail "builder принял локальный Git-файл info/attributes"
fi
attributes_error=$(<"${temporary_directory}/attributes-file.stderr")
[[ "${attributes_error}" == *"info/attributes"* ]] || \
  fail "builder завершился не из-за локального Git-файла attributes"
rm "${attributes_path}"
ln -s /dev/null "${attributes_path}"
if RELEASE_COMMIT="${commit}" RELEASE_OUTPUT_ROOT="${temporary_directory}/attributes-output" \
  bash "${attributes_checkout}/scripts/build-release-candidate.sh" \
  >"${temporary_directory}/attributes-symlink.stdout" \
  2>"${temporary_directory}/attributes-symlink.stderr"; then
  fail "builder принял символическую ссылку info/attributes"
fi
attributes_error=$(<"${temporary_directory}/attributes-symlink.stderr")
[[ "${attributes_error}" == *"info/attributes"* ]] || \
  fail "builder завершился не из-за символической ссылки attributes"
rm "${attributes_path}"
git -C "${root}" worktree remove --force "${attributes_checkout}" >/dev/null
attributes_checkout=""

index_flags_checkout="${temporary_directory}/index-flags-checkout"
git -C "${root}" worktree add --detach "${index_flags_checkout}" "${commit}" >/dev/null
flagged_path="scripts/release_metadata.py"
check_rejected_index_flag \
  --assume-unchanged --no-assume-unchanged assume-unchanged
check_rejected_index_flag \
  --skip-worktree --no-skip-worktree skip-worktree
git -C "${root}" worktree remove --force "${index_flags_checkout}" >/dev/null
index_flags_checkout=""
flagged_path=""

first_root="${temporary_directory}/first"
second_root="${temporary_directory}/second"
mkdir -p "${first_root}" "${second_root}"

# Invoked indirectly after export by the builder child.
# shellcheck disable=SC2329
env() {
  if [[ "${1:-}" == "-i" ]]; then
    shift
  fi
  /usr/bin/env "$@"
}
export -f env
# Invoked indirectly after export by the builder child.
# shellcheck disable=SC2329
strings() {
  return 0
}
export -f strings
GOCACHEPROG=/usr/bin/false \
GOFIPS140=latest \
GOWORK=/dev/null \
NODE_OPTIONS=--require=/guard-daemon-forbidden-node-hook \
NPM_CONFIG_SCRIPT_SHELL=/usr/bin/false \
PYTHONHOME=/guard-daemon-forbidden-python-home \
RELEASE_COMMIT="${commit}" RELEASE_OUTPUT_ROOT="${first_root}" \
  bash "${root}/scripts/build-release-candidate.sh"
unset -f env
unset -f strings
GOFIPS140=off \
npm_config_script_shell=/usr/bin/false \
RELEASE_COMMIT="${commit}" RELEASE_OUTPUT_ROOT="${second_root}" \
  bash "${root}/scripts/build-release-candidate.sh"

first="${first_root}/${commit}"
second="${second_root}/${commit}"
artifacts=(
  LICENSE
  RescuerV2.json
  SHA256SUMS
  guard-daemon-linux-amd64
  guard-daemon-source.tar
  guard-daemon.cdx.json
  guard-daemon.intoto.jsonl
  release-candidate.json
  rescuer-manifest.schema.json
)

for artifact in "${artifacts[@]}"; do
  cmp "${first}/${artifact}" "${second}/${artifact}"
done

python3 -I -B "${root}/scripts/release_metadata.py" verify --directory "${first}"
python3 -I -B "${root}/scripts/release_metadata.py" verify --directory "${second}"
gitleaks dir --no-banner --redact --max-archive-depth=1 \
  --config="${root}/.gitleaks.toml" "${first}"
gitleaks dir --no-banner --redact --max-archive-depth=1 \
  --config="${root}/.gitleaks.toml" "${second}"

binary_strings="${temporary_directory}/binary.strings"
if ! strings "${first}/guard-daemon-linux-amd64" >"${binary_strings}"; then
  fail "не удалось проверить строки исполняемого файла"
fi
while IFS= read -r binary_string; do
  if [[ "${binary_string}" =~ (PermitSweeper|permitAndTransfer|permitAndSweep) ]]; then
    printf 'Удалённый путь permit найден в исполняемом файле кандидата.\n' >&2
    exit 1
  fi
done <"${binary_strings}"

printf 'Commit снимка: %s\n' "${commit}"
while IFS= read -r checksum; do
  printf '%s\n' "${checksum}"
done <"${first}/SHA256SUMS"
