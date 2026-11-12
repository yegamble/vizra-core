// Package migrate applies the embedded migrations and reports the ledger state
// that /schemaz serves.
//
// Two rules from ADR-002 live here rather than in a script, because a script is
// not what `vizra update` and the deploy ordering actually call:
//
//   - The migrator takes a DSN and applies per DSN, with a per-database ledger
//     and an explicit partial-failure state (Q-008 checklist item 6). Applying
//     to three sites and failing on the second must not look like success.
//   - A dirty ledger is REPORTED, never auto-repaired. `force` exists in
//     golang-migrate; it is not wired to anything here, because the one time it
//     is convenient is the one time it destroys the evidence.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yegamble/vizra-core/internal/site"
	"github.com/yegamble/vizra-core/migrations"
)

// State is what /schemaz reports.
type State string

const (
	StateCurrent State = "current"
	StateBehind  State = "behind"
	StateAhead   State = "ahead"
	StateDirty   State = "dirty"
	StateUnknown State = "unknown"
)

// Status is the full ledger answer.
type Status struct {
	AppliedVersion  int64
	EmbeddedVersion int64
	Dirty           bool
	State           State
	Detail          string
}

// EmbeddedVersion is the highest migration version compiled into this binary.
// `vizra update` refuses an image whose embedded maximum is below live
// /schemaz (ADR-002 § Rollback floor), so this number is part of the release
// contract, not a diagnostic.
func EmbeddedVersion() (int64, error) {
	entries, err := fs.ReadDir(migrations.FS(), ".")
	if err != nil {
		return 0, err
	}
	var max int64
	seen := map[int64]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		i := strings.Index(name, "_")
		if i <= 0 {
			return 0, fmt.Errorf("migrate: %s does not start with a version prefix", name)
		}
		v, err := strconv.ParseInt(name[:i], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migrate: %s has a non-numeric version prefix", name)
		}
		if seen[v] {
			return 0, fmt.Errorf("migrate: version %d appears twice", v)
		}
		seen[v] = true
		if v > max {
			max = v
		}
	}
	if max == 0 {
		return 0, errors.New("migrate: no migrations are embedded")
	}
	// Sequence gaps would make "behind" ambiguous.
	versions := make([]int64, 0, len(seen))
	for v := range seen {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	for i, v := range versions {
		if v != int64(i+1) {
			return 0, fmt.Errorf("migrate: migration versions are not a gapless 1..N sequence; found %d at position %d", v, i+1)
		}
	}
	return max, nil
}

func newMigrator(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.FS(), ".")
	if err != nil {
		return nil, err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, normalizeDSN(dsn))
	if err != nil {
		// Never echo the DSN.
		return nil, fmt.Errorf("migrate: cannot open the migration ledger: %w", redact(err, dsn))
	}
	return m, nil
}

// normalizeDSN makes golang-migrate use the pgx/v5 driver rather than the
// stdlib one, which is what `_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"`
// registers.
func normalizeDSN(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "pgx5://"):
		return dsn
	case strings.HasPrefix(dsn, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgres://")
	case strings.HasPrefix(dsn, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgresql://")
	}
	return dsn
}

// SiteResult is one site's outcome in a multi-site run.
type SiteResult struct {
	Handle  string
	Applied bool
	Version int64
	Err     error
}

// ApplyAll applies migrations to every site the resolver knows, and reports a
// per-site result. It does NOT stop at the first failure: an operator needs to
// know which databases are now ahead of which, and a partial failure that
// reports only the first error hides exactly that.
func ApplyAll(ctx context.Context, r *site.Resolver) ([]SiteResult, error) {
	var out []SiteResult
	var failed int
	for _, s := range r.Sites() {
		res := SiteResult{Handle: s.Handle}
		applied, v, err := applyOne(ctx, s.DSN)
		res.Applied, res.Version, res.Err = applied, v, err
		if err != nil {
			failed++
		}
		out = append(out, res)
	}
	if failed > 0 {
		return out, fmt.Errorf("migrate: %d of %d site database(s) failed to migrate", failed, len(out))
	}
	return out, nil
}

func applyOne(ctx context.Context, dsn string) (bool, int64, error) {
	m, err := newMigrator(dsn)
	if err != nil {
		return false, 0, err
	}
	defer func() { _, _ = m.Close() }()

	before, dirty, verr := m.Version()
	if verr != nil && !errors.Is(verr, migrate.ErrNilVersion) {
		return false, 0, fmt.Errorf("migrate: reading the ledger: %w", verr)
	}
	if dirty {
		// Reported, never auto-repaired.
		return false, int64(before), fmt.Errorf(
			"migrate: the ledger is dirty at version %d; a previous migration failed part-way. "+
				"Inspect the database and resolve it deliberately: this is not repaired automatically", before)
	}

	err = m.Up()
	switch {
	case errors.Is(err, migrate.ErrNoChange):
		return false, int64(before), nil
	case err != nil:
		return false, int64(before), fmt.Errorf("migrate: applying: %w", err)
	}
	after, _, _ := m.Version()
	return true, int64(after), nil
}

// Apply migrates a single DSN.
func Apply(ctx context.Context, dsn string) (bool, int64, error) { return applyOne(ctx, dsn) }

// schemaMigrationsTable is golang-migrate's default ledger table.
const schemaMigrationsTable = "schema_migrations"

// Probe reads the ledger with the application's own pool, so /schemaz needs no
// second connection and no migration driver at request time.
//
// It always succeeds from the caller's point of view: an unreadable ledger is
// StateUnknown, because /schemaz always returns 200 (ADR-002 § Probes) and the
// deploy scripts must be able to distinguish "behind" from "unreachable".
func Probe(ctx context.Context, pool *pgxpool.Pool, embedded int64) Status {
	st := Status{EmbeddedVersion: embedded, State: StateUnknown}
	if pool == nil {
		st.Detail = "no database pool"
		return st
	}
	var version int64
	var dirty bool
	err := pool.QueryRow(ctx,
		`SELECT version, dirty FROM `+schemaMigrationsTable+` LIMIT 1`).Scan(&version, &dirty)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			st.AppliedVersion = 0
			st.State = StateBehind
			st.Detail = "the ledger is empty; run `vizra migrate`"
			return st
		}
		st.Detail = "the migration ledger could not be read"
		return st
	}
	st.AppliedVersion = version
	st.Dirty = dirty
	switch {
	case dirty:
		st.State = StateDirty
		st.Detail = "a previous migration failed part-way; resolve it deliberately"
	case version < embedded:
		st.State = StateBehind
		st.Detail = "run `vizra migrate`"
	case version > embedded:
		st.State = StateAhead
		st.Detail = "the database is newer than this binary; rolling this image out is refused"
	default:
		st.State = StateCurrent
	}
	return st
}

func redact(err error, dsn string) error {
	if dsn == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), dsn, "<DATABASE_URL>")
	return errors.New(msg)
}

// ensure the pgx/v5 driver is linked.
var _ = pgx.Postgres{}
