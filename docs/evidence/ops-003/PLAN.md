# VZ-OPS-003 — vizra status / logs / restart / update (resumable checklist)

Base: origin/main 33a7187. Branch feat/ops-003-status-logs.

- [x] internal/ops: Chain (env-derived --env-file/-f/--profile), model validation, bounded logs, restart refusal (unknown + one-shot), ps parse (array + NDJSON), health, update report (BLOCKED: no release feed)
- [x] cmd/vizra/ops.go: status/logs/restart/update over an exec seam; queue depth from jobs (JobDepthByKindState), BLOCKED (exit 3) when not measured, never 0
- [x] unit tests: internal/ops, cmd/vizra/ops_test.go (fake run seam, exact argv)
- [x] mutations: drop --env-file (red), accept unknown service (red), restored (green): this dir
- [x] draft PR
- [ ] test-floors entry for internal/ops; make ci via quiet.sh
- [ ] real-binary transcript (transcript.txt)
- [ ] docs note; ready when ci-required green

Open: per-service version uses the image reference from `compose ps`, not OCI image labels.
`vizra update` runs `docker compose version --short` (a version probe, no project) to report the floor.
