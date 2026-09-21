# hardening sweep A — evidence

Branch `chore/m0-hardening-a`, base `main` at `c043df72f06cc7b5d7237a12d2be4dac25fa788e`.

Every transcript here was produced on the builder's machine — **darwin/arm64**,
GNU Make 3.81, go1.27.1 — which is **not** the ADR-009 acceptance platform.
The acceptance platform is GitHub-hosted `ubuntu-24.04`/amd64 with GNU Make 4.3,
and the CI run on this PR's head SHA is what stands as evidence there. Where the
two versions of make differ, both were measured and the difference is recorded:

| | GNU Make 3.81 (here) | GNU Make 4.3 (the runner) |
|---|---|---|
| duplicate-target warning | `overriding commands for target` | `overriding recipe for target` |
| `MAKEFLAGS` from `make -pn` | `pn` | `np` — same letters, different order |

Both wordings are matched by `scripts/make-integrity-guard.py`, and the MAKEFLAGS
check compares letter SETS, not strings, for exactly this reason. The 4.3
measurements were taken in an `ubuntu:24.04` container on this arm64 host — a
native arm64 image, not an emulated amd64 one.

| File | What it shows |
|---|---|
| `01-vectors-BEFORE.txt` / `02-vectors-AFTER.txt` | `api/search-hmac-testvectors.json` before and after the rename: exactly one line changed, all 5 positive and 24 negative vectors byte-identical, and the canonical digest of the whole document minus `key_utf8_warning` unchanged (`411f7e2e…` both sides) |
| `03-manifest-repin.txt` | the fixtures manifest re-pin: all twelve fixture entries byte-identical, only `generator.source_sha256` moved |
| `D1-retired-name.txt` | the production refusal of a leftover `VIZRA_SEARCH_HMAC_KEY`, red against a controlled mutation |
| `D2-fixtures-verify.txt` | verifier findings V-2 and V-3 reproduced through the CLI, red, and the dotfile exemption shown still green |
| `D3-make-integrity.txt` | six Makefile mutations and six workflow mutations, each with a real failing test underneath |
| `D4-provenance.txt` | the merge-ref provenance step across depth-1, depth-2, pinned-to-head, mismatched-head, `merge_group` and `push` |

**How to read a demonstration.** Every mutation prints the short sha256 of each
file it touches BEFORE and AFTER, and `D3` aborts if a digest did not move —
a mutation that silently failed to apply must not be reported as a pass. Every
red is quoted with the reason it printed, not merely its exit code.

**What is NOT here.** No transcript in this directory is evidence about the
acceptance platform, and none of it is a verification. Implementation state and
verification state are different things (AGENTS.md § Evidence); this PR is
READY_FOR_REVIEW.
