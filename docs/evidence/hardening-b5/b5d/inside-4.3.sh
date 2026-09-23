#!/usr/bin/env bash
# inside-4.3.sh — the GNU Make 4.3 half of the B5d evidence, run INSIDE an
# ubuntu:24.04 container (arm64, --rm) from the root of a scratch clone:
#
#   docker run --rm --name <unique> -v <clone>:/src -v <out>:/out -w /src ubuntu:24.04 \
#     bash docs/evidence/hardening-b5/b5d/inside-4.3.sh /out
#
# It installs make, python3, PyYAML and git, then runs the Go-free rows:
# demo.sh with DEMO_ONLY=D (D rows, rows.py, C33-C37 and C41), both anchors and
# ci-required-guard on the real tree, and every committed makeguard fixture.
set -uo pipefail
out="${1:?usage: inside-4.3.sh <out-dir>}"
export DEBIAN_FRONTEND=noninteractive
# Go-free rows only; and no __pycache__ in the tree (Python 3.12 caches the
# anchor module when a harness loads it with importlib).
export DEMO_ONLY=D PYTHONDONTWRITEBYTECODE=1
apt-get update -qq >/dev/null && apt-get install -y -qq make python3 python3-yaml git >/dev/null || { echo "apt failed"; exit 2; }
git config --global --add safe.directory /src
make --version | head -1
bash docs/evidence/hardening-b5/b5d/demo.sh "$out"
{ echo "# $(make --version | head -1), $(python3 --version), tree $(git rev-parse HEAD)"
  ./scripts/make-integrity-guard.sh --workflow --targets ci; echo "exit=$?"; } > "$out/anchor-workflow-full.txt" 2>&1
{ echo "# $(make --version | head -1), $(python3 --version), tree $(git rev-parse HEAD)"
  ./scripts/make-integrity-guard.sh --targets ci; echo "exit=$?"; } > "$out/anchor-lenient-full.txt" 2>&1
{ echo "# $(make --version | head -1), $(python3 --version), tree $(git rev-parse HEAD)"
  ./scripts/ci-required-guard.sh; echo "exit=$?"; } > "$out/ci-required-guard-full.txt" 2>&1
{ make --version | head -1
  for d in scripts/testdata/makeguard/*/; do
    n="$(basename "$d")"
    o="$(./scripts/make-integrity-guard.sh --root "$d" --targets ci 2>&1)"; rc=$?
    ni=0; printf '%s' "$o" | grep -q "make was NOT invoked (0 make process(es) started)" && ni=1
    echo "$n exit=$rc notinv=$ni $(printf '%s\n' "$o" | grep -m1 -E '^ +FAIL' | cut -c1-140)"
  done
  echo "tree after: $(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s)"
} > "$out/makeguard-fixtures-4.3.txt" 2>&1
echo "inside-4.3.sh done"
