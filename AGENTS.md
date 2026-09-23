# vizra-core — repository engineering contract

This file is the `vizra-core` half of the contract. The other half is the meta
repository's `AGENTS.md`, which binds every Vizra repository and takes
precedence. Read that first; read this before editing anything here.

`vizra-core` is one Go module, one image and three entry points:

| Entry point | Responsibility | Hard rule |
|---|---|---|
| `cmd/api` | the HTTP API | **never decodes pixels** (ADR-002, Q-034) |
| `cmd/worker` | durable jobs, and the only process that runs libvips | every handler is idempotent |
| `cmd/vizra` | the operator CLI: setup, doctor, migrate, backup, restore, **healthcheck** | `doctor` performs real checks only; `healthcheck` cannot pass while the service it probes is broken |

It also owns three things the other repositories consume and must never edit:
`api/openapi.yaml`, `api/search-internal.openapi.yaml`, and `migrations/`.

## The gate

`make ci` is the contract. It runs exactly what `ci-required` runs:

```
fmt-check  vet  lint-imports  migrate-lint  config-template-check
openapi-verify  sqlc-verify  ci-guard  fixtures-verify  test-race
```

A missing tool is a FAILURE, never a silent skip. `make sqlc-verify` without
sqlc installed exits 1 and says the lane is BLOCKED — because a lane that did
not run is not a pass. The same applies to `python3` and PyYAML, which the gate
guard needs.

### The gate is make-driven, so the Makefile itself is gated

`make ci` is the contract, and that is also its weak point: ONE line —
`SHELL := /usr/bin/true`, or `MAKEFLAGS += -i` — makes every recipe in this
repository exit 0 without running (measured on GNU Make 3.81 and 4.3). No check
written inside a Makefile can prevent it, because the neutering disarms that
check too. Two out-of-make controls close it:

1. **`scripts/make-integrity-guard.sh`** runs as its own workflow step, BEFORE
   any `make` line, in every required lane that invokes make. It refuses a
   `SHELL` / `.SHELLFLAGS` / `MAKEFLAGS` / `GNUMAKEFLAGS` / `.ONESHELL`
   override, a `-`/`@-` prefix or `|| true` suffix on a gate recipe, and a
   duplicate gate target — in the Makefile **and everything it includes**, with
   the include list taken from make's own `MAKEFILE_LIST`.
2. **`build-test` runs `go test -race -count=1 ./...` directly**, with no make,
   so whatever `make ci` did, a real failing **unit** test still fails a
   required lane. It carries no `-tags=integration`, so it is the UNIT suite
   only — see the residual list below.

`ci-required-guard.py` asserts both are present and armed.

#### What these two controls do NOT give you

This list is meant to be exhaustive. If you find something that belongs on it
and is not here, that is a defect in this section, not a detail.

- **The guard never reads the workflow's own `make` invocation.** It checks the
  Makefile, its includes, and its OWN environment — it cannot see the argv or
  the step-level `env:` of a different workflow step, and `ci-required-guard.py`
  checks a make step's presence, position, `if:` and `continue-on-error` but
  never the TEXT of its `run:`. So **one word on a workflow line** still
  no-ops every make-driven lane with both guards exiting 0. Four spellings,
  all measured green at `f56dc03`: `run: make -i ci`,
  `run: make SHELL=/usr/bin/true ci`, `run: make MAKEFLAGS=-i ci`, and a
  step-level `env: MAKEFLAGS: -i` on an otherwise ordinary make step.
  Blast radius: everything make-driven goes silent — `fmt-check`, `vet`,
  `lint-imports`, `migrate-lint`, `config-template-check`, `openapi-verify`,
  `sqlc-verify`, `ci-guard`, `fixtures-verify`, `tidy-check`, `build`, and
  **both integration lanes, including both `cache-matrix` legs**. Only the unit
  suite survives, through control 2. Closing it — the guard refusing
  flag/variable overrides on a make step's `run:` and a `MAKEFLAGS`-family
  step-level `env:` — is queued for **core hardening sweep B**; it is
  deliberately not implemented here.
- **Control 2 covers the UNIT suite only.** Every integration invocation in this
  repository goes through make (`make test-integration`,
  `make test-integration-shuffle`, in `build-test` and in both `cache-matrix`
  legs). Under the evasion above, a failing INTEGRATION test — the migrator
  against real PostgreSQL 18, the permanent Valkey/Redis-7.2 matrix that
  ADR-001 Q-004 exists for — is silent, not red.
- **Control 2 does not fail when zero tests run.** `go test ./...` with every
  `*_test.go` moved aside exits 0, reporting `[no test files]` per package
  (measured at `f56dc03`). It is a control against make being neutered, not
  against the suite being EMPTIED. Queued for sweep B.
- **`append-only` is a required floor lane with no provenance step.** It checks
  out with `fetch-depth: 0`, computes a merge base and echoes that SHA, without
  saying which tree it is standing in — the shape meta-PR3 FINDING 5 is about.
  `provenance.sh` runs in every required workflow FILE, which is not the same as
  every required JOB. Queued.
- The guard, the workflows and `ci-required-guard.py` are all checked out from
  the pull request under test and can be edited in it — every such edit is
  visible in the diff, and CODEOWNERS is **advisory only** until the owner's
  ruleset exists (it currently returns 403 on their plan).

Read the guarantee at exactly that strength; the guard's own docstring states it
the same way.

`make ci-guard` also invokes the guard for local parity. That invocation is not
a control — a neutered Makefile no-ops it along with everything else.

CI adds two lanes `make ci` cannot run locally: `append-only`, which diffs the
migration manifest against the merge base (a merge-base diff needs the history,
not a working tree), and `docker-build`.

`image-scan` also runs on every pull request and is **deliberately not in the
required set**. The reason is in `.github/required-checks.txt` and is not a
dodge: a scan's result depends on the world, and a required lane that goes red
on its own is how a team learns to merge past red. Its refusals are not
weakened by that — `scripts/image-scan-verdict.py` fails on a scanner ERROR, on
an empty or `null` result set, on an unrecognised OS, on a report about another
image, and on an exit code nothing recorded, each with its own fixture.

Integration tests need real services and are behind `-tags=integration`:

```
make test-integration           # requires VIZRA_TEST_DATABASE_URL and VIZRA_TEST_CACHE_URL
make test-integration-shuffle   # the same suite, -shuffle=on
```

They **fail rather than skip** when those are unset, for the same reason.

CI runs both, on both cache flavours. The shuffled lane exists because an
order- or timing-dependent failure makes a required lane go red at random, and
a lane that is re-run until it is green has stopped being evidence. `go test`
prints `-test.shuffle <seed>` as the first line of a FAILING package's output
(a green run prints none), so a failing order is reproducible with
`-shuffle=<seed>`.

## Rules that are mechanical, and where they are enforced

These are not style preferences. Each one has a check, and the check names the
decision it protects.

| Rule | Enforced by |
|---|---|
| Echo types stay in `internal/httpapi`, so the major is replaceable in one package (ADR-001) | `scripts/lint-imports.sh` |
| No package-global database, cache, storage or search handle (Q-008 item 1) | `scripts/lint-imports.sh` |
| No hardcoded storage root or cache namespace; both come from the site resolver (Q-008 items 3, 4) | `scripts/lint-imports.sh` |
| The image decoder is never linked into `cmd/api` (Q-034) | `scripts/lint-imports.sh` |
| Hostname → site resolution in exactly ONE middleware (Q-008 item 2) | `internal/httpapi/middleware.go`, `siteMiddleware` |
| `Enqueue` requires a `pgx.Tx`; an enqueue outside a transaction does not compile (ADR-004) | `TestEnqueueRequiresATransaction`, which builds a program that tries it |
| Migrations are append-only; a merged migration's bytes are frozen | `scripts/migration-manifest.sh` + the CI additions-only diff |
| `api/openapi.yaml` is the API source; drift fails in BOTH directions | `TestEveryRouteHasASpecOperation`, `TestEverySpecOperationHasARoute` |
| Generated sqlc output is never hand-edited | `sqlc diff` in `make sqlc-verify` |
| Production refuses dev secrets, short secrets, wildcard CORS, plain-http origins and every named escape hatch | `internal/config`, `TestEveryEscapeHatchIsRefusedInProduction` |
| Every config key has exactly one documented home | `TestEveryKeyHasATemplateEntry` and its converse |
| No credential, signed URL, session id or API key ever reaches a log line | `internal/obs`, `TestRedactionOfEveryValueClass` |
| Default-deny authorization over the frozen ADR-007 matrix | `internal/authz`, `TestFrozenMatrix` (315 cases) |
| A required lane cannot be removed by the pull request it gates | `scripts/ci-required-guard.py`, `FLOOR_LANES`, with fixtures under `scripts/testdata/guard/` |
| A one-line edit to the **Makefile or its includes** cannot turn every required lane into a no-op (a one-word edit to a workflow's own `make` line still can — see "What these two controls do NOT give you") | `scripts/make-integrity-guard.py`, run as an out-of-make workflow step BEFORE any `make`; `ci-required-guard.py` checks 8–9 assert that step is present, unconditional and not continue-on-error, and that one required lane runs the **unit** suite via `go test ./...` without make. Fixtures under `scripts/testdata/makeguard/` |
| A variable the contract names keeps that name in every service | `internal/config`, `TestTheContractAndTheLoaderNameTheSameVariable`, which reads `api/search-internal.openapi.yaml`'s own bytes |
| A retired config name is a production boot refusal, never a silent ignore | `internal/config`, `RetiredKeys`, `TestProductionRefusesARetiredKeyName` |
| A lane records the tree it actually stood in, not the SHA it was asked about | `scripts/provenance.sh`, called by every required workflow |
| An unset or unrecognised visibility DENIES; it is never normalised to public | `internal/authz`, `TestUnknownVisibilityDenies` |
| A value this repository publishes is never a production secret | `internal/config`, `knownPublishedSecrets`, `TestProductionRefusesPublishedTestKeys` |
| A crash-looping job is dead-lettered, not left at the head of the claim order | `SweepExpiredLeases`, `TestACrashLoopingJobDeadLettersAndDoesNotBlockTheQueue` |
| A merged migration's bytes are frozen | the `append-only` CI job (merge-base diff) + `migration-manifest.sh` + CODEOWNERS |
| Every doctor verdict is tested | `internal/doctor` (the checks are pure; `cmd/vizra` only does I/O) |
| The container healthcheck cannot pass while the service it probes is broken | `internal/healthcheck` (the probe reads the service's own `/readyz`, never its own opinion) and `internal/jobs.HealthHandler` (the worker's readiness is the claim loop's progress plus a probe-time PostgreSQL ping, not "the process exists"). Demonstrated end to end in `internal/integration/healthcheck_test.go`, which runs the SHIPPED binaries as separate processes against real PostgreSQL and reads their exit codes |
| An image-scan lane cannot pass vacuously — scanner error, empty or `null` results, an unrecognised OS, a report about another image, a swallowed exit | `scripts/image-scan-verdict.py`, with its own exit code 3 for "there was no valid scan"; 16 fixtures under `scripts/testdata/imagescan/`, `scripts/imagescan_test.go` |
| The runtime image carries no toolchain, and the libraries it ships actually load | the runtime stage is `FROM` a clean digest-pinned base (never `FROM vips`, never a purge, no `\|\| true` anywhere in the `Dockerfile`); `docker-build` asserts the toolchain is absent, that `/var/lib/vizra/media` is writable by uid 10001, and that the loader list `vips -l` produces IN THE RUNTIME IMAGE equals the one recorded at build time |
| migrate-lint, the gate guard and the import lint have their own negative cases | `scripts/scripts_test.go` against `scripts/testdata/` |
| `last_error` is redacted before it is truncated | `internal/jobs`, `TestLastErrorIsRedactedBeforeItIsStored` |
| PostgreSQL is the SINGLE clock authority for job eligibility: a "run now" enqueue takes `run_after` from the database, never from the application host | `EnqueueJob`'s `COALESCE(…, now())`, `TestRunAfterComesFromTheDatabaseClockNotTheApplicationHost` |
| A worker leaks no credential into its OWN log whichever slog handler it was built with — redaction is at the call site, not in the process wiring | behaviour: `TestAWorkerWithAPlainHandlerLogsNoCredentials` drives the three handler-error branches with a plain `slog.TextHandler`. Coverage: `TestEveryErrorLogSiteInTheWorkerIsRedacted` parses `worker.go` and asserts **all 11** `"error"` attributes are `safeError(...)`, and fails if the count drifts |
| A requeue of a dead or exhausted job must reset `attempts` | the contract comment on `SweepExpiredLeases` (`store/queries/jobs.sql`), carried into the generated doc comment |
| The internal search client never follows a redirect | `internal/search`, `TestARedirectIsNeverFollowedAndNoSignatureLeaks` |
| Every route, including the 404 path, carries the hardening headers | `internal/httpapi`, `TestEveryRouteCarriesHardeningHeaders` |
| At most one LIVE owner exists, and a tombstoned owner does not brick the instance | `users_one_owner` (partial unique index, migration 0005); `TestASecondLiveOwnerIsRefusedByTheDatabase`, `TestATombstonedOwnerDoesNotPermanentlyBlockOwnership` |
| While an instance is unclaimed, only an explicit allowlist is reachable — every other route, including the 404 path, is 403 | `internal/httpapi`, `requireClaimedMiddleware` + `TestEveryRouteIsEitherUnclaimedAllowlistedOrGuarded` (a route in neither classification set fails `make test-race`, and with it `make ci` and the `build-test` job) |
| The owner-claim token is stored only as a SHA-256 digest, and never reaches a response body, a response header, or **the structured logger** — on any path, including the `stderr` opt-in | `TestClaimTokenIsStoredOnlyAsASHA256Digest`, `TestOwnerClaimTokenNeverReachesTheStructuredLogOrAResponse` (the claim path, driven with a PLAIN slog handler, so redaction cannot hide a leak), `TestClaimTokenCLIOnAnUnclaimedInstance` (the real binary: token alone on stdout, no logger output on either stream). The `stderr` opt-in is covered BY CONSTRUCTION, not by those tests: `ownerclaim.Boot` takes no logger, only the `io.Writer` that `cmd/api` hands it (`os.Stderr`), and its error — the only thing `cmd/api` logs about it — is built from database errors and never carries the token. `TestBootMintsOnlyUnderTheStderrOptIn` exercises that writer path. |
| **By default** the token reaches no log at all: an unclaimed instance prints a COMMAND. `VIZRA_OWNER_CLAIM_ANNOUNCE=stderr` is an explicit opt-in that writes the token to the process's stderr — which IS the container log, and which every log driver captures, ships and retains. `.env.example` and the printed line both say so. | `TestTheAnnounceDefaultIsOff`, `TestBootMintsOnlyUnderTheStderrOptIn` |
| **Zero** argon2id derivations on every non-201 claim path — a wrong token, a malformed field, a 415/413/429, an already-claimed instance, and a token that is CORRECT but expired, consumed or superseded. The bound that DOES hold: **at most one derivation per request, and only for a request that presents a live token and a valid body** — every refusal that can be decided without hashing is decided first. After that one derivation the request can still end non-201, and every such outcome is stated here because it is real: **409** when a concurrent claimant or an out-of-band account won (the in-transaction claimed gate, an empty redeem on a now-claimed instance, or `users_one_owner`); **403** when the token went terminal between the liveness pre-check and the redeem (it expired in that window, or a concurrent re-mint superseded it); **503** on an outage or deadline; and **400 / 500** only as defect backstops (a CHECK the validator should have caught; an unmapped error). | `internal/credential` (injectable hasher with a derivation counter) + the liveness pre-check in `ownerclaim.Claim`; `TestNoPasswordHashingOccursWithoutAValidToken`, `TestACorrectButDeadTokenCostsNoDerivation` |
| No pooled connection is held while a password is hashed | `ownerclaim.Claim` splits a read phase (no transaction) from the write transaction; `TestNoConnectionIsHeldWhileHashing` asserts `AcquiredConns() == 0` **on the server's own pool** from inside the hasher (watching the test's pool instead is how this assertion was vacuous in the first round) |
| A permanently closed endpoint is not an unauthenticated writer into the undeletable audit table, warm cache OR cold | warm: the claimed bit short-circuits from the monotonic cache before the pool is touched. Cold (a restarted server on a claimed instance): the claimed check is the FIRST thing the handler does after the hard ceiling — one `AnyUserExists` read, before content type, body, origin or token shape — so every body gets 409, no audit row is written, and the cache warms; `ownerclaim.Claim` also reads the claimed state before it validates anything (OQ-4). The `already_claimed` refusal writes no row. `TestRepeatedClaimsOnAClaimedInstanceDoNotGrowTheAuditTrail`, `TestAColdCacheOnAClaimedInstanceAnswers409WithoutAuditRowsForAnyBody` (each body on its own cold server), `TestClaimReadsTheClaimedStateBeforeExaminingTheToken` |
| A database outage on the claim endpoint answers 503 `unavailable` — never a 500, never "token not accepted", and never charged to the caller's failure budget. "Outage" covers a failure to connect, a connection lost mid-transaction, AND a server that answers in order to say it cannot serve (SQLSTATE class 08, class 53, 57P01/02/03), which arrives as a `*pgconn.PgError` and used to fall through to a 500. The cause is logged once, redacted, with the request id. | `ownerclaim.ErrUnavailable` + `ownerclaim.IsServerUnavailable`; `TestClaimErrorMapping`, `TestOwnerClaimAnswers503WhenTheDatabaseIsDown`, `TestAConnectionKilledMidClaimAnswers503NotFiveHundred` (terminates the claim's backend mid-transaction), `TestADatabaseOutageIsDiagnosableFromTheLog`, `TestEveryStatusTheContractDeclaresIsProducedAndNoOtherIs` |
| Every mint decision is taken inside the advisory lock, so concurrent boots mint once | `ownerclaim.Mint(onlyIfNoLiveToken)`; `TestConcurrentBootsMintExactlyOneToken` |
| "Never mint on a claimed instance" is a property of the STATEMENT, not of its callers | `MintOwnerClaimToken`'s `WHERE NOT EXISTS (SELECT 1 FROM users)`; `TestMintIsRefusedByTheDatabaseOnAClaimedInstance` (direct store call) |
| A claim never creates an owner on an instance that gained ANY user (owner or member) while the password was being hashed | two layers: the in-transaction `AnyUserExists` gate in `ownerclaim.Claim`, and the redeem statement's own `AND NOT EXISTS (SELECT 1 FROM users)`; `TestAClaimRefusesWhenAUserAppearsDuringTheHash` (a member committed mid-hash → 409, no owner), `TestTheRedeemStatementRefusesAClaimedInstance` (direct store call). Deleting only the Go gate is NOT observable — the statement answers identically — so that single deletion is declared review-only (MUT-53, measured); deleting only the statement's guard is caught by the direct store call (MUT-46); deleting both is MUT-54 and the end-to-end test goes red. |
| A same-origin browser claim cannot be broken by a trailing slash, letter case, a default port or a trailing dot in `VIZRA_PUBLIC_ORIGIN` | origins normalised once at config load and compared as values; `TestNormalizeOriginMatchesWhatABrowserSends`, `TestOriginsAreComparedNormalisedNotAsStrings` |
| `audit_events` refuses UPDATE, DELETE **and** TRUNCATE; a user named by an audit row cannot be deleted | migration 0005 triggers + `audit_events_actor_user_fk ON DELETE RESTRICT`; `TestAuditEventsCannotBeUpdatedOrDeleted`, `TestAuditEventsCannotBeTruncated`, `TestAUserWithAuditRowsCannotBeDeleted` |
| The `ip_prefix` writer is TOTAL against the frozen 0003 grammar, so an audit row can never abort the transaction it belongs to | `internal/audit`, `TestIPPrefixWriterOutputAlwaysSatisfiesTheFrozenCheck` (compiles the grammar from the migration's own bytes) |
| A rate-limited claim writes no audit row PER REQUEST; exactly ONE `rate_limited` row is written per bucket per window, on the transition into the limited state — so an anonymous flood is not an unbounded writer into an undeletable table | `TestARateLimitedClaimWritesNoAuditRow` asserts the exact shape: 0 rows before the transition, exactly 1 after, still exactly 1 after ten times more traffic |
| A request carrying the VALID claim token is never answered 429 by the failure limiter | `TestAValidTokenIsNeverRateLimitedByTheFailureLimiter` |
| The claim transaction pins READ COMMITTED explicitly, so the race holds whatever `default_transaction_isolation` the server is set to | `TestOwnerClaimRaceYieldsExactlyOneOwnerUnderEveryServerDefaultIsolation` (32 claimants × 3 server defaults) |
| The fixture corpus reproduces byte-identically from the pinned generator, and each fixture really carries the property it exists for | `make fixtures-verify` + the `fixtures` CI lane; `internal/fixtures`, `TestRemovingThePropertyAFixtureExistsForIsCaught` and `TestManifestDetectsEveryClassOfDrift` |
| A fixture is never a downloaded photograph: every byte is synthesised | `NOTICE`, `TestCommittedCodecSourceIsGeneratorOutput`, and the "No image is fetched" step in `.github/workflows/fixtures.yml` |

## Fixtures (VZ-FOUND-007, ADR-009)

The twelve M0 fixtures are **generated, not committed**. `fixtures/manifest.json`
is the committed artefact; `testdata/fixtures/` is gitignored.

```
make fixtures          # produce the corpus (needs only a Go toolchain)
make fixtures-verify   # regenerate and compare every byte against the manifest
make fixtures-manifest # RE-PIN the manifest: only when the corpus is meant to move
make load-corpus LOAD_CORPUS_OUT=/var/tmp/vizra-load   # the DECLARED load corpus
```

Three things about this corpus are load-bearing and easy to break by accident:

1. **The generator writes every byte itself** — pixels, EXIF and GPS IFDs, PNG
   chunks, GIF blocks, the RIFF/VP8L bitstream, the ISOBMFF box tree — using the
   Go standard library and nothing else. ADR-009 names libvips and exiftool;
   neither reproduces byte-identically (libjpeg-turbo/libwebp/libaom bytes move
   across versions and build options, and exiftool stamps its own version and a
   timestamp). ADR-001 permits this: "a pure-Go decoder path exists only to
   generate fixtures."
2. **The Go toolchain moves the bytes**, and this is measured, not assumed: the
   same generator source over the same raster produces `ae627ab…` under go1.26.2
   and `73d5acb…` under go1.27.1 (`docs/evidence/fixtures/2026-09-21-determinism.md`).
   The manifest pins it, and go.mod's `go` directive beside it — no byte
   difference has been observed from that directive alone, but under
   `GOTOOLCHAIN=auto` raising it is a way to make a different toolchain run the
   generator. Changing either without `make fixtures-manifest` turns
   `fixtures-verify` red by name.
3. **AVIF and WebM are committed generator inputs** under
   `internal/fixtures/codec/`, because AV1 and VP8 have no pure-Go encoder. They
   were encoded once from a PNG this generator produced; `NOTICE` and the
   manifest's `codec_inputs` record the exact commands, and a test asserts the
   committed source PNG is still the generator's own output.

Every fixture is asserted for what it is FOR, not merely hashed, and every one
of those assertions has a mutation case that shows it can fail.

## Pinned versions

Pins come from ADR-001 and are changed only through a reviewed PR that updates
the licence table in the same diff. Every version below was confirmed against
the live registry on 2026-09-20, not recalled.

| Pin | Value | Where |
|---|---|---|
| Go toolchain | `go1.27.1` | `go.mod` `toolchain` |
| Echo | `v5.3.1` | `go.mod` |
| pgx | `v5.11.0` | `go.mod` |
| golang-migrate | `v4.20.1` | `go.mod` |
| go-redis | `v9.22.0` | `go.mod` |
| otelhttp / otel | `v0.71.0` / `v1.46.0` | `go.mod` |
| sqlc | `1.31.1` | `.github/workflows/build-test.yml` |
| PostgreSQL | 18, digest-pinned | workflows |
| Valkey (managed) | 9.1.2, digest-pinned | workflows |
| Redis (CI matrix leg only) | 7.2, digest-pinned | workflows |
| libvips | 8.18.6, checksummed source tarball | `Dockerfile` |
| Debian base | 13, digest-pinned — the SAME digest in the build and runtime stages | `Dockerfile` |
| Trivy | 0.70.0, digest-pinned image `aquasec/trivy@sha256:be1190af…`; digest resolved from registry-1.docker.io on 2026-09-21 | `.github/workflows/image-scan.yml` |

`govips` and `minio-go` are ADR-001 pins that M0 does not yet link; they enter
`go.mod` with the slice that uses them (VZ-MEDIA-001, VZ-STORAGE-002). Listing
them in `go.mod` before then would fail `go mod tidy -diff`, so the pin lives in
ADR-001 and in `NOTICE` until the code arrives.

## The container healthcheck (what a compose file should use)

`vizra healthcheck TARGET` is the probe. It talks to the service's own listener
over loopback, reads the service's own readiness verdict, makes **exactly one**
request and never retries — a container runtime's `--interval` and `--retries`
already supply retries, and a probe that retries inside its own `--timeout` gets
killed by the runtime with no message at all.

| exit | meaning |
|---|---|
| `0` | ready. Any 2xx, **including `degraded`** — see below. The status is printed, so `degraded` is visible in the health log rather than flattened into "healthy". |
| `1` | NOT ready: connection refused, timed out, or a non-2xx answer. |
| `64` | usage error. Never a verdict about the service. (Docker reserves 2.) |

`degraded` maps to **0 on purpose, and the semantics are core's existing ones,
not a new stricter rule.** `/readyz` 503s for exactly one condition —
PostgreSQL unreachable — and returns 200 `degraded` when the cache is down,
search is unreachable, or the queue is past its age threshold, so a degraded
instance keeps serving reads instead of being pulled from rotation and taking
the site down with it (ADR-002 § Probes). A probe that failed on `degraded`
would remove every api from rotation during a Redis blip.

The **worker** has no API listener, so its readiness is served on the metrics
listener it already runs (`VIZRA_METRICS_ADDR`, loopback by default) at
`/readyz`. It reports, per site: PostgreSQL pinged **at probe time**, and the
claim loop's **last progress** against a staleness bound. `not_started` until
the loop's first iteration — the window between the listener opening and the
worker actually working is not reported ready; `--start-period` is what covers
that window, not a lying probe.

**The staleness bound is 15s** with the default 1s poll interval
(`HealthStaleBound` = 5 × poll, floor 15s). It is deliberately NOT derived from
the per-job timeout: with every slot busy the claim loop makes no database round
trip, and a job may legitimately run for `VIZRA_JOB_TIMEOUT` (5 minutes), so a
bound wide enough to tolerate that could not report a wedged loop inside a
deploy window. Instead the loop's wait for a slot is **bounded**, and a
saturated worker records progress of its own — which is also reported, so an
operator watching a slow queue can see that every slot is busy.

The lines a compose file should use (the healthcheck timeout is 3s against the
probe's own 2s deadline, so the probe always gets to report its reason):

```yaml
  api:
    healthcheck:
      test: ["CMD", "/usr/local/bin/vizra", "healthcheck", "api"]
      interval: 15s
      timeout: 3s
      start_period: 30s
      retries: 3

  worker:
    healthcheck:
      test: ["CMD", "/usr/local/bin/vizra", "healthcheck", "worker"]
      interval: 15s
      timeout: 3s
      start_period: 30s
      retries: 3
```

No `CMD-SHELL`, no `/dev/tcp`, no `curl`: the runtime image has no shell
dependency baked in for this, which is the chair's ruling on meta PR #4
FINDING 3. The image's own `HEALTHCHECK` is the `api` form, matching its default
`CMD`; a worker container overrides both.

## Contract ownership

`api/openapi.yaml` and `migrations/` have ONE owner per slice. `vizra-user` and
`vizra-search` consume them at a recorded commit SHA. If another repository needs
a contract change, it asks — it does not make one.

The core↔search contract is `api/search-internal.openapi.yaml`, owned here,
implemented by `vizra-search`, and drift-checked in both repositories. Its HMAC
scheme is pinned by `api/search-hmac-testvectors.json`, generated independently
of the Go implementation so the two repositories check against the same bytes
rather than against each other's prose. See `api/README.md`.

### A variable the contract names keeps that name in every service

**Rule (chair, 2026-09-20).** If a contract file under `api/` names an
environment variable, every service reads it under exactly that name. No
service-local prefix, no per-service spelling, no alias.

The shared core↔search secret is **`SEARCH_HMAC_KEY`** — the name
`api/search-internal.openapi.yaml` uses and the name `vizra-search` reads. It is
deliberately not `VIZRA_`-prefixed: the prefix marks a variable core owns, and
this one is the contract's.

Why it is a rule and not a preference: core read `VIZRA_SEARCH_HMAC_KEY` while
search read `SEARCH_HMAC_KEY` for the same shared secret, and the deployment
templates had begun to paper over the difference by setting both. Two names for
one secret means the moment a template sets only one of them, one side signs
with a key the other never loaded — and the failure surfaces as an
authentication error at the boundary, not as a configuration error at boot.

Mechanically: `internal/config.Registry` carries the contract's name;
`TestTheContractAndTheLoaderNameTheSameVariable` reads the contract file's own
bytes and fails if core and the contract disagree, in either direction.

**Renaming one.** There is no compatibility alias. A retired name goes in
`internal/config.RetiredKeys`, production **refuses to boot** while it is set to
a non-empty value, and the refusal names the replacement. A leftover old name
must never be silently ignored: the operator's file looks configured while the
process has no key at all. `.env.example` carries the retired name as a
commented tombstone (`TestRetiredKeysAreTombstonedInTheTemplate`).

## Evidence

Implementation state, verification state, merge state and release state are
different. Say READY_FOR_REVIEW until an independent verifier says otherwise.
Record exact commands, exit codes, test counts, skips, source SHA and
environment. A required test that is skipped, missing, cancelled, timed out or
not collected is not PASS.

Tests must challenge the implementation. Demonstrate the relevant check failing
against a controlled mutation before claiming it covers anything. Do not weaken
an assertion, delete a case, or narrow scope to turn CI green.

## What is deliberately absent in M0

Saying this plainly so nobody reads an absence as an oversight:

- No sessions, no sign-in, no API keys. M1-A (VZ-INSTALL-003) adds the owner
  claim and its two setup operations, and NOTHING else: claiming does **not**
  establish a session, because sessions arrive with VZ-AUTH-001 (M1-B) and
  faking one would be mock data in a production path. A client sends the new
  owner to the sign-in page; `Set-Cookie` is added to the same operation by that
  slice, which is a backward-compatible change.
- **`VIZRA_TRUSTED_PROXIES` does not exist yet.** Behind a reverse proxy,
  `ip_prefix` is therefore NULL and the per-origin rate-limit bucket is inert —
  the global bucket and the hard ceiling still apply. This is deliberate: writing
  the proxy's own address as if it were the client would put a false attribution
  in an immutable table and collapse every caller into one bucket. M1-B owns it.
- No media tables, no upload, no derivatives. `storage_locations` exists because
  migrations are append-only and `asset_files.storage_location_id` must be able
  to reference it from its first day.
- No compose file, installer or boot lane — VZ-ISSUE-002…004 own those.
- The fixture corpus (VZ-FOUND-007) has landed; see "Fixtures" below.
- The ruleset that makes `ci-required` and CODEOWNERS mandatory is an OWNER
  action after this PR lands: `ci-required` must exist before it can be
  required (ADR-002 item 9). Until it is applied, no ledger entry may reach
  VERIFIED on CI evidence alone (item 10).
