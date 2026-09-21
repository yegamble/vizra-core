# fakedocker — `docker` stubs for scripts/assert-runtime-image.sh

`assert-runtime-image.sh` reads `$DOCKER`, so these stand in for the daemon. It
is the only way to demonstrate the control the script exists for: the previous
inline version of those assertions PASSED when `docker run` itself failed
(PR#7 VERIFY, FINDING 1), and demonstrating that needs a `docker run` that
fails — not a broken image, which CI has no way to produce on demand.

| stub | behaves as |
|---|---|
| `clean` | a correct runtime image: no toolchain, no `-dev` packages, no `/src` |
| `broken` | a daemon/image that cannot run anything (exit 125) — the FINDING 1 case |
| `has-toolchain` | an image where `command -v gcc` succeeds |
| `has-src` | an image that still carries `/src` |
| `weird-exit` | `test -e /src` exits 7 — neither verdict, so nothing looked |
