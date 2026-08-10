#!/usr/bin/env bash

if [[ -n "${BASH_ENV:-}" || -n "${ENV:-}" || -n "$(builtin declare -F)" ]]; then
  builtin printf 'Release candidate build error: the shell must start in a clean environment.\n' >&2
  builtin exit 1
fi

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
readonly STRINGS_BINARY="/usr/bin/strings"
readonly NPM_GLOBAL_CONFIG="/var/empty/guard-daemon-npm-globalconfig"
readonly NPM_USER_CONFIG="/var/empty/guard-daemon-npm-userconfig"

fail() {
  printf 'Release candidate build error: %s\n' "$1" >&2
  exit 1
}

require_version() {
  local tool="$1"
  local expected="$2"
  local actual="$3"
  [[ "$actual" == "$expected" ]] || fail "$tool must be version $expected, received: $actual"
}

script_dir="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(git -C "$script_dir/.." rev-parse --show-toplevel)"
git_dir="$(git -C "$repo_root" rev-parse --absolute-git-dir)" || \
  fail "failed to determine git-dir"
git_common_dir="$(git -C "$repo_root" rev-parse --path-format=absolute --git-common-dir)" || \
  fail "failed to determine Git common directory"

require_no_repository_attributes() {
  local directory
  local attributes_path
  for directory in "$git_dir" "$git_common_dir"; do
    attributes_path="$directory/info/attributes"
    if [[ -e "$attributes_path" || -L "$attributes_path" ]]; then
      fail "local Git info/attributes file is forbidden: $attributes_path"
    fi
  done
}

require_no_hidden_index_flags() {
  local index_entries
  local entry
  local tag
  local path
  index_entries="$(git -C "$repo_root" ls-files -v --full-name)" || \
    fail "failed to check Git index flags"
  while IFS= read -r entry; do
    tag="${entry%% *}"
    path="${entry#? }"
    case "$tag" in
      [a-z])
        fail "Git index flag assume-unchanged is forbidden for tracked path: $path"
        ;;
      S)
        fail "Git index flag skip-worktree is forbidden for tracked path: $path"
        ;;
    esac
  done <<<"$index_entries"
}

require_no_npm_config_files() {
  local config_path
  for config_path in "$NPM_GLOBAL_CONFIG" "$NPM_USER_CONFIG"; do
    [[ ! -e "$config_path" && ! -L "$config_path" ]] || \
      fail "isolated npm config path must not exist: $config_path"
  done
}

contains_directory_path() {
  local value="$1"
  local directory="$2"
  [[ "$value" == *"$directory" || "$value" == *"$directory/"* ]]
}

require_no_repository_attributes

release_commit="${RELEASE_COMMIT:-}"
[[ "$release_commit" =~ ^[0-9a-f]{40}$ ]] || fail "RELEASE_COMMIT must contain exactly 40 lowercase hexadecimal characters"

resolved_commit="$(git -C "$repo_root" rev-parse --verify "${release_commit}^{commit}" 2>/dev/null)" || \
  fail "RELEASE_COMMIT is not an existing local commit"
[[ "$resolved_commit" == "$release_commit" ]] || fail "RELEASE_COMMIT did not resolve to the specified commit"
[[ "$(git -C "$repo_root" cat-file -t "$release_commit")" == "commit" ]] || \
  fail "RELEASE_COMMIT does not refer to a commit object"
head_commit="$(git -C "$repo_root" rev-parse --verify 'HEAD^{commit}' 2>/dev/null)" || \
  fail "HEAD does not refer to a commit"
[[ "$head_commit" == "$release_commit" ]] || fail "HEAD must match RELEASE_COMMIT"
require_no_hidden_index_flags
worktree_status="$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all)" || \
  fail "failed to check whether the checkout is clean"
[[ -z "$worktree_status" ]] || \
  fail "the build requires a clean checkout without tracked or untracked changes"
release_tree="$(git -C "$repo_root" show -s --format=%T "$release_commit")"
[[ "$release_tree" =~ ^[0-9a-f]{40}$ ]] || fail "release tree has a noncanonical identifier"

go_binary="$(command -v go)" || fail "go not found"
node_binary="$(command -v node)" || fail "node not found"
npm_binary="$(command -v npm)" || fail "npm not found"
python_binary="$(command -v python3)" || fail "python3 not found"
command -v tar >/dev/null 2>&1 || fail "tar not found"
command -v cmp >/dev/null 2>&1 || fail "cmp not found"

require_no_npm_config_files
[[ -f "$ENV_BINARY" && ! -L "$ENV_BINARY" && -x "$ENV_BINARY" ]] || \
  fail "regular executable $ENV_BINARY is required"
[[ -f "$STRINGS_BINARY" && -x "$STRINGS_BINARY" ]] || \
  fail "executable $STRINGS_BINARY is required"
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
[[ ! -L "$output_root" && -d "$output_root" ]] || fail "release output root must be a regular directory"

target="$output_root/$release_commit"
[[ ! -e "$target" && ! -L "$target" ]] || fail "candidate directory already exists: $target"

lock="$output_root/.${release_commit}.lock"
if ! mkdir -- "$lock" 2>/dev/null; then
  fail "another build of this commit is running or left a lock"
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

[[ ! -e "$target" && ! -L "$target" ]] || fail "candidate directory appeared after the lock was acquired"
candidate_tmp="$(mktemp -d "$output_root/.${release_commit}.tmp.XXXXXX")"
snapshot="$(mktemp -d "${TMPDIR:-/tmp}/guard-daemon-release.XXXXXX")"

source_archive="$candidate_tmp/guard-daemon-source.tar"
require_no_repository_attributes
git -C "$repo_root" -c core.attributesFile=/dev/null -c tar.umask=0002 archive \
  --format=tar \
  --prefix="guard-daemon-${release_commit}/" \
  "$release_commit" >"$source_archive"
require_no_repository_attributes
if ! git -C "$repo_root" -c core.attributesFile=/dev/null -c tar.umask=0002 archive \
  --format=tar \
  --prefix="guard-daemon-${release_commit}/" \
  "$release_commit" | cmp - "$source_archive"; then
  fail "source archive did not match a repeated canonical git archive"
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
    fail "snapshot does not contain the required regular file: $source_path"
done
cmp "$repo_root/scripts/build-release-candidate.sh" \
  "$snapshot/scripts/build-release-candidate.sh" || \
  fail "executed builder does not match the release commit"

npm_cache="$snapshot/.release-cache/npm"
go_build_cache="$snapshot/.release-cache/go-build"
go_module_cache_path=/var/tmp/guard-daemon-release-go-mod-v1
if ! mkdir -m 0700 -- "$go_module_cache_path" 2>/dev/null; then
  fail "isolated Go module cache is in use or was left by a previous build: $go_module_cache_path"
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
  fail "GOROOT must be a regular absolute directory"
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
if ! "$STRINGS_BINARY" "$candidate_tmp/guard-daemon-linux-amd64" >"$binary_strings"; then
  fail "failed to inspect executable strings"
fi
forbidden_directories=(
  "${repo_root%/}"
  "${snapshot%/}"
  "${output_root%/}"
  "${go_root%/}"
)
while IFS= read -r binary_string; do
  for forbidden_directory in "${forbidden_directories[@]}"; do
    [[ "$forbidden_directory" =~ ^/[^/]+/.+ ]] || \
      fail "build/toolchain directory path is too short: $forbidden_directory"
    if contains_directory_path "$binary_string" "$forbidden_directory"; then
      fail "executable contains a local build/toolchain path: $forbidden_directory"
    fi
  done
  [[ "$binary_string" != *"/nix/store/"* ]] || \
    fail "executable contains a local build/toolchain path: /nix/store/"
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
  fail "failed to record the candidate staging directory identity"
[[ ! -e "$target" && ! -L "$target" ]] || fail "candidate directory appeared before publication"
if ! mv -- "$candidate_tmp" "$target"; then
  fail "failed to publish the candidate directory"
fi
published_identity="$(directory_identity "$target")" || \
  fail "published candidate path is not a regular directory"
[[ "$published_identity" == "$candidate_identity" ]] || \
  fail "published path does not match the candidate staging directory"
candidate_tmp=""
printf '%s\n' "$target"
