# core PR2 "stabilise" — evidence index

Branch `fix/m0-stabilise`, base `415a6d19cfc0acedd8ad84c1857c95db0ed63627` (main).
Closes verifier FINDING V-1 and V-2 from core PR #1 and the backend seat's
carried-forward M2 `attempts` note. Refs yegamble/vizra#1.

**State: READY_FOR_REVIEW.** Not verified — an independent verifier that did not write
this code has to reproduce it, and `ci-required` has to be green on the verified SHA.

| File | What it shows |
|---|---|
| `MECHANISM-v1.md` | the reproduction, the diagnostic output, and why the verifier's inter-test-interference hypothesis is refuted |
| `ROUND1-wait-on-the-observable.md` | verifier round-1 BLOCKER: RED 3/3 -> GREEN 3/3 under a deterministic diagnostic, plus an audit of every wait this PR added or touched |
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

`go test -count=1 -tags=integration -v ./...` — **920 pass, 0 fail, 0 skip.**
PR #1 ended at 917; the three additions are
`TestRunAfterComesFromTheDatabaseClockNotTheApplicationHost`,
`TestAWorkerWithAPlainHandlerLogsNoCredentials` and, from verifier round 1,
`TestEveryErrorLogSiteInTheWorkerIsRedacted`. No test was removed or weakened: the
only deleted lines in `golden_test.go` are the five leaked
`go func() { _ = w.Run(ctx) }()` starts, replaced by a `startWorker` helper that waits.

## Provenance of the 30x loops

The four `AFTER-*.txt` transcripts were re-measured from scratch after the round-1 fixes,
on head `2f0688c10694cccaea5f6920bffb3a36f24bbe9c`. Every **code** file was committed
before the loops started; the only paths dirty during them were the transcripts being
written (`git status --porcelain | grep -v docs/evidence/pr2/` is empty). Each shuffled
run uses an EXPLICIT seed recorded on its own line, so every permutation is reproducible
with `go test -shuffle=<seed>` rather than depending on `go test` printing a seed only on
failure.

## Reported, not fixed here

`obs.Redact`'s presigned-URL rule is anchored on the query separator, so a **bare**
`X-Amz-Credential=…` outside a URL is not redacted. That is pattern coverage — the same
family as the verifier's still-open V-3(b) (`secretKeys` is exact-match, so
`session_secret` and `search_hmac_key` are uncovered) — and belongs in a slice that
reviews `internal/obs` as a whole, not in one whose subject is call sites. The fixture
in `TestAWorkerWithAPlainHandlerLogsNoCredentials` therefore places that credential in
query position, where its pinned class actually occurs, and says so in a comment.
