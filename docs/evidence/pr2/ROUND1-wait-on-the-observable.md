# Round 1, BLOCKER: wait on the observable the assertion reads

Verifier FINDING 1. `TestAWorkerWithAPlainHandlerLogsNoCredentials` waited on
`c.Attempts >= 1`. `ClaimJob` increments `attempts` AT CLAIM TIME — its own comment
says "\`attempts\` counts CLAIMS, not failures" — so the wait was satisfied before the
handler returned, before `RetryJob` ran, and before `jobs: retrying` reached the log
buffer the test then asserts against. The verifier saw it 2 of 120 full runs and
1 of 40 standalone.

The window is widened deterministically with `time.Sleep(3 * time.Second)` in the
`leaky-retry` handler ONLY. This diagnostic is not committed.

Environment: darwin/arm64, go1.27.1, PostgreSQL 18.6 (55433), Valkey 9.1.2 (56380).

## RED — the OLD wait (`c.Attempts >= 1`), diagnostic sleep in place

Wait condition under test:
```go
return err1 == nil && err2 == nil && err3 == nil &&
    a.State == "dead" && b.State == "failed" && c.Attempts >= 1
```
```
run 1: exit=1  the worker never logged "jobs: retrying"
run 2: exit=1  the worker never logged "jobs: retrying"
run 3: exit=1  the worker never logged "jobs: retrying"
```
**RED 3 of 3.**

## GREEN — the NEW wait (the log buffer), same diagnostic sleep still in place

Wait condition under test:
```go
… a.State != "dead" || b.State != "failed" || c.Attempts < 1 { return false }
logged := workerLog.String()
for _, line := range lines { if !strings.Contains(logged, line) { return false } }
return true
```
```
run 1: exit=0  
run 2: exit=0  
run 3: exit=0  
```
**GREEN 3 of 3 with the 3 s handler sleep still present.** The diagnostic sleep is
then removed and is not committed.

## Diagnostic removed, final state
```
    golden_test.go:1685: 1664 log bytes across three branches, all seven credential classes absent
--- PASS: TestAWorkerWithAPlainHandlerLogsNoCredentials (0.19s)
ok  	github.com/yegamble/vizra-core/internal/integration	1.654s
exit=0
```

## Audit of every other wait in the tests this PR added or touched

| Test | Waits on | Asserts | Verdict |
|---|---|---|---|
| `TestAWorkerWithAPlainHandlerLogsNoCredentials` (new) | ~~`attempts >= 1`~~ -> the three log lines | the log buffer | **was the defect, fixed** |
| `TestRunAfterComesFromTheDatabaseClockNotTheApplicationHost` (new) | nothing — fully synchronous | rows read straight back | no wait to get wrong |
| `startWorker` (new helper) | the `stopped` channel | that the goroutine returned | the observable itself; fails the test after 30 s |
| `TestWorkerRunsAndCompletesAJob` (touched) | both jobs `succeeded` | both jobs `succeeded` | same observable |
| `TestRetryLadderAndTerminalFailure` (touched) | `dead` / `failed` | `attempts`, `last_error` — written by the SAME statement as the state | same observable |
| `TestRetryLadderActuallyWalksTheLadder` (touched) | `queued && attempts==1`, then `dead` | `run_after`, `last_error`, handler call count | RetryJob writes state+run_after+last_error in one statement; the handler increments before it returns | 
| `TestAJobWhoseWorkerDiedIsReclaimedAndCompleted` (touched) | `succeeded` | `succeeded` | same observable |
| `TestAMultibyteErrorIsStoredInLastError` (touched) | terminal state | `last_error` — same statement | same observable |

Only the new V-2 test waited on a proxy. The others wait on exactly what they read,
or on a value the database writes in the same statement as the state they wait for.
