//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yegamble/vizra-core/internal/migrate"
)

// VZ-FOUND-003 R1: a dirty migration ledger is detected and reported, never
// auto-fixed. The ledger is set dirty exactly as golang-migrate leaves it when a
// migration fails part-way: same version, dirty = true.
func TestDirtyLedgerIsReportedNotRepaired(t *testing.T) {
	cfg, _, pool := freshDatabase(t)
	ctx := t.Context()

	embedded, err := migrate.EmbeddedVersion()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE schema_migrations SET dirty = true`); err != nil {
		t.Fatalf("marking the ledger dirty: %v", err)
	}

	applied, version, err := migrate.Apply(ctx, cfg.DatabaseURL)
	if err == nil {
		t.Fatal("migrate up on a dirty ledger returned no error; it must refuse")
	}
	if applied {
		t.Fatal("migrate up reported applying changes on a dirty ledger")
	}
	if want := fmt.Sprintf("dirty at version %d", embedded); !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not name the dirty version (want substring %q)", err, want)
	}
	if version != embedded {
		t.Fatalf("Apply returned version %d, want the dirty version %d", version, embedded)
	}

	st := migrate.Probe(ctx, pool, embedded)
	if st.State != migrate.StateDirty || !st.Dirty || st.AppliedVersion != embedded {
		t.Fatalf("status = %+v, want state %s, dirty, version %d", st, migrate.StateDirty, embedded)
	}

	// Nothing auto-repairs: the raw ledger row is unchanged.
	var v int64
	var dirty bool
	if err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&v, &dirty); err != nil {
		t.Fatal(err)
	}
	if !dirty || v != embedded {
		t.Fatalf("ledger after refused migrate = (version %d, dirty %v), want (%d, true): it was auto-repaired", v, dirty, embedded)
	}
}
