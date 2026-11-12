# VZ-FOUND-006 — boot-refusal demonstration + explicit signed-URL denylist case

Plan:
1. Process-level test `internal/integration/bootrefusal_test.go` (`-tags=integration`): it runs the real
   `vizra-api` / `vizra-worker` with `VIZRA_MODE=production` and expects a non-zero exit and a named
   error for a dev session secret, a blank session secret, a blank MFA KEK, and a plain-http origin
   (the insecure seam). It also never echoes the secret and includes a valid-baseline control.
2. Red: mutation `if production {` → `if false && production {` on the fail-secure block in
   `internal/config/config.go`. Green: the mutation is reverted.
3. `internal/obs/signedurl_test.go`: explicit signed-URL case (AWS SigV4, GCS V4, CloudFront) under neutral
   keys, groups, With, message and error strings. Red: the mutation drops the param-value redaction rule
   in `internal/obs/log.go`.

Transcripts: `green-*.txt`, `red-*.txt` in this directory.
