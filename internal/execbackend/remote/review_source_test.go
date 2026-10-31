package remote

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/credgw"
	"github.com/gitmoot/gitmoot/internal/execbackend"
)

func TestRemoteReviewFetchesExactSourceWithoutHostCheckout(t *testing.T) {
	ctx := context.Background()
	source := testSourceRepo(t)
	testGit(t, source, "add", "-A")
	testGit(t, source, "commit", "-q", "-m", "base")
	base := strings.TrimSpace(testGit(t, source, "rev-parse", "HEAD"))
	testGit(t, source, "branch", "-M", "main")
	testGit(t, source, "checkout", "-q", "-b", "review")
	testWriteFile(t, source, "input.txt", []byte("exact remote change\n"), 0600)
	testGit(t, source, "add", "-A")
	testGit(t, source, "commit", "-q", "-m", "review")
	head := strings.TrimSpace(testGit(t, source, "rev-parse", "HEAD"))
	gitExec, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	const upstreamSecret = "github-source-secret-must-stay-on-host"
	var reads atomic.Int32
	gitHTTP := &cgi.Handler{Path: filepath.Join(strings.TrimSpace(string(gitExec)), "git-http-backend"), Env: []string{"GIT_PROJECT_ROOT=" + source, "GIT_HTTP_EXPORT_ALL=1"}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, token, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || token != upstreamSecret {
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/owner/repo.git/") {
			http.NotFound(w, r)
			return
		}
		reads.Add(1)
		r.URL.Path = "/.git" + strings.TrimPrefix(r.URL.Path, "/owner/repo.git")
		gitHTTP.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	gateway, err := credgw.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close(ctx)
	if err := gateway.EnableRemote(credgw.RemoteListenerOptions{ListenAddress: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	url := upstream.URL + "/owner/repo.git"
	lease, err := gateway.RegisterProxy("source-review", credgw.ProxyPolicy{
		Upstream: url, AuthKind: credgw.ProxyAuthKind("git_read"), AllowLoopbackHTTP: true,
		SandboxID: "sandbox-one", Runtime: "shell", ExpiresAt: time.Now().Add(time.Minute), AllowedHosts: []string{"127.0.0.1"},
	}, func(context.Context) (credgw.ResolvedCredential, error) {
		return credgw.ResolvedCredential{Upstream: url, AuthKind: credgw.ProxyAuthKind("git_read"), Value: upstreamSecret}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Revoke()
	material := lease.RemoteMaterial()
	harness := newProviderHarness(t)
	backend := harness.backend(t)
	instance, err := backend.Provision(ctx, execbackend.JobScope{JobID: "source-review", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Destroy(ctx, instance)
	identityDir := filepath.Join(filepath.Dir(harness.workspace), ".gitmoot-source")
	config := "[remote \"source\"]\nurl = " + strconv.Quote(material.URL) + "\n[http]\n" +
		"sslCAInfo = " + strconv.Quote(filepath.Join(identityDir, "ca.pem")) + "\n" +
		"sslCert = " + strconv.Quote(filepath.Join(identityDir, "client.pem")) + "\n" +
		"sslKey = " + strconv.Quote(filepath.Join(identityDir, "client.key")) + "\n" +
		"extraHeader = " + strconv.Quote(credgw.CapabilityHeader+": "+material.Capability) + "\n" +
		"extraHeader = " + strconv.Quote("Authorization: Bearer "+material.Placeholder) + "\nfollowRedirects = false\n"
	// JSON exercises the extensible materials boundary and keeps the test
	// buildable on the prior implementation for behavioral red/green proof.
	encoded, err := json.Marshal(map[string]any{"RemoteReview": map[string]any{
		"Head": head, "BaseRef": "main", "Access": execbackend.CredentialMaterial{
			CACertificate: material.CACertificate, ClientCertificate: material.ClientCertificate,
			ClientPrivateKey: material.ClientPrivateKey, ClientConfig: []byte(config),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var materials execbackend.Materials
	if err := json.Unmarshal(encoded, &materials); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(source, "host-scratch-unavailable"))
	if err := backend.SyncIn(ctx, instance, materials); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(testGit(t, harness.workspace, "rev-parse", "HEAD")); got != head {
		t.Fatalf("review head = %s, want %s", got, head)
	}
	if got := strings.TrimSpace(testGit(t, harness.workspace, "rev-parse", "@{upstream}")); got != base {
		t.Fatalf("review base = %s, want %s", got, base)
	}
	if got := testGit(t, harness.workspace, "diff", "@{upstream}", "HEAD", "--", "input.txt"); !strings.Contains(got, "+exact remote change") {
		t.Fatalf("wrong review diff: %s", got)
	}
	if got := testGit(t, harness.workspace, "status", "--porcelain"); got != "" {
		t.Fatalf("dirty prepared source: %s", got)
	}
	if got := testGit(t, harness.workspace, "remote"); got != "" {
		t.Fatalf("source capability retained as a remote: %s", got)
	}
	if _, err := os.Stat(identityDir); !os.IsNotExist(err) {
		t.Fatalf("source identity survived preparation: %v", err)
	}
	if reads.Load() == 0 {
		t.Fatal("private repository was not fetched through the broker")
	}
	for _, uploaded := range harness.uploads {
		if bytes.Contains(uploaded, []byte(upstreamSecret)) || bytes.Contains(uploaded, []byte(base64.StdEncoding.EncodeToString([]byte("x-access-token:"+upstreamSecret)))) {
			t.Fatal("upstream credential left host in uploaded material")
		}
	}
	stream, err := backend.Exec(ctx, instance, execbackend.Command{Name: "git", Args: []string{"rev-parse", "HEAD"}, Dir: "/home/user/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stream.Wait()
	if err != nil {
		t.Fatalf("runtime result=%+v error=%v", result, err)
	}
	if got := strings.TrimSpace(result.Stdout); got != head {
		t.Fatalf("runtime saw head %s, want %s", got, head)
	}
}
