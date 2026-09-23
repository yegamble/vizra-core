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
  "$SVC" 'TestTheClaimTransactionPinsReadCommitted' "$INT" \
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

# The same mutation as MUT-17, scored END TO END: the unit table proves the
# mapper; this proves a real 23505 on users_one_owner reaches the operator as
# the owner-conflict message rather than as "that username is already taken".
run_case MUT-17b "delete the users_one_owner case from the error mapper (through the handler)" \
  "$HND" 'TestUsersOneOwnerFiresThroughTheHandler' "$INT" \
  perl -0pi -e 's/case pgErr\.Code == "23505" && pgErr\.ConstraintName == "users_one_owner":/case false:/' "$HND"

run_case MUT-11c "delete the handler's claimed short-circuit (every POST reaches the database again)" \
  "$HND" 'TestRepeatedClaimsOnAClaimedInstanceDoNotGrowTheAuditTrail' "$INT" \
  perl -0pi -e 's/\tclaimed, err := s\.instanceClaimed\(c\)\n\tif err != nil \{\n\t\treturn s\.unavailable\(c, "checking claimed state", err\)\n\t\}\n\tif claimed \{\n\t\treturn newCodedError\(http\.StatusConflict, "conflict", claimedMessage\)\n\t\}\n//' "$HND"

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
  perl -0pi -e 's/\tif !s\.allowSetupRequest\(c, "ceiling\.status", claimStatusCeiling\) \{\n\t\treturn newCodedError\(http\.StatusTooManyRequests, "rate_limited",\n\t\t\t"too many requests to the setup endpoint; try again shortly"\)\n\t\}\n//' "$HND"

run_case MUT-35 "compare origins as raw strings again" \
  "$HND" 'TestOriginsAreComparedNormalisedNotAsStrings' "$INT" \
  perl -0pi -e 's/gotN, wantN := config\.NormalizeOrigin\(origin\), config\.NormalizeOrigin\(want\)/gotN, wantN := origin, want/' "$HND"

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

# --- fix round 2: the regression round 1 introduced, and its neighbours ------

run_case MUT-41 "collapse the two setup ceilings back into one shared bucket" \
  "$HND" 'TestTheTwoSetupRoutesDoNotShareAHardCeilingBucket' "$UNI_API" \
  bash -c "perl -0pi -e 's/s\.allowSetupRequest\(c, \"ceiling\.status\", claimStatusCeiling\)/s.allowSetupRequest(c, \"ceiling\", claimOwnerCeiling)/' $HND && perl -0pi -e 's/s\.allowSetupRequest\(c, \"ceiling\.claim\", claimOwnerCeiling\)/s.allowSetupRequest(c, \"ceiling\", claimOwnerCeiling)/' $HND"

run_case MUT-42 "declare no 429 on getSetupClaimStatus" \
  api/openapi.yaml 'TestEveryStatusTheContractDeclaresIsProducedAndNoOtherIs' "$UNI_API" \
  perl -0pi -e 's/        "429":\n          description: \|\n            Too many requests to this route\. It carries its own hard ceiling,\n            separate from the claim endpoint.s, so a flood here can never spend\n            the budget a claim needs\.\n          content:\n            application\/json:\n              schema:\n                \$ref: "#\/components\/schemas\/Error"\n//' api/openapi.yaml

run_case MUT-43 "launder a failed claimed re-read into a token refusal" \
  "$HND" 'TestClaimErrorMapping' "$UNI_API" \
  perl -0pi -e 's/\t\tpool, perr := s\.poolFor\(c\)\n\t\tif perr != nil \{\n\t\t\treturn s\.unavailable\(c, "re-reading the claimed state", perr\)\n\t\t\}/\t\tpool, perr := s.poolFor(c)\n\t\tif perr != nil {\n\t\t\treturn s.refuseToken(c)\n\t\t}/' "$HND"

run_case MUT-44 "drop the cause from the 503 log line" \
  "$HND" 'TestADatabaseOutageIsDiagnosableFromTheLog' "$INT" \
  perl -0pi -e 's/\ts\.deps\.Logger\.Error\("http: the claim endpoint could not reach the database",\n\t\t"where", where,\n\t\t"error", obs\.Redact\(cause\.Error\(\)\),\n\t\t"request_id", requestIDOf\(c\)\)\n//' "$HND"

run_case MUT-45 "allow an origin that cannot be normalised" \
  "$HND" 'TestAnUnnormalisableOriginNeverMatches' "$UNI_API" \
  perl -0pi -e 's/\t\tif gotN == "" \|\| wantN == "" \|\| gotN != wantN \{/\t\tif gotN != wantN {/' "$HND"

EXTRA_RESTORE="internal/store/sqlcgen/"
run_case MUT-6 "remove the expires_at predicate from the redeem" \
  "$QRY" 'TestTheRedeemStatementRefusesADeadToken' "$INT" \
  bash -c "perl -0pi -e 's/\n       AND expires_at    > now\(\)//' $QRY && sqlc generate"

run_case MUT-1b "drop 'consumed_at IS NULL' from the redeem CTE" \
  "$QRY" 'TestTheRedeemStatementRefusesADeadToken' "$INT" \
  bash -c "perl -0pi -e 's/\n       AND consumed_at   IS NULL//' $QRY && sqlc generate"

run_case MUT-1c "drop 'superseded_at IS NULL' from the redeem CTE" \
  "$QRY" 'TestTheRedeemStatementRefusesADeadToken' "$INT" \
  bash -c "perl -0pi -e 's/\n       AND superseded_at IS NULL//' $QRY && sqlc generate"

run_case MUT-46 "drop the users guard from the redeem CTE" \
  "$QRY" 'TestTheRedeemStatementRefusesAClaimedInstance' "$INT" \
  bash -c "perl -0pi -e 's/\n       AND NOT EXISTS \(SELECT 1 FROM users\)//' $QRY && sqlc generate"
EXTRA_RESTORE=""

run_case MUT-47 "report a benign ErrHasUsers boot race as degraded" \
  "$ANN" 'TestABootRaceWithTheFirstAccountReportsClaimedNotDegraded' "$INT" \
  perl -0pi -e 's/\tif errors\.Is\(err, ErrHasUsers\) \{\n.*?\n\t\treturn BootOutcome\{Claimed: true\}, nil\n\t\}\n//s' "$ANN"

run_case MUT-48 "stop treating a server-signalled outage as unavailable" \
  "$SVC" 'TestAConnectionKilledMidClaimAnswers503NotFiveHundred' "$INT" \
  perl -0pi -e 's/\tcase strings\.HasPrefix\(code, "08"\), strings\.HasPrefix\(code, "53"\):\n\t\treturn true\n\tcase code == "57P01", code == "57P02", code == "57P03":\n\t\treturn true\n//' "$SVC"

# --- the verifier's R2-C and R2-D ----------------------------------------------

run_case MUT-50 "answer a claimed instance only from an ALREADY-warm cache again" \
  "$HND" 'TestAColdCacheOnAClaimedInstanceAnswers409WithoutAuditRowsForAnyBody' "$INT" \
  perl -0pi -e 's/\tclaimed, err := s\.instanceClaimed\(c\)\n\tif err != nil \{\n\t\treturn s\.unavailable\(c, "checking claimed state", err\)\n\t\}\n\tif claimed \{/\tif claimed, fresh := s.claimed.get(s.deps.Now()); fresh \&\& claimed {/' "$HND"

run_case MUT-51 "examine the token before the claimed state again (library)" \
  "$SVC" 'TestClaimReadsTheClaimedStateBeforeExaminingTheToken' "$INT" \
  perl -0pi -e 's/(\tq := sqlcgen\.New\(pool\)\n)/$1\tif err := in.Validate(); err != nil {\n\t\treturn Result{}, err\n\t}\n/' "$SVC"

run_case MUT-52 "answer the redeem's no-row case with a token refusal regardless" \
  "$HND" 'TestTheClaimTransactionPinsReadCommitted' "$INT" \
  perl -0pi -e 's/\t\tif claimed \{\n\t\t\ts\.claimed\.set\(true, s\.deps\.Now\(\)\)\n\t\t\treturn newCodedError\(http\.StatusConflict, "conflict", claimedMessage\)\n\t\t\}\n\t\treturn s\.refuseToken\(c\)/\t\t_ = claimed\n\t\treturn s.refuseToken(c)/' "$HND"

# Both defences against "a user appears during the hash" removed at once. Neither
# alone is observable through the handler (see MUT-53 in the review-only block),
# so this proves the end-to-end test has teeth and that these two are the only
# things standing between that race and an owner being created.
EXTRA_RESTORE="internal/store/sqlcgen/ $SVC"
run_case MUT-54 "delete BOTH the in-transaction gate and the redeem's users guard" \
  "$QRY" 'TestAClaimRefusesWhenAUserAppearsDuringTheHash' "$INT" \
  bash -c "perl -0pi -e 's/\n       AND NOT EXISTS \(SELECT 1 FROM users\)//' $QRY && sqlc generate && perl -0pi -e 's/\tclaimed, err = qtx\.AnyUserExists\(ctx\)\n\tif err != nil \{\n\t\treturn Result\{\}, unavailable\(\x22checking claimed state\x22, err\)\n\t\}\n\tif claimed \{\n\t\treturn Result\{\}, ErrAlreadyClaimed\n\t\}\n//' $SVC"
EXTRA_RESTORE=""

# R2-G(d): under --env F, the origin check's RAW half must come from F too.
DOC=cmd/vizra/doctor.go
run_case MUT-55 "read the raw public origin from the process env under --env again" \
  "$DOC" 'TestDoctorReadsTheRawPublicOriginFromTheSameSourceAsTheConfig' ./cmd/vizra/ \
  perl -0pi -e 's/\t\t\tl, err := source\(\)\n\t\t\tif err != nil \{\n\t\t\t\treturn "" \/\/ unreachable in practice: loadConfig already failed on it\n\t\t\t\}\n\t\t\tv, _ := l\("VIZRA_PUBLIC_ORIGIN"\)/\t\t\tv, _ := os.LookupEnv("VIZRA_PUBLIC_ORIGIN")/' "$DOC"

rule "REVIEW-ONLY PROPERTIES (no mutation here turns a test red)"
cat <<'NOTE'
Stated rather than implied, because a mutation matrix that quietly omits these
would overclaim:

  MUT-4  replacing subtle.ConstantTimeCompare with bytes.Equal. Behaviourally
         identical; a timing difference is not observable from a Go test.

  MUT-4b passing the PRESENTED digest rather than the row's own digest to the
         redeem statement. Identical results today; the property is defence
         against a future index-probing change. Code review owns it.

  MUT-53 deleting ONLY the in-transaction AnyUserExists gate in Claim — the one
         the code calls THE authoritative gate. MEASURED: nothing reddens, and
         the reason is structural. Since the redeem statement gained
         `AND NOT EXISTS (SELECT 1 FROM users)` (FU-2), a user present when the
         redeem runs makes it return no row, and the 409-vs-403 re-read — which
         keys on the CLAIMED state, as OQ-4 defines it — answers 409 exactly as
         the gate would, with no audit row and no budget charged. The two paths
         are indistinguishable from outside. Before FU-2 they were not: the
         verifier measured HTTP 201 and an owner created with this gate deleted.
         The statement guard is scored on its own by MUT-46 (a direct-query test
         that bypasses every Go check), and MUT-54 deletes both layers together
         to prove the end-to-end test catches the race they jointly prevent.
         Making this single deletion observable would require the re-read to
         answer 403 on a claimed instance, which is the wrong answer.

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

  MUT-14 re-minting unconditionally at boot. The decision it targeted MOVED into
         Mint, under the advisory lock, which is the whole point of the fix —
         MUT-14b mutates it in its new home and reddens
         TestConcurrentBootsMintExactlyOneToken.

NOTE

rule "SUMMARY"
echo "passed:       $PASS"
echo "failed:       $FAIL"
echo "harness-fail: $HARNESS"
[ "$FAIL" -eq 0 ] && [ "$HARNESS" -eq 0 ]
