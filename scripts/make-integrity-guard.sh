#!/usr/bin/env bash
# Thin wrapper around scripts/make-integrity-guard.py, so a workflow step and
# `make ci-guard` invoke the same thing.
#
# It is DELIBERATELY not a make target's only home: the whole point of the
# program it runs is that a neutered Makefile cannot disarm it, which is only
# true while it runs from outside make. See the workflow anchor steps in
# .github/workflows/build-test.yml and fixtures.yml, which invoke it BEFORE any
# `make` line, and scripts/ci-required-guard.py, which asserts those steps exist.
set -euo pipefail
cd "$(dirname "$0")/.."
if ! command -v python3 >/dev/null 2>&1; then
  echo "make-integrity-guard: python3 is required. This lane is BLOCKED, not passed." >&2
  exit 2
fi
exec python3 scripts/make-integrity-guard.py "$@"
