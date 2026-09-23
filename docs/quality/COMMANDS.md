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

On pinned bytes it then refuses a `SHELL` / `.SHELLFLAGS` / `MAKEFLAGS` /
`GNUMAKEFLAGS` / `.ONESHELL` override, a `-`/`@-` prefix or `|| true` suffix on
a gate recipe, and a duplicate gate target — over the prerequisite closure, with
make's own `MAKEFILE_LIST` required to equal the pinned set.

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
rc=0; go test -race -count=1 -json ./... > unit-events.json || rc=$?
echo "$rc" > unit-exit.txt
python3 scripts/go-test-report.py --events unit-events.json --suite unit \
  --floors scripts/test-floors.json --go-exit-file unit-exit.txt
```

```
export VIZRA_TEST_DATABASE_URL='postgres://…' VIZRA_TEST_CACHE_URL='redis://…'
rc=0; go test -race -count=1 -tags=integration -json ./... > int-events.json || rc=$?
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
