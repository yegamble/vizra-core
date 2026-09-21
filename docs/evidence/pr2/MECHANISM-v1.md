# V-1: reproducing the flake and identifying the mechanism

Verifier FINDING V-1 (PR #1, `docs/evidence/warroom/2026-09-20-vizra-core-pr1-foundation-VERIFY.md`)
recorded the symptom and explicitly marked the mechanism **UNVERIFIED**, with a leading
candidate of inter-test interference: `freshDatabase`'s `DROP SCHEMA public CASCADE`
racing a previous test's closing pool, and `ClaimJob`'s `FOR UPDATE SKIP LOCKED` then
skipping a row locked by a lingering connection.

That candidate is **refuted below**. The cause is a comparison between two different
clocks.

Environment for everything in this file: darwin/arm64, `go version go1.27.1 darwin/arm64`,
PostgreSQL 18.6 (`postgres@sha256:86c951e0…`) in container `vizra-pr2-pg` on host port
55433, Valkey 9.1.2 (`valkey/valkey@sha256:c123e371…`) on 56380, Redis 7.2.16
(`redis@sha256:06379549…`) on 56381. Source SHA for the BEFORE measurements:
`415a6d19cfc0acedd8ad84c1857c95db0ed63627` (main).

---

## 1. BEFORE the fix — 20 full runs

`go test -race -count=1 -tags=integration ./internal/integration/`, 20 consecutive runs
at `415a6d1`. Full transcript: `BEFORE-valkey-20runs.txt`.

```
run  1 rc=0 pass=38 fail=0
run  2 rc=0 pass=38 fail=0
run  3 rc=0 pass=38 fail=0
run  4 rc=1 pass=37 fail=1
    golden_test.go:412: no rows in result set
--- FAIL: TestSweepReclaimsOnlyElapsedLeases (0.04s)
…
run  8 rc=1 pass=36 fail=2
    golden_test.go:369: no rows in result set
--- FAIL: TestHeartbeatRequiresStillHoldingTheLease (0.03s)
    golden_test.go:412: no rows in result set
--- FAIL: TestSweepReclaimsOnlyElapsedLeases (0.03s)
…
run 16 rc=1 pass=37 fail=1
    golden_test.go:369: no rows in result set
--- FAIL: TestHeartbeatRequiresStillHoldingTheLease (0.03s)
…
TOTAL FAILED RUNS: 3 / 20
```

**Pre-fix failure rate: 3 of 20 full runs (15%).** Only ever the two named tests, and
always at a `ClaimJob` issued immediately after a successful `enqueue`.

## 2. The diagnostic

A temporary file `internal/integration/zz_diag_test.go` (kept verbatim here as
`DIAGNOSTIC-zz_diag_test.go.txt`, removed from the tree after use) dumped, at the instant
of failure: the whole `jobs` table with `run_after <= now()` evaluated by the server, the
server clock against the host clock, and **every backend connected to the database** from
`pg_stat_activity`.

The two `t.Fatal(err)` sites were temporarily wrapped to call it. No product code and no
assertion was changed for this measurement.

## 3. What it showed — the interference candidates are refuted

```
DIAG heartbeat/claim: err=no rows in result set
                      hostNow=2026-09-20T20:51:43.612713Z
                      dbClock=2026-09-20T20:51:43.610732Z  skew(db-host)=-1.987ms
DIAG heartbeat/claim: row id=01a0c096-cd7a-75a9-b7e4-e0e18040966b kind=noop state=queued
                      attempts=0/5 run_after=2026-09-20T20:51:43.610365Z eligible=true
                      leased_by=- corr=c created=2026-09-20T20:51:43.608356Z
DIAG heartbeat/claim: 1 rows in jobs
DIAG heartbeat/claim: backend pid=1419 state=active app= start=…43.582136Z q="select pid, …"

DIAG sweep/claim-stale: err=no rows in result set
                      hostNow=2026-09-20T20:51:43.644095Z
                      dbClock=2026-09-20T20:51:43.642141Z  skew(db-host)=-1.957ms
DIAG sweep/claim-stale: row … state=leased attempts=1/5 leased_by=w corr=live
DIAG sweep/claim-stale: row … state=queued attempts=0/5 leased_by=- corr=stale
DIAG sweep/claim-stale: 2 rows in jobs
DIAG sweep/claim-stale: backend pid=1421 …
```

Reading it:

- The row the claim wanted **is present**, `state=queued`, `attempts=0/5`, `kind=noop`,
  `leased_by` NULL. Nothing had taken it.
- **Exactly one backend is connected to the database** — the diagnostic's own connection.
  No leaked worker, no closing pool, no second claimer. `FOR UPDATE SKIP LOCKED` had
  nothing to skip. *The inter-test-interference hypothesis is refuted.*
- `run_after` (…610365Z) is **2.0 ms LATER than `created_at`** (…608356Z), although
  `run_after` was sampled *earlier in real time* than `created_at` was. `created_at` is
  the database's `now()`; `run_after` was the application host's `time.Now()`. The
  database clock trails the host clock by ~2 ms.

## 4. The variable is the margin, not the skew

Skew is steady, and identical whether the tests run alone or in the suite — so skew alone
is not what makes the failure intermittent:

```
DIAGSKEW TestHeartbeatRequiresStillHoldingTheLease: skew(db-host)=-975.25µs  rtt=756.5µs   (in suite)
DIAGSKEW TestHeartbeatRequiresStillHoldingTheLease: skew(db-host)=-984µs     rtt=820µs     (alone)
```

The variable is the **eligibility margin** `now() - run_after`, measured by the database
immediately before the claim — the enqueue→claim round trip *minus* the skew:

| | `now() - run_after`, 8 samples |
|---|---|
| the test **alone** (passes 20/20) | 847µs, 642µs, 733µs, 885µs, 618µs, 757µs, 1.153ms, 589µs |
| the **full suite** (fails ~15%) | 462µs, 415µs, **23µs**, 474µs, **−7µs**, 329µs, 75µs, 1ms |

A sub-millisecond margin against a ~1 ms clock offset. When it goes negative,
`run_after <= now()` is false, `ClaimJob` returns `pgx.ErrNoRows`, and the test fails on
a row that is sitting right there queued and unleased. That also explains the verifier's
"passes 8/8 alone, fails in the suite": alone, the margin never got close to zero.

## 5. Root cause

`internal/jobs/jobs.go` stamped `run_after` from the **application host clock** when the
caller asked for no delay:

```go
runAfter := j.RunAfter
if runAfter.IsZero() {
        runAfter = time.Now()          // host clock
}
```

`ClaimJob` tests eligibility against the **database clock**:

```sql
WHERE state = 'queued' AND run_after <= now() AND …
```

Every other timestamp in a job's life — `created_at`, `leased_until`, `RetryJob`'s
`run_after`, the sweep's `leased_until < now()`, `finished_at` — is assigned by the
database. **`run_after` at enqueue was the only place in the system where two clocks were
compared.**

This is not test-only. In production the API and PostgreSQL are separate containers or
hosts; ordinary NTP skew is milliseconds to seconds, and a clock step is larger still.
For the duration of that skew, freshly enqueued "run now" work is invisible to the claim
loop. The poll interval usually hides it — which is exactly why it would never have been
found from production behaviour.

## 6. The fix

PostgreSQL becomes the single clock authority for "now". `EnqueueJob` resolves an absent
`run_after` itself, in the same transaction that stamps `created_at`:

```sql
COALESCE(sqlc.narg(run_after)::timestamptz, now())
```

and `jobs.Enqueue` passes SQL `NULL` instead of `time.Now()`. An explicit caller-supplied
`RunAfter` — a deliberate schedule, past or future — is stored verbatim and is unchanged.

No sleep, no retry, no `t.Skip`, no lengthened timeout and no serialisation of the suite
was used. No existing assertion was weakened and no test was removed.

The regression guard is `TestRunAfterComesFromTheDatabaseClockNotTheApplicationHost`,
which pins `run_after = created_at` for a "run now" enqueue, immediate claimability, and
that an explicit future or past `RunAfter` survives intact (so the fix cannot decay into
"the database always assigns now()", which would fire every scheduled job at once).

## 7. Separately — the leaked worker goroutines

The verifier also observed that five tests started a worker with
`go func() { _ = w.Run(ctx) }()` and never waited for it. That is **not** what caused
V-1 — §3 shows no second backend at the moment of failure — but it is a real isolation
hazard, because `NewWorker` registers `KindNoop` unconditionally
(`internal/jobs/worker.go`), so every worker in this suite polls for the kind most of
these tests enqueue. With `-shuffle=on` now running on every CI run, those tests can
precede the claim tests in any permutation. All five now use `startWorker`, which waits
for the goroutine to return and **fails the test** if it does not within 30 s.
