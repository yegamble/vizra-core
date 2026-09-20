#!/usr/bin/env bash
# migrate-lint — migration discipline (ADR-002 § Migration discipline).
#
# Goes beyond Vidra's lint, which has no append-only immutability check, no
# filename-format lint, no sequence-gap detection and no required down file.
# All four are here:
#
#   1. filename format            NNNN_snake_name.{up,down}.sql
#   2. sequence gaps and duplicates
#   3. a down file, or an explicit `-- no-down: <reason>` in the up file
#   4. destructive statements inside the compatibility window
#   5. the append-only checksum manifest (scripts/migration-manifest.sh)
#
# Exit 1 on any violation. Every message names the file and says what to do.
set -euo pipefail

cd "$(dirname "$0")/.."
DIR=migrations
fail=0
err() { printf '  FAIL  %s\n' "$*" >&2; fail=1; }
ok()  { printf '  ok    %s\n' "$*"; }

printf 'migrate-lint: %s\n' "$DIR"

shopt -s nullglob
ups=("$DIR"/*.up.sql)
if [ ${#ups[@]} -eq 0 ]; then
  err "no migrations found in $DIR"
  exit 1
fi

# --- 1. filename format -------------------------------------------------------
for f in "$DIR"/*.sql; do
  base=$(basename "$f")
  if ! [[ "$base" =~ ^[0-9]{4}_[a-z0-9]+(_[a-z0-9]+)*\.(up|down)\.sql$ ]]; then
    err "$base: filename must be NNNN_snake_case_name.up.sql or .down.sql"
  fi
done

# --- 2. sequence: gapless 1..N, no duplicates --------------------------------
versions=()
for f in "${ups[@]}"; do
  versions+=("$(basename "$f" | cut -c1-4)")
done
IFS=$'\n' sorted=($(printf '%s\n' "${versions[@]}" | sort)); unset IFS
prev=0
for v in "${sorted[@]}"; do
  n=$((10#$v))
  if [ "$n" -eq "$prev" ]; then
    err "version $v appears more than once"
  elif [ "$n" -ne $((prev + 1)) ]; then
    err "version gap: $(printf '%04d' $((prev + 1))) is missing before $v"
  fi
  prev=$n
done
[ $fail -eq 0 ] && ok "$(printf '%d' ${#ups[@]}) migrations form a gapless 1..${prev} sequence"

# --- 3. a down file, or an explicit reason not to have one -------------------
for f in "${ups[@]}"; do
  base=$(basename "$f" .up.sql)
  down="$DIR/$base.down.sql"
  if [ ! -f "$down" ]; then
    if ! grep -qE '^\s*--\s*no-down:\s*\S' "$f"; then
      err "$base: no $base.down.sql and no '-- no-down: <reason>' line in the up file"
    else
      ok "$base: no down file, with a stated reason"
    fi
  elif [ ! -s "$down" ]; then
    err "$base.down.sql is empty; write DROP statements or delete it and state '-- no-down: <reason>'"
  fi
done

# --- 4. destructive statements in an up migration ----------------------------
# Inside the compatibility window an up migration must be additive: a rolled-back
# deploy has to keep working against the new schema (ADR-002 § Rollback floor).
# A deliberate exception is written as `-- allow-destructive: <reason>` on the
# line before, which makes it a reviewed decision rather than an accident.
destructive='DROP[[:space:]]+(TABLE|COLUMN|CONSTRAINT|INDEX|TYPE|SCHEMA|VIEW|SEQUENCE)|ALTER[[:space:]]+TABLE[[:space:]]+[^;]*RENAME|ALTER[[:space:]]+TABLE[[:space:]]+[^;]*ALTER[[:space:]]+COLUMN[[:space:]]+[^;]*TYPE|TRUNCATE'
for f in "${ups[@]}"; do
  base=$(basename "$f")
  while IFS=: read -r lineno line; do
    [ -z "${lineno:-}" ] && continue
    # Ignore matches inside a comment.
    case "$(printf '%s' "$line" | sed 's/^[[:space:]]*//')" in --*) continue;; esac
    prevline=$(sed -n "$((lineno - 1))p" "$f" || true)
    if printf '%s' "$prevline" | grep -qE '^\s*--\s*allow-destructive:\s*\S'; then
      ok "$base:$lineno destructive statement with a stated reason"
      continue
    fi
    err "$base:$lineno destructive statement in an up migration: ${line#"${line%%[![:space:]]*}"}"
    printf '        If this is deliberate, put '\''-- allow-destructive: <reason>'\'' on the line before.\n' >&2
  done < <(grep -nEi "$destructive" "$f" || true)
done

# --- 5. append-only checksum manifest ----------------------------------------
if ! ./scripts/migration-manifest.sh check; then
  fail=1
fi

if [ $fail -ne 0 ]; then
  printf 'migrate-lint: FAILED\n' >&2
  exit 1
fi
printf 'migrate-lint: passed\n'
