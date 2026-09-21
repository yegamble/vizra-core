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
CLI=cmd/vizra/claimtoken.go
CFG=internal/config/config.go
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
  perl -0pi -e 's/\ttx, err := pool\.BeginTx\(ctx, pgx\.TxOptions\{IsoLevel: pgx\.ReadCommitted\}\)\n\tif err != nil \{\n\t\treturn Result\{\}, unavailable\("beginning claim", err\)/\ttx, err := pool.Begin(ctx)\n\tif err != nil {\n\t\treturn Result{}, unavailable("beginning claim", err)/' "$SVC"

# --- the token ---------------------------------------------------------------
run_case MUT-3 "store the raw token instead of its SHA-256 digest" \
  "$SVC" 'TestClaimTokenIsStoredOnlyAsASHA256Digest' "$INT" \
  perl -0pi -e 's/sum := sha256\.Sum256\(\[\]byte\(normalized\)\)\n\treturn sum\[:\]/return []byte(normalized + strings.Repeat("\\x00", 32-len(normalized)%32))[:32]/' "$SVC"


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

# --- fix round 1: F1-F5, N-1..N-6 -------------------------------------------

run_case MUT-17 "delete the users_one_owner case from the error mapper" \
  "$HND" 'TestClaimErrorMapping' "$UNI_API" \
  perl -0pi -e 's/case pgErr\.Code == "23505" && pgErr\.ConstraintName == "users_one_owner":/case false:/' "$HND"

run_case MUT-11c "restore the per-request already_claimed audit row" \
  "$HND" 'TestRepeatedClaimsOnAClaimedInstanceDoNotGrowTheAuditTrail' "$INT" \
  perl -0pi -e 's/\tif claimed, fresh := s\.claimed\.get\(s\.deps\.Now\(\)\); fresh && claimed \{\n\t\treturn newCodedError\(http\.StatusConflict, "conflict", claimedMessage\)\n\t\}\n//' "$HND"

run_case MUT-11d "audit every already-claimed refusal again" \
  "$HND" 'TestRepeatedClaimsOnAClaimedInstanceDoNotGrowTheAuditTrail' "$INT" \
  bash -c "perl -0pi -e 's/\tif claimed, fresh := s\\.claimed\\.get\\(s\\.deps\\.Now\\(\\)\\); fresh && claimed \\{\\n\\t\\treturn newCodedError\\(http\\.StatusConflict, \\\"conflict\\\", claimedMessage\\)\\n\\t\\}\\n//' $HND && perl -0pi -e 's/\t\ts\\.claimed\\.set\\(true, s\\.deps\\.Now\\(\\)\\)\\n\\t\\treturn newCodedError\\(http\\.StatusConflict, \\\"conflict\\\", claimedMessage\\)/\t\ts.recordClaimRefusal(c, audit.ReasonAlreadyClaimed)\\n\\t\\treturn newCodedError(http.StatusConflict, \\\"conflict\\\", claimedMessage)/' $HND"

run_case MUT-29 "hash inside the claim transaction again" \
  "$SVC" 'TestNoConnectionIsHeldWhileHashing' "$INT" \
  perl -0pi -e 's/\thash, err := hasher\.Hash\(ctx, in\.Password\)\n\tif err != nil \{\n\t\treturn Result\{\}, err\n\t\}\n\n\tuserID/\tuserID/; s/\tcreated, err := qtx\.ClaimOwner\(ctx, sqlcgen\.ClaimOwnerParams\{/\thash, err := hasher.Hash(ctx, in.Password)\n\tif err != nil {\n\t\treturn Result{}, err\n\t}\n\tcreated, err := qtx.ClaimOwner(ctx, sqlcgen.ClaimOwnerParams{/' "$SVC"

run_case MUT-31 "remove the liveness pre-check before hashing" \
  "$SVC" 'TestACorrectButDeadTokenCostsNoDerivation' "$INT" \
  perl -0pi -e 's/\tif row\.Live == nil \|\| !\*row\.Live \{\n\t\treturn Result\{\}, ErrTokenNotAccepted\n\t\}\n//' "$SVC"

run_case MUT-14b "move the liveness decision back outside the advisory lock" \
  "$SVC" 'TestConcurrentBootsMintExactlyOneToken' "$INT" \
  perl -0pi -e 's/\tif onlyIfNoLiveToken && prior\.Live \{\n\t\treturn "", prior\.Generation, ErrLiveTokenExists\n\t\}\n//' "$SVC"

EXTRA_RESTORE="internal/store/sqlcgen/"
run_case MUT-32 "drop the users guard from the mint statement" \
  "$QRY" 'TestMintIsRefusedByTheDatabaseOnAClaimedInstance' "$INT" \
  bash -c "perl -0pi -e 's/\n WHERE NOT EXISTS \(SELECT 1 FROM users\)//' $QRY && sqlc generate"
EXTRA_RESTORE=""

run_case MUT-28 "delete the ErrUnavailable branch from the error mapper" \
  "$HND" 'TestClaimErrorMapping' "$UNI_API" \
  perl -0pi -e 's/\tcase errors\.Is\(err, ownerclaim\.ErrUnavailable\):/\tcase false:/' "$HND"

run_case MUT-30 "stamp minted_at from the application clock" \
  "$MIG" 'TestMintTimestampsComeFromTheDatabaseClock' "$INT" \
  perl -0pi -e "s/    minted_at     timestamptz NOT NULL DEFAULT now\(\),/    minted_at     timestamptz NOT NULL DEFAULT (now() - interval '3 seconds'),/" "$MIG"

run_case MUT-33 "serve claim-status from the uncached path again" \
  "$HND" 'TestClaimStatusIsServedFromTheMonotonicCacheOnceClaimed' "$INT" \
  perl -0pi -e 's/\tclaimed, err := s\.instanceClaimed\(c\)\n\tif err != nil \{\n\t\treturn newCodedError\(http\.StatusServiceUnavailable, "unavailable",\n\t\t\t"the instance state could not be read"\)\n\t\}\n\ts\.claimed\.set/\tclaimed, err := s.lookupClaimed(c)\n\tif err != nil {\n\t\treturn newCodedError(http.StatusServiceUnavailable, "unavailable",\n\t\t\t"the instance state could not be read")\n\t}\n\ts.claimed.set/' "$HND"

run_case MUT-34 "take the hard ceiling off the claim-status route" \
  "$HND" 'TestClaimStatusIsBoundedByTheHardCeiling' "$INT" \
  perl -0pi -e 's/\tif !s\.allowClaimRequest\(c\) \{\n\t\treturn newCodedError\(http\.StatusTooManyRequests, "rate_limited",\n\t\t\t"too many requests to the setup endpoint; try again shortly"\)\n\t\}\n\t\/\/ instanceClaimed, not lookupClaimed/\t\/\/ instanceClaimed, not lookupClaimed/' "$HND"

run_case MUT-35 "compare origins as raw strings again" \
  "$HND" 'TestOriginsAreComparedNormalisedNotAsStrings' "$INT" \
  perl -0pi -e 's/\t\tconfig\.NormalizeOrigin\(origin\) != config\.NormalizeOrigin\(want\) \{/\t\torigin != want {/' "$HND"

# --- verifier findings V1-V10 ----------------------------------------------

run_case MUT-37 "print the claim token to stderr as well as stdout" \
  "$CLI" 'TestClaimTokenCLIOnAnUnclaimedInstance' "$INT" \
  perl -0pi -e 's/\tfmt\.Println\(raw\)/\tfmt.Fprintln(os.Stderr, raw)\n\tfmt.Println(raw)/' "$CLI"

run_case MUT-38 "loosen credentials_password_is_argon2id to accept anything" \
  "$MIG" 'TestTheCredentialsCheckRefusesEveryNonArgon2idSecret' "$INT" \
  perl -0pi -e "s/kind <> 'password' OR secret LIKE '\\\$argon2id\\\$%'/kind <> 'password' OR secret LIKE '%'/" "$MIG"

run_case MUT-39 "audit every rate-limited request instead of the transition" \
  "$HND" 'TestARateLimitedClaimWritesNoAuditRow' "$INT" \
  perl -0pi -e 's/\t\tif s\.claimLimitTransition\(c\) \{\n\t\t\ts\.recordClaimRateLimited\(c, "failure"\)\n\t\t\}/\t\ts.recordClaimRateLimited(c, "failure")/' "$HND"

run_case MUT-40 "normalise the configured origin but not the request's" \
  "$CFG" 'TestLoadStoresTheNormalisedPublicOrigin' "./internal/config/" \
  perl -0pi -e 's/\t\tc\.PublicOrigin = n\n/\t\t_ = n\n/' "$CFG"

rule "REVIEW-ONLY PROPERTIES (no mutation here turns a test red)"
cat <<'NOTE'
Stated rather than implied, because a mutation matrix that quietly omits these
would overclaim:

  MUT-4  replacing subtle.ConstantTimeCompare with bytes.Equal. Behaviourally
         identical; a timing difference is not observable from a Go test.

  MUT-4b passing the PRESENTED digest rather than the row's own digest to the
         redeem statement. Identical results today; the property is defence
         against a future index-probing change. Code review owns it.

  MUT-36 flipping `refuseIfUsersExist` to false in `vizra claim-token` — the S-12
         escalation path the verifier found unobserved. MEASURED: it no longer
         reddens anything, and the reason is the fix N-6/F-2 asked for.
         `MintOwnerClaimToken` now carries
         `WHERE NOT EXISTS (SELECT 1 FROM users)`, so the STATEMENT refuses the
         mint and the CLI still exits non-zero with nothing minted. The scored
         proof of that guard is MUT-32, whose test
         (TestMintIsRefusedByTheDatabaseOnAClaimedInstance) calls the generated
         query directly, bypassing every Go-side check. The Go flag is now
         redundancy in front of a database guarantee — which is the right way
         round, and better than the first round where neither existed.

  MUT-6  dropping `expires_at > now()` from the redeem CTE. MEASURED: since the
         liveness pre-check landed (MUT-31), a correct-but-EXPIRED token is
         refused in Go before the CTE is reached, so removing the CTE predicate
         no longer reddens anything. The CTE guard remains the enforcement of
         record — the pre-check is an optimisation in front of it, and a race
         that expires a token between the two is still caught by the CTE. Both
         are kept; only one is observable, and this says which.

  MUT-14 re-minting unconditionally at boot. The decision it targeted MOVED into
         Mint, under the advisory lock, which is the whole point of the fix —
         MUT-14b mutates it in its new home and reddens
         TestConcurrentBootsMintExactlyOneToken.

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
         there is nothing for a test to observe. The index is proven by MUT-2b,
         by TestUsersOneOwnerFiresThroughTheHandler (which forces 23505 through
         the real handler with an uncommitted concurrent insert), and the mapper
         by MUT-17 — which only reddens because TestClaimErrorMapping pins the
         MESSAGE: both 23505 branches answer 409/conflict, so a status-and-code
         assertion alone left that mutation green. The row guard's own
         contribution is redundancy, and saying so is worth more than a test
         that pretends.
NOTE

rule "SUMMARY"
echo "passed:       $PASS"
echo "failed:       $FAIL"
echo "harness-fail: $HARNESS"
[ "$FAIL" -eq 0 ] && [ "$HARNESS" -eq 0 ]
