# VZ-INSTALL-003 (M1-A) — owner claim evidence

| File | What it is |
|---|---|
| `01-integration-pg18.txt` | the integration suite, `-race`, verbose, on real PostgreSQL 18.6 and Valkey 9.1.2 |
| `02-mutations.txt` | every mutation demonstration: RED under one controlled mutation, GREEN when reverted |
| `demonstrate.sh` | the harness that produced `02-mutations.txt`, re-runnable by a verifier |

## How to reproduce

```sh
docker run -d --name <yours>-pg18 -p 55438:5432 \
  -e POSTGRES_USER="$PGUSER" -e POSTGRES_PASSWORD="$PGPASSWORD" -e POSTGRES_DB=vizra_test postgres:18
docker run -d --name <yours>-valkey -p 63799:6379 valkey/valkey:9.1.2

# Build the DSN from the values you passed to the containers; do not paste a
# credentialed URI into a tracked file — a secret scanner cannot tell a throwaway
# local password from a real one, and an incident on any commit in a PR stays red.
export VIZRA_TEST_DATABASE_URL="postgres://${PGUSER:-vizra}:${PGPASSWORD:?set it to the POSTGRES_PASSWORD you used}@127.0.0.1:55438/vizra_test?sslmode=disable"
export VIZRA_TEST_CACHE_URL='redis://127.0.0.1:63799/0'

make ci
go test -tags=integration -race -count=1 -v ./internal/integration/
./docs/evidence/m1a-owner-claim/demonstrate.sh        # needs a CLEAN tree
```

## What the harness guarantees, at exactly that strength

It records the sha256 of the mutated file **before and after** and refuses to
score a case whose digest did not change — so a pattern that stopped matching is
reported `HARNESS-FAIL`, never as a pass. After restoring, it re-checks
`git status` and refuses to continue if the tree is dirty, because a polluted
tree would make the NEXT case's result meaningless. Both checks earned their
place: the first caught MUT-27's stale pattern, and the second caught the
harness restoring a mutated `.sql` file without the sqlc-generated Go beside it.

**Three properties have no mutation that turns a test red, and are listed as
review-only in `02-mutations.txt` rather than implied to be covered:** the
constant-time token comparison, passing the row's own digest rather than the
presented one to the redeem statement, and the `consumed_at`/`superseded_at`
predicates in the redeem CTE — both of which are measured to leave the suite
green because `users_one_owner` plus the error mapper produce the same answer.
That is defence in depth working, not a gap, and the transcript says which is
which.
