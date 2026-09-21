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
