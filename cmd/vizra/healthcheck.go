package main

import (
	"context"
	"os"

	"github.com/yegamble/vizra-core/internal/healthcheck"
)

// runHealthcheck is the `vizra healthcheck` subcommand. All of the behaviour
// lives in internal/healthcheck so it is testable without a process; this only
// does I/O and the exit, which is the same division `doctor` uses.
//
// It returns the exit code rather than an error: the probe has documented exit
// codes that a container runtime reads, and routing it through main's generic
// "error → exit 1" would collapse a usage mistake into a verdict about the
// service.
func runHealthcheck(args []string) int {
	return healthcheck.Run(context.Background(), args, os.LookupEnv, os.Stdout, os.Stderr)
}
