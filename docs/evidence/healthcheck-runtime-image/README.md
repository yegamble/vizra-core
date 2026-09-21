# `vizra healthcheck` and the runtime image — demonstrations

Branch `feat/m0-healthcheck-runtime-image`, from `origin/main` @ `4a80a1e`.
Slice: core hardening sweep B2 — meta PR #4's `vizra-infrastructure` findings
**F3** (a healthcheck that cannot fail) and **F5** (the media directory), plus
war-room board queue rows **2d** (runtime stage from a clean base, no `|| true`
purge, image-scan failing on scanner error) and **2h** (`vizra healthcheck`).

## How to read these transcripts

Each `M*` file is one controlled mutation, run by a harness that **refuses to
score a mutation that did not apply**: it records the target file's sha256
before and after, aborts if the digest did not change, runs the tests, restores
the file, and aborts if the restored digest is not byte-identical to the
original. Every transcript therefore carries four things — the two digests, the
diff, the RED run with the failing tests **by name**, and the GREEN run after
the restore.

A transcript that ends `VERDICT: red under mutation (exit N), green when
restored (exit 0)` is a demonstration. Anything else is not.

| file | mutation | what went red |
|---|---|---|
| `M1-probe-always-zero.txt` | `healthcheck.Run` returns `ExitReady` unconditionally | **11 tests**, including all five real-process ones: `TestHealthcheckApiIsZeroWhenReadyAndNonZeroWhenPostgresStops`, `TestHealthcheckApiIsNonZeroWhenTheListenerIsAbsent`, `TestHealthcheckWorkerIsZeroWhenTheLoopIsRunningAndNonZeroWhenPostgresStops`, `TestHealthcheckWorkerIsNonZeroWhenTheClaimLoopStallsWhilePostgresIsFine`, `TestHealthcheckUsageErrorsAreDistinctFromAVerdict`, and the six unit cases |
| `M2-no-staleness-bound.txt` | the worker's staleness bound is never applied (`case false:`) | `TestAClaimLoopStalledPastTheBoundIsNotReady`, `TestHealthcheckWorkerIsNonZeroWhenTheClaimLoopStallsWhilePostgresIsFine` |
| `M3-no-probe-time-ping.txt` | PostgreSQL is assumed reachable instead of pinged at probe time | `TestAnUnreachableDatabaseIsNotReadyEvenWithAFreshTick`, `TestEverySiteMustBeReadyForTheWorkerToBeReady`, `TestTheHandlerBoundsAHangingPing`, `TestHealthcheckWorkerIsZeroWhenTheLoopIsRunningAndNonZeroWhenPostgresStops` |
| `M4-scan-vacuous-os.txt` | an unrecognised OS no longer refuses the scan | `TestImageScanVerdictRefusesEveryVacuousPass/no-os-detected` |
| `M5-scan-swallows-scanner-exit.txt` | the scanner's own exit code is no longer judged | `.../scanner-error`, `.../scanner-crashed` |
| `M6-dockerfile-no-media-dir.txt` | the runtime image no longer declares `/var/lib/vizra/media` | exactly one image assertion: "could not write /var/lib/vizra/media … Directory nonexistent" |

## The real-process integration tests

`internal/integration/healthcheck_test.go` builds the **shipped** binaries and
runs them as separate processes against real PostgreSQL 18, reading their exit
codes. A test that called the probe in-process could not tell the difference
between `os.Exit(0)` and a correct verdict.

"PostgreSQL stopped" is a real TCP-level stop: the pool reaches PostgreSQL
through a proxy the test owns, and stopping it closes every established
connection and refuses new ones.

Environment for the transcripts: darwin/arm64, go1.27.1, PostgreSQL 18 and
Valkey 9.1.2 as local containers at the digests the CI workflows pin
(`postgres@sha256:86c951e0…`, `valkey/valkey@sha256:c123e371…`).

## The image assertions

`IMAGE-after.txt` and `IMAGE-before-red.txt` run the **same** assertions
`.github/workflows/docker-build.yml` runs, against images built locally.

**Platform caveat, stated plainly: these two files are linux/arm64.** This
machine is arm64 and cannot build an emulated amd64 image with the disk it has
(ADR-009). `ubuntu-24.04` / `linux/amd64` in CI is the acceptance platform; the
CI run on this PR's head is the evidence for amd64, and these files are a
local cross-check of the same assertions, not a substitute.

`IMAGE-before-red.txt` is `origin/main`'s Dockerfile at `4a80a1e` built over
**this** tree's source — so the binaries in it are the new ones, and only the
image-SHAPE assertions are a comparison. Four of them fail:

```
FAIL could not write /var/lib/vizra/media: Directory nonexistent
FAIL build tooling present: pkg-config pkgconf
FAIL -dev packages present: linux-libc-dev
FAIL /src is still in the image
```

Size, linux/arm64: **942 759 402 bytes (899 MiB) before → 232 051 189 bytes
(221 MiB) after**, −677 MiB.

Worth recording precisely, because it is the whole argument for the clean base:
on arm64 the old `apt-get purge … || true` **did** remove gcc, meson, ninja and
the rest from the final filesystem (80 packages, "After this operation, 324 MB
disk space will be freed" — `build-before.log`). The image is still 899 MiB,
because a file removed in a later layer still ships its bytes in the earlier
one. A purge cannot deliver what a clean base delivers. What the purge also
left behind, with the build green throughout: `pkgconf-bin` providing
`/usr/bin/pkg-config`, `linux-libc-dev`, and `/src` — the last because the
`vips` stage's `WORKDIR /src` is inherited by `FROM vips` and Docker recreates
the working directory at container start, so the `rm -rf /src` in the same
`RUN` was undone every time a container ran.
