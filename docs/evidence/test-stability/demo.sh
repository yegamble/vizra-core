#!/usr/bin/env bash
# demo.sh — red/green demonstrations for the test-stability slice. Run from the root of a scratch
# clone of the branch under test, with VIZRA_TEST_DATABASE_URL unset (none of these rows needs one):
#
#     bash docs/evidence/test-stability/demo.sh <out-dir>
#
# Mutations go through docs/evidence/hardening-b1/mutate.sh (aborts if the sha256 did not move,
# verifies the byte-identical restore); the bodies are in mutations.py beside this file. Every child
# test binary runs with a TMPDIR its parent test owns, so other processes cannot affect a verdict.
set -uo pipefail
out="${1:?usage: demo.sh <out-dir>}"
mkdir -p "$out"
M=docs/evidence/hardening-b1/mutate.sh
MUT=docs/evidence/test-stability/mutations.py
export MUT
header() {
  echo "# $1"
  echo "# host: $(uname -sm)  go: $(go version | awk '{print $3}')  load: $(uptime | sed 's/.*averages: //')"
  echo "# tree: $(git rev-parse HEAD)  ($(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s) before the run)"
  echo
}
gotest() { # tags run pkg
  local log rc
  log="$(mktemp)"
  go test -count=1 ${1:+-tags=$1} -v -run "$2" "$3" > "$log" 2>&1
  rc=$?
  # Absolute temporary paths are replaced by $TMPDIR: this repository is public.
  grep -E '^(--- FAIL|--- PASS|    --- FAIL|ok|FAIL|panic)|tmpleak_test.go|testtmp_test.go|build failed' "$log" \
    | sed -E 's#(/private)?/var/folders/[^ ]*/T/#$TMPDIR/#g; s#(/private)?/tmp/[^ ]*/#$TMPDIR/#g' | cut -c1-220 | head -30
  rm -f "$log"
  echo "go test exit=$rc"
  return "$rc"
}
export -f gotest
row() { # id title file tags run pkg
  { header "$1 $2"
    bash "$M" "$1 $2" "$3" "python3 $MUT $1" "gotest '$4' '$5' '$6'"
    echo "== after restore =="; gotest "$4" "$5" "$6"
  } > "$out/$1.txt" 2>&1
}
row T1 "fixtures TestMain without the testtmp root" internal/fixtures/fixtures_test.go "" '^TestTheFixturesTestsLeaveNoTemporaryEntry$' ./internal/fixtures/
row T2 "integration TestMain without the testtmp root" internal/integration/main_test.go integration '^TestTheIntegrationTestsLeaveNoTemporaryEntry$' ./internal/integration/
row T3 "Sweep never removes a dead run's root" internal/testtmp/testtmp.go "" '^TestSweepRemovesOnlyRootsOfDeadProcessesOfThisPackage$' ./internal/testtmp/
row T4 "alive() treats a foreign live process as dead" internal/testtmp/testtmp.go "" '^TestAliveTellsALiveProcessFromADeadOne$' ./internal/testtmp/
echo "tree after: $(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s)"
