# VZ-INSTALL-003 (M1-A) — owner claim evidence

| File | What it is |
|---|---|
| `01-integration-pg18.txt` | the integration suite exactly as `build-test`'s direct step runs it (`go test -race -count=1 -tags=integration -json ./...` + `scripts/go-test-report.py`) on PostgreSQL 18.6 and Valkey 9.1.2, plus `internal/integration`'s verbose per-test results |
| `02-mutations.txt` | every mutation demonstration: RED under one controlled mutation, GREEN when reverted |
| `03-shuffle.txt` | `make test-integration-shuffle` on Valkey and on Redis, so no test depends on another's leftovers |
| `04-make-ci.txt` | `make ci` (every `ci-required` floor lane that runs without Docker) |
| `05-mut53-measurement.txt` | the measurement behind MUT-53's review-only status, RE-TAKEN against the closing slice's code: the in-transaction gate deleted, `internal/integration`, `internal/ownerclaim` and `internal/httpapi` run, exit 0. The earlier whole-suite measurement at `95afb62` is in this file's history |
| `06-unit.txt` | the unit suite exactly as `build-test`'s direct step runs it (`-json` + `scripts/go-test-report.py`, floors enforced) |
| `07-race-stress.txt` | closing slice: `TestOwnerClaimRaceYieldsExactlyOneOwnerUnderEveryServerDefaultIsolation` repeated with `-count`, on Valkey 9.1.2 and Redis 7.2.16, with and without CPU contention, plus the baseline at `655f46a` |
| `08-shuffle-seeds.txt` | closing slice: the shuffled integration suite with RECORDED seeds — CI's failing `1790134723139270269` on Valkey (the leg that failed), plus `20260923` and `424242` |
| `demonstrate.sh` | the harness that produced `02-mutations.txt`, re-runnable by a verifier |

Every transcript was produced on the tested tree recorded in its own `src:` header line. Runs that failed for an environmental reason (the `internal/fixtures` 10-minute default timeout on a heavily loaded shared host) are kept in the transcript, labelled "recorded, not counted", beside the run that counts. The pushed
commit is that tree plus these transcript files and nothing else (`git diff <src> <pushed> --stat`
lists only `docs/evidence/m1a-owner-claim/`).

## How to reproduce

```sh
docker run -d --name <yours>-pg18 -p 55438:5432 \
  -e POSTGRES_USER="$PGUSER" -e POSTGRES_PASSWORD="$PGPASSWORD" -e POSTGRES_DB=vizra_test postgres:18
docker run -d --name <yours>-valkey -p 63799:6379 valkey/valkey:9.1.2

# Build the DSN from the values you passed to the containers; do not paste a
# credentialed URI into a tracked file — a secret scanner cannot tell a throwaway
# local password from a real one, and an incident on any commit in a PR stays red.
export VIZRA_TEST_DATABASE_URL="postgres://${PGUSER:-vizra}:${PGPASSWORD:?set it to the POSTGRES_PASSWORD you used}@127.0.0.1:55438/vizra_test?sslmode=disable"
export VIZRA_TEST_CACHE_URL='redis://127.0.0.1:63799/0'

make ci
go test -tags=integration -race -count=1 -v ./internal/integration/
./docs/evidence/m1a-owner-claim/demonstrate.sh        # needs a CLEAN tree
```

## What the harness guarantees, at exactly that strength

It records the sha256 of the mutated file **before and after** and refuses to
score a case whose digest did not change — so a pattern that stopped matching is
reported `HARNESS-FAIL`, never as a pass. After restoring, it re-checks
`git status` and refuses to continue if the tree is dirty, because a polluted
tree would make the NEXT case's result meaningless. Both checks earned their
place: the first caught MUT-27's stale pattern, and the second caught the
harness restoring a mutated `.sql` file without the sqlc-generated Go beside it.

### Fix round 1

Seven blocking specialist findings were closed on top of the first head: the
per-request `already_claimed` audit row, argon2id inside the claim transaction,
the liveness pre-check, the mint liveness decision outside the advisory lock, a
database outage answering 500 where the contract promises 503, two tests that
could not go red, and the frozen migration text. Thirteen mutation cases were
added, including the **MUT-17 this transcript previously cited without running**.
Every `MUT-` id referenced anywhere in the repository is now either declared in
`demonstrate.sh` or listed as review-only with its reason — audited, not assumed.

### Fix round 2

Round 1's own fix for claim-status created two blockers, both closed here: the
status GET shared ONE hard-ceiling counter with the claim POST (so a body-less
GET flood answered the operator's valid token 429), and the route could return a
429 the contract did not declare. The routes now have separate ceilings (claim
600, status 3000 per 15-minute window) and the status-coverage test is a table
over every setup operation. Also: a failed claimed-state re-read answers 503
instead of being laundered into "token not accepted"; a 503's cause is logged
once, redacted; `checkOrigin` denies when either side cannot be normalised; the
redeem CTE refuses on an instance that already has users; and a benign boot race
reports "claimed" rather than "degraded".

**A gap in round 1's own fix, found by re-reading AGENTS.md against the code.**
The row said a database outage answers 503, "never 500". It was not true: round
1's `ErrUnavailable` wrapped only non-`PgError` failures, deliberately leaving
`*pgconn.PgError` untouched so the mapper can key on constraint codes — but a
server that answers in order to say it cannot serve (57P01 admin shutdown,
57P03, 53300, class 08) also arrives as a `PgError`, matched no branch, and fell
through to a 500. The audit insert inside the claim transaction was not wrapped
at all. `TestAConnectionKilledMidClaimAnswers503NotFiveHundred` was written
first and went red with a real 500 (it terminates the claim's backend
mid-transaction), then green with `IsServerUnavailable`; MUT-48 scores it.

Four race tests used `time.Sleep` as a barrier. Under load that can never make
them go falsely red, but it can silently stop them exercising the race they exist
for. They now wait until `pg_stat_activity` reports the expected sessions blocked
on a lock, and FAIL if that does not happen within 60 s. This was prompted by a
measurement, not a hunch: this round's suite ran on a machine at load average 197
on 8 CPUs.

Three review-only properties became SCORED this round by calling the generated
`ClaimOwner` query directly, bypassing every Go-side check: MUT-6 (expiry),
MUT-1b (consumption) and MUT-1c (supersession). MUT-16 is scored again through
`TestTheClaimTransactionPinsReadCommitted`, which forces the REPEATABLE READ
40001 window deterministically — after the read/write split the ordinary race
test no longer reached it.

**The harness found two defects in itself this round, too.** A rename left two
mutators matching nothing (reported `HARNESS-FAIL`, never scored). And an edit
had indented the `NOTE` heredoc terminator, which would have swallowed the
SUMMARY and the final exit-status line so the harness exited 0 whatever happened;
that was caught by reading the file before the full run, and a single-case run
then confirmed the summary prints real values and the exit status is real. Nothing
in the harness enforces that check; it was done, not guaranteed.

**Final round (round 2, second half).** The verifier's re-verification at
`59a19c5` found two further REQUIRED defects, both real, and both are fixed here:

- **R2-C — a cold cache was an audit writer.** On a restarted server over a
  claimed instance, `Claim` validated the token's shape before asking whether
  the instance was claimed, so malformed POSTs wrote permanent `refused` rows,
  answered 403 instead of OQ-4's 409, and never warmed the cache. The handler now
  asks `instanceClaimed` first (right after the hard ceiling, before content
  type, body, origin or token), and `Claim` reads the claimed state before it
  validates anything. `TestAColdCacheOnAClaimedInstanceAnswers409WithoutAuditRowsForAnyBody`
  gives EACH body its own cold server — an earlier draft sent them in sequence
  to one server, where the first 409 warmed the cache and masked the rest (MUT-50
  stayed green until that was fixed). MUT-50 and MUT-51 score the two orderings.
- **R2-D — the "authoritative" in-transaction gate was unobserved.** The redeem
  statement already refused on any user (FU-2), but the no-row re-read keyed on
  a live OWNER, so a member-only or tombstoned-owner instance answered 403 and
  charged the budget. The re-read now keys on the claimed state (`ownerclaim.Claimed`,
  i.e. EXISTS(users)). `TestAClaimRefusesWhenAUserAppearsDuringTheHash` commits a
  member mid-hash and requires 409 with no owner. With both layers in place,
  deleting only the Go gate is indistinguishable from outside — declared
  review-only as MUT-53, with the full-suite measurement in
  `05-mut53-measurement.txt`; MUT-54 deletes both layers and goes red.
- **`vizra doctor --env F`** compared the process environment's
  `VIZRA_PUBLIC_ORIGIN` with F's normalised value. Both halves now come from one
  source; `TestDoctorReadsTheRawPublicOriginFromTheSameSourceAsTheConfig` went red
  first, and MUT-55 scores it.
- Comment-only corrections to migration 0005 (a wrong ADR-002 attribution, a
  misattributed ledger ID, a TRUNCATE sentence the new test falsified, a
  misquote of 0003, and TOTP speculation that contradicted ADR-003's
  reservation). The SQL is identical once comments are stripped.

**The review-only block in `02-mutations.txt` lists exactly five ids, and says
which kind each is.** Two are properties no Go test can observe: MUT-4 (the
constant-time comparison) and MUT-4b (passing the row's own digest to the
redeem). Two are Go-side redundancy in front of a scored database guard: MUT-36
(the CLI flag, behind MUT-32) and MUT-53 (the in-transaction gate, behind MUT-46,
with MUT-54 scoring the pair). One moved: MUT-14's decision now lives in `Mint`
and is scored as MUT-14b. The `consumed_at`/`superseded_at`/expiry predicates are
no longer review-only — MUT-1b, MUT-1c and MUT-6 score them by direct query.

### Closing slice (a fresh builder, on top of `655f46a`)

`655f46a` was **red** in CI: run 35814919455, cache-matrix valkey leg,
`make test-integration-shuffle` seed `1790134723139270269`,
`TestOwnerClaimRaceYieldsExactlyOneOwnerUnderEveryServerDefaultIsolation/serializable`
— "claimant 22 got an unexpected 403 … that claim token was not accepted", 30
declined where 31 were wanted.

**The defect was a class, not a line.** `Claim`'s read phase checked the claimed
state, then examined the token (shape, row, digest, liveness) and answered any
refusal directly. A winner that committed between a loser's claimed check and
its token read left the loser looking at a CONSUMED token, so the loser got 403,
a permanent `refused` audit row and a failure-budget charge on an instance that
was claimed — against OQ-4. The same shape held for every read-phase refusal
branch, while the redeem's empty result was classified by a second copy of the
logic in the HTTP layer.

**The fix.** `ownerclaim.classifyRefusal` is now the ONE place a token refusal
becomes an answer: a fresh claimed read → 409 (no row, no charge, claimed bit
set), a failed read → 503 (no charge), still unclaimed → the uniform 403 with its
audit and budget semantics unchanged. `examineToken` returns a verdict and never
`ErrTokenNotAccepted` itself, so the read phase has a single refusal exit; the
redeem's empty result calls the same function after releasing its transaction;
the handler's own re-read branch is deleted.

**How it is proven, deterministically.** A test seam (`afterClaimedCheck`, nil in
production; its setter is compiled only under `-tags=integration`, which
`TestTheClaimSeamCannotBeSetFromAProductionBuild` pins) pauses a claimant between
its claimed check and its token examination. For each branch — consumed,
digest mismatch, malformed, never minted — the instance becomes claimed while
it is paused, and on release it must answer 409 with no `refused` row, no
failure-budget charge (a recording limiter counts charges) and no derivation.
The same seam drives the controls (every branch on an instance that stays
unclaimed is still exactly one 403, one row, one charge) and the outage case (the
classification's read blocked on a table lock and cancelled → 503, nothing
charged).

**Mutations.** New: MUT-56 (bypass the classification on the read phase — the
defect itself), MUT-57 (classification answers 409 whatever the state), MUT-58
(the seam's setter compiled into production), MUT-59 (the handler decides
409-vs-403 again). Retargeted, because the code they mutated moved into
`classifyRefusal`: MUT-43 (a failed re-read laundered into a token refusal) and
MUT-52 (the redeem's empty result refused regardless). Patterns updated for code
that moved but did not change meaning: MUT-13 and MUT-31.

**The review-only block is unchanged in membership** — MUT-4, MUT-4b, MUT-53,
MUT-36, MUT-14 — re-examined against this change: none became observable.
MUT-53 (deleting only the in-transaction gate) is still indistinguishable from
outside, because the redeem's users guard then yields an empty result that the
same classification answers 409; its note now names `classifyRefusal`.

**The race test was tightened, not loosened.** It now resets and CHECKS the
server's own pool's isolation default (it used to reset only the test's pool and
relied, unchecked, on the server's pool not having connected before the
`ALTER DATABASE` — true today, but nothing held it true), and asserts zero `refused` rows and zero failure-budget charges from the
losers.

### Council re-review of `56504c1` (backend NEW-B, security F-2)

- **NEW-B.** `internal/integration` (floor 40, measured 165) and `internal/httpapi`
  (floor 26, measured 56) had outgrown their per-package floors, so this PR's own
  integration tests, or the setup handler's unit tests, could be deleted with every
  floor green. The floors were regenerated with `--emit-floors` from measured runs
  (integration 140, httpapi 48 in both suites; whole-suite `min_tests` unit 1006,
  integration 1147); the measurement is recorded in `scripts/test-floors.json`'s
  `_why`. Two harness cases score it, with their own runner (`run_floor_case`):
  **MUT-60** deletes `owner_claim_test.go` together with `claimtoken_cli_test.go`
  (the second uses the first's harness, so deleting one alone is a BUILD failure,
  not this control), and **MUT-61** deletes `internal/httpapi/setup_test.go`. Each
  judges the package with `scripts/go-test-report.py` against its COMMITTED floor,
  and is scored only if the report names that package's floor as the reason.
- **F-2.** The AGENTS.md row on the failure limiter now also states the per-route
  hard ceiling (claim 600, status 3000 per 15 minutes) as an accepted residual that
  CAN answer a valid token 429 until the window rolls, pending M1-B's concurrency
  bound — which `allowSetupRequest`'s comment already said AGENTS.md recorded.
