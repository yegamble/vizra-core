package main

import (
	"net/http"
	"testing"

	"github.com/yegamble/vizra-core/internal/config"
)

// R-2: the verifier removed ReadTimeout and WriteTimeout from the api server
// and every lane stayed green. cmd/ had no test files at all.
//
// Each of these bounds a different way a connection can be held open, and
// every one of them is zero by default in net/http — which means "no limit",
// not "a sensible one".
func TestAPIServerBoundsEveryPhaseOfAConnection(t *testing.T) {
	cfg := &config.Config{ListenAddr: ":8080"}
	srv := newAPIServer(cfg, http.NotFoundHandler())

	for _, tc := range []struct {
		name  string
		value func() int64
		why   string
	}{
		{"ReadHeaderTimeout", func() int64 { return int64(srv.ReadHeaderTimeout) },
			"a client that opens a connection and never finishes its headers holds it forever"},
		{"ReadTimeout", func() int64 { return int64(srv.ReadTimeout) },
			"once the headers are in, a slow-body client holds the connection indefinitely"},
		{"WriteTimeout", func() int64 { return int64(srv.WriteTimeout) },
			"a client that stops reading the response holds the handler and its connection"},
		{"IdleTimeout", func() int64 { return int64(srv.IdleTimeout) },
			"a keep-alive connection doing nothing is never reclaimed"},
	} {
		if tc.value() == 0 {
			t.Errorf("%s is zero. In net/http zero means NO LIMIT, not a sensible default: %s.", tc.name, tc.why)
		}
	}

	if srv.Addr != ":8080" {
		t.Errorf("Addr = %q, want the configured listen address", srv.Addr)
	}
	if srv.Handler == nil {
		t.Error("the server has no handler")
	}

	// Ordering that must hold for the timeouts to mean what they say.
	if srv.ReadHeaderTimeout > srv.ReadTimeout {
		t.Errorf("ReadHeaderTimeout (%v) exceeds ReadTimeout (%v); the header budget would never bite",
			srv.ReadHeaderTimeout, srv.ReadTimeout)
	}
}
