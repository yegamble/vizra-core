#!/usr/bin/env bash
# demo.sh — the red/green demonstrations for slice B5b (special targets and the
# rules the gate closure can reach). Run from the root of a checkout of the
# branch under test, in a scratch clone:
#
#     bash docs/evidence/hardening-b5/b5b/demo.sh <out-dir>
#
# Mutations go through docs/evidence/hardening-b1/mutate.sh (aborts if the
# sha256 did not move, verifies the byte-identical restore); the mutation
# bodies are in mutations.py beside this file. "make was not started" comes
# from scripts/testdata/spawn-recorder.py, not from the anchor's own output.
# DEMO_ONLY=D runs the rows that need no Go toolchain (for a make-4.3 container).
set -uo pipefail
out="${1:?usage: demo.sh <out-dir>}"
mkdir -p "$out"
M=docs/evidence/hardening-b1/mutate.sh
MUT=docs/evidence/hardening-b5/b5b/mutations.py
REC="$(mktemp)"
trap 'rm -f "$REC"' EXIT
export REC MUT

header() {
  echo "# $1"
  echo "# host: $(uname -sm)  make: $(make --version | head -1)  python3: $(python3 --version 2>&1)  go: $(go version 2>/dev/null | awk '{print $3}')"
  echo "# tree: $(git rev-parse HEAD)  ($(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s) before the run)"
  echo "# scripts/ tree object: $(git rev-parse HEAD:scripts)"
  echo
}

# A committed fixture, copied to a scratch dir with the FAILING gate stub, run
# through the anchor in both modes. Exit 0 = the control HELD: exit 1 in both
# modes, 0 make processes, the refusal names $2. Raw `make ci` on the same bytes
# is printed for contrast.
fixture_scenario() {
  local fixture="$1" name="$2" t rc=0
  t="$(mktemp -d)"
  cp -R "scripts/testdata/makeguard/$fixture/." "$t/"
  printf '#!/bin/sh\necho "the gate FAILED"\nexit 1\n' > "$t/run-the-real-tests.sh"
  chmod +x "$t/run-the-real-tests.sh"
  echo "fixture $fixture: raw \`make ci\` on these bytes (no anchor): exit $(cd "$t" && make ci >/dev/null 2>&1; echo $?)"
  for mode in --workflow ""; do
    python3 scripts/testdata/spawn-recorder.py "$REC" --root "$t" --targets ci $mode > "$REC.out" 2>&1
    local arc=$? makes
    makes="$(python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(sum(1 for c in d['calls'] if c['argv'][0].rsplit('/',1)[-1] in ('make','gmake')))" "$REC")"
    echo "anchor [${mode:-lenient}] exit=$arc make-processes=$makes"
    grep -E '^ +FAIL|make-integrity-guard: (passed|FAILED)' "$REC.out" | cut -c1-200
    [ "$arc" = 1 ] || rc=1
    [ "$makes" = 0 ] || rc=1
    grep -q "$name" "$REC.out" || rc=1
    rm -f "$REC.out"
  done
  rm -rf "$t"
  echo "fixture_scenario $fixture: control $([ $rc = 0 ] && echo HELD || echo BROKEN) (exit $rc)"
  return $rc
}
export -f fixture_scenario

gotest() {
  local log rc
  log="$(mktemp)"
  go test -count=1 -run "$1" ./scripts/ > "$log" 2>&1
  rc=$?
  grep -E '^(--- FAIL|    --- FAIL|ok|FAIL|panic)|was STARTED|must come BEFORE|does not (name|mention|say)|want failed' "$log" | cut -c1-220 | head -30
  rm -f "$log"
  echo "go test exit=$rc"
  return "$rc"
}
export -f gotest

# ------------------------------------------------ D: the fixtures, held ---
for pair in "ignore-bare:names \`.IGNORE\`" "ignore-per-target:names \`.IGNORE\`" \
            "default-recipe:names \`.DEFAULT\`" "extra-prereqs:names \`.EXTRA_PREREQS\`" \
            "pattern-rule:is a PATTERN rule" "closure-not-phony:not declared \`.PHONY\`" \
            "missing-prerequisite:has no explicit rule" "submake:starts a sub-make"; do
  f="${pair%%:*}"; n="${pair#*:}"
  { header "D-$f: the committed fixture, gate stub failing"; fixture_scenario "$f" "$n"; echo "fixture_scenario exit=$?"; } \
    > "$out/D-$f.txt" 2>&1
done

# ------------------------- C15/C15b: .IGNORE, no Go needed (runs on 4.3) ---
# On the PER-TARGET form. The bare `.IGNORE:` also puts `i` into MAKEFLAGS,
# which the resolver's MAKEFLAGS check already refused at 398ac4f (measured:
# 'pni' on 3.81, 'inp' on 4.3); `.IGNORE: ci` does not, and is the hole.
for f in ignore-per-target ignore-bare; do
{
  header "C15 ($f) the pre-make .IGNORE refusal removed (the resolver backstops remain)"
  bash "$M" "C15 .IGNORE refusal removed" scripts/make-integrity-guard.py "python3 $MUT C15" \
    "fixture_scenario $f 'names \`.IGNORE\`'"
  echo "== after restore =="; fixture_scenario $f 'names `.IGNORE`'
} > "$out/C15-$f-refusal-removed.txt" 2>&1
{
  header "C15b ($f) the .IGNORE refusal AND its .IGNORE resolver backstop removed (the 398ac4f state)"
  bash "$M" "C15b .IGNORE refusal and backstop removed" scripts/make-integrity-guard.py "python3 $MUT C15b" \
    "fixture_scenario $f 'names \`.IGNORE\`'"
  echo "== after restore =="; fixture_scenario $f 'names `.IGNORE`'
} > "$out/C15b-$f-refusal-and-backstop-removed.txt" 2>&1
done

# #11 fix round 1 (cross-check X-1) and B5c. Go-free, so they run on 4.3 too.
# fixture_scenario_post: the refusal comes AFTER make (from make's database) —
# HELD means exit 1 in both modes with the refusal naming $2.
fixture_scenario_post() {
  local fixture="$1" name="$2" t rc=0
  t="$(mktemp -d)"
  cp -R "scripts/testdata/makeguard/$fixture/." "$t/"
  printf '#!/bin/sh\necho "the gate FAILED"\nexit 1\n' > "$t/run-the-real-tests.sh"
  chmod +x "$t/run-the-real-tests.sh"
  echo "fixture $fixture: raw \`make ci\` on these bytes (no anchor): exit $(cd "$t" && make ci >/dev/null 2>&1; echo $?)"
  for mode in --workflow ""; do
    python3 scripts/testdata/spawn-recorder.py "$REC" --root "$t" --targets ci $mode > "$REC.out" 2>&1
    local arc=$?
    echo "anchor [${mode:-lenient}] exit=$arc"
    grep -E '^ +FAIL|make-integrity-guard: (passed|FAILED)' "$REC.out" | cut -c1-200
    [ "$arc" = 1 ] || rc=1
    grep -q "$name" "$REC.out" || rc=1
    rm -f "$REC.out"
  done
  rm -rf "$t"
  echo "fixture_scenario_post $fixture: control $([ $rc = 0 ] && echo HELD || echo BROKEN) (exit $rc)"
  return $rc
}
export -f fixture_scenario_post

for pair in "inline-recipe:is a rule with an INLINE" "multi-target-rule:is a MULTI-TARGET rule" \
            "computed-prerequisite:has a prerequisite make COMPUTES" "posix:names \`.POSIX\`"; do
  f="${pair%%:*}"; n="${pair#*:}"
  { header "D-$f: the committed fixture, gate stub failing"; fixture_scenario "$f" "$n"; echo "fixture_scenario exit=$?"; } \
    > "$out/D-$f.txt" 2>&1
done

xrow() { # id title fixture name [post]
  local fn=fixture_scenario; [ "${5:-}" = post ] && fn=fixture_scenario_post
  { header "$1 $2"
    bash "$M" "$1 $2" scripts/make-integrity-guard.py "python3 $MUT $1" "$fn $3 '$4'"
    echo "== after restore =="; $fn "$3" "$4"
  } > "$out/$1.txt" 2>&1
}
xrow C22 "the inline-recipe refusal removed" inline-recipe "is a rule with an INLINE"
xrow C23 "the multi-target refusal removed" multi-target-rule "is a MULTI-TARGET rule"
xrow C24 "the computed-prerequisite refusal removed" computed-prerequisite "has a prerequisite make COMPUTES"
xrow C25 "the .POSIX refusal removed" posix 'names `.POSIX`'

# #11 fix round 2 (R1-1). C27 on the committed fixture; C28-C32 through the
# in-process probe (scripts/testdata/db-scan-probe.py): exit 0 = every row as
# expected, 1 = some check no longer refuses what it must. Go-free, so they run
# on 4.3 too.
{ header "D-computed-target-recipe (round 2): a rule whose TARGET make computes, refused BEFORE make"
  fixture_scenario computed-target-recipe "is a rule whose TARGET make computes"
  echo "fixture_scenario exit=$?"; } > "$out/D-computed-target-recipe.txt" 2>&1
{ header "D-db-scan-probe: the database checks, fed strings in-process"
  python3 scripts/testdata/db-scan-probe.py; echo "probe exit=$?"; } > "$out/D-db-scan-probe.txt" 2>&1
xrow C27 "the computed-target refusal removed" computed-target-recipe "is a rule whose TARGET make computes"
probe_row() { # id title
  { header "$1 $2"
    bash "$M" "$1 $2" scripts/make-integrity-guard.py "python3 $MUT $1" "python3 scripts/testdata/db-scan-probe.py | grep -E '^ROW BAD|^PROBE'; exit \${PIPESTATUS[0]}"
    echo "== after restore =="; python3 scripts/testdata/db-scan-probe.py | tail -1
  } > "$out/$1.txt" 2>&1
}
probe_row C28 "a missing database entry no longer refused"
probe_row C29 "a duplicate database entry no longer refused"
probe_row C30 "an unreadable database header no longer refused"
probe_row C31 "a recipe differing from the pinned rule no longer refused"
probe_row C32 "a closure make widens or narrows no longer refused"

if [ "${DEMO_ONLY:-}" = "D" ]; then
  echo "DEMO_ONLY=D: the Go-test C rows skipped (they need go)."
  echo "tree after: $(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s)"
  exit 0
fi

# ------------------------------------------------ C: code, Go test rows ---
row() { # id title tests
  { header "$1 $2"
    bash "$M" "$1 $2" scripts/make-integrity-guard.py "python3 $MUT $1" "gotest '$3'"
    echo "== after restore =="; gotest "$3"
  } > "$out/$1.txt" 2>&1
}
row C16 "the pre-make .DEFAULT refusal removed" "TestMakeIntegrityGuardFixtures/default-recipe|TestEveryRefusedSpellingIsRefusedBeforeMake/default"
row C17 "the pre-make .EXTRA_PREREQS refusal removed" "TestMakeIntegrityGuardFixtures/extra-prereqs|TestEveryRefusedSpellingIsRefusedBeforeMake/extra"
row C18 "the pattern-rule refusal removed" "TestMakeIntegrityGuardFixtures/pattern-rule|TestEveryRefusedSpellingIsRefusedBeforeMake/pattern_rule"
row C19 "the .PHONY requirement removed" "TestMakeIntegrityGuardFixtures/closure-not-phony"
row C20 "the no-explicit-rule refusal removed (back to a 'note')" "TestMakeIntegrityGuardFixtures/missing-prerequisite"
row C21 "the sub-make refusal removed" "TestMakeIntegrityGuardFixtures/submake|TestEveryRefusedSpellingIsRefusedBeforeMake/sub-make"
# Since #11 fix round 2 every committed route to the database scan is refused
# before make, so its call is held by the real-tree test's ok line, and its
# refusals by the in-process probe rows (C28-C32).
row C26 "the database recipe scan call removed" "TestMakeIntegrityGuardPassesOnTheRealMakefile|TestTheDatabaseChecksFailClosed"

echo "tree after all demonstrations: $(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s)"
