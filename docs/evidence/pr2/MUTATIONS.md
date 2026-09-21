# Controlled mutations at 4f02e18

Each fix is reverted in turn on the final tree and the check that kills it is recorded.
Environment: darwin/arm64, go1.27.1, PostgreSQL 18.6 (port 55433), Valkey 9.1.2 (56380).

## Baseline — unmutated tree
```
ok  	github.com/yegamble/vizra-core/internal/integration	1.679s
exit=0
```

## M1 — V-1, Go half: resolve a zero RunAfter with the HOST clock again

Mutation (`internal/jobs/jobs.go`): `toTimestamptz(j.RunAfter)` -> `toTimestamptz(mutantNow(j.RunAfter))`,
where `mutantNow` returns `time.Now()` for a zero time — i.e. exactly the pre-fix behaviour.
```
--- FAIL: TestRunAfterComesFromTheDatabaseClockNotTheApplicationHost (0.09s)
    golden_test.go:352: run_after (2026-09-20T21:25:34.532958Z) is not created_at (2026-09-20T21:25:34.531318Z): they differ by 1640 µs, so run_after was read from a clock OTHER than the one ClaimJob compares it against. A job that is meant to run now is then not claimable for the duration of that difference.
FAIL
FAIL	github.com/yegamble/vizra-core/internal/integration	0.568s
FAIL
```

## M2 — V-1, SQL half: drop the `COALESCE(..., now())` from `EnqueueJob`

Mutation (`store/queries/jobs.sql`, then `sqlc generate`):
`COALESCE(sqlc.narg(run_after)::timestamptz, now())` -> `@run_after`.
The Go side now passes SQL NULL, so the two halves are coupled: removing either one
is caught.
```
--- FAIL: TestRunAfterComesFromTheDatabaseClockNotTheApplicationHost (0.07s)
    golden_test.go:339: Enqueue: jobs: enqueueing noop: ERROR: null value in column "run_after" of relation "jobs" violates not-null constraint (SQLSTATE 23502)
FAIL
FAIL	github.com/yegamble/vizra-core/internal/integration	0.584s
FAIL
```

## M3 — V-2: revert ALL THREE log call sites to the raw handler error

Mutation (`internal/jobs/worker.go`): `safeError(err.Error())` -> `err.Error()` at
`jobs: exhausted attempts` and `jobs: retrying`, and `safeError(msg)` -> `msg` at
`jobs: terminal failure` — the state at 415a6d1.
```
--- FAIL: TestAWorkerWithAPlainHandlerLogsNoCredentials (0.15s)
    golden_test.go:1649: the worker's own log leaked the bearer token ("eyJhJhJhJhJhJhJhJhJhZyI6MQ") with a plain handler installed. Redaction must happen at the call site, not in whichever handler the process happened to wire up.
FAIL
FAIL	github.com/yegamble/vizra-core/internal/integration	0.648s
```

## M4 — V-2: fix ONLY the two sites the verifier named, leave `jobs: retrying` raw

This is the mutation that matters for scope: the verifier's FINDING V-2 named only
`exhausted attempts` and `terminal failure`. `jobs: retrying` passes the identical raw
handler error. Fixing just the two named sites still leaks.
```
--- FAIL: TestAWorkerWithAPlainHandlerLogsNoCredentials (0.15s)
    golden_test.go:1649: the worker's own log leaked the presigned URL credential ("AKIAIOSFODNN7EXAMPLE") with a plain handler installed. Redaction must happen at the call site, not in whichever handler the process happened to wire up.
FAIL
FAIL	github.com/yegamble/vizra-core/internal/integration	0.719s
```

## M5 — the `attempts` contract note: greppable, and guarded against drift

Greppability (the whole point of the backend seat's note — it was "recorded nowhere greppable"):
```
store/queries/jobs.sql: … MUST RESET `attempts` TO 0 (or raise `max_attempts`) IN THE SAME
internal/store/sqlcgen/jobs.sql.go: … MUST RESET `attempts` TO 0 (or raise `max_attempts`) IN THE SAME
internal/store/sqlcgen/querier.go: … MUST RESET `attempts` TO 0 (or raise `max_attempts`) IN THE SAME
```

It is carried into the generated code by sqlc, not pasted there, so it cannot drift:
removing it from the source query WITHOUT regenerating fails `make sqlc-verify`.
```
==> sqlc-verify
--- a/internal/store/sqlcgen/jobs.sql.go
+++ b/internal/store/sqlcgen/jobs.sql.go
@@ -482,25 +482,6 @@
 // the sweep can declare it dead. Requeueing it instead left an unclaimable row
 // at the head of the claim order forever.
 //
-// CONTRACT FOR ANY FUTURE REQUEUE OF A DEAD OR EXHAUSTED ROW — an M2
-// `vizra jobs retry`, an admin "retry this job" control, a bulk replay:
-// IT MUST RESET `attempts` TO 0 (or raise `max_attempts`) IN THE SAME
-// STATEMENT that sets state back to 'queued'. Setting state alone recreates
-// exactly the row this sweep exists to prevent: `state='queued' AND
-// attempts >= max_attempts`, which ClaimJob's `AND attempts < max_attempts`
-// can never take, and which the claim subselect nevertheless re-orders to the
-// head of the queue on every poll. That is the unclaimable-but-queued zombie,
-// and clearing it needs an operator with psql. The invariant is greppable:
-//
-//	SELECT count(*) FROM jobs WHERE state='queued' AND attempts>=max_attempts; -- must be 0
-//
-// `attempts` counts CLAIMS, so resetting it is the correct semantic for a
-// deliberate human replay: the operator is granting a fresh budget, not
-// pretending the earlier crashes never happened — last_error still records
-// them. Written here rather than in a design note because this is the
-// statement whoever writes that command will read.
-// (Backend seat's carried-forward M2 note, PR #1 final closure.)
-//
 // One statement, so the two outcomes cannot diverge under concurrency.
 func (q *Queries) SweepExpiredLeases(ctx context.Context) ([]SweepExpiredLeasesRow, error) {
 	rows, err := q.db.Query(ctx, sweepExpiredLeases)
--- a/internal/store/sqlcgen/querier.go
+++ b/internal/store/sqlcgen/querier.go
@@ -95,25 +95,6 @@
 	// the sweep can declare it dead. Requeueing it instead left an unclaimable row
 	// at the head of the claim order forever.
 	//
-	// CONTRACT FOR ANY FUTURE REQUEUE OF A DEAD OR EXHAUSTED ROW — an M2
-	// `vizra jobs retry`, an admin "retry this job" control, a bulk replay:
-	// IT MUST RESET `attempts` TO 0 (or raise `max_attempts`) IN THE SAME
-	// STATEMENT that sets state back to 'queued'. Setting state alone recreates
-	// exactly the row this sweep exists to prevent: `state='queued' AND
-	// attempts >= max_attempts`, which ClaimJob's `AND attempts < max_attempts`
-	// can never take, and which the claim subselect nevertheless re-orders to the
-	// head of the queue on every poll. That is the unclaimable-but-queued zombie,
-	// and clearing it needs an operator with psql. The invariant is greppable:
-	//
-	//   SELECT count(*) FROM jobs WHERE state='queued' AND attempts>=max_attempts; -- must be 0
-	//
-	// `attempts` counts CLAIMS, so resetting it is the correct semantic for a
-	// deliberate human replay: the operator is granting a fresh budget, not
-	// pretending the earlier crashes never happened — last_error still records
-	// them. Written here rather than in a design note because this is the
-	// statement whoever writes that command will read.
-	// (Backend seat's carried-forward M2 note, PR #1 final closure.)
-	//
 	// One statement, so the two outcomes cannot diverge under concurrency.
 	SweepExpiredLeases(ctx context.Context) ([]SweepExpiredLeasesRow, error)
 	// Leader election by a SESSION-scoped advisory lock in its TWO-INTEGER form, so
make: *** [sqlc-verify] Error 1
exit=2
```
