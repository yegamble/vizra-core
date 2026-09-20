#!/usr/bin/env bash
# lint-imports — two architectural boundaries that must be mechanical.
#
# 1. ECHO CONFINEMENT (ADR-001). Echo types stay inside internal/httpapi, so the
#    framework major is replaceable in one package. Without this check the
#    import spreads into handlers over a few months and the next major becomes a
#    repository-wide migration.
#
# 2. NO PACKAGE-GLOBAL HANDLES (Q-008 M0 plumbing checklist, item 1). No
#    package-global DB pool, cache client, storage client or search client: all
#    handles are resolved from one context resolver. A global is what makes
#    database-per-tenant a rewrite instead of a configuration change.
set -euo pipefail
cd "$(dirname "$0")/.."
fail=0

printf 'lint-imports:\n'

# --- 1. Echo confinement ------------------------------------------------------
echo_users=$(grep -rln --include='*.go' 'github.com/labstack/echo' . \
  | grep -v '^./internal/httpapi/' \
  | grep -v '^./vendor/' || true)
if [ -n "$echo_users" ]; then
  printf '  FAIL  Echo is imported outside internal/httpapi:\n' >&2
  printf '%s\n' "$echo_users" | sed 's/^/          /' >&2
  printf '        ADR-001 confines Echo types to internal/httpapi so the major version is\n' >&2
  printf '        replaceable in one package. Take a context.Context in your package and\n' >&2
  printf '        adapt it in internal/httpapi.\n' >&2
  fail=1
else
  printf '  ok    Echo is imported only by internal/httpapi\n'
fi

# --- 2. no package-global handles --------------------------------------------
# A package-level var (not inside a function, so no leading tab) whose type is a
# pool or client. Test files are exempt: a test-local fixture is not a runtime
# global.
global_re='^var[[:space:]]+[A-Za-z_][A-Za-z0-9_]*[[:space:]]+\**(pgxpool\.Pool|pgx\.Conn|redis\.Client|cache\.Client|search\.(Service|RemoteClient)|db\.Pools|minio\.Client)'
globals=$(grep -rnE --include='*.go' "$global_re" . | grep -v '_test.go:' | grep -v '^./vendor/' || true)
if [ -n "$globals" ]; then
  printf '  FAIL  package-global handle(s) found:\n' >&2
  printf '%s\n' "$globals" | sed 's/^/          /' >&2
  printf '        Q-008 item 1: every handle is resolved from the request context, never\n' >&2
  printf '        from a package global, so tenancy stays a connection-routing concern.\n' >&2
  fail=1
else
  printf '  ok    no package-global database, cache, storage or search handle\n'
fi

# --- 3. no hardcoded storage root or cache namespace -------------------------
# Q-008 items 3 and 4: the prefix and namespace come from the site resolver.
hardcoded=$(grep -rnE --include='*.go' '"(originals|derived|incoming)/' . \
  | grep -v '_test.go:' | grep -v '^./internal/site/' | grep -v '^./vendor/' || true)
if [ -n "$hardcoded" ]; then
  printf '  FAIL  hardcoded storage root(s) found:\n' >&2
  printf '%s\n' "$hardcoded" | sed 's/^/          /' >&2
  printf '        Q-008 item 3: build keys with site.Site.StorageKey, which applies the\n' >&2
  printf '        resolver-supplied prefix.\n' >&2
  fail=1
else
  printf '  ok    no hardcoded storage root\n'
fi

# --- 4. the api never decodes pixels -----------------------------------------
# ADR-002 and Q-034: cmd/api never decodes. The decoder binding must not even be
# linked into it, so a decoder bomb cannot reach the surface that serves every
# other request.
if grep -rqn --include='*.go' 'govips' ./cmd/api ./internal/httpapi 2>/dev/null; then
  printf '  FAIL  the image decoder is reachable from cmd/api or internal/httpapi.\n' >&2
  printf '        ADR-002/Q-034: the api NEVER decodes pixels. Decoding belongs in cmd/worker.\n' >&2
  fail=1
else
  printf '  ok    the image decoder is not linked into the api\n'
fi

if [ $fail -ne 0 ]; then
  printf 'lint-imports: FAILED\n' >&2
  exit 1
fi
printf 'lint-imports: passed\n'
