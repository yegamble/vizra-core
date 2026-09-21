#!/usr/bin/env bash
# mutate.sh — the red/green demonstration harness for hardening sweep B1.
#
# It exists because "I mutated X and the check went red" is worth nothing
# unless the mutation provably APPLIED and the restore provably put the bytes
# back. So:
#
#   1. record sha256 of the target file BEFORE
#   2. apply the mutation (a `python3 -c` / `sed` expression given as $MUTATE)
#   3. ABORT if the digest did not move — a mutation that did not apply would
#      otherwise be reported as "the check caught it" when the check saw the
#      unmutated tree
#   4. run the check, record its exit code and output
#   5. restore from a BYTE COPY taken in step 1 — never `git checkout --`,
#      which restores from the INDEX and therefore silently discards
#      uncommitted work in the same file (it destroyed this branch's own
#      workflow edits on the first run of this script)
#   6. ABORT if the restored digest is not byte-identical to the BEFORE digest
#
# Usage:  mutate.sh <label> <file> <mutate-cmd> <check-cmd>
set -uo pipefail

label="$1"; file="$2"; mutate="$3"; check="$4"

digest() { shasum -a 256 "$1" 2>/dev/null | awk '{print $1}'; }

echo "=============================================================="
echo "MUTATION: $label"
echo "  file:   $file"
echo "  mutate: $mutate"
echo "  check:  $check"
echo "--------------------------------------------------------------"

before="$(digest "$file")"
backup="$(mktemp)"
cp -p "$file" "$backup"
echo "  sha256 BEFORE: $before"

bash -c "$mutate"
after="$(digest "$file")"
echo "  sha256 AFTER:  $after"
if [ "$before" = "$after" ]; then
  echo "  ABORT: the mutation did not change the file. Nothing below would mean anything."
  exit 90
fi

set +e
out="$(bash -c "$check" 2>&1)"
rc=$?
set -e
echo "  CHECK EXIT: $rc"
echo "--- check output (last 40 lines) ---"
printf '%s\n' "$out" | tail -40
echo "------------------------------------"

cp -p "$backup" "$file"
rm -f "$backup"
restored="$(digest "$file")"
echo "  sha256 RESTORED: $restored"
if [ "$restored" != "$before" ]; then
  echo "  ABORT: restore is NOT byte-identical. Working tree is dirty."
  exit 91
fi
echo "  restore byte-identical: yes"
echo "  RESULT: $label -> exit $rc"
echo
exit "$rc"
