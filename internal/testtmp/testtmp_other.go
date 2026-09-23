//go:build !unix

package testtmp

import "testing"

// Run on a platform without the unix process model: the tests run with no
// temporary root, no TMPDIR redirection and no sweep. Vizra's tests and CI run
// on unix only; this exists so the packages that call Run still build and vet
// elsewhere.
func Run(m *testing.M, name string) int {
	_ = name
	return m.Run()
}
