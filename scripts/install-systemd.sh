#!/usr/bin/env bash

set -euo pipefail

fail() {
  printf 'Installation failed: %s\n' "$1" >&2
  exit 1
}

if (($# != 2)); then
  printf 'Usage: install-systemd.sh <verified-extracted-release> <expected-40hex-commit>\n' >&2
  exit 2
fi
[[ "$(id -u)" == 0 ]] || fail 'run as root'
[[ "$(uname -s):$(uname -m)" == Linux:x86_64 ]] || fail 'Linux/amd64 is required'
[[ "$2" =~ ^[0-9a-f]{40}$ ]] || fail 'expected commit must be 40 lowercase hex characters'

bundle=$(cd -- "$1" && pwd -P)
tooling=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
commit=$2
root=/usr/lib/guard-daemon
release="$root/releases/$commit"
unit=/usr/lib/systemd/system/guard-daemon.service
profile=/etc/systemd/system/guard-daemon.service.d/20-shared-host.conf

for command in flock install systemctl systemd-analyze systemd-sysusers systemd-tmpfiles; do
  command -v "$command" >/dev/null || fail "missing command: $command"
done
exec 9>/run/lock/guard-daemon-install.lock
flock -n 9 || fail 'another installation is running'

for path in "$root/current" "$release" "$unit" \
  /etc/systemd/system/guard-daemon.service /etc/systemd/system/guard-daemon.service.d; do
  [[ ! -e "$path" && ! -L "$path" ]] || fail "first installation only; path already exists: $path"
done

for path in "$bundle/guard-daemon" "$bundle/artifacts/contracts/RescuerV2.json" \
  "$tooling/packaging/systemd/guard-daemon.service" \
  "$tooling/packaging/systemd/guard-daemon.shared-host.conf" \
  "$tooling/packaging/systemd/guard-daemon.sysusers" \
  "$tooling/packaging/systemd/guard-daemon.tmpfiles" \
  "$tooling/packaging/systemd/guard-daemon.state.env" \
  "$tooling/scripts/check-systemd-health.sh" "$tooling/scripts/verify-systemd-account.sh"; do
  [[ -f "$path" && ! -L "$path" ]] || fail "missing regular file: $path"
done
[[ -x "$bundle/guard-daemon" ]] || fail 'binary is not executable'
[[ "$("$bundle/guard-daemon" --version)" == "$commit" ]] || fail 'binary commit does not match'

install -d -o root -g root -m 0755 "$root/releases"
staging=$(mktemp -d "$root/releases/.install.XXXXXX")
# shellcheck disable=SC2329
cleanup() {
  if [[ -n "$staging" ]]; then rm -rf -- "$staging"; fi
}
trap cleanup EXIT
chmod 0755 "$staging"
install -d -o root -g root -m 0755 "$staging/artifacts/contracts"
install -o root -g root -m 0755 "$bundle/guard-daemon" "$staging/guard-daemon"
install -o root -g root -m 0644 "$bundle/artifacts/contracts/RescuerV2.json" "$staging/artifacts/contracts/RescuerV2.json"
mv -Tn -- "$staging" "$release"
[[ ! -e "$staging" ]] || fail 'release path appeared during installation'
staging=''

install -D -o root -g root -m 0644 "$tooling/packaging/systemd/guard-daemon.sysusers" /usr/lib/sysusers.d/guard-daemon.conf
install -D -o root -g root -m 0644 "$tooling/packaging/systemd/guard-daemon.tmpfiles" /usr/lib/tmpfiles.d/guard-daemon.conf
install -o root -g root -m 0644 "$tooling/scripts/verify-systemd-account.sh" "$root/verify-systemd-account.sh"
install -o root -g root -m 0644 "$tooling/scripts/check-systemd-health.sh" "$root/check-systemd-health.sh"
systemd-sysusers /usr/lib/sysusers.d/guard-daemon.conf
/usr/bin/env -i PATH=/usr/bin:/bin /bin/bash --noprofile --norc "$root/verify-systemd-account.sh"
systemd-tmpfiles --create /usr/lib/tmpfiles.d/guard-daemon.conf
install -o root -g root -m 0644 "$tooling/packaging/systemd/guard-daemon.state.env" "$root/guard-daemon.state.env"
ln -s "releases/$commit" "$root/current"
install -D -o root -g root -m 0644 "$tooling/packaging/systemd/guard-daemon.service" "$unit"
install -D -o root -g root -m 0644 "$tooling/packaging/systemd/guard-daemon.shared-host.conf" "$profile"
systemctl daemon-reload
systemd-analyze verify "$unit"
printf 'Installed %s. Configure /etc/guard-daemon before starting the service.\n' "$commit"
