package config

import (
	"net/url"
	"strings"
)

// isASCII reports whether s is entirely ASCII.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// NormalizeOrigin renders an origin in the form a browser sends in `Origin`, so
// the two can be compared as values rather than as strings.
//
// This exists because `VIZRA_PUBLIC_ORIGIN=https://photos.example.org/` passes
// validation (a path of "/" is accepted) and is stored verbatim, while a browser
// NEVER sends a trailing slash. A single character in .env then turned every
// browser claim into 403 `origin_mismatch` while curl still worked — on the one
// endpoint an operator cannot skip, with no boot-time signal. The same trap
// applies to an uppercase host, an explicitly written default port, a trailing
// dot on the host, and a Unicode host, which never equals the browser's A-label.
//
// A trailing slash is NORMALISED AWAY rather than refused at boot: normalising
// removes the failure altogether, which is strictly better than a refusal that
// merely makes it legible. A non-ASCII host is the one case that cannot be
// normalised without a new dependency, so that one IS a boot refusal.
//
// It returns "" when the input is not an absolute http(s) origin, so a caller
// comparing two normalised values can never match on "both were unparseable".
func NormalizeOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	// A browser sends scheme://host[:port] and nothing else.
	if u.Path != "" && u.Path != "/" {
		return ""
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return ""
	}

	host := strings.ToLower(u.Hostname())
	// A fully-qualified name with a trailing dot addresses the same host.
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return ""
	}
	// A Unicode host never equals what a browser sends, which sends A-labels.
	// Converting one here would mean taking golang.org/x/net/idna as a new direct
	// dependency, which is a pinned-version decision this slice does not own — so
	// instead config REFUSES a non-ASCII origin at boot with a message naming the
	// A-label form. A boot refusal an operator reads beats a 403 they have to
	// diagnose, which is the same reasoning applied to the trailing slash below.
	if !isASCII(host) {
		return ""
	}
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}

	port := u.Port()
	// The default port is elided by every browser.
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + host + ":" + port
	}
	return scheme + "://" + host
}
