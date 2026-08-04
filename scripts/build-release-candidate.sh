#!/usr/bin/env bash
set -euo pipefail

export LC_ALL=C
export TZ=UTC
export GIT_NO_LAZY_FETCH=1
export GIT_NO_REPLACE_OBJECTS=1
export GIT_ATTR_NOSYSTEM=1
export GIT_CONFIG_COUNT=0
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1
export GOPROXY=https://proxy.golang.org,direct
export GOSUMDB=sum.golang.org
unset GIT_ALTERNATE_OBJECT_DIRECTORIES
unset GIT_ATTR_SOURCE
unset GIT_COMMON_DIR
unset GIT_CONFIG_PARAMETERS
unset GIT_DIR
unset GIT_INDEX_FILE
unset GIT_OBJECT_DIRECTORY
unset GIT_NAMESPACE
unset GIT_SHALLOW_FILE
unset GIT_WORK_TREE
unset GONOPROXY
unset GONOSUMDB
unset GOPRIVATE
unset NPM_CONFIG_GLOBALCONFIG
unset NPM_CONFIG_REGISTRY
unset NPM_CONFIG_USERCONFIG
umask 022

readonly EXPECTED_GO_VERSION="go1.26.5"
readonly EXPECTED_NODE_VERSION="v24.18.1"
readonly EXPECTED_NPM_VERSION="11.16.0"
readonly EXPECTED_PYTHON_VERSION="Python 3.14.6"
readonly ENV_BINARY="/usr/bin/env"
readonly NPM_GLOBAL_CONFIG="/var/empty/guard-daemon-npm-globalconfig"
readonly NPM_USER_CONFIG="/var/empty/guard-daemon-npm-userconfig"

fail() {
  printf 'Ошибка сборки кандидата: %s\n' "$1" >&2
  exit 1
}

require_version() {
  local tool="$1"
  local expected="$2"
  local actual="$3"
  [[ "$actual" == "$expected" ]] || fail "$tool должен иметь версию $expected, получено: $actual"
}

script_dir="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(git -C "$script_dir/.." rev-parse --show-toplevel)"
git_dir="$(git -C "$repo_root" rev-parse --absolute-git-dir)" || \
  fail "не удалось определить git-dir"
git_common_dir="$(git -C "$repo_root" rev-parse --path-format=absolute --git-common-dir)" || \
  fail "не удалось определить git common-dir"

require_no_repository_attributes() {
  local directory
  local attributes_path
  for directory in "$git_dir" "$git_common_dir"; do
    attributes_path="$directory/info/attributes"
    if [[ -e "$attributes_path" || -L "$attributes_path" ]]; then
      fail "локальный Git-файл info/attributes запрещён: $attributes_path"
    fi
  done
}

require_no_hidden_index_flags() {
  local index_entries
  local entry
  local tag
  local path
  index_entries="$(git -C "$repo_root" ls-files -v --full-name)" || \
    fail "не удалось проверить флаги Git index"
  while IFS= read -r entry; do
    tag="${entry%% *}"
    path="${entry#? }"
    case "$tag" in
      [a-z])
        fail "для отслеживаемого пути запрещён Git index flag assume-unchanged: $path"
        ;;
      S)
        fail "для отслеживаемого пути запрещён Git index flag skip-worktree: $path"
        ;;
    esac
  done <<<"$index_entries"
}

require_no_npm_config_files() {
  local config_path
  for config_path in "$NPM_GLOBAL_CONFIG" "$NPM_USER_CONFIG"; do
    [[ ! -e "$config_path" && ! -L "$config_path" ]] || \
      fail "изолированный npm config path должен отсутствовать: $config_path"
  done
}

require_no_repository_attributes

release_commit="${RELEASE_COMMIT:-}"
[[ "$release_commit" =~ ^[0-9a-f]{40}$ ]] || fail "RELEASE_COMMIT должен состоять ровно из 40 lowercase hex символов"

resolved_commit="$(git -C "$repo_root" rev-parse --verify "${release_commit}^{commit}" 2>/dev/null)" || \
  fail "RELEASE_COMMIT не является существующим локальным commit"
[[ "$resolved_commit" == "$release_commit" ]] || fail "RELEASE_COMMIT разрешился не в указанный commit"
[[ "$(git -C "$repo_root" cat-file -t "$release_commit")" == "commit" ]] || \
  fail "RELEASE_COMMIT не указывает на commit object"
head_commit="$(git -C "$repo_root" rev-parse --verify 'HEAD^{commit}' 2>/dev/null)" || \
  fail "HEAD не указывает на commit"
[[ "$head_commit" == "$release_commit" ]] || fail "HEAD должен совпадать с RELEASE_COMMIT"
require_no_hidden_index_flags
worktree_status="$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all)" || \
  fail "не удалось проверить чистоту checkout"
[[ -z "$worktree_status" ]] || \
  fail "для сборки требуется clean checkout без tracked и untracked изменений"
release_tree="$(git -C "$repo_root" show -s --format=%T "$release_commit")"
[[ "$release_tree" =~ ^[0-9a-f]{40}$ ]] || fail "release tree имеет неканонический идентификатор"

go_binary="$(command -v go)" || fail "go не найден"
node_binary="$(command -v node)" || fail "node не найден"
npm_binary="$(command -v npm)" || fail "npm не найден"
python_binary="$(command -v python3)" || fail "python3 не найден"
strings_binary="$(command -v strings)" || fail "strings не найден"
command -v tar >/dev/null 2>&1 || fail "tar не найден"
command -v cmp >/dev/null 2>&1 || fail "cmp не найден"

require_no_npm_config_files
[[ -f "$ENV_BINARY" && ! -L "$ENV_BINARY" && -x "$ENV_BINARY" ]] || \
  fail "требуется обычный executable $ENV_BINARY"
go_version_output="$(
  "$ENV_BINARY" -i LC_ALL=C PATH="$PATH" TZ=UTC \
    GOENV=off GOEXPERIMENT='' GOFIPS140=off GOFLAGS='' GOTOOLCHAIN=local \
    "$go_binary" version
)"
read -r _ _ go_version _ <<<"$go_version_output"
require_version "Go" "$EXPECTED_GO_VERSION" "$go_version"
require_version "Node.js" "$EXPECTED_NODE_VERSION" "$(
  "$ENV_BINARY" -i LC_ALL=C PATH="$PATH" TZ=UTC "$node_binary" --version
)"
require_version "npm" "$EXPECTED_NPM_VERSION" "$(
  "$ENV_BINARY" -i LC_ALL=C PATH="$PATH" TZ=UTC \
    npm_config_globalconfig="$NPM_GLOBAL_CONFIG" \
    npm_config_registry=https://registry.npmjs.org/ \
    npm_config_userconfig="$NPM_USER_CONFIG" \
    "$npm_binary" --version
)"
require_version "Python" "$EXPECTED_PYTHON_VERSION" "$(
  "$ENV_BINARY" -i LC_ALL=C PATH="$PATH" TZ=UTC "$python_binary" --version
)"

output_setting="${RELEASE_OUTPUT_ROOT:-dist/release}"
if [[ "$output_setting" == /* ]]; then
  output_root="$output_setting"
else
  output_root="$repo_root/$output_setting"
fi
mkdir -p -- "$output_root"
[[ ! -L "$output_root" && -d "$output_root" ]] || fail "release output root должен быть обычным каталогом"

target="$output_root/$release_commit"
[[ ! -e "$target" && ! -L "$target" ]] || fail "каталог кандидата уже существует: $target"

lock="$output_root/.${release_commit}.lock"
if ! mkdir -- "$lock" 2>/dev/null; then
  fail "другая сборка этого commit уже выполняется или оставила lock"
fi

snapshot=""
candidate_tmp=""
go_module_cache=""
cleanup() {
  cleanup_status=$?
  if [[ -n "$go_module_cache" && -d "$go_module_cache" ]]; then
    chmod -R u+w -- "$go_module_cache" 2>/dev/null || true
    rm -rf -- "$go_module_cache"
  fi
  if [[ -n "$snapshot" && -d "$snapshot" ]]; then
    rm -rf -- "$snapshot"
  fi
  if [[ -n "$candidate_tmp" && -d "$candidate_tmp" ]]; then
    rm -rf -- "$candidate_tmp"
  fi
  rmdir -- "$lock" 2>/dev/null || true
  return "$cleanup_status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

[[ ! -e "$target" && ! -L "$target" ]] || fail "каталог кандидата появился после получения lock"
candidate_tmp="$(mktemp -d "$output_root/.${release_commit}.tmp.XXXXXX")"
snapshot="$(mktemp -d "${TMPDIR:-/tmp}/guard-daemon-release.XXXXXX")"

source_archive="$candidate_tmp/guard-daemon-source.tar"
require_no_repository_attributes
git -C "$repo_root" -c core.attributesFile=/dev/null archive \
  --format=tar \
  --prefix="guard-daemon-${release_commit}/" \
  "$release_commit" >"$source_archive"
require_no_repository_attributes
if ! git -C "$repo_root" -c core.attributesFile=/dev/null archive \
  --format=tar \
  --prefix="guard-daemon-${release_commit}/" \
  "$release_commit" | cmp - "$source_archive"; then
  fail "source archive не совпал с повторным каноническим git archive"
fi
require_no_repository_attributes
tar -xf "$source_archive" -C "$snapshot" --strip-components=1

for source_path in \
  LICENSE \
  artifacts/contracts/RescuerV2.json \
  deployments/schema/rescuer-manifest.schema.json \
  go.mod \
  go.sum \
  package-lock.json \
  scripts/build-release-candidate.sh \
  scripts/release_metadata.py; do
  [[ -f "$snapshot/$source_path" && ! -L "$snapshot/$source_path" ]] || \
    fail "snapshot не содержит обязательный обычный файл: $source_path"
done
cmp "$repo_root/scripts/build-release-candidate.sh" \
  "$snapshot/scripts/build-release-candidate.sh" || \
  fail "выполняемый builder не совпадает с release commit"

npm_cache="$snapshot/.release-cache/npm"
go_build_cache="$snapshot/.release-cache/go-build"
go_module_cache_path=/var/tmp/guard-daemon-release-go-mod-v1
if ! mkdir -m 0700 -- "$go_module_cache_path" 2>/dev/null; then
  fail "изолированный Go module cache занят или оставлен предыдущей сборкой: $go_module_cache_path"
fi
go_module_cache="$go_module_cache_path"
isolated_home="$snapshot/.release-home"
mkdir -p -- "$npm_cache" "$go_build_cache" "$isolated_home"

require_no_npm_config_files
npm_environment=(
  "$ENV_BINARY" -i
  HOME="$isolated_home"
  LC_ALL=C
  PATH="$PATH"
  TZ=UTC
  NODE_OPTIONS=
  npm_config_cache="$npm_cache"
  npm_config_globalconfig="$NPM_GLOBAL_CONFIG"
  npm_config_registry=https://registry.npmjs.org/
  npm_config_userconfig="$NPM_USER_CONFIG"
)

(
  cd "$snapshot"
  "${npm_environment[@]}" "$npm_binary" --no-audit --no-fund --ignore-scripts ci
  "${npm_environment[@]}" "$npm_binary" run artifacts:verify
)

go_modules="$candidate_tmp/.go-modules.json"
go_graph="$candidate_tmp/.go-graph.txt"
go_environment=(
  "$ENV_BINARY" -i
  HOME="$isolated_home"
  LC_ALL=C
  PATH="$PATH"
  TZ=UTC
  GOCACHE="$go_build_cache"
  GOMODCACHE="$go_module_cache"
  GOENV=off
  GOEXPERIMENT=
  GOFIPS140=off
  GOFLAGS=
  "GOPROXY=https://proxy.golang.org,direct"
  GOSUMDB=sum.golang.org
  GOTOOLCHAIN=local
  GOWORK=off
  GIT_ATTR_NOSYSTEM=1
  GIT_CONFIG_COUNT=0
  GIT_CONFIG_GLOBAL=/dev/null
  GIT_CONFIG_NOSYSTEM=1
  GIT_NO_LAZY_FETCH=1
  GIT_NO_REPLACE_OBJECTS=1
  GIT_TERMINAL_PROMPT=0
)
go_root="$("${go_environment[@]}" "$go_binary" env GOROOT)"
[[ "$go_root" == /* && -d "$go_root" && ! -L "$go_root" ]] || \
  fail "GOROOT должен быть обычным абсолютным каталогом"
(
  cd "$snapshot"
  "${go_environment[@]}" "$go_binary" mod download
  "${go_environment[@]}" "$go_binary" mod verify
  "${go_environment[@]}" "$go_binary" list \
    -mod=readonly -m -json all >"$go_modules"
  "${go_environment[@]}" "$go_binary" mod graph >"$go_graph"
  "${go_environment[@]}" \
    GOOS=linux GOARCH=amd64 GOAMD64=v1 CGO_ENABLED=0 \
    "$go_binary" build \
      -trimpath \
      -buildvcs=false \
      -mod=readonly \
      -ldflags="-s -w -buildid= -X guard-daemon/internal/buildinfo.ReleaseCommit=${release_commit}" \
      -o "$candidate_tmp/guard-daemon-linux-amd64" \
      ./cmd/guard-daemon
)
chmod 0755 "$candidate_tmp/guard-daemon-linux-amd64"

binary_strings="$candidate_tmp/.binary-strings"
if ! "$strings_binary" "$candidate_tmp/guard-daemon-linux-amd64" >"$binary_strings"; then
  fail "не удалось проверить строки исполняемого файла"
fi
forbidden_paths=(
  "$repo_root"
  "$snapshot"
  "$output_root"
  "$go_root"
  /nix/store/
)
while IFS= read -r binary_string; do
  for forbidden_path in "${forbidden_paths[@]}"; do
    if [[ "$binary_string" == *"$forbidden_path"* ]]; then
      fail "исполняемый файл содержит локальный build/toolchain path: $forbidden_path"
    fi
  done
done <"$binary_strings"
rm -- "$binary_strings"

cp -- "$snapshot/LICENSE" "$candidate_tmp/LICENSE"
cp -- "$snapshot/artifacts/contracts/RescuerV2.json" "$candidate_tmp/RescuerV2.json"
cp -- "$snapshot/deployments/schema/rescuer-manifest.schema.json" \
  "$candidate_tmp/rescuer-manifest.schema.json"
chmod 0644 \
  "$candidate_tmp/LICENSE" \
  "$candidate_tmp/RescuerV2.json" \
  "$candidate_tmp/rescuer-manifest.schema.json" \
  "$source_archive"

metadata="$snapshot/scripts/release_metadata.py"
run_metadata() {
  "$ENV_BINARY" -i HOME="$isolated_home" LC_ALL=C PATH="$PATH" TZ=UTC \
    "$python_binary" -I -B "$metadata" "$@"
}

directory_identity() {
  "$ENV_BINARY" -i LC_ALL=C PATH="$PATH" TZ=UTC \
    "$python_binary" -I -B -c \
    'import os, stat, sys
value = os.lstat(sys.argv[1])
if not stat.S_ISDIR(value.st_mode):
    raise SystemExit(1)
print(f"{value.st_dev}:{value.st_ino}")' "$1"
}

run_metadata sbom \
  --package-lock "$snapshot/package-lock.json" \
  --go-modules "$go_modules" \
  --go-graph "$go_graph" \
  --release-commit "$release_commit" \
  --output "$candidate_tmp/guard-daemon.cdx.json"
rm -- "$go_modules" "$go_graph"

run_metadata candidate \
  --directory "$candidate_tmp" \
  --release-commit "$release_commit" \
  --release-tree "$release_tree" \
  --output "$candidate_tmp/release-candidate.json"
run_metadata provenance \
  --directory "$candidate_tmp" \
  --builder-script "$repo_root/scripts/build-release-candidate.sh" \
  --release-commit "$release_commit" \
  --release-tree "$release_tree" \
  --output "$candidate_tmp/guard-daemon.intoto.jsonl"
run_metadata checksums \
  --directory "$candidate_tmp" \
  --output "$candidate_tmp/SHA256SUMS"
run_metadata verify --directory "$candidate_tmp"

candidate_identity="$(directory_identity "$candidate_tmp")" || \
  fail "не удалось зафиксировать identity staging-каталога кандидата"
[[ ! -e "$target" && ! -L "$target" ]] || fail "каталог кандидата появился до публикации результата"
if ! mv -- "$candidate_tmp" "$target"; then
  fail "не удалось опубликовать каталог кандидата"
fi
published_identity="$(directory_identity "$target")" || \
  fail "опубликованный путь кандидата не является обычным каталогом"
[[ "$published_identity" == "$candidate_identity" ]] || \
  fail "опубликованный путь не совпадает со staging-каталогом кандидата"
candidate_tmp=""
printf '%s\n' "$target"
