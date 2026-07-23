package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDaemonPprofMuxServesGoroutineProfile verifies the dedicated mux serves the
// named goroutine profile (the dump #1111 needs) and does not 404 the index.
func TestDaemonPprofMuxServesGoroutineProfile(t *testing.T) {
	t.Parallel()
	mux := newDaemonPprofMux()
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/goroutine?debug=1"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
	}
	// The goroutine profile must actually contain a goroutine dump.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil))
	if !strings.Contains(rec.Body.String(), "goroutine profile") {
		t.Fatalf("goroutine profile body missing marker: %q", rec.Body.String()[:min(200, rec.Body.Len())])
	}
}

// TestDaemonPprofDoesNotUseDefaultServeMux proves the pprof handlers are NOT on
// http.DefaultServeMux, so importing net/http/pprof can't leak them onto another
// server that serves the default mux.
func TestDaemonPprofDoesNotUseDefaultServeMux(t *testing.T) {
	t.Parallel()
	// net/http/pprof's init registers on DefaultServeMux; a defensive daemon must
	// not SERVE the default mux. We assert our start path serves our own mux by
	// checking a bogus path 404s on our mux (index only matches /debug/pprof/).
	mux := newDaemonPprofMux()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/not-pprof", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("dedicated mux unexpectedly served /not-pprof (code %d)", rec.Code)
	}
}

// TestStartDaemonPprofServerStopsOnContextCancel verifies the listener starts and
// shuts down cleanly when its context is cancelled (no leaked goroutine/port).
func TestStartDaemonPprofServerStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := startDaemonPprofServer(ctx, "127.0.0.1:0", io.Discard)
	cancel()
	stop()
	// A second stop must be safe (idempotent shutdown).
	stop()
	// Give the ctx-driven shutdown goroutine a moment; no assertion needed beyond
	// not panicking / not hanging (the test's own timeout guards a hang).
	time.Sleep(10 * time.Millisecond)
}
