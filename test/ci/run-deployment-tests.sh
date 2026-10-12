#!/usr/bin/env bash

set -euo pipefail

report_directory=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/guard-node-reports.XXXXXX")
trap 'rm -rf "${report_directory}"' EXIT

export NODE_OPTIONS="--report-on-fatalerror --report-uncaught-exception --report-directory=${report_directory}"

set +e
npm run deploy:test
status=$?
set -e

if ((status != 0)); then
  printf 'Deployment tests exited with code %d; Node.js %s.\n' \
    "${status}" "$(node --version)" >&2

  shopt -s nullglob
  reports=("${report_directory}"/report.*.json)
  shopt -u nullglob
  if ((${#reports[@]} > 0)); then
    report=${reports[${#reports[@]} - 1]}
    printf 'Safe Node.js diagnostic report summary:\n' >&2
    # Оболочка не должна подставлять значения в шаблонную строку JavaScript.
    # shellcheck disable=SC2016
    REPORT_PATH="${report}" node --input-type=module --eval '
      import { readFileSync } from "node:fs";
      const { header = {} } = JSON.parse(readFileSync(process.env.REPORT_PATH, "utf8"));
      const summary = {
        arch: header.arch,
        event: header.event,
        nodejsVersion: header.nodejsVersion,
        osName: header.osName,
        osRelease: header.osRelease,
        platform: header.platform,
        trigger: header.trigger,
      };
      process.stderr.write(`${JSON.stringify(summary, null, 2)}\n`);
    ' || printf 'Failed to read the diagnostic report without exposing the environment.\n' >&2
  else
    printf 'Node.js did not create a diagnostic report.\n' >&2
  fi
fi

exit "${status}"
