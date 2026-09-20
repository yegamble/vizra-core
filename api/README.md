# `api/` — contract sources

Two hand-written OpenAPI documents. Both are **sources**, not generated output:
routes are written here first and implemented second.

| File | Who serves it | Who consumes it | Gate |
|---|---|---|---|
| `openapi.yaml` | `vizra-core` `cmd/api` | `vizra-user` (generated TypeScript client), operators | `make openapi-verify` — fails on a route with no operation **and** on an operation with no route |
| `search-internal.openapi.yaml` | `vizra-search` | `vizra-core` `internal/search/remote` | `make openapi-verify` validates it; `vizra-search` runs the byte-identical drift check |

## Why the internal contract is a separate file

ADR-002 (Q-001) puts the canonical copy of the core↔search contract **in core**.
It is a separate file beside `openapi.yaml` rather than paths inside it, for two
reasons that are not style preferences:

1. **core never serves `/internal/v1/*`.** It is the client. Putting those paths
   in `openapi.yaml` would make core's own spec-without-route check fail
   permanently, and the only way to keep it green would be to weaken the check —
   which `AGENTS.md` forbids.
2. **`vizra-user` generates its client from `openapi.yaml`.** Internal
   HMAC-authenticated operations must not appear in a browser-facing client.

`vizra-search` vendors a byte-identical copy at its own
`api/search-internal.openapi.yaml` and fails CI on any difference, so the two
repositories cannot drift. **Change it here first**; the search repository's
drift check then forces the follow-up PR.

## Changing a contract

`api/openapi.yaml` and `migrations/` have one owner per slice: the `vizra-core`
builder. Other repositories consume them at a recorded commit SHA. If you need a
contract change, ask for it — do not make it in a consuming repository.
