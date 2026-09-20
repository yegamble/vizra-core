#!/usr/bin/env bash
# migration-manifest — append-only immutability for merged migrations.
#
# Migrations are append-only (AGENTS.md). Once a migration has been merged, its
# bytes are frozen: a database that already applied it will never re-apply it,
# so an edit silently produces two different schemas that both claim the same
# version. A bad merged migration is fixed only by a NEWER migration.
#
#   check     recompute every hash and compare with migrations/manifest.sha256
#   generate  rewrite the manifest (for a NEW migration only)
#
# `check` is the local half. CI adds the other half: it asserts that the diff of
# manifest.sha256 against the base branch contains only ADDED lines, so
# regenerating the manifest cannot launder an edit to a merged migration.
set -euo pipefail

cd "$(dirname "$0")/.."
DIR=migrations
MANIFEST="$DIR/manifest.sha256"

hash_all() {
  # Sorted, so the manifest is stable across filesystems.
  ( cd "$DIR" && find . -maxdepth 1 -name '*.sql' -print0 \
      | sort -z \
      | xargs -0 shasum -a 256 \
      | sed 's|\./||' )
}

case "${1:-check}" in
generate)
  hash_all > "$MANIFEST"
  printf 'migration-manifest: wrote %s (%d entries)\n' "$MANIFEST" "$(wc -l < "$MANIFEST" | tr -d ' ')"
  printf 'Commit it in the same PR as the new migration.\n'
  ;;
check)
  if [ ! -f "$MANIFEST" ]; then
    printf '  FAIL  %s is missing. Run: make migrations-manifest\n' "$MANIFEST" >&2
    exit 1
  fi
  actual=$(hash_all)
  expected=$(cat "$MANIFEST")
  if [ "$actual" = "$expected" ]; then
    printf '  ok    append-only manifest matches (%d migrations)\n' "$(printf '%s\n' "$expected" | wc -l | tr -d ' ')"
    exit 0
  fi
  printf '  FAIL  migrations/manifest.sha256 does not match the files on disk.\n' >&2
  printf '\n' >&2
  diff <(printf '%s\n' "$expected") <(printf '%s\n' "$actual") | sed 's/^/        /' >&2 || true
  printf '\n' >&2
  printf '        A CHANGED hash for an existing migration is an append-only violation:\n' >&2
  printf '        a database that already applied it will never re-apply it, so the edit\n' >&2
  printf '        produces two different schemas claiming the same version. Fix it with a\n' >&2
  printf '        NEW migration instead.\n' >&2
  printf '        Only when you have ADDED a migration: make migrations-manifest\n' >&2
  exit 1
  ;;
*)
  printf 'usage: %s [check|generate]\n' "$0" >&2
  exit 2
  ;;
esac
