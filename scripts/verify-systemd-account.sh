#!/usr/bin/env bash

set -euo pipefail

fail() {
  printf 'Некорректная системная учётная запись guard-daemon: %s\n' "$1" >&2
  exit 1
}

read_identity_database() {
  local database=$1

  identity_entries="$(getent "${database}")" || fail "не удалось полностью прочитать ${database}"
  [[ -n "${identity_entries}" ]] || fail "база ${database} пуста"
}

validate_passwd_entries() {
  local entries=$1
  local expected_uid=$2
  local expected_gid=$3
  local guard_entries=0
  local candidate_name candidate_uid candidate_gid

  while IFS=: read -r candidate_name _ candidate_uid candidate_gid _; do
    [[ -n "${candidate_name}" && "${candidate_uid}" =~ ^[0-9]+$ && "${candidate_gid}" =~ ^[0-9]+$ ]] || \
      fail "база passwd содержит некорректную запись"
    if [[ "${candidate_name}" == "guard-daemon" ]]; then
      guard_entries=$((guard_entries + 1))
      [[ "${candidate_uid}" == "${expected_uid}" && "${candidate_gid}" == "${expected_gid}" ]] || \
        fail "дублирующая passwd entry не совпадает"
      continue
    fi
    [[ "${candidate_uid}" != "${expected_uid}" ]] || fail "UID используется другой учётной записью"
    [[ "${candidate_gid}" != "${expected_gid}" ]] || fail "основная группа используется другой учётной записью"
  done <<<"${entries}"
  ((guard_entries == 1)) || fail "база passwd не содержит ровно одну учётную запись guard-daemon"
}

validate_group_entries() {
  local entries=$1
  local expected_gid=$2
  local guard_entries=0
  local candidate_group candidate_gid

  while IFS=: read -r candidate_group _ candidate_gid _; do
    [[ -n "${candidate_group}" && "${candidate_gid}" =~ ^[0-9]+$ ]] || \
      fail "база group содержит некорректную запись"
    if [[ "${candidate_group}" == "guard-daemon" ]]; then
      guard_entries=$((guard_entries + 1))
      [[ "${candidate_gid}" == "${expected_gid}" ]] || fail "дублирующая group entry не совпадает"
      continue
    fi
    [[ "${candidate_gid}" != "${expected_gid}" ]] || fail "GID используется другой группой"
  done <<<"${entries}"
  ((guard_entries == 1)) || fail "база group не содержит ровно одну группу guard-daemon"
}

main() {
[[ "$(id -u)" == "0" ]] || fail "проверка должна выполняться от root"

passwd_entry="$(getent passwd guard-daemon)" || fail "пользователь отсутствует"
IFS=: read -r name _ uid gid _ home shell <<<"${passwd_entry}"
[[ "${name}" == "guard-daemon" ]] || fail "неожиданное имя"
[[ "${uid}" =~ ^[0-9]+$ && "${gid}" =~ ^[0-9]+$ ]] || fail "UID/GID не являются числами"
((uid > 0)) || fail "UID 0 запрещён"

uid_min=""
while read -r key value _; do
  if [[ "${key}" == "UID_MIN" && "${value}" =~ ^[0-9]+$ ]]; then
    uid_min="${value}"
    break
  fi
done </etc/login.defs
[[ -n "${uid_min}" ]] || fail "не удалось определить UID_MIN"
((uid < uid_min)) || fail "UID не входит в системный диапазон"

identity_entries=""
read_identity_database passwd
validate_passwd_entries "${identity_entries}" "${uid}" "${gid}"

group_entry="$(getent group guard-daemon)" || fail "основная группа отсутствует"
IFS=: read -r group_name _ group_gid group_members <<<"${group_entry}"
[[ "${group_name}" == "guard-daemon" && "${group_gid}" == "${gid}" ]] || \
  fail "основная группа не совпадает"
identity_entries=""
read_identity_database group
validate_group_entries "${identity_entries}" "${gid}"
[[ -z "${group_members}" ]] || fail "группа содержит посторонних участников"
[[ "$(id -G guard-daemon)" == "${gid}" ]] || fail "обнаружены дополнительные группы"

[[ "${home}" == "/var/lib/guard-daemon" ]] || fail "неожиданный home"
case "${shell}" in
  /usr/sbin/nologin | /sbin/nologin) ;;
  *) fail "login shell не заблокирован" ;;
esac

shadow_entry="$(getent shadow guard-daemon)" || fail "shadow entry отсутствует"
IFS=: read -r shadow_name shadow_password _ <<<"${shadow_entry}"
[[ "${shadow_name}" == "guard-daemon" ]] || fail "неожиданная shadow entry"
case "${shadow_password}" in
  "!"* | "*"*) ;;
  *) fail "пароль не заблокирован" ;;
esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
