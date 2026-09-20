package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/migrate"
	"github.com/yegamble/vizra-core/internal/site"
)

func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "report what would be applied, change nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("vizra migrate refused to run.\n%w", err)
	}
	resolver := site.NewResolver(cfg)
	ctx := context.Background()

	embedded, err := migrate.EmbeddedVersion()
	if err != nil {
		return err
	}
	fmt.Printf("embedded schema version: %d\n", embedded)

	if *dryRun {
		// A dry run must not open a migration ledger lock. It reports the
		// embedded set and stops.
		fmt.Printf("dry run: %d site database(s) would be migrated to version %d\n",
			len(resolver.Sites()), embedded)
		for _, s := range resolver.Sites() {
			fmt.Printf("  - %s\n", s.Handle)
		}
		return nil
	}

	// Applies PER DSN with a per-database ledger, and reports a partial failure
	// explicitly rather than stopping at the first error (Q-008 item 6): an
	// operator needs to know which databases are now ahead of which.
	results, runErr := migrate.ApplyAll(ctx, resolver)
	for _, r := range results {
		switch {
		case r.Err != nil:
			fmt.Printf("  %-20s FAIL   %v\n", r.Handle, r.Err)
		case r.Applied:
			fmt.Printf("  %-20s APPLIED to version %d\n", r.Handle, r.Version)
		default:
			fmt.Printf("  %-20s current at version %d\n", r.Handle, r.Version)
		}
	}
	return runErr
}
