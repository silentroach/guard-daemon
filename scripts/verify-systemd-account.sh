#!/usr/bin/env bash

set -euo pipefail

fail() {
  printf 'Invalid guard-daemon system account: %s\n' "$1" >&2
  exit 1
}

read_identity_database() {
  local database=$1

  identity_entries="$(getent "${database}")" || fail "failed to read the complete ${database} database"
  [[ -n "${identity_entries}" ]] || fail "${database} database is empty"
}

validate_passwd_entries() {
  local entries=$1
  local expected_uid=$2
  local expected_gid=$3
  local guard_entries=0
  local candidate_name candidate_uid candidate_gid

  while IFS=: read -r candidate_name _ candidate_uid candidate_gid _; do
    [[ -n "${candidate_name}" && "${candidate_uid}" =~ ^[0-9]+$ && "${candidate_gid}" =~ ^[0-9]+$ ]] || \
      fail "passwd database contains an invalid entry"
    if [[ "${candidate_name}" == "guard-daemon" ]]; then
      guard_entries=$((guard_entries + 1))
      [[ "${candidate_uid}" == "${expected_uid}" && "${candidate_gid}" == "${expected_gid}" ]] || \
        fail "duplicate passwd entry does not match"
      continue
    fi
    [[ "${candidate_uid}" != "${expected_uid}" ]] || fail "UID is used by another account"
    [[ "${candidate_gid}" != "${expected_gid}" ]] || fail "primary group is used by another account"
  done <<<"${entries}"
  ((guard_entries == 1)) || fail "passwd database does not contain exactly one guard-daemon account"
}

validate_group_entries() {
  local entries=$1
  local expected_gid=$2
  local guard_entries=0
  local candidate_group candidate_gid

  while IFS=: read -r candidate_group _ candidate_gid _; do
    [[ -n "${candidate_group}" && "${candidate_gid}" =~ ^[0-9]+$ ]] || \
      fail "group database contains an invalid entry"
    if [[ "${candidate_group}" == "guard-daemon" ]]; then
      guard_entries=$((guard_entries + 1))
      [[ "${candidate_gid}" == "${expected_gid}" ]] || fail "duplicate group entry does not match"
      continue
    fi
    [[ "${candidate_gid}" != "${expected_gid}" ]] || fail "GID is used by another group"
  done <<<"${entries}"
  ((guard_entries == 1)) || fail "group database does not contain exactly one guard-daemon group"
}

main() {
[[ "$(id -u)" == "0" ]] || fail "check must run as root"

passwd_entry="$(getent passwd guard-daemon)" || fail "user is missing"
IFS=: read -r name _ uid gid _ home shell <<<"${passwd_entry}"
[[ "${name}" == "guard-daemon" ]] || fail "unexpected name"
[[ "${uid}" =~ ^[0-9]+$ && "${gid}" =~ ^[0-9]+$ ]] || fail "UID/GID are not numeric"
((uid > 0)) || fail "UID 0 is forbidden"

uid_min=""
while read -r key value _; do
  if [[ "${key}" == "UID_MIN" && "${value}" =~ ^[0-9]+$ ]]; then
    uid_min="${value}"
    break
  fi
done </etc/login.defs
[[ -n "${uid_min}" ]] || fail "failed to determine UID_MIN"
((uid < uid_min)) || fail "UID is outside the system range"

identity_entries=""
read_identity_database passwd
validate_passwd_entries "${identity_entries}" "${uid}" "${gid}"

group_entry="$(getent group guard-daemon)" || fail "primary group is missing"
IFS=: read -r group_name _ group_gid group_members <<<"${group_entry}"
[[ "${group_name}" == "guard-daemon" && "${group_gid}" == "${gid}" ]] || \
  fail "primary group does not match"
identity_entries=""
read_identity_database group
validate_group_entries "${identity_entries}" "${gid}"
[[ -z "${group_members}" ]] || fail "group contains unexpected members"
[[ "$(id -G guard-daemon)" == "${gid}" ]] || fail "additional groups detected"

[[ "${home}" == "/var/lib/guard-daemon" ]] || fail "unexpected home directory"
case "${shell}" in
  /usr/sbin/nologin | /sbin/nologin) ;;
  *) fail "login shell is not disabled" ;;
esac

shadow_entry="$(getent shadow guard-daemon)" || fail "shadow entry is missing"
IFS=: read -r shadow_name shadow_password _ <<<"${shadow_entry}"
[[ "${shadow_name}" == "guard-daemon" ]] || fail "unexpected shadow entry"
case "${shadow_password}" in
  "!"* | "*"*) ;;
  *) fail "password is not locked" ;;
esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
