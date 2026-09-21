# vizra-core — repository engineering contract

This file is the `vizra-core` half of the contract. The other half is the meta
repository's `AGENTS.md`, which binds every Vizra repository and takes
precedence. Read that first; read this before editing anything here.

`vizra-core` is one Go module, one image and three entry points:

| Entry point | Responsibility | Hard rule |
|---|---|---|
| `cmd/api` | the HTTP API | **never decodes pixels** (ADR-002, Q-034) |
| `cmd/worker` | durable jobs, and the only process that runs libvips | every handler is idempotent |
| `cmd/vizra` | the operator CLI: setup, doctor, migrate, backup, restore | `doctor` performs real checks only |

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

CI adds two lanes `make ci` cannot run locally: `append-only`, which diffs the
migration manifest against the merge base (a merge-base diff needs the history,
not a working tree), and `docker-build`.

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
| An unset or unrecognised visibility DENIES; it is never normalised to public | `internal/authz`, `TestUnknownVisibilityDenies` |
| A value this repository publishes is never a production secret | `internal/config`, `knownPublishedSecrets`, `TestProductionRefusesPublishedTestKeys` |
| A crash-looping job is dead-lettered, not left at the head of the claim order | `SweepExpiredLeases`, `TestACrashLoopingJobDeadLettersAndDoesNotBlockTheQueue` |
| A merged migration's bytes are frozen | the `append-only` CI job (merge-base diff) + `migration-manifest.sh` + CODEOWNERS |
| Every doctor verdict is tested | `internal/doctor` (the checks are pure; `cmd/vizra` only does I/O) |
| migrate-lint, the gate guard and the import lint have their own negative cases | `scripts/scripts_test.go` against `scripts/testdata/` |
| `last_error` is redacted before it is truncated | `internal/jobs`, `TestLastErrorIsRedactedBeforeItIsStored` |
| PostgreSQL is the SINGLE clock authority for job eligibility: a "run now" enqueue takes `run_after` from the database, never from the application host | `EnqueueJob`'s `COALESCE(…, now())`, `TestRunAfterComesFromTheDatabaseClockNotTheApplicationHost` |
| A worker leaks no credential into its OWN log whichever slog handler it was built with — redaction is at the call site, not in the process wiring | behaviour: `TestAWorkerWithAPlainHandlerLogsNoCredentials` drives the three handler-error branches with a plain `slog.TextHandler`. Coverage: `TestEveryErrorLogSiteInTheWorkerIsRedacted` parses `worker.go` and asserts **all 11** `"error"` attributes are `safeError(...)`, and fails if the count drifts |
| A requeue of a dead or exhausted job must reset `attempts` | the contract comment on `SweepExpiredLeases` (`store/queries/jobs.sql`), carried into the generated doc comment |
| The internal search client never follows a redirect | `internal/search`, `TestARedirectIsNeverFollowedAndNoSignatureLeaks` |
| Every route, including the 404 path, carries the hardening headers | `internal/httpapi`, `TestEveryRouteCarriesHardeningHeaders` |
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
| Debian base | 13, digest-pinned | `Dockerfile` |

`govips` and `minio-go` are ADR-001 pins that M0 does not yet link; they enter
`go.mod` with the slice that uses them (VZ-MEDIA-001, VZ-STORAGE-002). Listing
them in `go.mod` before then would fail `go mod tidy -diff`, so the pin lives in
ADR-001 and in `NOTICE` until the code arrives.

## Contract ownership

`api/openapi.yaml` and `migrations/` have ONE owner per slice. `vizra-user` and
`vizra-search` consume them at a recorded commit SHA. If another repository needs
a contract change, it asks — it does not make one.

The core↔search contract is `api/search-internal.openapi.yaml`, owned here,
implemented by `vizra-search`, and drift-checked in both repositories. Its HMAC
scheme is pinned by `api/search-hmac-testvectors.json`, generated independently
of the Go implementation so the two repositories check against the same bytes
rather than against each other's prose. See `api/README.md`.

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

- No auth endpoints, no sessions, no API keys. `internal/authz` exists and is
  tested against the frozen matrix; the routes that call it arrive in M1.
- No media tables, no upload, no derivatives. `storage_locations` exists because
  migrations are append-only and `asset_files.storage_location_id` must be able
  to reference it from its first day.
- No compose file, installer or boot lane — VZ-ISSUE-002…004 own those.
- The fixture corpus (VZ-FOUND-007) has landed; see "Fixtures" below.
- The ruleset that makes `ci-required` and CODEOWNERS mandatory is an OWNER
  action after this PR lands: `ci-required` must exist before it can be
  required (ADR-002 item 9). Until it is applied, no ledger entry may reach
  VERIFIED on CI evidence alone (item 10).
