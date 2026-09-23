# vizra-core

The Go API, worker and operator CLI of [Vizra](https://github.com/yegamble/vizra) —
a self-hosted photo-community application.

This repository owns the canonical API contract, the database migrations and the
sqlc queries. `vizra-user` and `vizra-search` consume them at a recorded commit
SHA.

## Layout

```
api/                      contract sources — see api/README.md
  openapi.yaml            the public API source (vizra-user generates its client from this)
  search-internal.openapi.yaml
                          the core<->search contract (vizra-search implements it)
cmd/api                   the HTTP API. Never decodes pixels.
cmd/worker                durable jobs. The only process that runs libvips.
cmd/vizra                 the operator CLI: version, doctor, migrate
internal/authz            the single authorization evaluator, default-deny
internal/config           the one configuration validator, shared by boot, setup, doctor and CI
internal/jobs             one durable jobs table; Enqueue requires a transaction
internal/search           the frozen core<->search boundary
internal/site             the tenancy seam: one site in core
migrations/               append-only, embedded in the binary, checksum-manifested
store/queries/            sqlc sources; internal/store/sqlcgen is generated
```

## Getting started

Requires Go (the toolchain in `go.mod` is fetched automatically), Docker, and
[sqlc 1.31.1](https://docs.sqlc.dev/en/latest/overview/install.html).

```sh
cp .env.example .env          # development defaults; production refuses every secret in it
make ci                       # the whole gate
make build                    # bin/vizra-api, bin/vizra-worker, bin/vizra
./bin/vizra doctor            # real checks only; a missing prerequisite is SKIP, never OK
```

Integration tests need real PostgreSQL and a real RESP server, and **fail rather
than skip** without them:

```sh
docker run -d --name vizra-pg    -p 55432:5432 \
  -e POSTGRES_USER=vizra -e POSTGRES_PASSWORD=vizra -e POSTGRES_DB=vizra_test \
  postgres@sha256:86c951e05bf56c93d95d397747fb8820ac76cc3bedb78f43abd83eedbe3666ae
docker run -d --name vizra-cache -p 56379:6379 \
  valkey/valkey@sha256:c123e3715db63d06d4ad6964884037aa0d5d4d703939b9929954112889708e1d \
  valkey-server --maxmemory 128mb --maxmemory-policy allkeys-lru --save ''

VIZRA_TEST_DATABASE_URL='postgres://vizra:vizra@127.0.0.1:55432/vizra_test?sslmode=disable' \
VIZRA_TEST_CACHE_URL='redis://127.0.0.1:56379/0' \
  make test-integration
```

### Reproducing what CI asserts about the suites

CI does not run `make test-integration` in `build-test`: it invokes both suites
directly, with no make, and then judges the machine-readable results — because
`go test ./...` exits 0 having run nothing, and a non-verbose `go test` prints
nothing at all for a skipped test. To reproduce that locally:

```sh
go test -race -count=1 -timeout 8m -json ./... > unit-events.json; echo $? > unit-exit.txt
python3 scripts/go-test-report.py --events unit-events.json --suite unit \
  --floors scripts/test-floors.json --go-exit-file unit-exit.txt
```

It prints the executed/passed/failed/skipped counts, names every failure and
every skip, and fails below the floor recorded in `scripts/test-floors.json`.
Use `--suite integration` with `-tags=integration` for the other suite.

The two out-of-make guards. CI runs `make-integrity-guard.sh` as its own step
**immediately before every** `make` step, and the make steps themselves are
pinned byte-for-byte in `.github/pinned-steps.yml` — the control is default-deny
on the step's shape, not a parser for shell:

```sh
./scripts/make-integrity-guard.sh --workflow   # exactly as CI's pinned anchor step runs it
./scripts/ci-required-guard.sh      # the manifest, the workflows, and each make step's own argv
```

The anchor runs make only on **reviewed Makefile bytes**: `.github/pinned-makefiles.yml`
pins the sha256 of every file make reads, and the anchor checks it before make is
invoked at all, because make EVALUATES a makefile while reading it. Those bytes
still run the reviewed `$(shell git rev-parse …)` / `$(shell date …)` calls at
Makefile:22-23; a reviewer approving a malicious Makefile together with its pin
update is the residual, and CODEOWNERS is advisory. **Editing the Makefile? Update
its pin in the same diff:** `shasum -a 256 Makefile` into
`.github/pinned-makefiles.yml` — otherwise every make lane, `ci-guard` and the
direct suite fail by name.

`scripts/assert-runtime-image.sh <image>` is the image assertion `docker-build`
runs; it reads `$DOCKER`, so `scripts/testdata/fakedocker/` can drive it with no
daemon.

## Claiming a new instance

A fresh instance has no owner, and **every route except the four probes and the
two setup operations answers 403 until it does** — so the first thing an
operator does is claim it.

```sh
docker compose exec api vizra claim-token   # prints a one-time token to YOUR terminal
```

Then open `/setup/claim`, paste the token, and create the owner account. The
token is 64 hexadecimal characters, works exactly once, and expires after
`VIZRA_OWNER_CLAIM_TTL` (default 1h). Running the command again mints a new
token and invalidates the previous one. Once the instance has any account it is
claimed for good: `vizra claim-token` then refuses, because minting on a claimed
instance would manufacture a live owner-creating credential on a running system.

**Why a command rather than a line in the log.** Only the SHA-256 digest of the
token is ever stored, so it cannot be read back — it can only be re-minted. More
importantly, `docker compose exec` writes to your terminal, not to the api
container's log stream, so no log driver captures it, no aggregator indexes it
and no retention policy keeps it. `VIZRA_OWNER_CLAIM_ANNOUNCE=stderr` will print
the token at boot instead, which is convenient on a single host with no log
shipping and a credential leak anywhere else — the default is `off`, and the
boot line names this command instead.

`vizra doctor` reports whether the instance is claimed and what to run if not.

## Contributing

`AGENTS.md` is the engineering contract: what the gate checks, which rules are
mechanical and where each is enforced, and which pins may not move without a
reviewed ADR change.
