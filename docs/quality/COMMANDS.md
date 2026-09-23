# Verification commands — `vizra-core`

The actual commands this repository's required checks run, and what each one
proves. The meta repository's `AGENTS.md` § Required workflow point 3 points at
`docs/quality/COMMANDS.md`; the meta repo's own copy is **explicitly scoped to
the meta repository** and says it lists nothing for the component repos. This
file is `vizra-core`'s.

**Only commands that exist here and that were actually run are listed.** A
missing command or dependency is BLOCKED, never a pass.

## Prerequisites

| Requirement | Used for | Checked with |
|---|---|---|
| Go, the toolchain `go.mod` pins | everything | `go version` — must equal `go.mod`'s `toolchain` line; `build-test` asserts this |
| `python3` ≥ 3.9 | both guards, the test report, the image-scan verdict | `python3 --version` |
| PyYAML | `ci-required-guard.py` parses workflows rather than grepping them | `python3 -c 'import yaml; print(yaml.__version__)'` |
| sqlc 1.31.1 | `make sqlc-verify` | `sqlc version` |
| Docker | `docker-build`, `image-scan`, and the integration services | `docker version` |
| PostgreSQL 18 + a RESP server | the integration suite | the suite **fails rather than skips** without them |

## The gate

```
make ci
```

Runs exactly what `ci-required` runs: `fmt-check vet lint-imports migrate-lint
config-template-check openapi-verify sqlc-verify ci-guard fixtures-verify
test-race`.

## The two out-of-make guards

`make ci` is make-driven, so the Makefile and the workflow lines that invoke it
are gated from outside make. CI runs both as their own steps, before any `make`.

```
./scripts/make-integrity-guard.sh --workflow   # STRICT: exactly as CI's pinned anchor runs it
./scripts/make-integrity-guard.sh              # local-parity mode, what `make ci-guard` runs
./scripts/ci-required-guard.sh         # the manifest, the workflows, and each make step's own argv
```

`make-integrity-guard` runs make only on **reviewed Makefile bytes** (sweep B5).
Before invoking make at all, in both modes, it checks every file make will read
against its sha256 in `.github/pinned-makefiles.yml`; what make will read is
determined from the pinned bytes without running them (the root `Makefile` plus
every literal `include`/`-include`/`sinclude`/`load`, transitively — sound only
because those bytes are themselves pinned), and every text check (SHELL /
MAKEFLAGS assignments, recipe prefixes and suffixes, duplicate gate targets)
runs on those files before make too. Its first make invocation is ONE
`make -q` naming every pinned makefile as a goal — `-q` applies in make's
remake phase only to goals — to refuse a makefile make would REMAKE from
something unpinned (it does that even under `-n`). `-q` runs no ordinary
recipe, but a `+` or `$(MAKE)` recipe line still runs under it; such a line can
come only from the pinned, reviewed bytes.
Every process it starts gets an environment without `GITHUB_ENV`,
`GITHUB_PATH`, `GITHUB_OUTPUT`, `GITHUB_STATE`, `GITHUB_STEP_SUMMARY` or any other
runner command-file variable. The reviewed bytes still run their own
`$(shell git rev-parse …)` / `$(shell date …)` at Makefile:22-23; the residual is a
reviewer approving a malicious Makefile together with its pin, and CODEOWNERS is
advisory. Changing the Makefile:

```
shasum -a 256 Makefile      # paste into .github/pinned-makefiles.yml, same diff
```

**The allowlist grammar is the primary pre-make control** (queue 2p, core
B5d; ported from vizra-search `scripts/makegate.py` at `4810048`). Before make,
in both modes and in `ci-required-guard` check 11 (both call
`scripts/makefile_pin.py` `verify_pin`), every logical line of every pinned
makefile must be one of five shapes, or it is refused by file and line number
with make not started:

- empty, or a `#` comment in column 0 (a comment continued by a trailing
  backslash is refused);
- a column-0 literal assignment `NAME :=` / `?=` / `=` whose NAME is not a
  directive keyword and whose value uses only `$$`, `$(NAME)` / `${NAME}` and a
  `$(shell …)` (listed on the ok line as a reviewed parse-time call);
- `.PHONY: names`;
- a rule with ONE literal target and literal prerequisites, on one physical
  line;
- a TAB recipe line of such a rule, using only `$$` and `$(NAME)` / `${NAME}`.

Plus byte refusals anywhere: a carriage return, NUL, any other control
character, an invisible format (Cf) character, and non-ASCII whitespace.
Everything else — `include` and every other directive, conditionals, `define`,
`export`, `override`, target- and pattern-specific assignments, `+=` / `!=`,
special targets other than `.PHONY`, functions and substitution references,
inline `;` recipes, multi-target, double-colon and pattern rules, and every
character above — is outside the grammar. The real Makefile fits it (64
blank/comment, 11 assignment, 25 phony, 25 rule, 72 recipe lines since the
test-stability slice added its `-timeout` comment; the anchor's ok line prints
the counts). Because `include` is refused, the pinned
read set is the root `Makefile` alone; the include reading and the post-make
checks below remain as defence in depth.

Every text reading of a makefile — the grammar, the static read set, the
parse-time sites, the anchor's closure, recipe, assignment and definition
readings, and `ci-required-guard`'s env-name and PKGS / test-race readings —
consumes ONE line sequence, `makefile_pin.makefile_lines`, so no two readers
can disagree on where a line or a recipe ends.
`scripts/testdata/one-reader-probe.py` proves it three ways (POISON: rewriting
the text inside `makefile_lines` changes every reader's verdict; SOURCE: no
reader splits or decodes text itself, and every such spelling in the three
files is a named read of a non-makefile input; IDENTITY: every reader of one
text gets the same sequence object).

Then, as the SECOND diagnosis on the same text (the gate has already failed),
it refuses by name: a `SHELL` / `.SHELLFLAGS` /
`MAKEFLAGS` / `GNUMAKEFLAGS` / `MFLAGS` assignment in any form (global with any
modifier, `define`, target- or pattern-specific) other than the two approved
global ones; `.ONESHELL`; any mention of `.RECIPEPREFIX`, `.SECONDEXPANSION`,
`.IGNORE`, `.DEFAULT` (whole word), `.EXTRA_PREREQS` or `.POSIX`; a pattern
rule; a rule with an INLINE `;` recipe; a MULTI-TARGET rule line; a `$`-named
prerequisite on a gate closure rule; a LITERAL `-`/`@-`/`+` prefix,
`|| true`-family suffix or `$(MAKE)` on a gate recipe line; a duplicate or
conditional gate target; and a gate closure target with no explicit rule or
not declared `.PHONY`. **No gate lane may depend on a real file target**:
every target the gate closure reaches must be an explicit, phony rule. So
every recipe the closure reaches is scanned — GNU make skips implicit-rule
search for phony targets (measured on 3.81 and 4.3), so the recipe is on an
explicit rule, and with inline and multi-target forms refused it is on the
TAB lines under that target's own rule line, which the text reading scans
before make; a rule whose TARGET make computes is refused before make too
(#11 fix round 2). After make, and failing CLOSED, the closure make reports
from its own prerequisite lists must EQUAL the text closure, each closure
target must have exactly one readable entry in make's `-pn` database, and that
entry's recipe must EQUAL the pinned rule's TAB lines (whitespace collapsed) —
so every recipe of every target make reaches is one the text reading scanned
(in-process rows: `scripts/testdata/db-scan-probe.py`). The expanded-prefix
line count differed between versions before that normalisation — 66 on GNU
Make 3.81, 62 on 4.3, because 3.81's database joins four continued recipe
lines of this Makefile with different indentation (#11 R1-2); both now read 62.
After make has run on the pinned bytes, the resolver refuses what only make can
see: a computed variable name that sets SHELL / MAKEFLAGS / `.RECIPEPREFIX` /
`.EXTRA_PREREQS` or declares `.SECONDEXPANSION` / `.IGNORE` / a `.DEFAULT`
recipe, a closure target missing from make's own `.PHONY` list, a `-`/`+`
prefix a leading variable expands to (a
leading function or target-specific variable is refused as undeterminable), a
`|| true` suffix in the expanded dry-run command, and a `MAKEFILE_LIST` that is
not the pinned set. Since the grammar, every committed route to these
post-make branches is refused before make, so their refusals are exercised
in-process by `scripts/testdata/db-scan-probe.py` (26 rows). **What the grammar
does not see:** make's BUILT-IN implicit rules and variables — no makefile line
names them. They are answered by the one `make -q` (a pinned makefile make
would remake) and by the rule that every gate closure target is an explicit
`.PHONY` rule, not by the grammar. The value of any OTHER variable (`GO`, `PKGS`, …) in the
pinned bytes is not checked here: review is the control for that.

`ci-required-guard` runs its checks over `set(FLOOR_LANES) | set(required)`.
Since sweep B1 round 2 the make-step control is **default-deny on the shape**,
not a blacklist of shell spellings — a verifier found thirteen spellings the
flag-parsing version could not see, and a blacklist over arbitrary shell cannot
be exhaustive:

* **8b** — a step whose `run:` mentions `make` must be **byte-equal** to a
  literal in `.github/pinned-steps.yml`, may carry no key but `name`/`run`/`id`,
  and must be **immediately** preceded by the anchor.
* **8c** — the lane must actually RUN the invocations that file records for it.
  This is the positive half: it closes indirection (`M=make; $M -i ci`) without
  parsing shell, because it does not care what replaced them.
* **9** — the direct test steps are pinned the same way, including their exit
  handling.
* **10** — a lane that checks out runs `scripts/provenance.sh`.
* **11** — `.github/pinned-makefiles.yml` exists, has its one accepted shape,
  pins `Makefile`, and matches the tree: a Makefile edit without the paired pin
  update fails by name. It calls the anchor's own `scripts/makefile_pin.py`, so
  it refuses whatever the anchor refuses before make
  (`scripts/testdata/makefilepin/`, 12 fixtures).

It also refuses a job-level `container:`, a `defaults.run` at either scope, an
undigested service image, and a MAKEFLAGS/GNUMAKEFLAGS/MFLAGS/MAKEFILES/SHELL/
PATH/BASH_ENV/ENV `env:` at job or workflow level. Its own negative cases are
`scripts/testdata/guard/` (75 fixtures), driven by `scripts/scripts_test.go`.

With `--workflow` — the only form a floor lane may use, because the anchor
step is pinned byte-equal — `make-integrity-guard` also checks its own process
in STRICT mode. The mode is chosen by that argument, never by the environment.
It requires the MAKEFLAGS family to be unset; make's recipe variables
(MAKELEVEL, MAKE_RESTARTS, MAKEOVERRIDES, MAKECMDGOALS) to be absent; every
variable the makefiles take from the environment (`?=`, or referenced but never
assigned) to be absent; MAKEFILES/BASH_ENV/ENV to be unset; `SHELL` to be a real
shell; and `make` to resolve to a file named make in a system directory. It
does not inspect that file's contents. Adjacency is what makes this meaningful:
a `$GITHUB_ENV`/`$GITHUB_PATH` write applies to LATER steps. Without
`--workflow`, as `make ci-guard` runs it, MAKEFLAGS may carry only the words
GNU make itself was measured to export. That mode is local parity, not a
control.

## The suites, the way CI runs them

CI does not run `make test-race` or `make test-integration` in `build-test`: it
invokes both suites directly, with no make, and judges the machine-readable
results — because `go test ./...` exits 0 having run nothing, and a non-verbose
`go test` prints nothing at all for a skipped test.

```
rc=0; go test -race -count=1 -timeout 8m -json ./... > unit-events.json || rc=$?
echo "$rc" > unit-exit.txt
python3 scripts/go-test-report.py --events unit-events.json --suite unit \
  --floors scripts/test-floors.json --go-exit-file unit-exit.txt
```

```
export VIZRA_TEST_DATABASE_URL='postgres://…' VIZRA_TEST_CACHE_URL='redis://…'
rc=0; go test -race -count=1 -timeout 8m -tags=integration -json ./... > int-events.json || rc=$?
echo "$rc" > int-exit.txt
python3 scripts/go-test-report.py --events int-events.json --suite integration \
  --floors scripts/test-floors.json --go-exit-file int-exit.txt
```

The report judges `go test`'s own exit code, names every failure and every skip,
fails on any skip not allowlisted by test name **with a reason**, and fails below
the floors recorded in `scripts/test-floors.json` — whole-suite **and one per
package, for every package in both suites**, because a whole-suite floor does
not detect one package disappearing. Regenerate the per-package block after a
measured run with `--emit-floors`. The recipes keep their own required coverage in both
`cache-matrix` legs.

## The image assertions

```
./scripts/assert-runtime-image.sh vizra-core:ci
```

Every `docker run` exit is captured and judged, so the assertions cannot pass on
a container that never ran. `$DOCKER` is injectable; `scripts/testdata/fakedocker/`
drives it with no daemon at all.

```
python3 scripts/image-scan-verdict.py --report trivy-image.json \
  --scanner-exit-code-file trivy-exit-code.txt --image-ref vizra-core:scan \
  --fail-on HIGH,CRITICAL
```

Exit 0 = clean, 1 = findings at or above the threshold, **3 = there was no valid
scan** — and 3 is never to be read as "clean".

## Last measured run

Host `darwin/arm64`, GNU Make 3.81 (CI has 4.3), `go1.27.1`, python3 3.9.6 +
PyYAML 6.0.3, sqlc v1.31.1, Docker 29.8.0. Tree
`chore/m0-hardening-b1`, fix round 1, counts from `go test -count=1 -json` as printed by
`go-test-report.py`. **This machine is not the ADR-009 acceptance platform;
CI `ubuntu-24.04` is.**

| Command | Exit | Detail |
|---|---|---|
| `make ci` | 0 | 10 lanes; `test-race` 14 ok, 8 `[no test files]`, 0 FAIL |
| `./scripts/make-integrity-guard.sh` | 0 | `passed (8 gate target(s))` |
| `./scripts/ci-required-guard.sh` | 0 | `passed (6 required check(s))` |
| unit suite + report | 0 | **1109 executed, 0 skipped**, floor 943; per-package floors for all 14 packages (CI, `d435cd8`) |
| integration suite + report | 0 | **1154 executed, 0 skipped**, floor 981; per-package floors for all 15; `internal/integration` 45, floor 40 (CI, `d435cd8`) |
| `go test -race -count=1 ./scripts/` | 0 | 75 guard + 12 gotest + 6 fakedocker + 16 imagescan fixtures; `TestMakeIntegrityGuardEnvironment` 27 rows |

The red/green transcripts for every control are in
`docs/evidence/hardening-b1/`, produced by `mutate.sh`, which aborts unless the
mutation's digest moved and the restore is byte-identical.

### Sweep B5 (the Makefile digest pin), measured on tree `36c3f7c`

Same host, GNU Make 3.81; GNU Make 4.3 in an `ubuntu:24.04` container for the
anchor, both modes, and check 11 (`docs/evidence/hardening-b5/make-4.3-ubuntu24.04/`).
CI could not run (GitHub Actions refused jobs for billing), so nothing here is
CI-corroborated.

| Command | Exit | Detail |
|---|---|---|
| `make ci` | 0 | 10 lanes; `test-race` 14 ok, 8 `[no test files]`, 0 FAIL |
| `./scripts/make-integrity-guard.sh --workflow` / no flag | 0 / 0 | `passed (8 gate target(s); make ran 18 time(s), only on the pinned bytes of Makefile)` |
| `./scripts/ci-required-guard.sh` | 0 | check 11: `pins 1 makefile(s) (Makefile), covers the Makefile, and every digest matches the tree` |
| direct unit step + `go-test-report.py` | 0 | **1149 executed, 0 skipped**, floor 943; `scripts` 229 (floor 161) |
| `go test -race -count=1 ./scripts/` | 0 | 229 pass, 0 fail, 0 skip |

Red/green transcripts: `docs/evidence/hardening-b5/` (`demo.sh`, over the B1
`mutate.sh`).

### Sweep B5 fix round 1, measured on tree `ca9b21e` (scripts/ tree `70d09e55…`)

Same host, heavily loaded (load average ~360). CI could not run (billing).

| Command | Exit | Detail |
|---|---|---|
| `make ci` | **2** | every lane before `test-race` passed (both guards included); `test-race`: 13 ok, 8 `[no test files]`, and `internal/fixtures` **timed out** (`panic: test timed out after 10m0s` in `TestManifestDetectsEveryClassOfDrift`) — an infrastructure timeout under host load, NOT a pass. This slice changes nothing under `internal/` |
| `go test -race -count=1 -timeout 20m ./internal/fixtures/` | 0 | `ok … 409.522s` |
| direct unit step + `go-test-report.py` | 0 | **1157 executed, 0 failed, 0 skipped**, floor 943, 14 packages; `internal/fixtures` 42, `scripts` 237 |
| `go test -race -count=1 ./scripts/` | 0 | 237 pass, 0 fail, 0 skip |
| `./scripts/make-integrity-guard.sh --workflow` / no flag | 0 / 0 | |
| `./scripts/ci-required-guard.sh` | 0 | |
| GNU Make 4.3 (`ubuntu:24.04` container): both anchors, ci-required-guard, D0–D6, C7, P1 | 0 / 0 / 0 / as expected | `docs/evidence/hardening-b5/make-4.3-ubuntu24.04/` |

### Sweep B5 fix round 2, measured on scripts/ tree `7d236f17…`

Same host (GNU Make 3.81, go1.27.1); CI could not run (billing).

| Command | Exit | Detail |
|---|---|---|
| `make ci` | 0 | all 10 lanes; `test-race` 14 ok, 8 `[no test files]`, 0 FAIL (`internal/fixtures` 320s) |
| direct unit step + `go-test-report.py` | 0 | **1206 executed, 0 failed, 0 skipped**, floor 943, 14 packages; `scripts` 286 |
| `go test -race -count=1 ./scripts/` | 0 | 286 pass, 0 fail, 0 skip |
| `./scripts/make-integrity-guard.sh --workflow` / no flag | 0 / 0 | |
| `./scripts/ci-required-guard.sh` | 0 | |
| GNU Make 4.3 (`ubuntu:24.04` container): both anchors, ci-required-guard, D0–D7, C7, C10, C10b, P1, all 46 makeguard fixtures | 0 / 0 / 0 / as expected | `docs/evidence/hardening-b5/make-4.3-ubuntu24.04/` |

### Slice B5b (special targets; the closure reaches only explicit, phony rules), measured on scripts/ tree `6e3ed51e…`

Same host (GNU Make 3.81, go1.27.1). Not pushed at the time of measurement (the chair holds it until PR #10 merges).

| Command | Exit | Detail |
|---|---|---|
| `make ci` | 0 | all 10 lanes; `test-race` 14 ok, 8 `[no test files]`, 0 FAIL |
| direct unit step + `go-test-report.py` | 0 | **1230 executed, 0 failed, 0 skipped**, floor 943; `scripts` 310 |
| `go test -race -count=1 ./scripts/` | 0 | 310 pass, 0 fail, 0 skip |
| `./scripts/make-integrity-guard.sh --workflow` / no flag | 0 / 0 | |
| `./scripts/ci-required-guard.sh` | 0 | |
| `docs/evidence/hardening-b5/b5b/demo.sh` + `measure.sh` on 3.81 | 0 | D rows HELD; C15–C21 red, green after restore |
| GNU Make 4.3 (`ubuntu:24.04` container): D rows, C15/C15b, `measure.sh`, both anchors, ci-required-guard, all 57 makeguard fixtures | as expected | `docs/evidence/hardening-b5/b5b/make-4.3-ubuntu24.04/`; 55 red, 2 green |

### #11 fix round 1 (cross-check X-1, B5c), measured on scripts/ tree `173c57f1…`

| Command | Exit | Detail |
|---|---|---|
| `make ci` | 0 | all 10 lanes; `test-race` 14 ok, 8 `[no test files]`, 0 FAIL |
| direct unit step + `go-test-report.py` | 0 | **1247 executed, 0 failed, 0 skipped**, floor 943; `scripts` 327 |
| `go test -race -count=1 ./scripts/` | 0 | 327 pass, 0 fail, 0 skip |
| `./scripts/make-integrity-guard.sh --workflow` / no flag | 0 / 0 | |
| `./scripts/ci-required-guard.sh` | 0 | |
| `b5b/demo.sh` on 3.81 | 0 | 13 D rows HELD; C15–C26 red, green after restore |
| GNU Make 4.3 container: D rows, C15/C15b, C22–C26, `measure.sh`, both anchors, the guard, all 62 makeguard fixtures | as expected | 60 red, 2 green |

### #11 fix round 2 (R1-1, R1-2), measured on scripts/ tree `4be7543e…`

| Command | Exit | Detail |
|---|---|---|
| `make ci` | 0 | all 10 lanes; `test-race` 14 ok, 8 `[no test files]`, 0 FAIL |
| direct unit step + `go-test-report.py` | 0 | **1252 executed, 0 failed, 0 skipped**, floor 943; `scripts` 332 |
| `go test -race -count=1 ./scripts/` | 0 | 332 pass, 0 fail, 0 skip |
| `./scripts/make-integrity-guard.sh --workflow` / no flag | 0 / 0 | database scan: 17 targets, 62 recipe lines equal; closure make reports = text closure; 62 distinct expanded-prefix lines on 3.81 AND 4.3 |
| `./scripts/ci-required-guard.sh` | 0 | |
| `python3 scripts/testdata/db-scan-probe.py` | 0 | 15 rows as expected (3.81 host and 4.3 container) |
| `b5b/demo.sh` on 3.81 | 0 | 13 D rows HELD + probe; C15–C32 red, green after restore |
| GNU Make 4.3 container: D rows, C15/C15b, C22–C25, C27–C32, both anchors, the guard, all 62 makeguard fixtures | as expected | 60 red, 2 green |

### #11 on main (merge of #10's squash `36a72df`, with R2-1), measured on scripts/ tree `0647549c…`

| Command | Exit | Detail |
|---|---|---|
| `make ci` | 0 | all 10 lanes; `test-race` 17 ok, 8 `[no test files]`, 0 FAIL. A first run exited 2: `link: mapping output file failed: no space left on device` for `internal/httpapi` and `internal/jobs` — a host disk-exhaustion failure, NOT a pass; the re-run on the same tree passed |
| direct unit step + `go-test-report.py` | 0 | **1328 executed, 0 failed, 0 skipped**, floor 1006, 17 packages; `scripts` 333 |
| `go test -race -count=1 ./scripts/` | 0 | 333 pass, 0 fail, 0 skip |
| `./scripts/make-integrity-guard.sh --workflow` / no flag | 0 / 0 | 17 closure targets, 62 recipe lines equal, closures equal |
| `./scripts/ci-required-guard.sh` | 0 | |
| `python3 scripts/testdata/db-scan-probe.py` | 0 | 15 rows as expected |
| `b5b/demo.sh` on 3.81 | 0 | 13 D rows HELD + probe; all 21 C rows red, green after restore (`b5b/on-main-36a72df/`) |

### Queue 2p (core B5d: the allowlist grammar, one line reader), measured on scripts/ tree `925bdc1c…` (tree `c6170f0`)

| Command | Exit | Detail |
|---|---|---|
| `make ci` | 0 | all 10 lanes; `test-race` 19 ok, 8 `[no test files]`, 0 FAIL (`internal/fixtures` 534.8s) |
| direct unit step + `go-test-report.py` | **1 — FAIL, not a pass** | 1459 executed, 0 failed, 0 skipped, floor 1006; `scripts` 493. `internal/fixtures` hit `panic: test timed out after 10m0s` (host load average ~315 from other agents' work) and so ran 13 of its floor of 36 tests. No code under `internal/` changed in this slice. Re-run alone: `go test -race -count=1 -timeout 30m ./internal/fixtures/` exit 0 (1015.9s). CI (ubuntu-24.04) is the clean target for this lane |
| `go test -race -count=1 -v ./scripts/` | 0 | 39 top-level tests, 493 PASS lines with subtests, 0 FAIL, 0 SKIP |
| `./scripts/make-integrity-guard.sh --workflow` / no flag | 0 / 0 | the grammar ok line: 56 blank/comment, 11 assignment, 25 phony, 25 rule, 72 recipe; 8 gate targets, 17 closure targets, 62 recipe lines equal |
| `./scripts/ci-required-guard.sh` | 0 | check 11: "every line of which fits the allowlist grammar" |
| `python3 scripts/testdata/db-scan-probe.py` | 0 | 26 rows as expected |
| `b5d/demo.sh` on 3.81 | 0 | D rows held (rows.py 84/84, 80 refusals + 4 controls); C33–C41 red, green after byte-identical restore; T0: the new tests red against `96d19b3` without the implementation (11 top-level tests FAIL) |
| GNU Make 4.3 (`ubuntu:24.04` container): `inside-4.3.sh` | as expected | D rows held, rows.py 84/84, C33–C37 and C41 red then green, both anchors 0, ci-required-guard 0, makeguard fixtures 66 refused / 1 green (`good`) |

The include-based rows of the earlier B5 demonstrations (D3, D6, C7 in
`hardening-b5/`) now meet the grammar's `include` refusal before they reach
the checks they were written for. Those checks stay in the code as defence in
depth; `db-scan-probe.py` and `TestAMakefileMakeWouldRemakeIsRefusedWithoutRunningARecipe`
cover them.

### Test stability (sentinel S-0016 / S-0001), measured on tree `d817f33`

Every `go test` lane — the Makefile's `test`, `test-race`, `test-integration`,
`test-integration-shuffle`, the three pinned direct steps and the `fixtures`
workflow's `internal/fixtures` step — passes `-timeout 8m`. The value comes from
measurement (`docs/evidence/test-stability/timings.txt`), not from a guess:

| Measurement | Slowest package | 8m is |
|---|---|---|
| CI, 4 green build-test runs before the change (35914283132, 35914133999, 35910352499, 35899392555) | `internal/fixtures` 143.5s | 3.3x |
| CI, 12 green build-test runs before the change | `internal/fixtures` 146.1s | 3.3x |
| this host, two suites at once, after the change | `internal/integration` 186.4s | 2.6x |
| this host, one lane, after the change | `internal/integration` 142.1s | 3.4x |
| this host, one lane, merged head `888a003` at load ~44 | `internal/integration` 201.2s | 2.4x |
| CI on `888a003` (build-test + both cache-matrix legs) | `internal/integration` 112.5s | 4.3x |

It is below go's 10m default on purpose: the last test step of `build-test` starts
about 8.7 minutes into a 20-minute job, and the second step of `cache-matrix-leg`
about 3.7 minutes into a 15-minute one, so 8m still prints go's goroutine dump for
a hang before the job is killed. A host whose load average is several times its
core count can still exceed it; that is contention, not a property of the
suite, and `go test -timeout` can be given directly there.

Limits, at the strength that holds:

- The flag is enforced by byte-equality only in the three pinned direct steps
  (`.github/pinned-steps.yml`, check 9). On the Makefile recipes and on the
  `fixtures` workflow's step it is kept by review alone: no guard checks it, and
  a re-pinned Makefile without it passes every anchor.
- A test binary killed by go's `-timeout` leaves the processes it started
  running (the integration tests' `go build`). Such an orphan can still be
  writing into its root when the next run sweeps it, and can re-create part of
  it. The leak test avoids this by killing the whole process group; a real
  timeout does not.
- The sweep decides "dead" by asking the local kernel about the PID in the
  root's name. Runs in different PID namespaces that share one TMPDIR (a
  container with the host's /tmp mounted) cannot see each other's processes, so
  one can remove the other's LIVE root. That set-up is not supported.
- `TestAliveTellsALiveProcessFromADeadOne` checks the "another user's live
  process" branch through PID 1, which answers EPERM only to a non-root caller.
  Run as root it passes without exercising that branch, and mutation T4 would
  survive.

What was cut: `TestManifestDetectsEveryClassOfDrift` generated the corpus 6 times
(three of them only to put files on disk for a mutation) and ran its cases one
after another. Its mutations now copy the shared corpus, its cases run in
parallel, and the four heavy fixtures tests run in parallel with each other; the
package generates the corpus 6 times instead of 9 (each 13-35s under `-race`).

| Lane (own TMPDIR, own containers) | before `3994893` | after `d817f33` |
|---|---|---|
| `make ci` | 107s, load ~12 | 89s, load ~20 |
| direct unit step + report | 134s, load ~12 | 109s, load ~20–50 |
| direct integration step, Valkey 9.1.2 | 156s, leaves 74 MB | 150s, leaves nothing |
| direct integration step, Redis 7.2 | 168s, leaves 75 MB | 144s, leaves nothing |
| unit + integration(Valkey) at the same time | 238s / 236s | 145s / 198s |
| CI `internal/fixtures` per package (the 4 runs above) | 77.4–143.5s | 56.5–98.2s |
| CI `internal/integration` per package (the 4 runs above) | 54.9–99.5s | 71.2–112.5s |
| CI build-test job (the 4 runs above) | 7.8–10.8 min | 7.4 min |
| CI cache-matrix-leg jobs (the 4 runs above) | 4.5–5.8 min | 4.2–4.5 min |

Temporary files: every package with a TestMain here runs inside ONE root from
`internal/testtmp` (`vizra-test-<pkg>-<pid>-*`, TMPDIR pointed at it), removed at
the end of the run. A run killed before that (go's `-timeout`, an OOM kill, a job
timeout) leaves its root, and the next run of the same package removes it once
its PID is gone. `TestTheFixturesTestsLeaveNoTemporaryEntry` and
`TestTheIntegrationTestsLeaveNoTemporaryEntry` run the package's own test binary
with a TMPDIR only they own; both are red on `3994893`
(`docs/evidence/test-stability/host-darwin-arm64/R0-red-on-main.txt`), and T1–T4
in the same directory are the byte mutations that turn them red again.
