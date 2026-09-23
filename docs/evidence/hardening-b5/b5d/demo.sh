#!/usr/bin/env bash
# demo.sh — the red/green demonstrations for queue 2p (core B5d): the Makefile
# line grammar is an allowlist, and there is ONE line reader. Run from the root
# of a checkout of the branch under test, in a scratch clone:
#
#     bash docs/evidence/hardening-b5/b5d/demo.sh <out-dir>
#
# Mutations go through docs/evidence/hardening-b1/mutate.sh (aborts if the
# sha256 did not move, verifies the byte-identical restore); the mutation
# bodies are in mutations.py beside this file. "make was not started" comes
# from scripts/testdata/spawn-recorder.py, not from the anchor's own output.
# DEMO_ONLY=D runs the rows that need no Go toolchain (for a make-4.3 container).
#
# The exported helper functions carry no multi-line text that looks like a rule:
# bash exports them as environment variables, make prints the environment in
# its -pn database, and the anchor refuses (fails closed on) a database entry
# it cannot read as one name. A heredoc here first did exactly that.
set -uo pipefail
out="${1:?usage: demo.sh <out-dir>}"
mkdir -p "$out"
M=docs/evidence/hardening-b1/mutate.sh
MUT=docs/evidence/hardening-b5/b5d/mutations.py
REC="$(mktemp)"
trap 'rm -f "$REC"' EXIT
export REC MUT
FIXTURES="grammar-conditional grammar-export grammar-continued-comment grammar-vpath grammar-substitution-reference"
PINS="grammar-conditional grammar-include-pinned"
export FIXTURES PINS

header() {
  echo "# $1"
  echo "# host: $(uname -sm)  make: $(make --version | head -1)  python3: $(python3 --version 2>&1)  go: $(go version 2>/dev/null | awk '{print $3}')"
  echo "# tree: $(git rev-parse HEAD)  ($(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s) before the run)"
  echo "# scripts/ tree object: $(git rev-parse HEAD:scripts)"
  echo
}

# A committed grammar-only fixture, copied to a scratch dir with the FAILING
# gate stub, run through the anchor in both modes. Exit 0 = the control HELD:
# exit 1 in both modes, 0 make processes (recorded), the refusal is the grammar's.
fixture_scenario() {
  local fixture="$1" t rc=0
  t="$(mktemp -d)"
  cp -R "scripts/testdata/makeguard/$fixture/." "$t/"
  printf '#!/bin/sh\necho "the gate FAILED"\nexit 1\n' > "$t/run-the-real-tests.sh"
  chmod +x "$t/run-the-real-tests.sh"
  for mode in --workflow ""; do
    python3 scripts/testdata/spawn-recorder.py "$REC" --root "$t" --targets ci $mode > "$REC.out" 2>&1
    local arc=$? makes
    makes="$(python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(sum(1 for c in d['calls'] if c['argv'][0].rsplit('/',1)[-1] in ('make','gmake')))" "$REC")"
    echo "$fixture anchor [${mode:-lenient}] exit=$arc make-processes=$makes"
    grep -E '^ +FAIL|make-integrity-guard: (passed|FAILED)' "$REC.out" | cut -c1-160
    [ "$arc" = 1 ] || rc=1
    [ "$makes" = 0 ] || rc=1
    grep -q "is outside the Makefile grammar this anchor allows" "$REC.out" || rc=1
    rm -f "$REC.out"
  done
  rm -rf "$t"
  echo "fixture_scenario $fixture: control $([ $rc = 0 ] && echo HELD || echo BROKEN) (exit $rc)"
  return $rc
}
all_fixtures() { local f rc=0; for f in $FIXTURES; do fixture_scenario "$f" || rc=1; done; echo "all_fixtures exit=$rc"; return $rc; }

# Check 11 on a committed grammar pin fixture. Exit 0 = HELD (check 11 exit 1 naming the grammar).
pin_scenario() {
  local p="$1" rc=0 o
  o="$(python3 scripts/ci-required-guard.py --makefile-pins "scripts/testdata/makefilepin/$p/.github/pinned-makefiles.yml" 2>&1)"
  local code=$?
  echo "check 11 on $p: exit=$code"
  printf '%s\n' "$o" | grep -E '^ +FAIL|ci-required-guard: ' | cut -c1-160
  [ "$code" = 1 ] || rc=1
  printf '%s\n' "$o" | grep -q "is outside the Makefile grammar this anchor allows" || rc=1
  echo "pin_scenario $p: control $([ $rc = 0 ] && echo HELD || echo BROKEN) (exit $rc)"
  return $rc
}
all_pins() { local p rc=0; for p in $PINS; do pin_scenario "$p" || rc=1; done; echo "all_pins exit=$rc"; return $rc; }

# The one-reader probe. Exit 0 = every poison changed its reader's verdict, no
# second reader in SOURCE, one sequence per text.
probe_verdict() {
  local t rc
  t="$(mktemp -d)"
  python3 scripts/testdata/one-reader-probe.py scripts "$t" > "$t/out.json" 2> "$t/err.txt"
  rc=$?
  if [ "$rc" != 0 ]; then echo "probe did not run (exit $rc)"; cat "$t/err.txt" | tail -5; rm -rf "$t"; return 2; fi
  python3 docs/evidence/hardening-b5/b5d/probe-verdict.py "$t/out.json"
  rc=$?
  rm -rf "$t"
  echo "probe_verdict exit=$rc"
  return $rc
}
export -f fixture_scenario all_fixtures pin_scenario all_pins probe_verdict

gotest() {
  local log rc
  log="$(mktemp)"
  go test -count=1 -run "$1" ./scripts/ > "$log" 2>&1
  rc=$?
  grep -E '^(--- FAIL|    --- FAIL|ok|FAIL|panic)' "$log" | cut -c1-200 | head -40
  rm -f "$log"
  echo "go test exit=$rc"
  return "$rc"
}
export -f gotest

xrow() { # id title file check
  { header "$1 $2"
    bash "$M" "$1 $2" "$3" "python3 $MUT $1" "$4"
    echo "== after restore =="; bash -c "$4"
  } > "$out/$1.txt" 2>&1
}

# ---------------------------------------------- D: the controls, held ---
{ header "D-fixtures: the five grammar-only makeguard fixtures, gate stub failing"; all_fixtures; } > "$out/D-fixtures.txt" 2>&1
{ header "D-pins: check 11 on the grammar pin fixtures"; all_pins; } > "$out/D-pins.txt" 2>&1
{ header "D-one-reader-probe: POISON / SOURCE / IDENTITY on the unmutated tree"; probe_verdict; } > "$out/D-one-reader-probe.txt" 2>&1
{ header "D-db-scan-probe: the post-make database checks (defence in depth), fed strings in-process"
  python3 scripts/testdata/db-scan-probe.py | tail -3; echo "probe exit=${PIPESTATUS[0]}"; } > "$out/D-db-scan-probe.txt" 2>&1
{ header "D-real-tree: both anchor modes and ci-required-guard on the real, unchanged Makefile"
  ./scripts/make-integrity-guard.sh --workflow --targets ci | grep -E 'allowlist grammar|make-integrity-guard: (passed|FAILED)'; echo "anchor --workflow exit=${PIPESTATUS[0]}"
  ./scripts/make-integrity-guard.sh --targets ci | grep -E 'allowlist grammar|make-integrity-guard: (passed|FAILED)'; echo "anchor lenient exit=${PIPESTATUS[0]}"
  ./scripts/ci-required-guard.sh | grep -E 'allowlist grammar|ci-required-guard: (passed|FAILED)'; echo "ci-required-guard exit=${PIPESTATUS[0]}"
} > "$out/D-real-tree.txt" 2>&1

{ header "D-rows: every row of TestEveryOutOfGrammarLineIsRefusedBeforeMake, Go-free (rows.py)"
  python3 docs/evidence/hardening-b5/b5d/rows.py; echo "rows.py exit=$?"; } > "$out/D-rows.txt" 2>&1

# ------------------------------------ C: Go-free (run on 3.81 and 4.3) ---
xrow C33 "the grammar removed at its source (verify_pin)" scripts/makefile_pin.py "all_fixtures && all_pins"
xrow C34 "check 11 no longer reports the grammar" scripts/ci-required-guard.py "all_pins"
xrow C35 "the anchor no longer reports the grammar" scripts/make-integrity-guard.py "all_fixtures"
xrow C36 "the one reader's cache removed (IDENTITY)" scripts/makefile_pin.py "probe_verdict"
xrow C37 "a second line reader in ci-required-guard (POISON and SOURCE)" scripts/ci-required-guard.py "probe_verdict"
xrow C41 "the grammar removed, through every out-of-grammar row (Go-free)" scripts/makefile_pin.py \
  "python3 docs/evidence/hardening-b5/b5d/rows.py | grep -E '^BROKEN|^rows:' | cut -c1-200; exit \${PIPESTATUS[0]}"

if [ "${DEMO_ONLY:-}" = "D" ]; then
  echo "DEMO_ONLY=D: the Go-test C rows skipped (they need go)."
  echo "tree after: $(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s)"
  exit 0
fi

# ------------------------------------------------ C: code, Go test rows ---
xrow C38 "a backslash-continued rule line accepted again" scripts/makefile_pin.py \
  "gotest 'TestEveryOutOfGrammarLineIsRefusedBeforeMake/a_(rule|.PHONY)_line_continued'"
xrow C39 "the grammar removed, through the Go tables" scripts/makefile_pin.py \
  "gotest 'TestEveryOutOfGrammarLineIsRefusedBeforeMake|TestMakeIntegrityGuardFixtures/grammar|TestCIRequiredGuardMakefilePin/grammar'"
xrow C40 "a second line reader, through the Go probe test" scripts/ci-required-guard.py \
  "gotest 'TestEveryMakefileReaderConsumesTheOneLineReader'"

echo "tree after all demonstrations: $(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s)"
