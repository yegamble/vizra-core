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
#
# INVARIANT (rule 4): main's per-line detection and exemption run unchanged; the statement parser (4b) only adds
# failures. So this script rejects (what main 5f615d7 rejects) UNION (new hits), and accepts a subset of what main accepts.
set -euo pipefail

cd "$(dirname "$0")/.."

# --dir lets the meta-test (scripts/scripts_test.go) run this against fixture
# trees under scripts/testdata/migratelint, so the rules have negative cases of
# their own. Without that, the destructive-statement regex was protected by
# nothing: a mutant that removed COLUMN from the alternation survived the whole
# gate.
DIR=migrations
CHECK_MANIFEST=1
while [ $# -gt 0 ]; do
  case "$1" in
    --dir) DIR="$2"; CHECK_MANIFEST=0; shift 2 ;;
    --no-manifest) CHECK_MANIFEST=0; shift ;;
    *) printf 'migrate-lint: unknown argument %s\n' "$1" >&2; exit 2 ;;
  esac
done
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

# --- 4b. column drops the per-line pattern cannot see -------------------------
# The loop above is main's (5f615d7), unchanged: detection and exemption per
# line. Its column rule needs the literal COLUMN keyword on the line of DROP.
# PostgreSQL needs neither: `ALTER TABLE sites DROP handle;` is a column drop,
# and so is DROP on one line with COLUMN on the next. So column drops are ALSO
# found per STATEMENT, and this part can only ADD failures: it never
# suppresses, skips or exempts a hit of the loop above.
#
# column_drops prints the line of the DROP token of every
# `DROP [COLUMN] [IF EXISTS] name` action of an ALTER TABLE statement. It
# tokenises the file: `--` and (nested) /* */ comments are dropped, 'strings',
# "quoted identifiers" and $tag$ dollar-quoted bodies each become one opaque
# token, and tokens are collected up to `;` (or end of file).
#
# In an ALTER TABLE statement a DROP outside parentheses is a column drop
# unless it is `DROP CONSTRAINT` (the line pattern's business, as before), or
# it is `DROP DEFAULT | NOT NULL | EXPRESSION | IDENTITY` inside an
# `ALTER [COLUMN] name ...` action. The second condition matters: EXPRESSION and
# IDENTITY are not reserved, so `ALTER TABLE t DROP identity` drops a column.
# Anything the rule does not recognise is reported, not passed.
#
# The exemption for an added hit is the same as everywhere else and nothing
# more: the marker on the line immediately before the line holding the DROP
# token. A marker before a multi-line statement does NOT cover a DROP further
# down; put the marker directly above the DROP line.
#
# Limits, stated rather than hidden. Each one can only cost an ADDED detection;
# none can turn a failure of the loop above into a pass:
#   - the inside of a dollar-quoted body or a string is not parsed, so dynamic
#     SQL (DO $$ ... $$, EXECUTE 'ALTER TABLE ...') is seen only by the line
#     pattern above, which still needs `DROP COLUMN` on one line;
#   - E'...' strings with backslash-escaped quotes and psql meta-commands are
#     not understood;
#   - only ALTER TABLE is parsed. ALTER FOREIGN TABLE, ALTER TYPE ... DROP
#     ATTRIBUTE and every other rule (DROP CONSTRAINT, RENAME, TYPE, ...) stay
#     per-line, exactly as they were.
column_drops() {
  awk '
    function emit(t) {
      n++; tok[n] = t; tl[n] = NR
    }
    function flush(   i, d, altering, nx) {
      if (n >= 2 && tok[1] == "ALTER" && tok[2] == "TABLE") {
        d = 0; altering = 0
        for (i = 3; i <= n; i++) {
          if (tok[i] == "(") d++
          else if (tok[i] == ")") d--
          else if (d != 0) continue
          else if (tok[i] == ",") altering = 0
          else if (tok[i] == "ALTER") altering = 1
          else if (tok[i] == "DROP") {
            nx = (i < n) ? tok[i + 1] : ""
            if (nx == "CONSTRAINT") continue
            if (altering && (nx == "DEFAULT" || nx == "EXPRESSION" || nx == "IDENTITY")) continue
            if (altering && nx == "NOT" && i + 2 <= n && tok[i + 2] == "NULL") continue
            print tl[i]
          }
        }
      }
      for (i = 1; i <= n; i++) { delete tok[i]; delete tl[i] }
      n = 0
    }
    {
      s = $0; L = length(s); p = 1
      while (p <= L) {
        c = substr(s, p, 1); c2 = substr(s, p, 2)
        if (bc > 0) {
          if (c2 == "*/") { bc--; p += 2 } else if (c2 == "/*") { bc++; p += 2 } else p++
          continue
        }
        if (sq) {
          if (c2 == "\047\047") p += 2; else { if (c == "\047") sq = 0; p++ }
          continue
        }
        if (dq) {
          if (c2 == "\"\"") p += 2; else { if (c == "\"") dq = 0; p++ }
          continue
        }
        if (dtag != "") {
          if (substr(s, p, length(dtag)) == dtag) { p += length(dtag); dtag = "" } else p++
          continue
        }
        if (c2 == "--") break
        if (c2 == "/*") { bc = 1; p += 2; continue }
        if (c == "\047") { sq = 1; emit("\047"); p++; continue }
        if (c == "\"") { dq = 1; emit("\""); p++; continue }
        rest = substr(s, p)
        if (c == "$" && match(rest, /^\$([A-Za-z_][A-Za-z_0-9]*)?\$/)) {
          dtag = substr(rest, 1, RLENGTH); emit("$"); p += RLENGTH; continue
        }
        if (match(rest, /^[A-Za-z_][A-Za-z_0-9$]*/)) {
          emit(toupper(substr(rest, 1, RLENGTH))); p += RLENGTH; continue
        }
        if (c == ";") { flush(); p++; continue }
        if (c != " " && c != "\t" && c != "\r") emit(c)
        p++
      }
    }
    END { flush() }
  ' "$1"
}

for f in "${ups[@]}"; do
  base=$(basename "$f")
  while read -r lineno; do
    [ -z "${lineno:-}" ] && continue
    line=$(sed -n "${lineno}p" "$f")
    # A line the per-line pattern matches was judged by the loop above, by the
    # same rule (marker on the line before); do not report it twice.
    if printf '%s\n' "$line" | grep -qEi "$destructive"; then continue; fi
    prevline=$(sed -n "$((lineno - 1))p" "$f" || true)
    if printf '%s' "$prevline" | grep -qE '^\s*--\s*allow-destructive:\s*\S'; then
      ok "$base:$lineno column drop with a stated reason"
      continue
    fi
    err "$base:$lineno destructive statement in an up migration (column drop): ${line#"${line%%[![:space:]]*}"}"
    printf '        If this is deliberate, put '\''-- allow-destructive: <reason>'\'' on the line directly before this line.\n' >&2
  done < <(column_drops "$f" | sort -un)
done

# --- 5. append-only checksum manifest ----------------------------------------
if [ "$CHECK_MANIFEST" -eq 1 ]; then
  if ! ./scripts/migration-manifest.sh check; then
    fail=1
  fi
fi

if [ $fail -ne 0 ]; then
  printf 'migrate-lint: FAILED\n' >&2
  exit 1
fi
printf 'migrate-lint: passed\n'
