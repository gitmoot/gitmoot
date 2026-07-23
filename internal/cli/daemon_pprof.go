package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/pprof"
	"time"
)

// startDaemonPprofServer starts an off-by-default localhost diagnostics listener
// exposing net/http/pprof so a RUNNING daemon can be profiled (goroutine dumps,
// CPU/heap profiles) without a restart or a crashing SIGQUIT (#1111).
//
// It serves ONLY a dedicated ServeMux and never http.DefaultServeMux, so the
// pprof handlers can never leak onto another server that happens to serve the
// default mux. The caller passes the operator-supplied address (use a loopback
// address such as 127.0.0.1:6060; profile a remote host over an SSH tunnel). The
// listener is opt-in via --pprof-addr and returns a stop func for a deferred
// close; it also shuts itself down when ctx is cancelled.
func startDaemonPprofServer(ctx context.Context, addr string, stdout io.Writer) func() {
	srv := &http.Server{Addr: addr, Handler: newDaemonPprofMux(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		writeLine(stdout, "pprof: diagnostics listener on http://%s/debug/pprof/ (enabled by --pprof-addr; omit it to disable)", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			writeLine(stdout, "pprof: listener error: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownDaemonPprofServer(srv)
	}()
	return func() { shutdownDaemonPprofServer(srv) }
}

// newDaemonPprofMux builds the dedicated pprof mux (never DefaultServeMux).
func newDaemonPprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	// pprof.Index also dispatches /debug/pprof/<name> (goroutine, heap, block,
	// mutex, threadcreate, ...), so this one handler covers the named profiles.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

func shutdownDaemonPprofServer(srv *http.Server) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
