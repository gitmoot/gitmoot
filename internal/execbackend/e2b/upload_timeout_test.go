package e2b

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestUploadUsesItsOwnDeadline is the regression for the defect that made remote
// execution work only for toy checkouts.
//
// The workspace upload shared DefaultRequestTimeout with every control-plane
// call. 15 seconds is generous for a status poll and impossible for a 73 MiB
// tarball, so syncing this repository's own worktree into a sandbox died with
// "envd upload request failed: context deadline exceeded" while a small stand-in
// repo synced fine.
//
// The server here sleeps LONGER than a control-plane timeout and shorter than an
// upload timeout, so the test fails if the two are ever merged again.
//
// MUTATION: change Upload back to e.requestTimeout and this goes red with a
// deadline error.
func TestUploadUsesItsOwnDeadline(t *testing.T) {
	const controlTimeout = 50 * time.Millisecond
	const serverDelay = 250 * time.Millisecond

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(serverDelay)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	envd := newUploadTestEnvd(t, server.URL, controlTimeout, 5*time.Second)
	if err := envd.Upload(context.Background(), "/home/user/.gitmoot-sync.tar.gz", bytes.NewReader([]byte("payload"))); err != nil {
		t.Fatalf("Upload returned error: %v\nthe upload must not inherit the control-plane deadline", err)
	}
}

// TestUploadStillRespectsItsOwnDeadline pins that the separate timeout is a real
// bound rather than an absence of one. An unbounded upload would hang a job
// instead of failing it.
func TestUploadStillRespectsItsOwnDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	envd := newUploadTestEnvd(t, server.URL, 5*time.Second, 50*time.Millisecond)
	if err := envd.Upload(context.Background(), "/home/user/.gitmoot-sync.tar.gz", bytes.NewReader([]byte("payload"))); err == nil {
		t.Fatal("Upload succeeded past its own deadline; the bound must still apply")
	}
}

// TestZeroUploadTimeoutSelectsTheBulkDefault pins that an unset option does not
// silently fall back to the control-plane value, which is how the defect would
// return.
func TestZeroUploadTimeoutSelectsTheBulkDefault(t *testing.T) {
	envd := newUploadTestEnvd(t, "http://127.0.0.1:1", 15*time.Second, 0)
	if envd.uploadTimeout != DefaultUploadTimeout {
		t.Fatalf("uploadTimeout = %s, want DefaultUploadTimeout (%s)", envd.uploadTimeout, DefaultUploadTimeout)
	}
	if envd.uploadTimeout == envd.requestTimeout {
		t.Fatal("upload and control-plane timeouts are equal again; a tarball cannot share a status poll's deadline")
	}
}

func newUploadTestEnvd(t *testing.T, baseURL string, requestTimeout, uploadTimeout time.Duration) *Envd {
	t.Helper()
	envd, err := NewEnvd(Sandbox{ID: "sbx-upload"}, EnvdCredential{token: testEnvdToken}, EnvdOptions{
		RequestTimeout:   requestTimeout,
		UploadTimeout:    uploadTimeout,
		EndpointResolver: func(string, int) string { return baseURL },
	})
	if err != nil {
		t.Fatalf("NewEnvd returned error: %v", err)
	}
	return envd
}
