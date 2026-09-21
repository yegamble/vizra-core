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
| `D3-make-integrity.txt` | six Makefile mutations and six workflow mutations against a clean CLONE, each with a real failing test planted underneath. Ends with a note on exactly what line (a) shows for each mutation — `make ci` exits 0 for M1–M3 and 2 for M4–M6, for different reasons, and the difference matters |
| `D4-provenance.txt` | the merge-ref provenance step across depth-1, depth-2, pinned-to-head, mismatched-head, `merge_group` and `push` |

**How to read a demonstration.** Every mutation prints the short sha256 of each
file it touches BEFORE and AFTER, and `D3` aborts if a digest did not move —
a mutation that silently failed to apply must not be reported as a pass. Every
red is quoted with the reason it printed, not merely its exit code.

**What is NOT here.** No transcript in this directory is evidence about the
acceptance platform, and none of it is a verification. Implementation state and
verification state are different things (AGENTS.md § Evidence); this PR is
READY_FOR_REVIEW.

---

## Note, 2026-09-21 (round 2, docs-only commit)

The verifier's PASS at `f56dc03` came with four disclosure findings. They were
addressed in a documentation-and-comments-only commit on top; **no code, test,
workflow, fixture or manifest byte changed** (the two `.py` files' executable
ASTs are identical with docstrings stripped — see the commit message).

One consequence for a reader comparing digests: `D3-make-integrity.txt` line 9
records

```
      3421fd3e75d321b7  scripts/make-integrity-guard.py
```

as the baseline digest of the file **at `f56dc03`**, which is the SHA that
transcript was produced at and the right value for it. The docstring gained a
residual bullet in the next commit, so that file's digest at the branch head is
no longer `3421fd3e…`. Nothing pins or enforces it — it is a transcript of a
historical run, and it has deliberately not been rewritten. The demonstration's
before/after digest PAIRS inside the transcript are internally consistent and
unaffected: each mutation is compared against the baseline captured in that same
run.

The residual list those findings produced now lives, identically, in three
places: `AGENTS.md` ("What these two controls do NOT give you"), the
"WHAT IT DOES NOT GUARANTEE" docstring of `scripts/make-integrity-guard.py`, and
the PR body. The short version: **one word on a workflow line**
(`run: make -i ci`, `make SHELL=/usr/bin/true ci`, `make MAKEFLAGS=-i ci`, or a
step-level `env: MAKEFLAGS: -i`) still no-ops every make-driven lane with both
guards green; control 2 covers the **unit** suite only and does not fail on zero
tests run; and `append-only` has no provenance step. All are queued for core
hardening sweep B and none is implemented in this PR.
