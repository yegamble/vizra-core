// Package scripts exists so the shell and Python programs in this directory can
// have Go meta-tests.
//
// Three scripts carry load-bearing CI semantics — migrate-lint, the
// required-checks guard and the import lint — and none of them had a test.
// Mutants of all three survived the entire gate: removing COLUMN from
// migrate-lint's destructive alternation, and every spelling of
// continue-on-error the guard's regex could not see. A check whose own
// correctness nothing pins is a check that quietly stops checking.
//
// There is no Go code here beyond this file; scripts_test.go runs the scripts
// against scripts/testdata and asserts their exit codes.
package scripts
