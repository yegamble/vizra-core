# VZ-FOUND-005 readiness remainder: execution plan

Repo vizra-core, branch feat/found-005-readyz-remainder, PR #24. This repo has no docs/plans/, so the plan lives here.

1. Add a `schema` component to /readyz that fails closed unless the migration state is `current`, behind a Deps.SchemaState seam. Done in 4cf61dd.
2. Unit red/green: red-unit.txt, green-unit.txt.
3. Integration against real PG18: schema behind gives 503 (red-integration-schema.txt). Stopping PostgreSQL gives 503 within 10 s, and the API recovers with the same PID (green-integration.txt).
4. Lanes: make ci, make test-race, make test-integration.
5. Compose transcript (case 3) and rendered compose model (case 4) belong to meta. Their state is in the PR body.
