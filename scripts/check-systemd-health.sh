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
    [[ "${http_code}" == "${expected_http}" ]]; then
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

health = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
metrics = json.loads(pathlib.Path(sys.argv[2]).read_text(encoding="utf-8"))
expected_status = sys.argv[3]
expected_alert = sys.argv[4]
health_networks = health.get("networks")
metrics_networks = metrics.get("networks")
stopped = expected_status == "stopped"
valid = (
    health.get("status") == expected_status
    and isinstance(health_networks, dict)
    and bool(health_networks)
    and isinstance(metrics_networks, dict)
    and set(metrics_networks) == set(health_networks)
    and all(
        isinstance(network, dict)
        and network.get("status") == expected_status
        and network.get("rpc_degraded") is False
        and network.get("budget_blocked") is False
        and network.get("ambiguous_rescue") is False
        and network.get("stopped") is stopped
        for network in health_networks.values()
    )
)
alerts = metrics.get("alerts")
active_alerts = metrics.get("active_alerts")
valid = valid and isinstance(alerts, list) and type(active_alerts) is int and active_alerts == len(alerts)
if valid and expected_alert == "":
    valid = len(alerts) == 0
elif valid:
    valid_alerts = [
        alert
        for alert in alerts
        if isinstance(alert, dict)
        and isinstance(alert.get("chain_id"), str)
        and alert.get("code") == expected_alert
    ]
    alert_chains = {alert["chain_id"] for alert in valid_alerts}
    valid = (
        len(alerts) == len(health_networks)
        and len(valid_alerts) == len(alerts)
        and alert_chains == set(health_networks)
    )
raise SystemExit(0 if valid else 1)
' "${health_file}" "${metrics_file}" "${expected_status}" "${expected_alert}"; then
      exit 0
    fi
  fi
  sleep 1
done

printf 'guard-daemon не достиг ожидаемого проверенного состояния %s/HTTP %s.\n' \
  "${expected_status}" "${expected_http}" >&2
exit 1
