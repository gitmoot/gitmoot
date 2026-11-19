//go:build e2e

package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/credgw"
)

// enableReviewSourceGatewayFixture configures the remote credential gateway on
// an ephemeral loopback port and starts it on the socket that chose the port.
// Closing that socket and letting the dispatch bind the address later lost the
// port to parallel tests ("bind: address already in use", #2673); the dispatch
// now finds the gateway serving the configured coordinates and reuses it.
func enableReviewSourceGatewayFixture(t *testing.T, paths config.Paths) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	appendConfig(t, paths, fmt.Sprintf("\ncredential_gateway_listen = %q\ncredential_gateway_url = %q\n", address, "https://"+address))
	cfg, err := config.LoadRemoteExecConfig(paths)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	gateway, err := credgw.DefaultRegistry.Gateway(paths.Home, credgw.DefaultLogf)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = credgw.DefaultRegistry.CloseHome(context.Background(), paths.Home) })
	// The same coordinates the dispatch requests, so it reuses this listener.
	if err := gateway.ServeRemote(listener, credgw.RemoteListenerOptions{ListenAddress: cfg.CredentialGatewayListen, AdvertiseURLs: cfg.CredentialGatewayURLs()}); err != nil {
		t.Fatal(err)
	}
}

// Keep the production broker pinned to github.com. Only its HTTP transport is
// redirected to a real local Git server; no host GitHub credential is accessed.
func serveReviewSourceFixture(t *testing.T, paths config.Paths, source, secret string) {
	t.Helper()
	gitExec, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	gitHTTP := &cgi.Handler{Path: filepath.Join(strings.TrimSpace(string(gitExec)), "git-http-backend"), Env: []string{"GIT_PROJECT_ROOT=" + source, "GIT_HTTP_EXPORT_ALL=1"}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, token, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || token != secret {
			http.Error(w, "source authentication required", http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/owner/repo.git/") {
			http.NotFound(w, r)
			return
		}
		r.URL.Path = "/.git" + strings.TrimPrefix(r.URL.Path, "/owner/repo.git")
		gitHTTP.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	previous := http.DefaultTransport
	transport := previous.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "github.com:443" {
			address = server.Listener.Addr().String()
		}
		return dial(ctx, network, address)
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { transport.CloseIdleConnections(); http.DefaultTransport = previous })
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$*\" = 'auth token --hostname github.com' ]; then printf '%s\\n' '" + secret + "'; else exit 99; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	enableReviewSourceGatewayFixture(t, paths)
}
