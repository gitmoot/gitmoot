//go:build e2e

package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

type localProvisionBackend struct {
	provisionCalls int
	syncInCalls    int
	scope          execbackend.JobScope
	materials      execbackend.Materials
	instance       *execbackend.Instance
}

func (*localProvisionBackend) Name() execbackend.Backend { return execbackend.Local }

func (b *localProvisionBackend) Provision(_ context.Context, scope execbackend.JobScope) (*execbackend.Instance, error) {
	b.provisionCalls++
	b.scope = scope
	if b.instance == nil {
		b.instance = &execbackend.Instance{ID: "local-provision", JobID: scope.JobID, LifecycleGeneration: scope.LifecycleGeneration, Workspace: "/local-provision"}
	}
	return b.instance, nil
}

func (*localProvisionBackend) Attach(context.Context, string) (*execbackend.Instance, error) {
	return nil, nil
}

func (b *localProvisionBackend) SyncIn(_ context.Context, _ *execbackend.Instance, materials execbackend.Materials) error {
	b.syncInCalls++
	b.materials = materials
	return nil
}

func (*localProvisionBackend) Exec(context.Context, *execbackend.Instance, execbackend.Command) (execbackend.Stream, error) {
	return nil, nil
}

func (*localProvisionBackend) Cancel(context.Context, *execbackend.Instance) error { return nil }

func (*localProvisionBackend) Destroy(context.Context, *execbackend.Instance) error { return nil }

func TestProvisionExecutionBackendLocalBehaviorUnchanged(t *testing.T) {
	backend := &localProvisionBackend{}
	uid, gid := uint32(996), uint32(986)
	cfg := config.DefaultRemoteExecConfig()
	cfg.LocalUID, cfg.LocalGID = &uid, &gid
	worker := jobWorker{ExecutionBackendFactory: func(got execbackend.Backend, gotCfg config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		if got != execbackend.Local {
			t.Fatalf("factory backend = %q, want %q", got, execbackend.Local)
		}
		identity := gotCfg.LocalIdentity()
		if identity == nil || identity.UID != uid || identity.GID != gid {
			t.Fatalf("factory config identity = %+v, want uid %d gid %d", identity, uid, gid)
		}
		return backend, nil
	}}
	job := db.Job{ID: "local-credential-gate", LifecycleGeneration: 4}

	const ttl = 9 * time.Minute
	lifecycle, instance, lease, env, err := worker.provisionExecutionBackend(context.Background(), execbackend.Local, cfg, runtime.ShellRuntime, job, ttl, "/checkout")
	if err != nil {
		t.Fatalf("provisionExecutionBackend(local): %v", err)
	}
	if lease != nil || len(env) != 0 {
		t.Fatalf("local credential gateway lease/env = %v %v", lease, env)
	}
	if lifecycle != backend || instance != backend.instance {
		t.Fatalf("local lifecycle = %T, instance = %+v; want injected backend and its instance", lifecycle, instance)
	}
	if backend.provisionCalls != 1 || backend.syncInCalls != 1 {
		t.Fatalf("local Provision calls = %d, SyncIn calls = %d; want 1, 1", backend.provisionCalls, backend.syncInCalls)
	}
	if backend.scope.JobID != job.ID || backend.scope.LifecycleGeneration != job.LifecycleGeneration || backend.scope.TTL != ttl {
		t.Fatalf("local provision scope = %+v, want job id %q generation %d ttl %s", backend.scope, job.ID, job.LifecycleGeneration, ttl)
	}
	if backend.materials.SourceWorktree != "/checkout" {
		t.Fatalf("local SyncIn source = %q, want /checkout", backend.materials.SourceWorktree)
	}
}

func TestLocalBackendInstanceRunnerPreservesCuratedEnvironment(t *testing.T) {
	checkout := createDaemonWorkerGitCheckout(t, "curated-backend")
	backend, err := execbackend.NewLocalBackend(filepath.Join(t.TempDir(), "instances"), nil)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: "curated"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })
	if err := backend.SyncIn(context.Background(), instance, execbackend.Materials{SourceWorktree: checkout}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITMOOT_BACKEND_MUST_NOT_LEAK", "host-secret")
	runner := graftRuntimeBaseRunner(
		execbackend.InstanceRunner{Backend: backend, Instance: instance},
		subprocess.CuratedGroupRunner{BaseEnv: []string{"PATH=" + os.Getenv("PATH"), "CURATED=yes"}},
		"",
	)
	result, err := runner.Run(context.Background(), instance.Workspace, "sh", "-c", "env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Stdout, "GITMOOT_BACKEND_MUST_NOT_LEAK=") {
		t.Fatalf("backend inherited an environment entry omitted by curation:\n%s", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "CURATED=yes") {
		t.Fatalf("backend dropped curated environment:\n%s", result.Stdout)
	}
}

func TestDefaultExecutionBackendUsesConfiguredIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("no privilege to apply a configured non-root identity")
	}
	uid, gid := configuredLocalTestIdentity(t)
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	localParent, err := os.MkdirTemp(os.TempDir(), "gitmoot-configured-local-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(localParent, 0o755); err != nil {
		t.Fatal(err)
	}
	localRoot := filepath.Join(localParent, "instances")
	t.Cleanup(func() {
		_ = os.Remove(localRoot)
		_ = os.Remove(localParent)
	})
	content := fmt.Sprintf("[remote_exec]\nbackend = \"local\"\nlocal_uid = %d\nlocal_gid = %d\nlocal_root = %q\n", uid, gid, localRoot)
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	worker := jobWorker{ConfigHome: home, ConfigHomeExplicit: true}
	cfg := executionBackendConfigForTest(t, worker)
	// Delivery must consume the resolved snapshot, not re-read a config that may
	// change after preflight and select a different execution identity.
	if err := os.WriteFile(paths.ConfigFile, []byte("[remote_exec]\nbackend = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := worker.defaultExecutionBackend(execbackend.Local, cfg)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := lifecycle.Provision(context.Background(), execbackend.JobScope{JobID: "configured-identity"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lifecycle.Destroy(context.Background(), instance) })
	checkout := createDaemonWorkerGitCheckout(t, "configured-identity")
	if err := lifecycle.SyncIn(context.Background(), instance, execbackend.Materials{SourceWorktree: checkout}); err != nil {
		t.Fatal(err)
	}
	stream, err := lifecycle.Exec(context.Background(), instance, execbackend.Command{Dir: instance.Workspace, Name: "id", Args: []string{"-u"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stream.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(result.Stdout), strconv.FormatUint(uint64(uid), 10); got != want {
		t.Fatalf("configured command-reported euid = %q, want %s", got, want)
	}
}

func TestRuntimeContractPreflightUsesConfiguredExecutionIdentityOnlyForLifecycleDelivery(t *testing.T) {
	const configured = "[remote_exec]\nbackend = \"local\"\nlocal_uid = 996\nlocal_gid = 986\nlocal_root = \"/var/tmp/gitmoot-local\"\n"
	tests := []struct {
		name          string
		config        string
		withLifecycle bool
		wantUID       int
		wantKnown     bool
	}{
		{
			name:          "configured identity with lifecycle delivery",
			config:        configured,
			withLifecycle: true,
			wantUID:       996,
			wantKnown:     true,
		},
		{
			name:   "configured identity without lifecycle delivery",
			config: configured,
		},
		{
			name:          "default daemon identity with lifecycle delivery",
			withLifecycle: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			if tt.config != "" {
				paths := config.PathsForHome(home)
				if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.ConfigFile, []byte(tt.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			worker := jobWorker{
				ConfigHome:         home,
				ConfigHomeExplicit: true,
				RuntimePreflight: func(_ context.Context, _ runtime.Agent, request runtime.RuntimeContractRequest) runtime.RuntimeContractResult {
					calls++
					if request.EffectiveUIDKnown != tt.wantKnown || request.EffectiveUID != tt.wantUID {
						t.Fatalf("runtime preflight effective uid = %d known %t, want %d known %t", request.EffectiveUID, request.EffectiveUIDKnown, tt.wantUID, tt.wantKnown)
					}
					return runtime.RuntimeContractResult{State: runtime.RuntimeContractSupported}
				},
			}
			if tt.withLifecycle {
				worker.ExecutionBackendFactory = func(execbackend.Backend, config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
					return nil, nil
				}
			}
			cfg := executionBackendConfigForTest(t, worker)
			_, checked, err := worker.runtimeContractPreflight(context.Background(), execbackend.Local, cfg, runtime.Agent{}, runtime.RuntimeContractRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if !checked || calls != 1 {
				t.Fatalf("runtime preflight = checked %t calls %d, want checked with one call", checked, calls)
			}
		})
	}
}

func configuredLocalTestIdentity(t *testing.T) (uint32, uint32) {
	t.Helper()
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		t.Skipf("no unprivileged identity configured: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		uid, uidErr := strconv.ParseUint(fields[2], 10, 32)
		gid, gidErr := strconv.ParseUint(fields[3], 10, 32)
		if uidErr == nil && gidErr == nil && uid > 0 && gid > 0 && uid < uint64(^uint32(0)) && gid < uint64(^uint32(0)) {
			return uint32(uid), uint32(gid)
		}
	}
	t.Skip("no unprivileged identity configured in /etc/passwd")
	return 0, 0
}

const (
	remoteLifecycleAPIKey      = "api-key-GITMOOT-IMPL-remote-lifecycle"
	remoteLifecycleAccessToken = "envd-access-token-GITMOOT-IMPL"
)

type remoteLifecycleCreateRequest struct {
	TemplateID string            `json:"templateID"`
	Timeout    int32             `json:"timeout"`
	AutoPause  bool              `json:"autoPause"`
	Secure     bool              `json:"secure"`
	Metadata   map[string]string `json:"metadata"`
}

type remoteLifecycleStartRequest struct {
	Process struct {
		Command string            `json:"cmd"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"envs"`
		Dir     string            `json:"cwd"`
	} `json:"process"`
}

type remoteLifecycleHarness struct {
	t *testing.T

	control *httptest.Server
	envd    *httptest.Server

	mu             sync.Mutex
	creates        []remoteLifecycleCreateRequest
	deleted        []string
	listCalls      int
	listStatus     int
	uploads        map[string][]byte
	workspace      string
	runtimeEnv     map[string]string
	runtimeStarted bool
	createdAt      time.Time
}

// firstCreateAt is when the provider received its first create request, or
// zero before then.
func (h *remoteLifecycleHarness) firstCreateAt() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.createdAt
}

// GITMOOT-IMPL: newRemoteLifecycleHarness stands in for both paid E2B planes.
// It runs the provider lifecycle and git scripts locally, but every production
// client request still crosses HTTP and Connect framing exactly as it would off-box.
func newRemoteLifecycleHarness(t *testing.T) *remoteLifecycleHarness {
	t.Helper()
	h := &remoteLifecycleHarness{t: t, workspace: filepath.Join(t.TempDir(), "workspace"), uploads: make(map[string][]byte)}
	h.control = httptest.NewServer(http.HandlerFunc(h.serveControl))
	h.envd = httptest.NewServer(http.HandlerFunc(h.serveEnvd))
	t.Cleanup(h.control.Close)
	t.Cleanup(h.envd.Close)
	return h
}

func (h *remoteLifecycleHarness) serveControl(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("X-API-Key"); got != remoteLifecycleAPIKey {
		h.t.Errorf("control X-API-Key = %q", got)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v2/sandboxes":
		h.mu.Lock()
		h.listCalls++
		status := h.listStatus
		h.mu.Unlock()
		if status != 0 {
			http.Error(w, "list failed", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "[]")
	case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
		var request remoteLifecycleCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			h.t.Errorf("decode create request: %v", err)
			http.Error(w, "bad create", http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.creates = append(h.creates, request)
		if h.createdAt.IsZero() {
			h.createdAt = time.Now()
		}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sandboxID":       "sandbox-1",
			"templateID":      request.TemplateID,
			"envdAccessToken": remoteLifecycleAccessToken,
			"domain":          nil,
		})
	case r.Method == http.MethodDelete && r.URL.Path == "/sandboxes/sandbox-1":
		h.mu.Lock()
		h.deleted = append(h.deleted, "sandbox-1")
		h.mu.Unlock()
		// A destroyed sandbox takes read-only trees with it; the fake restores
		// write bits first so an unprivileged test user can remove them too.
		restoreTreeWriteBits(h.workspace)
		if err := os.RemoveAll(h.workspace); err != nil {
			h.t.Errorf("remove sandbox workspace: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		h.t.Errorf("unexpected control request %s %s", r.Method, r.URL.RequestURI())
		http.NotFound(w, r)
	}
}

func (h *remoteLifecycleHarness) serveEnvd(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("X-Access-Token"); got != remoteLifecycleAccessToken {
		h.t.Errorf("envd X-Access-Token = %q", got)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/files":
		remotePath := r.URL.Query().Get("path")
		local, err := h.guestPath(remotePath)
		if err != nil {
			h.t.Errorf("upload path = %q: %v", remotePath, err)
			http.Error(w, "bad upload path", http.StatusBadRequest)
			return
		}
		if got := r.URL.Query().Get("username"); got != "user" {
			h.t.Errorf("upload username = %q", got)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			h.t.Errorf("read upload: %v", err)
			http.Error(w, "bad upload", http.StatusBadRequest)
			return
		}
		// Land the upload where the instance would see it, so the REAL guest
		// scripts run against it rather than a Go re-implementation.
		landed := data
		if remotePath == "/home/user/.gitmoot-source/gitconfig" {
			landed = bytes.ReplaceAll(data, []byte("/home/user/"), []byte(filepath.Dir(h.workspace)+"/"))
		}
		if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
			h.t.Errorf("stage upload: %v", err)
		} else if err := os.WriteFile(local, landed, 0o600); err != nil {
			h.t.Errorf("stage upload: %v", err)
		}
		h.mu.Lock()
		h.uploads[remotePath] = append([]byte(nil), data...)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	case r.Method == http.MethodPost && r.URL.Path == "/process.Process/Start":
		flag, data, err := readRemoteLifecycleConnectFrame(r.Body)
		if err != nil || flag != 0 {
			h.t.Errorf("read Start frame flag=%d err=%v", flag, err)
			http.Error(w, "bad frame", http.StatusBadRequest)
			return
		}
		var request remoteLifecycleStartRequest
		if err := json.Unmarshal(data, &request); err != nil {
			h.t.Errorf("decode Start request: %v", err)
			http.Error(w, "bad start", http.StatusBadRequest)
			return
		}
		stdout, stderr, exitCode := h.runStart(request)
		w.Header().Set("Content-Type", "application/connect+json")
		writeRemoteLifecycleConnectJSON(h.t, w, 0, map[string]any{"event": map[string]any{"start": map[string]any{"pid": 73}}})
		if len(stdout) > 0 || len(stderr) > 0 {
			writeRemoteLifecycleConnectJSON(h.t, w, 0, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": stdout, "stderr": stderr}}})
		}
		writeRemoteLifecycleConnectJSON(h.t, w, 0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": exitCode, "exited": true, "status": fmt.Sprintf("exit status %d", exitCode)}}})
		writeRemoteLifecycleConnectJSON(h.t, w, 2, map[string]any{})
	default:
		h.t.Errorf("unexpected envd request %s %s", r.Method, r.URL.RequestURI())
		http.NotFound(w, r)
	}
}

func (h *remoteLifecycleHarness) runStart(request remoteLifecycleStartRequest) ([]byte, []byte, int) {
	dir, err := h.guestPath(request.Process.Dir)
	if err != nil {
		return nil, []byte(err.Error()), 1
	}
	args := make([]string, len(request.Process.Args))
	for index, arg := range request.Process.Args {
		args[index] = h.hostPathFor(arg)
	}
	cmd := exec.Command(request.Process.Command, args...)
	cmd.Dir = dir
	env := []string{"HOME=" + filepath.Dir(h.workspace), "PATH=" + os.Getenv("PATH")}
	keys := make([]string, 0, len(request.Process.Env))
	for key := range request.Process.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+h.hostPathFor(request.Process.Env[key]))
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			stderr.WriteString(err.Error())
			exitCode = 1
		}
	}
	runtimeCommand := false
	for _, arg := range request.Process.Args {
		if strings.Contains(arg, "artifact.bin") {
			runtimeCommand = true
			break
		}
	}
	if request.Process.Command == "sh" && runtimeCommand {
		h.mu.Lock()
		h.runtimeStarted = true
		h.runtimeEnv = make(map[string]string, len(request.Process.Env))
		for key, value := range request.Process.Env {
			h.runtimeEnv[key] = value
		}
		h.mu.Unlock()
	}
	return stdout.Bytes(), stderr.Bytes(), exitCode
}

func (h *remoteLifecycleHarness) snapshot() ([]remoteLifecycleCreateRequest, []string, int, map[string]string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	creates := append([]remoteLifecycleCreateRequest(nil), h.creates...)
	deleted := append([]string(nil), h.deleted...)
	env := make(map[string]string, len(h.runtimeEnv))
	for key, value := range h.runtimeEnv {
		env[key] = value
	}
	return creates, deleted, h.listCalls, env, h.runtimeStarted
}

// guestPath maps an instance path below /home/user onto the harness root.
func (h *remoteLifecycleHarness) guestPath(remote string) (string, error) {
	remote = filepath.ToSlash(filepath.Clean(remote))
	root := filepath.Dir(h.workspace)
	if remote == "/home/user" {
		return root, nil
	}
	if strings.HasPrefix(remote, "/home/user/") {
		return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(remote, "/home/user/"))), nil
	}
	return "", fmt.Errorf("unexpected instance path %q", remote)
}

// hostPathFor rewrites an absolute instance path argument so a command run
// on the test host touches the harness root, never the host's /home/user.
func (h *remoteLifecycleHarness) hostPathFor(value string) string {
	if !strings.HasPrefix(value, "/home/user/") {
		return value
	}
	local, err := h.guestPath(value)
	if err != nil {
		return value
	}
	return local
}

// remoteLifecycleArchiveEntries returns every entry of an uploaded archive.
func remoteLifecycleArchiveEntries(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	entries := make(map[string][]byte)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = content
	}
}

func readRemoteLifecycleConnectFrame(reader io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	data := make([]byte, binary.BigEndian.Uint32(header[1:]))
	if _, err := io.ReadFull(reader, data); err != nil {
		return 0, nil, err
	}
	return header[0], data, nil
}

func writeRemoteLifecycleConnectJSON(t *testing.T, writer io.Writer, flag byte, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var header [5]byte
	header[0] = flag
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := writer.Write(append(header[:], payload...)); err != nil {
		t.Errorf("write Connect frame: %v", err)
	}
}

// GITMOOT-IMPL: a daemon restart must reconcile the configured remote account
// before waiting for another job; the startup path may list but must not create.
func TestExecutionBackendJobWorkerReapsConfiguredRemoteAtStartup(t *testing.T) {
	harness := newRemoteLifecycleHarness(t)
	home, paths, store := heartbeatLoopE2EHome(t)
	writeRemoteLifecycleConfig(t, paths, harness.control.URL)
	staleLocalRoot := filepath.Join(paths.Home, "execbackends", string(execbackend.Local), "stale-local")
	if err := os.MkdirAll(staleLocalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleLocalRoot, "instance.json"), []byte(`{"version":1,"id":"stale-local","job_id":"stale-local","owner_pid":0,"state":"running"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = executionBackendJobWorker(store, io.Discard, home)
	creates, deleted, listCalls, _, _ := harness.snapshot()
	if listCalls != 1 || len(creates) != 0 || len(deleted) != 0 {
		t.Fatalf("startup provider calls: list=%d create=%d delete=%d; want 1, 0, 0", listCalls, len(creates), len(deleted))
	}
	if _, err := os.Stat(staleLocalRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale local instance survived remote-configured startup: %v", err)
	}
}

// GITMOOT-IMPL: a failed provider inventory must stay loud and retryable; a
// daemon must not provision against an account it could not reconcile.
func TestExecutionBackendJobWorkerReportsAndRetriesRemoteStartupReapFailure(t *testing.T) {
	harness := newRemoteLifecycleHarness(t)
	harness.listStatus = http.StatusServiceUnavailable
	home, paths, store := heartbeatLoopE2EHome(t)
	writeRemoteLifecycleConfig(t, paths, harness.control.URL)
	var output bytes.Buffer
	worker := executionBackendJobWorker(store, &output, home)
	if !strings.Contains(output.String(), "execution backend startup reap failed for remote: reap remote execution backends") {
		t.Fatalf("startup output = %q, want loud remote reap failure", output.String())
	}
	if _, err := worker.ExecutionBackendFactory(execbackend.Remote, executionBackendConfigForTest(t, worker)); err == nil || !strings.Contains(err.Error(), "reap remote execution backends") {
		t.Fatalf("retry remote startup reap error = %v, want loud reconciliation failure", err)
	}
	_, _, listCalls, _, _ := harness.snapshot()
	if listCalls != 2 {
		t.Fatalf("remote reap list calls = %d, want 2 after sentinel-restored retry", listCalls)
	}
}

// GITMOOT-IMPL: the remote-only job-type refusal must never reject ordinary
// local work. A local ask reaches the shell runtime and succeeds.
func TestLocalExecutionBackendAllowsNonImplement(t *testing.T) {
	requireReadOnlySeatLaunchable(t)
	ctx := context.Background()
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(""))
	jobID := execBackendDispatchAsk(t, home)
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}
	assertExecBackendLocalSucceeded(t, store, jobID)
}

func writeRemoteLifecycleConfig(t *testing.T, paths config.Paths, baseURL string) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "e2b-api-key")
	if err := os.WriteFile(keyFile, []byte(remoteLifecycleAPIKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A cost cap is now REQUIRED for any remote provision (#1540): an unset cap
	// denies rather than permitting unlimited spend, so this E2E must name one
	// the way a real deployment enabling cloud has to. The values are a
	// disposable ceiling for the fake provider, not a spend authorisation.
	content := fmt.Sprintf("\n[remote_exec]\nbackend = \"remote\"\ne2b_api_key_file = %q\ne2b_template = \"template-GITMOOT-IMPL\"\ncost_max_reserved_usd = 25.0\ncost_per_attempt_usd = 0.5\n", keyFile)
	if baseURL != "" {
		content += fmt.Sprintf("e2b_base_url = %q\n", baseURL)
	}
	_, writeErr := io.WriteString(file, content)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}
