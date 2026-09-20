# core PR2 "stabilise" — evidence index

Branch `fix/m0-stabilise`, base `415a6d19cfc0acedd8ad84c1857c95db0ed63627` (main).
Closes verifier FINDING V-1 and V-2 from core PR #1 and the backend seat's
carried-forward M2 `attempts` note. Refs yegamble/vizra#1.

**State: READY_FOR_REVIEW.** Not verified — an independent verifier that did not write
this code has to reproduce it, and `ci-required` has to be green on the verified SHA.

| File | What it shows |
|---|---|
| `MECHANISM-v1.md` | the reproduction, the diagnostic output, and why the verifier's inter-test-interference hypothesis is refuted |
| `BEFORE-valkey-20runs.txt` | the pre-fix failure rate: **3 of 20** full runs red, at `415a6d1` |
| `AFTER-valkey-30runs.txt` | 30/30 green, valkey, source order |
| `AFTER-valkey-30runs-shuffle.txt` | 30/30 green, valkey, `-shuffle=on` |
| `AFTER-redis-30runs.txt` | 30/30 green, redis, source order |
| `AFTER-redis-30runs-shuffle.txt` | 30/30 green, redis, `-shuffle=on` |
| `RED-v1-v2.txt` | both new tests failing before the fix |
| `GREEN-v1-v2.txt` | both new tests passing after it |
| `MUTATIONS.md` | five controlled mutations, each with the check that kills it |
| `FINAL-LANES.txt` | every required lane, exact commands and exit codes |
| `DIAGNOSTIC-zz_diag_test.go.txt` | the temporary diagnostic, verbatim; removed from the tree after use |

## Environment

darwin/arm64, `go1.27.1`, sqlc 1.31.1, Docker 29.8.0. PostgreSQL 18.6
(`postgres@sha256:86c951e05bf56c93d95d397747fb8820ac76cc3bedb78f43abd83eedbe3666ae`,
host port 55433), Valkey 9.1.2
(`valkey/valkey@sha256:c123e3715db63d06d4ad6964884037aa0d5d4d703939b9929954112889708e1d`,
56380), Redis 7.2.16
(`redis@sha256:0637954999d01b7c9ce9167db2da50656e2590d3b884f1c600c5f63bb6e6773c`, 56381).
Containers `vizra-pr2-pg`, `vizra-pr2-valkey`, `vizra-pr2-redis`, removed afterwards.

## Test counts

`go test -count=1 -tags=integration -v ./...` — **919 pass, 0 fail, 0 skip.**
PR #1 ended at 917; the two additions are
`TestRunAfterComesFromTheDatabaseClockNotTheApplicationHost` and
`TestAWorkerWithAPlainHandlerLogsNoCredentials`. No test was removed or weakened: the
only deleted lines in `golden_test.go` are the five leaked
`go func() { _ = w.Run(ctx) }()` starts, replaced by a `startWorker` helper that waits.

## Provenance of the 30× loops

The first loop (`AFTER-valkey-30runs.txt`) records `git rev-parse HEAD` as `415a6d1`
because the work was not yet committed when it started; the other three record the
commit `4f02e18`. All four ran against the same working tree. Every **executable** file
in that tree is byte-identical to the committed blobs:

```
679752ca4b8c53fb0c03e2f18138d751598edb16  .github/workflows/build-test.yml
bc7bcb2b217d68081b62d4cb50b01c303160a521  internal/integration/golden_test.go
22dc8499dbd013a9f7b78bd86bbfb12ad45b1d5c  internal/jobs/jobs.go
06f73713610fdd1ee7ea29b43fde99c90fc3a95e  internal/jobs/worker.go
9d1507a456560d0b3ff6df941962fcfbfcde272f  internal/store/sqlcgen/jobs.sql.go
5f1dd7e88a1022333fbf913f03b3870f406f410c  internal/store/sqlcgen/querier.go
f94bd97f25a7b27fceb352beca81e09a4f85f49e  store/queries/jobs.sql
```

Two files were edited after the loops began and therefore differ from the committed
blobs: `AGENTS.md` and `Makefile`. Both edits are prose/comment only — correcting the
claim about when `go test` prints the shuffle seed (it prints
`-test.shuffle <seed>` as the first line of a FAILING package's output, not on a green
run; verified on go1.27.1). The `test-integration-shuffle` recipe line itself is
unchanged, and the loops invoked `go test` directly rather than through `make`.

## Reported, not fixed here

`obs.Redact`'s presigned-URL rule is anchored on the query separator, so a **bare**
`X-Amz-Credential=…` outside a URL is not redacted. That is pattern coverage — the same
family as the verifier's still-open V-3(b) (`secretKeys` is exact-match, so
`session_secret` and `search_hmac_key` are uncovered) — and belongs in a slice that
reviews `internal/obs` as a whole, not in one whose subject is call sites. The fixture
in `TestAWorkerWithAPlainHandlerLogsNoCredentials` therefore places that credential in
query position, where its pinned class actually occurs, and says so in a comment.
