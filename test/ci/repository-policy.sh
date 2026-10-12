#!/usr/bin/env bash

set -euo pipefail

exec python3 -B test/ci/repository_policy.py "$@"
