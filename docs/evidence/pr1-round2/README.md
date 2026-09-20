# vizra-core PR #1 — review round 2 transcripts

Durable copies of the red/green evidence for the four items in the chair's
round-2 ruling on `4f8d0fc207f94a2a516607d167fcf69abd7a2148`. Captured on the
owner's machine (darwin/arm64), against PostgreSQL 18.6 and Valkey 9.1.2 in
Docker on non-default host ports.

| File | What it shows |
|---|---|
| `01-ip-prefix-RED.txt` | the five values the previous `audit_events_ip_prefix_shape` accepted, including the /96 and /112 |
| `02-ip-prefix-GREEN.txt` | the frozen grammar, all five subtests passing |
| `03-truncate-RED.txt` | `truncate` cutting a rune in half — a dangling `c3` byte |
| `04-truncate-GREEN.txt` | rune-safe `truncate`, `internal/jobs` green |
| `05-lasterror-utf8-RED.txt` | the end-to-end failure: `ERROR: invalid byte sequence for encoding "UTF8": 0xe6 0xe2 0x80`, the row left `leased` with no cause recorded |
| `06-lasterror-utf8-GREEN.txt` | 2014 bytes stored, valid UTF-8, redacted, message preserved |
| `07-verifier-mutants-R1-R2.txt` | R-1 (deleted `CheckSchema` call site) and R-2 (removed Read/WriteTimeout) both red, then green |
| `08-make-ci.txt` | the full gate |
| `09-test-integration.txt` | the integration lane, Valkey leg |
| `10-cache-matrix-redis72.txt` | the integration lane, Redis 7.2 leg |
| `11-counts.txt` | 917 tests, 0 skips, 315 frozen-matrix cases |

Round-1 transcripts are in `../pr1-round1/`.

## A note on `07-verifier-mutants-R1-R2.txt`

The R-1 section deliberately shows `internal/doctor`'s own tests still PASSING
under the mutant. That is the point: the verdicts were always covered, the
WIRING was not, and a check can vanish from what an operator sees while the
package that computes it stays green.
