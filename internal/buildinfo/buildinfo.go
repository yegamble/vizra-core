// Package buildinfo carries the identity of this binary and the image it runs
// in. Everything here is set with -ldflags at build time; nothing is discovered
// at runtime, so /version reports what was actually built rather than what the
// process can currently see.
package buildinfo

import (
	"runtime"
	"strings"
)

// Values injected with -ldflags -X. Defaults describe a `go build` from source.
var (
	Release     = "dev"
	Commit      = "unknown"
	BuiltAt     = ""
	ImageDigest = ""

	// LibvipsVersion and libvipsLoaders are recorded by the Dockerfile from the
	// libvips it built, not read from the linked library, so the value is stable
	// for the life of an image tag (ADR-001).
	LibvipsVersion = ""
	libvipsLoaders = ""
)

// GoVersion reports the toolchain that built this binary.
func GoVersion() string { return runtime.Version() }

// Loaders returns the libvips loader list recorded at image build time, or nil
// when this binary was not built into the image. An unexpected entry here is
// the only detector for a codec that entered the image silently (ADR-001), so
// the list is served by /version rather than only logged.
func Loaders() []string {
	if libvipsLoaders == "" {
		return nil
	}
	parts := strings.Split(libvipsLoaders, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// HasLibvips reports whether image-build libvips facts are present.
func HasLibvips() bool { return LibvipsVersion != "" }
