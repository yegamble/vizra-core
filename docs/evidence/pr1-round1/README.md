# vizra-core PR #1 — initial slice and review round 1 transcripts

Durable copies of the red/green evidence captured while building the slice and
applying review round 1. Filenames beginning `D` and `HMAC`/`FLOOR`/`AUTHZ`/
`MIGRATELINT` are from the initial build; those beginning `R2-` are round 1.

| Group | What it shows |
|---|---|
| `D1`…`D6` | the six demonstrations the slice brief required: route-without-spec, spec-without-route, hand-edited sqlc output, production boot refusing dev secrets, an edited merged migration, the golden path after `FLUSHALL` |
| `FLOOR-*` | four ways to shrink the required-checks gate, each refused |
| `AUTHZ-*` | the frozen matrix catching a listing-rule relaxation |
| `MIGRATELINT-*` | a destructive statement in an up migration |
| `HMAC-*` | the timestamp window not closing, and the negative vectors |
| `R2-authz-*` | round 1: an unset visibility granting an anonymous viewer access |
| `R2-config-*` | round 1: the published key accepted as a production secret; the value-bearing escape hatch |
| `R2-crashloop-*` | round 1: `jobs_attempts_bounded` violated on every claim |
| `R2-doctor-mutants.txt` | round 1: the three surviving doctor mutants |
| `R2-scripts-mutants.txt` | round 1: migrate-lint's `COLUMN` mutant, and the Makefile test-selection checks |
| `R2-append-only-proof.txt` | round 1: the two throwaway PRs |
| `docker-*`, `cache-matrix-*` | image build and the permanent two-image cache matrix |

**Correction.** `R2-append-only-proof.txt` records the positive throwaway run
35532590902 as passing. The `append-only` JOB passed; the RUN concluded
`failure`, on `build-test` → `sqlc-verify`, because that branch added a
migration without regenerating the sqlc output. See `../pr1-round2/README.md`
and the round-2 section of the execution plan.
