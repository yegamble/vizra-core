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
  echo "# host: $(uname -sm)  make: $(make --version | head -1)  python3: $(python3 --version 2>&1)  go: $(go version 2>/dev/null | awk '{print $3}')"
  echo "# tree: $(git rev-parse HEAD)  ($(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s) before the run)"
  echo "# scripts/ tree object: $(git rev-parse HEAD:scripts)  (identical in any commit with the same scripts/ bytes)"
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

{
  header "D5 a newer SIBLING Makefile.sh beside the Makefile (make's builtin rule \`%: %.sh\` would remake the Makefile)"
  scratch="$(mktemp -d)"
  echo "== raw make, in a scratch dir holding only a 3-line Makefile and a newer copy of it plus one comment =="
  ( cd "$scratch" && printf '.PHONY: ci\nci:\n\t@echo gate\n' > Makefile && touch -t 202001010000 Makefile \
      && { cat Makefile; echo '# sibling copy, one comment longer'; } > Makefile.sh \
      && b="$(shasum -a 256 Makefile | cut -d' ' -f1)" \
      && make -q Makefile; echo "make -q Makefile exit=$? ; Makefile unchanged: $([ "$(shasum -a 256 Makefile | cut -d' ' -f1)" = "$b" ] && echo yes || echo NO)" \
      && make -pn ci > p.txt 2>&1; echo "make -pn ci exit=$? ; Makefile unchanged: $([ "$(shasum -a 256 Makefile | cut -d' ' -f1)" = "$b" ] && echo yes || echo NO) ; recipe lines make ran/printed: $(grep -c '^cat Makefile.sh >Makefile' p.txt)" )
  rm -rf "$scratch"
  echo "== the anchor, on this tree, with the same kind of sibling =="
  before="$(shasum -a 256 Makefile | cut -d' ' -f1)"
  { cat Makefile; echo '# sibling copy, one comment longer'; } > Makefile.sh
  touch -t 202001010000 Makefile
  echo "  added Makefile.sh sha256 $(shasum -a 256 Makefile.sh | cut -d' ' -f1); Makefile sha256 $before (mtime set older)"
  anchor --workflow
  anchor
  after="$(shasum -a 256 Makefile | cut -d' ' -f1)"
  echo "  Makefile after the anchor: $after  byte-identical: $([ "$after" = "$before" ] && echo yes || echo NO)"
  rm -f Makefile.sh
  touch Makefile
  echo "== sibling removed =="
  anchor --workflow
} > "$out/D5-remake-sibling.txt" 2>&1

# D6 / C7 — fix round 1 (PR#10 VERIFY FINDING 1). A tree whose pin lists an
# INCLUDE: the real Makefile plus `include a.mk` / `sinclude b.mk`, all three
# pinned. A newer sibling `a.mk.sh` (a copy of a.mk plus one comment). Exit 0
# means the control HELD: the anchor refused in both modes, make was started
# exactly once as `make -q Makefile a.mk b.mk`, and a.mk is byte-identical.
include_sibling() {
  local t rc=0 before after
  t="$(mktemp -d)"
  mkdir -p "$t/.github"
  cp Makefile "$t/Makefile"
  printf 'include a.mk\nsinclude b.mk\n' >> "$t/Makefile"
  printf '# a.mk: pinned\nA_VALUE := a\n' > "$t/a.mk"
  printf '# b.mk: pinned\nB_VALUE := b\n' > "$t/b.mk"
  { echo "makefiles:"; for f in Makefile a.mk b.mk; do printf '  %s: %s\n' "$f" "$(shasum -a 256 "$t/$f" | cut -d' ' -f1)"; done; } > "$t/.github/pinned-makefiles.yml"
  echo "== control: the include tree, pinned, no sibling =="
  anchor --root "$t" --targets ci --workflow
  before="$(shasum -a 256 "$t/a.mk" | cut -d' ' -f1)"
  { cat "$t/a.mk"; echo '# sibling copy, one comment longer'; } > "$t/a.mk.sh"
  touch -t 202001010000 "$t/a.mk"
  echo "== newer a.mk.sh beside the pinned include a.mk (a.mk sha256 $before) =="
  for mode in --workflow ""; do
    python3 scripts/testdata/spawn-recorder.py "$REC" --root "$t" --targets ci $mode > "$REC.out" 2>&1
    local arc=$?
    local makes
    makes="$(python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(';'.join(' '.join(c['argv']) for c in d['calls'] if c['argv'][0].rsplit('/',1)[-1] in ('make','gmake')))" "$REC")"
    after="$(shasum -a 256 "$t/a.mk" | cut -d' ' -f1)"
    echo "anchor [${mode:-lenient}] exit=$arc make-processes=[$makes] a.mk-after=$after"
    grep -E '^ +FAIL|^ +ok +`make -q|CHANGED|make-integrity-guard: (passed|FAILED)' "$REC.out" | cut -c1-200
    rm -f "$REC.out"
    [ "$arc" = 1 ] || rc=1
    [ "$makes" = "make -q Makefile a.mk b.mk" ] || rc=1
    [ "$after" = "$before" ] || rc=1
    # restore a.mk for the next mode if make rewrote it
    if [ "$after" != "$before" ]; then printf '# a.mk: pinned\nA_VALUE := a\n' > "$t/a.mk"; touch -t 202001010000 "$t/a.mk"; fi
  done
  rm -rf "$t"
  echo "include_sibling: control $([ $rc = 0 ] && echo HELD || echo BROKEN) (exit $rc)"
  return $rc
}
export -f include_sibling

{
  header "D6 a newer sibling of a pinned INCLUDE (a.mk.sh beside a.mk; fix round 1, PR#10 VERIFY FINDING 1)"
  include_sibling
  echo "include_sibling exit=$?"
} > "$out/D6-include-sibling.txt" 2>&1

{
  header "C7 the remake probe reverted to one make -q PER FILE (the 62d16aa shape)"
  bash "$M" "C7 per-file probe" scripts/make-integrity-guard.py \
    "python3 -c \"p='scripts/make-integrity-guard.py';s=open(p).read();n='    probes = [files]  #';assert s.count(n)==1;open(p,'w').write(s.replace(n,'    probes = [[f] for f in files]  #'))\"" \
    "include_sibling"
  echo "== after restore =="; include_sibling
} > "$out/C7-per-file-probe.txt" 2>&1

# P1 — fix round 1 (PR#10 VERIFY FINDING 3): check 11 at 62d16aa against the
# parity fixtures (exit 0 = the gap), then check 11 on this tree (exit 1).
{
  header "P1 check 11 parity: the 62d16aa ci-required-guard vs this tree's, on the new makefilepin fixtures"
  old="$(mktemp -d)"
  git show 62d16aa773a2db4414bc2f96b3381c732e2ebf88:scripts/ci-required-guard.py > "$old/ci-required-guard.py"
  for f in gnumakefile-present makefile-symlink stale-entry include-unpinned include-computed; do
    pin="scripts/testdata/makefilepin/$f/.github/pinned-makefiles.yml"
    python3 "$old/ci-required-guard.py" --workflows .github/workflows --manifest .github/required-checks.txt \
      --makefile Makefile --pins .github/pinned-steps.yml --makefile-pins "$pin" > "$REC.p" 2>&1
    o=$?
    ./scripts/ci-required-guard.sh --makefile-pins "$pin" > "$REC.q" 2>&1
    n=$?
    echo "$f: check11@62d16aa exit=$o | check11@this-tree exit=$n  $(grep -m1 'FAIL' "$REC.q" | cut -c1-150)"
  done
  rm -rf "$old" "$REC.p" "$REC.q"
} > "$out/P1-check11-parity.txt" 2>&1

if [ "${DEMO_ONLY:-}" = "D" ]; then
  echo "DEMO_ONLY=D: code mutations (C*) skipped; they need go."
  echo "tree after the byte demonstrations: $(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s)"
  exit 0
fi

# ----------------------------------------------------------------- C: code ---
# go test's OWN exit code is the check's exit code; the grep only trims the log
# to the verdict lines and the assertion that fired.
gotest() {
  local log rc
  log="$(mktemp)"
  go test -count=1 -run "$1" ./scripts/ > "$log" 2>&1
  rc=$?
  grep -E '^(--- FAIL|    --- FAIL|ok|FAIL|panic)|is visible|was STARTED|REWRITTEN|want the remake|did not say make|must come BEFORE|does not (name|mention|say)|want failed' "$log" | cut -c1-220 | head -30
  rm -f "$log"
  echo "go test exit=$rc"
  return "$rc"
}
export -f gotest

{
  header "C1 the digest comparison disabled (scripts/makefile_pin.py, shared by the anchor and check 11)"
  bash "$M" "C1 digest compare disabled" scripts/makefile_pin.py \
    "python3 -c \"p='scripts/makefile_pin.py';s=open(p).read();n='        if r.digests[rel] != r.pins[rel]:\n';assert s.count(n)==1;open(p,'w').write(s.replace(n,'        if False:\n'))\"" \
    "gotest 'TestMakefileDigestMutations|TestMakeIntegrityGuardFixtures/digest-mismatch|TestCIRequiredGuardMakefilePin/stale'"
  echo "== after restore =="; gotest 'TestMakefileDigestMutations|TestMakeIntegrityGuardFixtures/digest-mismatch|TestCIRequiredGuardMakefilePin/stale'
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
  header "C5 check 11 stops reporting a changed or missing pinned file"
  bash "$M" "C5 check 11 drops mismatches" scripts/ci-required-guard.py \
    "python3 -c \"p='scripts/ci-required-guard.py';s=open(p).read();n='    changed = [p for p in r.problems if p.kind in (\\\"mismatch\\\", \\\"absent\\\")]\n';assert s.count(n)==1;open(p,'w').write(s.replace(n,'    changed = []\n'))\"" \
    "gotest 'TestCIRequiredGuardMakefilePin'"
  echo "== after restore =="; gotest 'TestCIRequiredGuardMakefilePin'
} > "$out/C5-check11-compare-disabled.txt" 2>&1

{
  header "C6 the static read set no longer refuses an unpinned include (scripts/makefile_pin.py)"
  bash "$M" "C6 unpinned include accepted" scripts/makefile_pin.py \
    "python3 -c \"p='scripts/makefile_pin.py';s=open(p).read();n='        if f not in r.pins:\n';assert s.count(n)==1;open(p,'w').write(s.replace(n,'        if False:\n'))\"" \
    "gotest 'TestMakeIntegrityGuardFixtures/include-unpinned|TestMakefileDigestMutations|TestCIRequiredGuardMakefilePin/include-unpinned'"
  echo "== after restore =="; gotest 'TestMakeIntegrityGuardFixtures/include-unpinned|TestMakefileDigestMutations|TestCIRequiredGuardMakefilePin/include-unpinned'
} > "$out/C6-unpinned-include-accepted.txt" 2>&1

{
  header "C8 check 11 ignores every verify_pin problem it has no special wording for (the parity rows)"
  bash "$M" "C8 check-11 parity dropped" scripts/ci-required-guard.py \
    "python3 -c \"p='scripts/ci-required-guard.py';s=open(p).read();n='            g.fail(f\\\"{pin_path.name}: {p.message}\\\", \\\"The workflow anchor refuses this tree before invoking make.\\\")\n';assert s.count(n)==1,s.count(n);open(p,'w').write(s.replace(n,'            continue\n'))\"" \
    "gotest 'TestCIRequiredGuardMakefilePin'"
  echo "== after restore =="; gotest 'TestCIRequiredGuardMakefilePin'
} > "$out/C8-check11-parity-dropped.txt" 2>&1

{
  header "C9 the text checks moved back AFTER make (the 62d16aa order: refused by name, but only after make ran)"
  bash "$M" "C9 text checks after make" scripts/make-integrity-guard.py \
    "python3 -c \"p='scripts/make-integrity-guard.py';s=open(p).read();a='        check_text(g, root, pinned[0], closure, targets)\n';b='        g.ok(f\\\"make\\'s own MAKEFILE_LIST {made} is exactly the pinned set\\\")\n';assert s.count(a)==1 and s.count(b)==1;s=s.replace(a,'');open(p,'w').write(s.replace(b,b+'    check_text(g, root, files, prerequisite_closure(root, files, targets), targets)\n'))\"" \
    "gotest 'TestMakeIntegrityGuardFixtures'"
  echo "== after restore =="; gotest 'TestMakeIntegrityGuardFixtures'
} > "$out/C9-text-checks-after-make.txt" 2>&1

echo "tree after all demonstrations: $(git status --porcelain | wc -l | tr -d ' ') uncommitted path(s)"
