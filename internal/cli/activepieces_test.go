package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/activepieces"
)

func TestResolveBridgeBind(t *testing.T) {
	cases := []struct {
		goos        string
		wantAddr    string
		wantRemote  bool
		wantURLHost string
	}{
		{goos: "linux", wantAddr: "172.17.0.1:8791", wantRemote: true, wantURLHost: "host.docker.internal:8791"},
		{goos: "darwin", wantAddr: "127.0.0.1:8791", wantRemote: false, wantURLHost: "host.docker.internal:8791"},
		{goos: "windows", wantAddr: "127.0.0.1:8791", wantRemote: false, wantURLHost: "host.docker.internal:8791"},
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			addr, allowRemote, reachableURL := resolveBridgeBind(tc.goos)
			if addr != tc.wantAddr || allowRemote != tc.wantRemote || !strings.Contains(reachableURL, tc.wantURLHost) {
				t.Fatalf("resolveBridgeBind(%q) = %q, %v, %q", tc.goos, addr, allowRemote, reachableURL)
			}
			if tc.goos == "linux" && bridgeHostIsLoopback(strings.Split(addr, ":")[0]) {
				t.Fatalf("linux address %q is loopback", addr)
			}
		})
	}
}

func TestIsExactPieceVersion(t *testing.T) {
	exact := []string{"0.1.2", "1.0.0", "12.34.56"}
	for _, v := range exact {
		if !isExactPieceVersion(v) {
			t.Errorf("isExactPieceVersion(%q) = false, want true", v)
		}
	}
	ranged := []string{"", "~0.1.2", "^0.1.0", "0.1", "latest", "0.1.2-beta", "v0.1.2"}
	for _, v := range ranged {
		if isExactPieceVersion(v) {
			t.Errorf("isExactPieceVersion(%q) = true, want false", v)
		}
	}
}

func TestActivepiecesTemplatesList(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runActivepieces([]string{"templates", "list"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"webhook-run-pipeline",
		"Receive a webhook and enqueue a named Gitmoot pipeline.",
		"gmail-imap-ask-agent",
		"send an SMTP acknowledgement.",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("templates list output missing %q:\n%s", want, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestActivepiecesBridgePIDFileRoundTrip(t *testing.T) {
	stackDir := t.TempDir()
	if err := writeActivepiecesBridgePID(stackDir, 1234, "127.0.0.1:8791"); err != nil {
		t.Fatalf("writeActivepiecesBridgePID: %v", err)
	}
	process, err := readActivepiecesBridgePID(stackDir)
	if err != nil {
		t.Fatalf("readActivepiecesBridgePID: %v", err)
	}
	if process.PID != 1234 || process.Addr != "127.0.0.1:8791" {
		t.Fatalf("process = %+v", process)
	}
	info, err := os.Stat(filepath.Join(stackDir, activepiecesBridgePIDFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("pid file mode = %o, want 600", got)
	}
}

func TestActivepiecesBridgeVerifyBeforeStopAndStaleCleanup(t *testing.T) {
	stackDir := t.TempDir()
	writePID := func() {
		t.Helper()
		if err := writeActivepiecesBridgePID(stackDir, 1234, "127.0.0.1:8791"); err != nil {
			t.Fatal(err)
		}
	}
	writePID()
	terminated := false
	var stdout bytes.Buffer
	err := reconcileActivepiecesBridgePID(
		stackDir,
		true,
		&stdout,
		func(int) (bool, bool) { return true, false },
		func(int) error {
			terminated = true
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if terminated {
		t.Fatal("unverified process was terminated")
	}
	if !strings.Contains(stdout.String(), "nothing to stop") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(stackDir, activepiecesBridgePIDFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified pid file was not removed: %v", err)
	}

	writePID()
	stdout.Reset()
	err = reconcileActivepiecesBridgePID(
		stackDir,
		false,
		&stdout,
		func(int) (bool, bool) { return false, false },
		func(int) error { return syscall.EPERM },
	)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stale cleanup stdout = %q, want empty", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(stackDir, activepiecesBridgePIDFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale pid file was not removed: %v", err)
	}
}

func TestIsGitmootBridgeCmdline(t *testing.T) {
	if !isGitmootBridgeCmdline([]byte("/usr/local/bin/gitmoot\x00bridge\x00serve\x00--addr\x00127.0.0.1:8791\x00")) {
		t.Fatal("expected gitmoot bridge command line to be recognized")
	}
	if isGitmootBridgeCmdline([]byte("/usr/local/bin/gitmoot\x00bridge\x00token\x00")) {
		t.Fatal("non-serve bridge command line was recognized")
	}
}

func TestConfirmStarterTemplatesAcceptsNonTerminalStdin(t *testing.T) {
	stdin, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		stdin.Close()
		stdinWriter.Close()
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = stdin
	defer func() {
		os.Stdin = originalStdin
		stdin.Close()
		stdinWriter.Close()
		stdout.Close()
	}()

	result := make(chan bool, 1)
	go func() { result <- confirmStarterTemplates(stdout, false) }()
	select {
	case accepted := <-result:
		if !accepted {
			t.Fatal("confirmStarterTemplates rejected non-terminal stdin")
		}
	case <-time.After(time.Second):
		stdinWriter.Close()
		<-result
		t.Fatal("confirmStarterTemplates blocked on non-terminal stdin")
	}
}

func TestRunActivepiecesTemplatesImportReordersFlags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/authentication/sign-up":
			_, _ = w.Write([]byte(`{"token":"test-token","projectId":"project-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/flows":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/flows":
			_, _ = w.Write([]byte(`{"id":"flow-1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/flows/flow-1":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := runActivepiecesTemplatesImport([]string{
		"gmail-imap-ask-agent",
		"--url", server.URL,
		"--password", "test-password",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Imported Gitmoot: Email to Agent Acknowledgement") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestImportActivepiecesTemplatesDeletesFlowAfterImportFailure(t *testing.T) {
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/flows":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/flows":
			_, _ = w.Write([]byte(`{"id":"flow-1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/flows/flow-1":
			http.Error(w, "import failed", http.StatusBadRequest)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/flows/flow-1":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := activepieces.NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = importActivepiecesTemplates(context.Background(), client, "test-token", "project-1", []string{"gmail-imap-ask-agent"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "import failed") {
		t.Fatalf("error = %v, want import failure", err)
	}
	if !deleted {
		t.Fatal("created flow was not deleted after import failure")
	}
}
