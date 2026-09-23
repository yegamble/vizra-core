#!/usr/bin/env bash
# demo.sh — the red/green demonstrations for hardening B5 (the Makefile digest pin).
#
# Run from the root of a CHECKOUT OF THE BRANCH UNDER TEST (the transcripts in
# this directory were produced in a scratch clone, never in a shared checkout):
#
#     bash docs/evidence/hardening-b5/demo.sh <out-dir>
#
# Every mutation goes through docs/evidence/hardening-b1/mutate.sh, which
# ABORTS (exit 90) if the mutation did not change the file's sha256 and (exit
# 91) if the restore is not byte-identical — so no line below can report a
# demonstration whose mutation silently did not apply.
#
# D* are BYTE MUTATIONS of the tree the anchor guards. None constructs a
# payload. "make was not invoked" is read from scripts/testdata/spawn-recorder.py,
# which runs the anchor in-process and records every process it starts — not
# from the anchor's own output.
#
# C* are CODE mutations of the controls themselves: each must turn the named
# test in the REQUIRED `scripts` package red, and green again once restored.
set -uo pipefail

out="${1:?usage: demo.sh <out-dir>}"
mkdir -p "$out"
M=docs/evidence/hardening-b1/mutate.sh
REC="$(mktemp)"
trap 'rm -f "$REC"' EXIT

header() {
  echo "# $1"
  echo "# host: $(uname -sm)  make: $(make --version | head -1)  python3: $(python3 --version 2>&1)  go: $(go version | awk '{print $3}')"
  echo "# tree: $(git rev-parse HEAD)  ($(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s) before the run)"
  echo
}

# The anchor under the recorder, in one mode; prints exit, process counts and FAIL lines.
anchor() { # anchor <mode-args...>
  python3 scripts/testdata/spawn-recorder.py "$REC" "$@" > "$REC.out" 2>&1
  local rc=$?
  python3 - "$REC" "$rc" "$*" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
make = [c["argv"] for c in d["calls"] if c["argv"] and c["argv"][0].rsplit("/", 1)[-1] in ("make", "gmake")]
print(f"anchor [{sys.argv[3] or 'lenient'}] exit={sys.argv[2]} processes-started={len(d['calls'])} make-processes={len(make)}"
      + (f" first-make-argv={make[0]}" if make else ""))
PY
  grep -E '^ +FAIL|make was NOT invoked|make-integrity-guard: passed' "$REC.out" | cut -c1-200
  rm -f "$REC.out"
}

guard() {
  ./scripts/ci-required-guard.sh "$@" > "$REC.g" 2>&1
  echo "ci-required-guard exit=$?"
  grep -E 'FAIL|pinned-makefiles' "$REC.g" | head -6 | cut -c1-200
  rm -f "$REC.g"
}
export -f anchor guard
export REC

# ---------------------------------------------------------------- D: bytes ---
{
  header "D0 control: the clean tree, both modes, and check 11"
  anchor --workflow
  anchor
  guard
} > "$out/D0-clean-tree.txt" 2>&1

{
  header "D1 one byte of the Makefile changed ('contract' -> 'Contract' in line 1's comment)"
  bash "$M" "D1 one Makefile byte" Makefile \
    "python3 -c \"p='Makefile';b=open(p,'rb').read();n=b'is the contract.';assert b.count(n)==1;open(p,'wb').write(b.replace(n,b'is the Contract.'))\"" \
    "anchor --workflow; anchor; guard"
  echo "== after restore =="; anchor --workflow
} > "$out/D1-one-makefile-byte.txt" 2>&1

{
  header "D2 an extra include line appended to the Makefile (pin not updated)"
  bash "$M" "D2 extra include line" Makefile \
    "printf 'include extra.mk\n' >> Makefile" \
    "anchor --workflow; anchor; guard"
  echo "== after restore =="; anchor --workflow
} > "$out/D2-extra-include-line.txt" 2>&1

{
  header "D3 an INCLUDED file's bytes changed (fixture include-pinned-good: Makefile includes a.mk and b.mk, all pinned)"
  echo "== control =="; anchor --root scripts/testdata/makeguard/include-pinned-good --targets ci --workflow
  bash "$M" "D3 one byte of included a.mk" scripts/testdata/makeguard/include-pinned-good/a.mk \
    "python3 -c \"p='scripts/testdata/makeguard/include-pinned-good/a.mk';b=open(p,'rb').read();assert b.count(b'A_VALUE := a')==1;open(p,'wb').write(b.replace(b'A_VALUE := a',b'A_VALUE := A'))\"" \
    "anchor --root scripts/testdata/makeguard/include-pinned-good --targets ci --workflow; anchor --root scripts/testdata/makeguard/include-pinned-good --targets ci"
  echo "== after restore =="; anchor --root scripts/testdata/makeguard/include-pinned-good --targets ci --workflow
} > "$out/D3-included-file-bytes.txt" 2>&1

{
  header "D4 a pin entry deleted (the Makefile's line in .github/pinned-makefiles.yml)"
  bash "$M" "D4 pin entry deleted" .github/pinned-makefiles.yml \
    "python3 -c \"import re;p='.github/pinned-makefiles.yml';s=open(p).read();n=len(re.findall(r'(?m)^  Makefile: [0-9a-f]{64}\n',s));assert n==1;open(p,'w').write(re.sub(r'(?m)^  Makefile: [0-9a-f]{64}\n','',s))\"" \
    "anchor --workflow; anchor; guard"
  echo "== after restore =="; anchor --workflow
} > "$out/D4-pin-entry-deleted.txt" 2>&1

# ----------------------------------------------------------------- C: code ---
gotest() { go test -count=1 -run "$1" ./scripts/ 2>&1 | grep -E '^(--- FAIL|    --- FAIL|ok|FAIL|panic)' | head -20; }
export -f gotest

{
  header "C1 the digest comparison disabled in the anchor"
  bash "$M" "C1 digest compare disabled" scripts/make-integrity-guard.py \
    "python3 -c \"p='scripts/make-integrity-guard.py';s=open(p).read();n='        if digests[rel] != pins[rel]:\n';assert s.count(n)==1;open(p,'w').write(s.replace(n,'        if False:\n'))\"" \
    "gotest 'TestMakefileDigestMutations|TestMakeIntegrityGuardFixtures/digest-mismatch'"
  echo "== after restore =="; gotest 'TestMakefileDigestMutations|TestMakeIntegrityGuardFixtures/digest-mismatch'
} > "$out/C1-digest-compare-disabled.txt" 2>&1

{
  header "C2 the pre-make gate narrowed to pin failures only (an environment failure would reach make again)"
  bash "$M" "C2 gate narrowed" scripts/make-integrity-guard.py \
    "python3 -c \"p='scripts/make-integrity-guard.py';s=open(p).read();n='    if g.failed:\n        print(f\\\"make-integrity-guard: FAILED — make was NOT invoked';assert s.count(n)==1;open(p,'w').write(s.replace(n,'    if pinned is None:\n        print(f\\\"make-integrity-guard: FAILED — make was NOT invoked'))\"" \
    "gotest 'TestAFailedEnvironmentCheckStopsTheAnchorBeforeMake'"
  echo "== after restore =="; gotest 'TestAFailedEnvironmentCheckStopsTheAnchorBeforeMake'
} > "$out/C2-gate-narrowed.txt" 2>&1

{
  header "C3 clean_env keeps the runner command-file variables (named list AND directory rule removed)"
  bash "$M" "C3 scrub removed" scripts/make-integrity-guard.py \
    "python3 -c \"p='scripts/make-integrity-guard.py';s=open(p).read();a='    drop = set(_ALWAYS_DROPPED) | set(RUNNER_COMMAND_FILES)\n';b='    drop |= {k for k, v in env.items() if _is_runner_command_file(v, command_dirs)}\n';assert s.count(a)==1 and s.count(b)==1;open(p,'w').write(s.replace(a,'    drop = set(_ALWAYS_DROPPED)\n').replace(b,''))\"" \
    "gotest 'TestTheAnchorsSubprocessesCannotSeeTheRunnerCommandFiles'"
  echo "== after restore =="; gotest 'TestTheAnchorsSubprocessesCannotSeeTheRunnerCommandFiles'
} > "$out/C3-command-file-scrub-removed.txt" 2>&1

{
  header "C3b only the directory rule removed (a command file under a name nothing lists)"
  bash "$M" "C3b directory rule removed" scripts/make-integrity-guard.py \
    "python3 -c \"p='scripts/make-integrity-guard.py';s=open(p).read();b='    drop |= {k for k, v in env.items() if _is_runner_command_file(v, command_dirs)}\n';assert s.count(b)==1;open(p,'w').write(s.replace(b,''))\"" \
    "gotest 'TestTheAnchorsSubprocessesCannotSeeTheRunnerCommandFiles'"
} > "$out/C3b-directory-rule-removed.txt" 2>&1

{
  header "C4 the make -q remake probe removed"
  bash "$M" "C4 remake probe removed" scripts/make-integrity-guard.py \
    "python3 -c \"p='scripts/make-integrity-guard.py';s=open(p).read();n='    check_no_pinned_makefile_would_be_remade(g, root, pinned_files)\n';assert s.count(n)==1;open(p,'w').write(s.replace(n,''))\"" \
    "gotest 'TestAMakefileMakeWouldRemakeIsRefusedWithoutRunningARecipe'"
  echo "== after restore =="; gotest 'TestAMakefileMakeWouldRemakeIsRefusedWithoutRunningARecipe'
} > "$out/C4-remake-probe-removed.txt" 2>&1

{
  header "C5 ci-required-guard check 11 no longer compares digests"
  bash "$M" "C5 check 11 compare disabled" scripts/ci-required-guard.py \
    "python3 -c \"p='scripts/ci-required-guard.py';s=open(p).read();n='        if got != want:\n';assert s.count(n)==1;open(p,'w').write(s.replace(n,'        if False:\n'))\"" \
    "gotest 'TestCIRequiredGuardMakefilePin'"
  echo "== after restore =="; gotest 'TestCIRequiredGuardMakefilePin'
} > "$out/C5-check11-compare-disabled.txt" 2>&1

{
  header "C6 the static read set no longer refuses an unpinned include"
  bash "$M" "C6 unpinned include accepted" scripts/make-integrity-guard.py \
    "python3 -c \"p='scripts/make-integrity-guard.py';s=open(p).read();n='    unpinned = [f for f in order if f not in pins]\n';assert s.count(n)==1;open(p,'w').write(s.replace(n,'    unpinned = []\n'))\"" \
    "gotest 'TestMakeIntegrityGuardFixtures/include-unpinned|TestMakefileDigestMutations'"
  echo "== after restore =="; gotest 'TestMakeIntegrityGuardFixtures/include-unpinned|TestMakefileDigestMutations'
} > "$out/C6-unpinned-include-accepted.txt" 2>&1

echo "tree after all demonstrations: $(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s)"
