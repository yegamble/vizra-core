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
go test -race -count=1 -json ./... > unit-events.json; echo $? > unit-exit.txt
python3 scripts/go-test-report.py --events unit-events.json --suite unit \
  --floors scripts/test-floors.json --go-exit-file unit-exit.txt
```

It prints the executed/passed/failed/skipped counts, names every failure and
every skip, and fails below the floor recorded in `scripts/test-floors.json`.
Use `--suite integration` with `-tags=integration` for the other suite.

The two out-of-make guards, which CI runs as their own steps before any `make`:

```sh
./scripts/make-integrity-guard.sh   # the Makefile and everything it includes
./scripts/ci-required-guard.sh      # the manifest, the workflows, and each make step's own argv
```

`scripts/assert-runtime-image.sh <image>` is the image assertion `docker-build`
runs; it reads `$DOCKER`, so `scripts/testdata/fakedocker/` can drive it with no
daemon.

## Contributing

`AGENTS.md` is the engineering contract: what the gate checks, which rules are
mechanical and where each is enforced, and which pins may not move without a
reviewed ADR change.
