package config

import "testing"

// originEnv is a minimal development environment with one variable under test.
// Everything else falls through to the registry defaults.
func originEnv(origin string) Lookup {
	return func(k string) (string, bool) {
		switch k {
		case "VIZRA_PUBLIC_ORIGIN":
			return origin, true
		case "DATABASE_URL":
			return "postgres://vizra@127.0.0.1:5432/vizra?sslmode=disable", true
		}
		return "", false
	}
}

// TestNormalizeOriginMatchesWhatABrowserSends.
//
// `VIZRA_PUBLIC_ORIGIN=https://photos.example.org/` passes validation — a path
// of "/" is accepted — and used to be stored verbatim and compared as a string.
// Browsers never send a trailing slash, so one character in .env turned every
// browser claim into 403 `origin_mismatch` while curl still worked, on the first
// endpoint an operator touches, with no boot-time signal.
func TestNormalizeOriginMatchesWhatABrowserSends(t *testing.T) {
	const browser = "https://photos.example.org"
	equivalent := []string{
		"https://photos.example.org",
		"https://photos.example.org/",     // the trailing slash config accepts
		"HTTPS://Photos.Example.ORG",      // case, both halves
		"https://photos.example.org:443",  // the default port written explicitly
		"https://photos.example.org.",     // a fully-qualified trailing dot
		"  https://photos.example.org  ",  // stray whitespace in .env
		"HTTPS://PHOTOS.EXAMPLE.ORG:443/", // all of them at once
	}
	want := NormalizeOrigin(browser)
	if want != browser {
		t.Fatalf("NormalizeOrigin(%q) = %q, want it unchanged", browser, want)
	}
	for _, in := range equivalent {
		if got := NormalizeOrigin(in); got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNormalizeOriginKeepsGenuinelyDifferentOriginsDifferent: the whole point of
// the check is that a cross-origin request is refused, so normalisation must not
// become a way to make anything match.
func TestNormalizeOriginKeepsGenuinelyDifferentOriginsDifferent(t *testing.T) {
	const base = "https://photos.example.org"
	for _, other := range []string{
		"http://photos.example.org",       // scheme
		"https://evil.example",            // host
		"https://photos.example.org:8443", // a NON-default port
		"https://photos.example.org.evil.co",
		"https://sub.photos.example.org", // a subdomain is a different origin
	} {
		if NormalizeOrigin(other) == NormalizeOrigin(base) {
			t.Errorf("NormalizeOrigin collapsed %q into %q", other, base)
		}
	}
}

// TestNormalizeOriginRefusesWhatItCannotCompare returns "" rather than a
// best-effort value, so two unparseable inputs can never compare equal.
func TestNormalizeOriginRefusesWhatItCannotCompare(t *testing.T) {
	for _, bad := range []string{
		"", "   ", "photos.example.org", "ftp://photos.example.org",
		"https://", "https://photos.example.org/path",
		"https://photos.example.org?q=1", "https://photos.example.org#f",
		"https://user:pw@photos.example.org",
		"https://фотки.example", // non-ASCII: refused at boot, never guessed at
	} {
		if got := NormalizeOrigin(bad); got != "" {
			t.Errorf("NormalizeOrigin(%q) = %q, want \"\"", bad, got)
		}
	}
	if NormalizeOrigin("https://ф.example") == NormalizeOrigin("https://x.example") {
		t.Error("two unnormalisable values must not compare equal")
	}
}

// TestLoadStoresTheNormalisedPublicOrigin: normalising once at boot is what
// makes the handler's comparison a value comparison.
func TestLoadStoresTheNormalisedPublicOrigin(t *testing.T) {
	cfg, err := LoadFrom(originEnv("HTTPS://Photos.Example.ORG:443/"))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if cfg.PublicOrigin != "https://photos.example.org" {
		t.Fatalf("PublicOrigin = %q, want the normalised form", cfg.PublicOrigin)
	}
}

// TestProductionRefusesAnOriginItCannotCompare: a boot refusal an operator reads
// beats a 403 they have to diagnose.
func TestProductionRefusesAnOriginItCannotCompare(t *testing.T) {
	_, err := LoadFrom(originEnv("https://фотки.example"))
	if err == nil {
		t.Fatal("a non-ASCII public origin must be refused at boot, not silently 403 every browser claim")
	}
	ve, ok := AsValidationError(err)
	if !ok || !ve.Has("VIZRA_PUBLIC_ORIGIN") {
		t.Fatalf("the refusal must name VIZRA_PUBLIC_ORIGIN, got %v", err)
	}
}
