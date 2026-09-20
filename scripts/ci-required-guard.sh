#!/usr/bin/env bash
# ci-required-guard — the guard on the gate.
#
# `ci-required` reads .github/required-checks.txt from the checkout UNDER TEST.
# That means the PR being gated can edit its own gate. A guard that only checks
# "every name present maps to a real job" is defeated by DELETING a line: the
# manifest stays internally consistent, the gate shrinks, and `ci-required`
# stays green.
#
# (Found by the security review of vizra-user PR1, Finding 2:
#  docs/evidence/warroom/2026-09-20-vizra-user-pr1-skeleton-SECURITY.md)
#
# So this guard enforces four things:
#
#   1. FLOOR      — every lane in FLOOR_LANES is PRESENT and NOT optional.
#                   Removing one, or commenting it out, is a red lane with a
#                   named error.
#   2. RESOLVABLE — every name in the manifest maps to a job that exists in a
#                   workflow triggered on pull_request.
#   3. NO OPT-OUT — no required lane carries `continue-on-error` (VZ-CI-001
#                   negative case 1).
#   4. NO EMPTY   — no required lane runs an empty test selection (VZ-CI-001
#                   negative case 2).
#
# CODEOWNERS makes editing the manifest need owner review; this makes editing it
# need a reason.
set -euo pipefail
cd "$(dirname "$0")/.."

MANIFEST=.github/required-checks.txt
WORKFLOWS=.github/workflows

# ---------------------------------------------------------------------------
# 1. THE FLOOR.
#
# These lanes are NOT optional and NOT removable by the PR they gate. Each one
# is here because dropping it would let a whole class of defect merge:
#
#   build-test   the unit suite, the both-direction OpenAPI contract check, the
#                sqlc drift check, migrate-lint and the append-only manifest.
#                Without it nothing is checked at all.
#   cache-matrix the permanent two-image Valkey/Redis-7.2 matrix. ADR-001 Q-004
#                makes it permanent precisely because no upstream guarantees the
#                compatibility it asserts.
#   govulncheck  a known-vulnerable dependency set.
#   docker-build the release image, its digest-pinned base and its loader list.
#
# Adding a lane to the floor is a deliberate widening. REMOVING one requires
# editing this script, which CODEOWNERS also protects — so the removal is
# visible in review instead of hiding in a one-line manifest diff.
# ---------------------------------------------------------------------------
FLOOR_LANES=(
  build-test
  cache-matrix
  govulncheck
  docker-build
)

fail=0
err() { printf '  FAIL  %s\n' "$*" >&2; fail=1; }
ok()  { printf '  ok    %s\n' "$*"; }

printf 'ci-required-guard:\n'

if [ ! -f "$MANIFEST" ]; then
  err "$MANIFEST is missing; ci-required has no gate to enforce"
  exit 1
fi

# Active (uncommented, non-blank) entries. Read with a loop rather than
# `mapfile`, which bash 3.2 (the macOS default) does not have: a guard that only
# runs in CI cannot be demonstrated locally.
required=()
while IFS= read -r line; do
  [ -n "$line" ] && required+=("$line")
done < <(grep -vE '^[[:space:]]*(#|$)' "$MANIFEST" | sed 's/[[:space:]]*$//')

if [ ${#required[@]} -eq 0 ]; then
  err "$MANIFEST lists no checks; ci-required would pass with nothing verified"
  exit 1
fi

# --- 1. floor ---------------------------------------------------------------
for lane in "${FLOOR_LANES[@]}"; do
  found=0
  for r in "${required[@]}"; do
    [ "$r" = "$lane" ] && found=1
  done
  if [ $found -eq 0 ]; then
    if grep -qE "^\s*#.*\b${lane}\b" "$MANIFEST"; then
      err "required lane '${lane}' is COMMENTED OUT in $MANIFEST."
      printf "        It is a floor lane: it cannot be made optional by the pull request it gates.\n" >&2
    else
      err "required lane '${lane}' is MISSING from $MANIFEST."
      printf "        It is a floor lane (scripts/ci-required-guard.sh, FLOOR_LANES).\n" >&2
      printf "        Deleting a manifest line does not shrink the gate; it turns this lane red.\n" >&2
    fi
  fi
done
[ $fail -eq 0 ] && ok "all ${#FLOOR_LANES[@]} floor lane(s) are present and non-optional"

# --- 2. every required name resolves to a real job --------------------------
for name in "${required[@]}"; do
  # A job whose `name:` is the check name, or whose job KEY is, when it has no
  # explicit name. GitHub reports the display name when one is set.
  if grep -rqE "^\s{2}${name}:\s*$" "$WORKFLOWS"/*.yml 2>/dev/null \
     || grep -rqE "^\s+name:\s*['\"]?${name}['\"]?\s*$" "$WORKFLOWS"/*.yml 2>/dev/null; then
    ok "required check '${name}' resolves to a job"
  else
    err "required check '${name}' matches no job in $WORKFLOWS."
    printf "        ci-required would wait forever for a check nothing produces.\n" >&2
  fi
done

# --- 3. no continue-on-error on a required lane -----------------------------
# VZ-CI-001 negative case: a lane that cannot fail is not a gate.
if grep -rnE '^\s*continue-on-error:\s*true' "$WORKFLOWS"/*.yml >/dev/null 2>&1; then
  printf '  FAIL  continue-on-error: true appears in a workflow:\n' >&2
  grep -rnE '^\s*continue-on-error:\s*true' "$WORKFLOWS"/*.yml | sed 's/^/          /' >&2
  printf '        A required lane that cannot fail is not a gate.\n' >&2
  fail=1
else
  ok "no continue-on-error on any lane"
fi

# --- 4. no empty test selection ---------------------------------------------
# VZ-CI-001 negative case: `go test` with no packages, or a -run pattern that
# matches nothing, reports success while running nothing.
if grep -rnE 'go test( -[^ ]+)* *$' "$WORKFLOWS"/*.yml >/dev/null 2>&1; then
  printf '  FAIL  a workflow runs `go test` with no package selection:\n' >&2
  grep -rnE 'go test( -[^ ]+)* *$' "$WORKFLOWS"/*.yml | sed 's/^/          /' >&2
  fail=1
else
  ok "no lane runs an empty test selection"
fi

# --- 5. GitHub-hosted ubuntu-24.04 only, and actions pinned by SHA ----------
if grep -rnE '^\s*runs-on:' "$WORKFLOWS"/*.yml | grep -vqE 'ubuntu-24\.04'; then
  printf '  FAIL  a job runs on a runner other than ubuntu-24.04:\n' >&2
  grep -rnE '^\s*runs-on:' "$WORKFLOWS"/*.yml | grep -vE 'ubuntu-24\.04' | sed 's/^/          /' >&2
  fail=1
else
  ok "every job runs on GitHub-hosted ubuntu-24.04"
fi

unpinned=$(grep -rnE '^\s*-?\s*uses:\s*[^./]' "$WORKFLOWS"/*.yml | grep -vE 'uses:\s*[^@]+@[0-9a-f]{40}' || true)
if [ -n "$unpinned" ]; then
  printf '  FAIL  action(s) not pinned to a 40-character commit SHA:\n' >&2
  printf '%s\n' "$unpinned" | sed 's/^/          /' >&2
  printf '        A moving tag is a supply-chain hole: the tag can be repointed.\n' >&2
  fail=1
else
  ok "every third-party action is pinned to a commit SHA"
fi

if [ $fail -ne 0 ]; then
  printf 'ci-required-guard: FAILED\n' >&2
  exit 1
fi
printf 'ci-required-guard: passed (%d required check(s))\n' "${#required[@]}"
