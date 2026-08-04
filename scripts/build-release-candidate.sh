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
export npm_config_globalconfig=/dev/null
export npm_config_registry=https://registry.npmjs.org/
export npm_config_userconfig=/dev/null
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

command -v go >/dev/null 2>&1 || fail "go не найден"
command -v node >/dev/null 2>&1 || fail "node не найден"
command -v npm >/dev/null 2>&1 || fail "npm не найден"
command -v python3 >/dev/null 2>&1 || fail "python3 не найден"
command -v tar >/dev/null 2>&1 || fail "tar не найден"
command -v cmp >/dev/null 2>&1 || fail "cmp не найден"

go_version_output="$(go version)"
read -r _ go_version _ <<<"$go_version_output"
require_version "Go" "$EXPECTED_GO_VERSION" "$go_version"
require_version "Node.js" "$EXPECTED_NODE_VERSION" "$(node --version)"
require_version "npm" "$EXPECTED_NPM_VERSION" "$(npm --version)"
require_version "Python" "$EXPECTED_PYTHON_VERSION" "$(python3 --version)"

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
cleanup() {
  cleanup_status=$?
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
go_module_cache="$snapshot/.release-cache/go-mod"
mkdir -p -- "$npm_cache" "$go_build_cache" "$go_module_cache"

(
  cd "$snapshot"
  NODE_OPTIONS='' npm_config_cache="$npm_cache" \
    npm --no-audit --no-fund --ignore-scripts ci
  NODE_OPTIONS='' npm_config_cache="$npm_cache" npm run artifacts:verify
)

go_modules="$candidate_tmp/.go-modules.json"
go_graph="$candidate_tmp/.go-graph.txt"
(
  cd "$snapshot"
  GOCACHE="$go_build_cache" GOMODCACHE="$go_module_cache" \
    GOENV=off GOFLAGS='' GOTOOLCHAIN=local go mod download
  GOCACHE="$go_build_cache" GOMODCACHE="$go_module_cache" \
    GOENV=off GOFLAGS='' GOTOOLCHAIN=local go mod verify
  GOCACHE="$go_build_cache" GOMODCACHE="$go_module_cache" \
    GOENV=off GOFLAGS='' GOTOOLCHAIN=local \
    go list -mod=readonly -m -json all >"$go_modules"
  GOCACHE="$go_build_cache" GOMODCACHE="$go_module_cache" \
    GOENV=off GOFLAGS='' GOTOOLCHAIN=local go mod graph >"$go_graph"
  GOCACHE="$go_build_cache" GOMODCACHE="$go_module_cache" \
    GOENV=off GOEXPERIMENT='' GOFLAGS='' GOTOOLCHAIN=local \
    GOOS=linux GOARCH=amd64 GOAMD64=v1 CGO_ENABLED=0 \
    go build \
      -trimpath \
      -buildvcs=false \
      -mod=readonly \
      -ldflags="-buildid= -X guard-daemon/internal/buildinfo.ReleaseCommit=${release_commit}" \
      -o "$candidate_tmp/guard-daemon-linux-amd64" \
      ./cmd/guard-daemon
)
chmod 0755 "$candidate_tmp/guard-daemon-linux-amd64"

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
  env \
    -u PYTHONBREAKPOINT \
    -u PYTHONHASHSEED \
    -u PYTHONHOME \
    -u PYTHONINSPECT \
    -u PYTHONPATH \
    -u PYTHONPYCACHEPREFIX \
    -u PYTHONSAFEPATH \
    -u PYTHONSTARTUP \
    -u PYTHONUSERBASE \
    -u PYTHONWARNINGS \
    python3 -I -B "$metadata" "$@"
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

[[ ! -e "$target" && ! -L "$target" ]] || fail "каталог кандидата появился до публикации результата"
mv -- "$candidate_tmp" "$target"
candidate_tmp=""
printf '%s\n' "$target"
