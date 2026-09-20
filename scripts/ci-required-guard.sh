#!/usr/bin/env bash
# Thin wrapper so `make ci-guard`, the ci-required workflow and any existing
# muscle memory keep working. The guard itself is Python, because the checks it
# makes need a YAML PARSER rather than a regex — see scripts/ci-required-guard.py
# for why the regex version had holes.
set -euo pipefail
cd "$(dirname "$0")/.."
if ! command -v python3 >/dev/null 2>&1; then
  echo "ci-required-guard: python3 is required. This lane is BLOCKED, not passed." >&2
  exit 2
fi
exec python3 scripts/ci-required-guard.py "$@"
