#!/usr/bin/env bash
# VZ-INSTALL-003 (M1-A) mutation demonstrations.
#
# Each acceptance-bearing check is shown RED against ONE controlled mutation and
# GREEN when that mutation is reverted.
#
#     export VIZRA_TEST_DATABASE_URL=postgres://...
#     export VIZRA_TEST_CACHE_URL=redis://...
#     ./docs/evidence/m1a-owner-claim/demonstrate.sh            # all cases
#     ./docs/evidence/m1a-owner-claim/demonstrate.sh MUT-13     # one case
#
# The harness REFUSES TO SCORE A MUTATION THAT DID NOT APPLY. Every case records
# the sha256 of the mutated file before and after; if they are equal the case is
# reported HARNESS-FAIL, never PASS. Without that, a `sed` whose pattern stopped
# matching would silently turn into "the test passed unmutated", which is the
# exact false-positive this evidence exists to rule out.
#
# It mutates tracked files and restores them with `git checkout --`, so it
# refuses to run with a dirty tree.
set -uo pipefail
cd "$(dirname "$0")/../../.."

if [ -n "$(git status --porcelain)" ]; then
  echo "refusing to run: the working tree is dirty and this script restores files with 'git checkout --'." >&2
  git status --porcelain >&2
  exit 2
fi
for v in VIZRA_TEST_DATABASE_URL VIZRA_TEST_CACHE_URL; do
  if [ -z "${!v:-}" ]; then
    echo "$v is unset. This lane is BLOCKED, not passed." >&2
    exit 2
  fi
done

ONLY="${1:-}"
PASS=0; FAIL=0; HARNESS=0

# The full digest is compared internally; only a short labelled prefix is
# PRINTED. A bare 64-hex string is credential-shaped — these are digests of
# tracked repository files and secret in no sense, but a secret scanner cannot
# tell that apart from a claim token, and neither can a reviewer skimming the
# transcript. 16 hex characters is ample to show a file changed and was restored.
digest() { shasum -a 256 "$1" 2>/dev/null | awk '{print $1}' || sha256sum "$1" | awk '{print $1}'; }
short() { printf '%.16s…(sha256 of a tracked file, truncated)' "$1"; }
rule() { printf '\n========================================================================\n%s\n========================================================================\n' "$1"; }

# EXTRA_RESTORE lets a case name files the mutator changes INDIRECTLY — notably
# the sqlc-generated Go, which `sqlc generate` rewrites from a mutated .sql.
# Restoring only the file the case names left the generated code mutated, which
# made the following GREEN run fail for a reason that had nothing to do with the
# case. The harness found that defect in itself, which is the point of the
# before/after digest gate.
EXTRA_RESTORE=""

# case <id> <description> <file> <test-regex> <package> <mutator...>
run_case() {
  local id="$1" desc="$2" file="$3" regex="$4" pkg="$5"; shift 5
  if [ -n "$ONLY" ] && [ "$ONLY" != "$id" ]; then return 0; fi

  rule "$id — $desc"
  echo "mutates: $file"
  echo "must turn RED: $regex  ($pkg)"

  local before after
  before="$(digest "$file")"
  "$@"
  after="$(digest "$file")"
  echo "file sha256 before: $(short "$before")"
  echo "file sha256 after:  $(short "$after")"

  if [ "$before" = "$after" ]; then
    echo "HARNESS-FAIL: the mutation did not change $file (digest identical). Scoring nothing."
    HARNESS=$((HARNESS+1))
    git checkout -- "$file" ${EXTRA_RESTORE}
    return 0
  fi

  echo "--- RED run ---"
  go test -tags=integration -count=1 -run "$regex" "$pkg" 2>&1 | tail -20
  local red=${PIPESTATUS[0]}
  echo "RED exit=$red (expected non-zero)"

  git checkout -- "$file" ${EXTRA_RESTORE}
  if [ "$(digest "$file")" != "$before" ]; then
    echo "HARNESS-FAIL: $file was not restored."
    HARNESS=$((HARNESS+1)); return 0
  fi
  if [ -n "$(git status --porcelain)" ]; then
    echo "HARNESS-FAIL: the tree is still dirty after restoring; a later case would score a polluted run:"
    git status --porcelain
    git checkout -- .
    HARNESS=$((HARNESS+1)); return 0
  fi

  echo "--- GREEN run (mutation reverted) ---"
  go test -tags=integration -count=1 -run "$regex" "$pkg" 2>&1 | tail -6
  local green=${PIPESTATUS[0]}
  echo "GREEN exit=$green (expected zero)"

  if [ "$red" -ne 0 ] && [ "$green" -eq 0 ]; then
    echo "RESULT: $id PASS (red under mutation, green when restored)"
    PASS=$((PASS+1))
  else
    echo "RESULT: $id FAIL (red=$red green=$green)"
    FAIL=$((FAIL+1))
  fi
}

echo "host:   $(uname -s)/$(uname -m)"
echo "go:     $(go version)"
echo "HEAD:   $(git rev-parse HEAD)"
echo "date:   $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "pg:     $(psql "$VIZRA_TEST_DATABASE_URL" -tAc 'select version()' 2>/dev/null | head -1)"

MIG=migrations/0005_users_credentials_owner_claim.up.sql
QRY=store/queries/owner_claim.sql
SVC=internal/ownerclaim/ownerclaim.go
ANN=internal/ownerclaim/announce.go
HND=internal/httpapi/setup.go
LIM=internal/httpapi/setup_limits.go
AUD=internal/audit/audit.go
KEY=internal/config/keys.go
INT=./internal/integration/
UNI_AUD=./internal/audit/
UNI_API=./internal/httpapi/

# --- the race and the constraint -------------------------------------------
# A re-mint must invalidate the previous token. That is what actually makes
# `vizra claim-token` a safe recovery path: the operator can re-mint freely
# knowing the token they lost stops working.
EXTRA_RESTORE="internal/store/sqlcgen/"
run_case MUT-1 "let a re-mint KEEP the previous token digest" \
  "$QRY" 'TestOwnerClaimRejectsASupersededOrExpiredToken' "$INT" \
  bash -c "perl -0pi -e 's/SET token_sha256  = EXCLUDED\.token_sha256,/SET token_sha256  = owner_claim_tokens.token_sha256,/' $QRY && sqlc generate"
EXTRA_RESTORE=""

run_case MUT-2 "remove 'AND tombstoned_at IS NULL' from users_one_owner" \
  "$MIG" 'TestATombstonedOwnerDoesNotPermanentlyBlockOwnership' "$INT" \
  perl -0pi -e "s/WHERE role = 'owner' AND tombstoned_at IS NULL;/WHERE role = 'owner';/" "$MIG"

run_case MUT-2b "drop the users_one_owner index entirely" \
  "$MIG" 'TestASecondLiveOwnerIsRefusedByTheDatabase' "$INT" \
  perl -0pi -e "s/CREATE UNIQUE INDEX users_one_owner ON users \(role\)\n    WHERE role = 'owner' AND tombstoned_at IS NULL;/-- index removed by the mutation harness/" "$MIG"

run_case MUT-16 "drop the explicit ReadCommitted TxOptions from the claim" \
  "$SVC" 'TestOwnerClaimRaceYieldsExactlyOneOwnerUnderEveryServerDefaultIsolation' "$INT" \
  perl -0pi -e 's/tx, err := pool\.BeginTx\(ctx, pgx\.TxOptions\{IsoLevel: pgx\.ReadCommitted\}\)\n\tif err != nil \{\n\t\treturn Result\{\}, fmt\.Errorf\("ownerclaim: beginning claim/tx, err := pool.Begin(ctx)\n\tif err != nil {\n\t\treturn Result{}, fmt.Errorf("ownerclaim: beginning claim/' "$SVC"

# --- the token ---------------------------------------------------------------
run_case MUT-3 "store the raw token instead of its SHA-256 digest" \
  "$SVC" 'TestClaimTokenIsStoredOnlyAsASHA256Digest' "$INT" \
  perl -0pi -e 's/sum := sha256\.Sum256\(\[\]byte\(normalized\)\)\n\treturn sum\[:\]/return []byte(normalized + strings.Repeat("\\x00", 32-len(normalized)%32))[:32]/' "$SVC"

EXTRA_RESTORE="internal/store/sqlcgen/"
run_case MUT-6 "remove the expires_at predicate from the redeem" \
  "$QRY" 'TestOwnerClaimRejectsASupersededOrExpiredToken' "$INT" \
  bash -c "perl -0pi -e 's/\n       AND expires_at    > now\(\)//' $QRY && sqlc generate"
EXTRA_RESTORE=""

run_case MUT-14 "re-mint unconditionally at boot" \
  "$ANN" 'TestARestartDoesNotInvalidateALiveToken' "$INT" \
  perl -0pi -e 's/\tif state\.Live \{\n\t\tannounceCommand\(w, state\)\n\t\treturn BootOutcome\{Generation: state\.Generation\}, nil\n\t\}\n//' "$ANN"

run_case MUT-5b "flip the announce default to stderr" \
  "$KEY" 'TestTheAnnounceDefaultIsOff' "$UNI_API" \
  perl -0pi -e 's/\{Name: "VIZRA_OWNER_CLAIM_ANNOUNCE", Default: "off"/{Name: "VIZRA_OWNER_CLAIM_ANNOUNCE", Default: "stderr"/' "$KEY"

# --- the audit table ---------------------------------------------------------
run_case MUT-9 "drop the UPDATE/DELETE immutability trigger" \
  "$MIG" 'TestAuditEventsCannotBeUpdatedOrDeleted' "$INT" \
  perl -0pi -e 's/CREATE TRIGGER audit_events_no_update_or_delete\n    BEFORE UPDATE OR DELETE ON audit_events\n    FOR EACH ROW EXECUTE FUNCTION audit_events_append_only\(\);/-- trigger removed by the mutation harness/' "$MIG"

run_case MUT-9b "drop the statement-level TRUNCATE trigger" \
  "$MIG" 'TestAuditEventsCannotBeTruncated' "$INT" \
  perl -0pi -e 's/^CREATE TRIGGER audit_events_refuse_truncation BEFORE TRUNCATE.*$/-- trigger removed by the mutation harness/m' "$MIG"

run_case MUT-19 "change the audit actor FK to NO ACTION" \
  "$MIG" 'TestAUserWithAuditRowsCannotBeDeleted' "$INT" \
  perl -0pi -e 's/FOREIGN KEY \(actor_user_id\) REFERENCES users \(id\) ON DELETE RESTRICT;/FOREIGN KEY (actor_user_id) REFERENCES users (id) ON DELETE CASCADE;/' "$MIG"

run_case MUT-11b "write an audit row for every refusal, including rate-limited ones" \
  "$HND" 'TestARateLimitedClaimWritesNoAuditRow' "$INT" \
  perl -0pi -e 's/\t\tif s\.claimLimitTransition\(c\) \{\n\t\t\ts\.recordClaimRateLimited\(c, "failure"\)\n\t\t\}/\t\ts.recordClaimRefusal(c, audit.ReasonTokenNotAccepted)/' "$HND"

# --- the ip_prefix writer ----------------------------------------------------
run_case MUT-10 "mask IPv4 to /32 instead of /24" \
  "$AUD" 'TestIPPrefixWriterOutputAlwaysSatisfiesTheFrozenCheck|TestIPPrefixIsNeverFinerThanTheContract' "$UNI_AUD" \
  perl -0pi -e 's/bits, re = 24, ipv4PrefixRe/bits, re = 32, ipv4PrefixRe/' "$AUD"

run_case MUT-10b "remove the output-grammar validation from the ip_prefix writer" \
  "$AUD" 'TestIPPrefixWriterOutputAlwaysSatisfiesTheFrozenCheck' "$UNI_AUD" \
  perl -0pi -e 's/\tif !addr\.IsValid\(\) \|\| addr\.IsUnspecified\(\) \|\| addr\.IsLoopback\(\) \{\n\t\treturn nil\n\t\}\n//' "$AUD"

run_case MUT-24 "let the proxy's own prefix be written as if it were the client" \
  "$LIM" 'TestForwardedHeaderWithoutTrustedProxyYieldsNullIPPrefix' "$INT" \
  perl -0pi -e 's/\tif req\.Header\.Get\("X-Forwarded-For"\) != "" \|\| req\.Header\.Get\("Forwarded"\) != "" \{\n\t\treturn nil\n\t\}\n//' "$LIM"

# --- the request posture and hashing order -----------------------------------
run_case MUT-13 "hash the password BEFORE comparing the claim token" \
  "$SVC" 'TestNoPasswordHashingOccursWithoutAValidToken' "$INT" \
  perl -0pi -e 's/\trow, err := q\.GetOwnerClaimToken\(ctx\)/\tif _, herr := hasher.Hash(ctx, in.Password); herr != nil {\n\t\treturn Result{}, herr\n\t}\n\trow, err := q.GetOwnerClaimToken(ctx)/' "$SVC"

run_case MUT-18 "accept any content type (drop the JSON media-type check)" \
  "$HND" 'TestClaimRefusesANonJSONContentType' "$INT" \
  perl -0pi -e 's/\tif err := requireJSONContentType\(req\); err != nil \{\n\t\treturn err\n\t\}\n//' "$HND"

run_case MUT-8 "remove the MaxBytesReader body bound" \
  "$HND" 'TestClaimBodyLimitAppliesToAChunkedRequest' "$INT" \
  perl -0pi -e 's/body := http\.MaxBytesReader\(c\.Response\(\), req\.Body, claimBodyLimitBytes\)/body := req.Body/' "$HND"

run_case MUT-25 "allow unknown JSON fields (drop DisallowUnknownFields)" \
  "$HND" 'TestClaimRefusesAnUnknownField' "$INT" \
  perl -0pi -e 's/\tdec\.DisallowUnknownFields\(\)\n//' "$HND"

run_case MUT-26 "drop the Origin / Sec-Fetch-Site posture check" \
  "$HND" 'TestClaimRefusesACrossOriginRequest' "$INT" \
  perl -0pi -e 's/\tif err := s\.checkOrigin\(req\); err != nil \{\n\t\treturn err\n\t\}\n//' "$HND"

run_case MUT-15 "spend failure budget on success too" \
  "$HND" 'TestAValidTokenIsNeverRateLimitedByTheFailureLimiter' "$INT" \
  perl -0pi -e 's/\tpool, err := s\.poolFor\(c\)\n\tif err != nil \{\n\t\treturn newCodedError\(http\.StatusServiceUnavailable, "unavailable",\n\t\t\t"the instance state could not be read"\)\n\t\}\n\n\tresult, err := ownerclaim\.Claim/\tpool, err := s.poolFor(c)\n\tif err != nil {\n\t\treturn newCodedError(http.StatusServiceUnavailable, "unavailable",\n\t\t\t"the instance state could not be read")\n\t}\n\tif s.consumeClaimFailure(c) {\n\t\treturn newCodedError(http.StatusTooManyRequests, "rate_limited", "too many attempts")\n\t}\n\n\tresult, err := ownerclaim.Claim/' "$HND"

# --- the structural unclaimed guard ------------------------------------------
run_case MUT-12 "add a route without classifying it" \
  internal/httpapi/server.go 'TestEveryRouteIsEitherUnclaimedAllowlistedOrGuarded' "$UNI_API" \
  perl -0pi -e 's/\tv1\.GET\("\/setup\/claim-status", s\.handleClaimStatus\)/\tv1.GET("\/setup\/claim-status", s.handleClaimStatus)\n\tv1.POST("\/auth\/register", func(c *echo.Context) error { return nil })/' internal/httpapi/server.go

run_case MUT-27 "let the unclaimed guard fall through on a lookup error" \
  "$HND" 'TestClaimGuardReturns503WhenTheStateCannotBeRead' "$UNI_API" \
  perl -0pi -e 's/claimed, err := s\.instanceClaimed\(c\)\n\t\t\tif err != nil \{/claimed, err := s.instanceClaimed(c)\n\t\t\tif false \&\& err != nil {/' "$HND"

rule "REVIEW-ONLY PROPERTIES (no mutation here turns a test red)"
cat <<'NOTE'
Stated rather than implied, because a mutation matrix that quietly omits these
would overclaim:

  MUT-4  replacing subtle.ConstantTimeCompare with bytes.Equal. Behaviourally
         identical; a timing difference is not observable from a Go test.

  MUT-4b passing the PRESENTED digest rather than the row's own digest to the
         redeem statement. Identical results today; the property is defence
         against a future index-probing change. Code review owns it.

  MUT-1c dropping `superseded_at IS NULL` from the redeem CTE. MEASURED: no
         test goes red, and the reason is structural rather than a gap. A
         re-mint UPSERTS a new digest over the old row, so a token that was
         superseded-and-replaced no longer matches on its digest and never
         reaches this predicate. The only path that supersedes WITHOUT
         re-minting is boot on an instance that already has accounts — where
         the claimed check answers 409 before any token is examined. The
         predicate is a correct guard for a path that does not exist yet.

  MUT-1b dropping ONLY `consumed_at IS NULL` from the redeem CTE. MEASURED, not
         assumed: the race test stays GREEN. This is defence in depth working
         rather than a gap — with the row guard gone, every losing claimant
         raises 23505 on users_one_owner instead of matching no row, and the
         error mapper turns that into the same 409 the guard would have
         produced. Exactly one owner still exists and no 5xx is returned, so
         there is nothing for a test to observe. The index is proven by MUT-2b
         and the mapper by MUT-17; the row guard's own contribution is
         redundancy, and saying so is worth more than a test that pretends.
NOTE

rule "SUMMARY"
echo "passed:       $PASS"
echo "failed:       $FAIL"
echo "harness-fail: $HARNESS"
[ "$FAIL" -eq 0 ] && [ "$HARNESS" -eq 0 ]
