#!/usr/bin/env bash

set -euo pipefail

if (($# < 2 || $# > 3)); then
  printf 'Использование: check-systemd-health.sh <HTTP-code> <status> [alert-code]\n' >&2
  exit 2
fi

expected_http=$1
expected_status=$2
expected_alert=${3:-}
case "${expected_http}:${expected_status}" in
  200:healthy_idle | 503:stopped) ;;
  *)
    printf 'Неподдерживаемое ожидаемое состояние health.\n' >&2
    exit 2
    ;;
esac
if [[ -n "${expected_alert}" && "${expected_alert}" != "paid_actions_stopped" ]]; then
  printf 'Неподдерживаемый ожидаемый alert.\n' >&2
  exit 2
fi

umask 077
health_file=$(mktemp "${TMPDIR:-/tmp}/guard-daemon-health.XXXXXX")
metrics_file=$(mktemp "${TMPDIR:-/tmp}/guard-daemon-metrics.XXXXXX")
# Invoked by the EXIT trap.
# shellcheck disable=SC2329
cleanup() {
  rm -f -- "${health_file}" "${metrics_file}"
}
trap cleanup EXIT

for ((attempt = 1; attempt <= 30; attempt++)); do
  http_code=""
  if http_code=$(curl --silent --show-error --max-time 2 \
    --output "${health_file}" \
    --unix-socket /var/lib/guard-daemon/diagnostics.sock \
    --write-out '%{http_code}' http://localhost/healthz 2>/dev/null) &&
    [[ "${http_code}" == "${expected_http}" ]] &&
    python3 -I -B -c '
import json
import pathlib
import sys

value = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
expected = sys.argv[2]
networks = value.get("networks")
stopped = expected == "stopped"
valid = (
    value.get("status") == expected
    and isinstance(networks, dict)
    and bool(networks)
    and all(
        isinstance(network, dict)
        and network.get("status") == expected
        and network.get("rpc_degraded") is False
        and network.get("budget_blocked") is False
        and network.get("ambiguous_rescue") is False
        and network.get("stopped") is stopped
        for network in networks.values()
    )
)
raise SystemExit(0 if valid else 1)
' "${health_file}" "${expected_status}"; then
    if [[ -z "${expected_alert}" ]]; then
      exit 0
    fi

    metrics_http=""
    if metrics_http=$(curl --silent --show-error --max-time 2 \
      --output "${metrics_file}" \
      --unix-socket /var/lib/guard-daemon/diagnostics.sock \
      --write-out '%{http_code}' http://localhost/metrics 2>/dev/null) &&
      [[ "${metrics_http}" == "200" ]] &&
      python3 -I -B -c '
import json
import pathlib
import sys

value = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
alerts = value.get("alerts")
valid = (
    isinstance(alerts, list)
    and value.get("active_alerts") == len(alerts)
    and any(isinstance(alert, dict) and alert.get("code") == sys.argv[2] for alert in alerts)
)
raise SystemExit(0 if valid else 1)
' "${metrics_file}" "${expected_alert}"; then
      exit 0
    fi
  fi
  sleep 1
done

printf 'guard-daemon не достиг ожидаемого проверенного состояния %s/HTTP %s.\n' \
  "${expected_status}" "${expected_http}" >&2
exit 1
