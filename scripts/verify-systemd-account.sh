#!/usr/bin/env bash

set -euo pipefail

fail() {
  printf 'Некорректная системная учётная запись guard-daemon: %s\n' "$1" >&2
  exit 1
}

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

group_entry="$(getent group guard-daemon)" || fail "основная группа отсутствует"
IFS=: read -r group_name _ group_gid group_members <<<"${group_entry}"
[[ "${group_name}" == "guard-daemon" && "${group_gid}" == "${gid}" ]] || \
  fail "основная группа не совпадает"
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
