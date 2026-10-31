package credgw

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemoteGitLeasePinsRepositoryReadAuthority(t *testing.T) {
	const secret = "host-private-repository-credential"
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + secret))
	var reached atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		user, token, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || token != secret {
			t.Error("host credential missing from upstream request")
		}
		if r.URL.Path != "/owner/repo.git/info/refs" && r.URL.Path != "/owner/repo.git/git-upload-pack" {
			t.Errorf("scope escaped: %s", r.URL.Path)
		}
		w.Header().Set("X-Reflected-Auth", encoded)
		io.WriteString(w, "source-positive-control:"+secret+":"+encoded)
	}))
	defer upstream.Close()
	gateway, err := Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(context.Background())
	if err := gateway.EnableRemote(RemoteListenerOptions{ListenAddress: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	url := upstream.URL + "/owner/repo.git"
	lease, err := gateway.RegisterProxy("source-scope", ProxyPolicy{
		Upstream: url, AuthKind: ProxyAuthKind("git_read"), AllowLoopbackHTTP: true,
		SandboxID: "source-instance", Runtime: "shell", ExpiresAt: time.Now().Add(time.Minute), AllowedHosts: []string{"127.0.0.1"},
	}, func(context.Context) (ResolvedCredential, error) {
		return ResolvedCredential{Upstream: url, AuthKind: ProxyAuthKind("git_read"), Value: secret}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Revoke()
	material := lease.RemoteMaterial()
	client := remoteMaterialClient(t, material)
	call := func(method, suffix, contentType string) (int, string) {
		t.Helper()
		request, err := http.NewRequest(method, material.URL+suffix, strings.NewReader("0000"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set(CapabilityHeader, material.Capability)
		request.Header.Set("Authorization", "Bearer "+material.Placeholder)
		request.Header.Set("Content-Type", contentType)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), secret) || strings.Contains(string(body), encoded) || strings.Contains(response.Header.Get("X-Reflected-Auth"), encoded) {
			t.Fatal("source gateway leaked its host credential")
		}
		return response.StatusCode, string(body)
	}
	for _, allowed := range []struct{ method, suffix, contentType string }{
		{"GET", "/info/refs?service=git-upload-pack", ""},
		{"POST", "/git-upload-pack", "application/x-git-upload-pack-request"},
	} {
		status, body := call(allowed.method, allowed.suffix, allowed.contentType)
		if status != http.StatusOK || !strings.Contains(body, "source-positive-control") {
			t.Fatalf("allowed source operation: status=%d", status)
		}
	}
	positive := reached.Load()
	if positive != 2 {
		t.Fatalf("positive requests reached=%d", positive)
	}
	for _, denied := range []struct{ method, suffix, contentType string }{
		{"GET", "/info/refs?service=git-receive-pack", ""},
		{"POST", "/git-receive-pack", "application/x-git-receive-pack-request"},
		{"POST", "/git-upload-pack?repository=other", "application/x-git-upload-pack-request"},
		{"POST", "/git-upload-pack", "application/json"},
		{"GET", "/other/repo.git/info/refs?service=git-upload-pack", ""},
		{"GET", "/info/refs?service=git-upload-pack&service=git-receive-pack", ""},
		{"DELETE", "/git-upload-pack", ""},
	} {
		status, _ := call(denied.method, denied.suffix, denied.contentType)
		if status != http.StatusForbidden {
			t.Fatalf("denied %s %s status=%d", denied.method, denied.suffix, status)
		}
	}
	if got := reached.Load(); got != positive {
		t.Fatalf("forbidden operations reached upstream: %d -> %d", positive, got)
	}
	lease.Revoke()
	if status, _ := call("GET", "/info/refs?service=git-upload-pack", ""); status != http.StatusUnauthorized {
		t.Fatalf("revoked source lease status=%d", status)
	}
	if got := reached.Load(); got != positive {
		t.Fatal("revoked source lease reached upstream")
	}
}
