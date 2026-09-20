// Command vizra is the operator CLI (ADR-002): it orchestrates compose and runs
// setup, doctor, migrate, backup and restore. M0 ships `version`, `doctor` and
// `migrate`; setup, backup and restore arrive with VZ-ISSUE-002…004.
//
// `doctor` performs REAL checks only. A check whose prerequisite is missing
// reports SKIP with the reason; it never reports OK for something it did not
// test. That rule is why doctor is worth running at all.
package main

import (
	"fmt"
	"os"
)

const usage = `vizra — the Vizra operator CLI

Usage:
  vizra version            Print build identity, including the libvips loader list.
  vizra doctor [--env F]   Check configuration, database, cache and search. Exit 1 on any FAIL.
  vizra migrate [--dry-run]
                           Apply embedded migrations to every configured site.

Commands arriving with later slices: setup, backup, restore, update, jobs.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "version":
		err = runVersion(os.Args[2:])
	case "doctor":
		err = runDoctor(os.Args[2:])
	case "migrate":
		err = runMigrate(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "vizra: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
