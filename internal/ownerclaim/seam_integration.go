//go:build integration

package ownerclaim

// SetAfterClaimedCheckHookForTest installs h as the afterClaimedCheck seam and
// returns a function that removes it.
//
// This file is compiled ONLY under `-tags=integration`, so no binary built for
// production can call it. It exists so the integration suite can pause a
// claimant between its claimed check and its token examination and let the
// world change underneath it, deterministically, instead of hoping a race test
// hits the window (CI run 35814919455 hit it once, and only once).
func SetAfterClaimedCheckHookForTest(h func()) (restore func()) {
	afterClaimedCheck.Store(&h)
	return func() { afterClaimedCheck.Store(nil) }
}
