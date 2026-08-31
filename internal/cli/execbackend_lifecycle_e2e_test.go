package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/permissionpolicy"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
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

func (*localProvisionBackend) Collect(context.Context, *execbackend.Instance) (execbackend.ChangeSet, error) {
	return execbackend.ChangeSet{}, nil
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

func TestExecutionChangeSetCollectorRequiresLiveOwnedInstance(t *testing.T) {
	backend, err := execbackend.NewLocalBackend(filepath.Join(t.TempDir(), "instances"), nil)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Destroy(context.Background(), instance) })

	for name, lifecycleAndInstance := range map[string]struct {
		lifecycle execbackend.ExecutionBackend
		instance  *execbackend.Instance
	}{
		"neither":       {},
		"backend only":  {lifecycle: backend},
		"instance only": {instance: instance},
	} {
		if collector := executionChangeSetCollector(lifecycleAndInstance.lifecycle, lifecycleAndInstance.instance, execbackend.Local, "owner"); collector != nil {
			t.Fatalf("%s: backend-less job received a live ChangeSet collector", name)
		}
	}
	collector := executionChangeSetCollector(backend, instance, execbackend.Local, "owner")
	if _, err := collector(context.Background(), execbackend.Backend("e2b"), "owner"); err == nil || !strings.Contains(err.Error(), "does not match live instance backend") {
		t.Fatalf("backend ownership error = %v", err)
	}
	if _, err := collector(context.Background(), execbackend.Local, "intruder"); err == nil || !strings.Contains(err.Error(), "does not own live instance") {
		t.Fatalf("job ownership error = %v", err)
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

func executionBackendConfigForTest(t *testing.T, worker jobWorker) config.RemoteExecConfig {
	t.Helper()
	cfg, err := worker.executionBackendConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
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

type byteIdentityHostFinalizer struct {
	checkout    string
	expectedSHA string
	called      *bool
}

const (
	remoteLifecycleAPIKey      = "api-key-GITMOOT-IMPL-remote-lifecycle"
	remoteLifecycleAccessToken = "envd-access-token-GITMOOT-IMPL"
	remoteLifecycleWorkspace   = "/home/user/workspace"
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
	upload         []byte
	workspace      string
	runtimeEnv     map[string]string
	runtimeStarted bool
}

// GITMOOT-IMPL: newRemoteLifecycleHarness stands in for both paid E2B planes.
// It runs the provider lifecycle and git scripts locally, but every production
// client request still crosses HTTP and Connect framing exactly as it would off-box.
func newRemoteLifecycleHarness(t *testing.T) *remoteLifecycleHarness {
	t.Helper()
	h := &remoteLifecycleHarness{t: t, workspace: filepath.Join(t.TempDir(), "sandbox-workspace")}
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
		if got := r.URL.Query().Get("path"); got != "/home/user/.gitmoot-sync.tar.gz" {
			h.t.Errorf("upload path = %q", got)
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
		h.mu.Lock()
		h.upload = append([]byte(nil), data...)
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
	if request.Process.Command == "sh" && len(request.Process.Args) == 2 && request.Process.Args[0] == "-c" && strings.Contains(request.Process.Args[1], ".gitmoot-sync.tar.gz") {
		return h.initializeWorkspace()
	}
	dir, err := h.remoteDir(request.Process.Dir)
	if err != nil {
		return nil, []byte(err.Error()), 1
	}
	cmd := exec.Command(request.Process.Command, request.Process.Args...)
	cmd.Dir = dir
	env := []string{"HOME=" + filepath.Dir(h.workspace), "PATH=" + os.Getenv("PATH")}
	keys := make([]string, 0, len(request.Process.Env))
	for key := range request.Process.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+request.Process.Env[key])
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

func (h *remoteLifecycleHarness) initializeWorkspace() ([]byte, []byte, int) {
	h.mu.Lock()
	upload := append([]byte(nil), h.upload...)
	h.mu.Unlock()
	if len(upload) == 0 {
		return nil, []byte("workspace archive was not uploaded"), 1
	}
	stage := h.t.TempDir()
	if err := extractRemoteLifecycleArchive(stage, upload); err != nil {
		return nil, []byte(err.Error()), 1
	}
	if err := os.RemoveAll(h.workspace); err != nil {
		return nil, []byte(err.Error()), 1
	}
	if err := os.Rename(filepath.Join(stage, "workspace"), h.workspace); err != nil {
		return nil, []byte(err.Error()), 1
	}
	commands := [][]string{
		{"init", "-q"},
		{"config", "user.name", "gitmoot"},
		{"config", "user.email", "gitmoot@localhost"},
		{"add", "-A"},
		{"commit", "-q", "--allow-empty", "-m", "gitmoot sync base"},
	}
	for _, args := range commands {
		if output, err := runRemoteLifecycleGit(h.workspace, args...); err != nil {
			return nil, output, 1
		}
	}
	patch, err := os.ReadFile(filepath.Join(stage, "changes.patch"))
	if err != nil {
		return nil, []byte(err.Error()), 1
	}
	if len(patch) > 0 {
		cmd := exec.Command("git", "-C", h.workspace, "apply", "--binary", "--whitespace=nowarn", "-")
		cmd.Stdin = bytes.NewReader(patch)
		if output, err := cmd.CombinedOutput(); err != nil {
			return nil, append(output, []byte(err.Error())...), 1
		}
	}
	head, err := runRemoteLifecycleGit(h.workspace, "rev-parse", "HEAD")
	if err != nil {
		return nil, head, 1
	}
	return head, nil, 0
}

func (h *remoteLifecycleHarness) remoteDir(remote string) (string, error) {
	remote = filepath.ToSlash(filepath.Clean(remote))
	if remote == remoteLifecycleWorkspace {
		return h.workspace, nil
	}
	prefix := remoteLifecycleWorkspace + "/"
	if strings.HasPrefix(remote, prefix) {
		return filepath.Join(h.workspace, filepath.FromSlash(strings.TrimPrefix(remote, prefix))), nil
	}
	return "", fmt.Errorf("unexpected remote cwd %q", remote)
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

func extractRemoteLifecycleArchive(destination string, data []byte) error {
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path %q", header.Name)
		}
		target := filepath.Join(destination, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, tarReader)
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry %q type %d", header.Name, header.Typeflag)
		}
	}
}

func runRemoteLifecycleGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return cmd.CombinedOutput()
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

func (f byteIdentityHostFinalizer) FinalizeImplementation(ctx context.Context, _ db.Job, payload workflow.JobPayload) (workflow.JobPayload, error) {
	*f.called = true
	expected, err := os.ReadFile(f.expectedSHA)
	if err != nil {
		return payload, err
	}
	content, err := os.ReadFile(filepath.Join(f.checkout, "artifact.bin"))
	if err != nil {
		return payload, err
	}
	digest := sha256.Sum256(content)
	if got, want := hex.EncodeToString(digest[:]), strings.TrimSpace(string(expected)); got != want {
		return payload, fmt.Errorf("host import hash %s is not backend hash %s", got, want)
	}
	git := gitutil.NewHostClient(f.checkout)
	if err := git.CommitAll(ctx, "test: finalize backend bytes"); err != nil {
		return payload, err
	}
	payload.HeadSHA, err = git.HeadSHA(ctx)
	return payload, err
}

// TestLocalExecutionBackendShellImplementRoundTripE2E is the P2b no-LLM
// acceptance path: a real shell implement job runs in a distinct local backend
// workspace, Mailbox imports through the production collector before result
// observation, and a host finalizer commits bytes whose SHA-256 exactly matches
// the backend-produced file.
func TestLocalExecutionBackendShellImplementRoundTripE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)
	toolCachePolicy, err := config.LoadToolCache(config.PathsForHome(home))
	if err != nil {
		t.Fatalf("LoadToolCache: %v", err)
	}
	const branch = "gitmoot-delegation-parent-local-roundtrip"
	checkout := createDaemonWorkerGitCheckout(t, branch)
	baseHEAD := strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "HEAD"))
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	expectedSHA := filepath.Join(t.TempDir(), "backend.sha256")
	backendPWD := filepath.Join(t.TempDir(), "backend.pwd")
	toolCacheEnvFile := filepath.Join(t.TempDir(), "tool-cache.env")
	script := `printf 'UV_CACHE_DIR=%s\nPIP_CACHE_DIR=%s\nnpm_config_cache=%s\nGOCACHE=%s\nGOMODCACHE=%s\n' "$UV_CACHE_DIR" "$PIP_CACHE_DIR" "$npm_config_cache" "$GOCACHE" "$GOMODCACHE" > "$TOOL_CACHE_ENV_FILE"
printf 'backend\000produced\377bytes\n' > artifact.bin
sha256sum artifact.bin | cut -d ' ' -f 1 > "$EXPECTED_SHA"
pwd > "$BACKEND_PWD"
printf '%s' '{"gitmoot_result":{"decision":"implemented","summary":"backend bytes produced","findings":[],"changes_made":["created artifact.bin"],"tests_run":[],"needs":[],"delegations":[]}}'`
	seedDaemonWorkerAgent(t, store, "local-coder", runtime.ShellRuntime, script, []string{"ask", "implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-local-backend", RepoFullName: "owner/repo", GoalID: "goal-local-backend",
		Title: "Local backend", State: string(workflow.TaskImplementing), Branch: branch, WorktreePath: checkout,
	}); err != nil {
		t.Fatal(err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: branch, Owner: "local-coder"}); err != nil || !acquired {
		t.Fatalf("AcquireLock acquired=%v err=%v", acquired, err)
	}
	mailbox := workflow.NewMailbox(store, workflow.PayloadDeliveryWorktreeResolver)
	parent, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "local-backend-parent", Agent: "local-coder", Action: "ask", Repo: "owner/repo", Instructions: "delegate the byte round-trip",
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "local-backend-shell-implement", Agent: "local-coder", Action: "implement",
		Repo: "owner/repo", Branch: branch, GoalID: "goal-local-backend", TaskID: "task-local-backend",
		TaskTitle: "Local backend", WorktreePath: checkout, HeadSHA: baseHEAD,
		ParentJobID: parent.ID, DelegationID: "local-roundtrip", DelegationDepth: 1, DelegatedBy: "local-coder", RootJobID: parent.ID,
		ShellEnv: []string{"EXPECTED_SHA=" + expectedSHA, "BACKEND_PWD=" + backendPWD, "TOOL_CACHE_ENV_FILE=" + toolCacheEnvFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	finalizerCalled := false
	worker := executionBackendJobWorker(store, os.Stderr, home)
	worker.PermissionPolicyEffectGit = func(string) permissionpolicy.EffectGit {
		return &permissionPolicyEffectGitFake{remote: map[string]struct{}{branch: {}}}
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{
			Store:                   store,
			ResolveDeliveryWorktree: workflow.PayloadDeliveryWorktreeResolver,
			ImplementationFinalizer: byteIdentityHostFinalizer{checkout: checkout, expectedSHA: expectedSHA, called: &finalizerCalled},
		}
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}
	if !finalizerCalled {
		t.Fatal("host finalizer did not run")
	}
	completed, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", completed.State)
	}
	payload, err := workflow.ParseJobPayload(completed.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ResultObservation == nil || fmt.Sprint(payload.ResultObservation.TouchedFiles) != "[artifact.bin]" {
		t.Fatalf("result observation = %+v, want imported artifact.bin", payload.ResultObservation)
	}
	pwdBytes, err := os.ReadFile(backendPWD)
	if err != nil {
		t.Fatal(err)
	}
	backendWorkspace := strings.TrimSpace(string(pwdBytes))
	if backendWorkspace == checkout {
		t.Fatalf("shell ran in host checkout %q, want distinct backend workspace", checkout)
	}
	if _, err := os.Stat(backendWorkspace); !os.IsNotExist(err) {
		t.Fatalf("backend workspace still exists after job teardown: %v", err)
	}
	want := []byte{'b', 'a', 'c', 'k', 'e', 'n', 'd', 0, 'p', 'r', 'o', 'd', 'u', 'c', 'e', 'd', 0xff, 'b', 'y', 't', 'e', 's', '\n'}
	show := exec.Command("git", "-C", checkout, "show", "HEAD:artifact.bin")
	got, err := show.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("committed artifact is not byte-identical: got %x want %x", got, want)
	}
	if status := strings.TrimSpace(runGitOutput(t, checkout, "status", "--porcelain")); status != "" {
		t.Fatalf("host finalizer left a dirty tree: %q", status)
	}
	toolCacheEnv, err := os.ReadFile(toolCacheEnvFile)
	if err != nil {
		t.Fatalf("read local tool-cache environment: %v", err)
	}
	observedToolCacheEnv := make(map[string]string, len(toolCacheEnvSubdirs))
	for _, line := range strings.Split(strings.TrimSpace(string(toolCacheEnv)), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if ok {
			observedToolCacheEnv[name] = value
		}
	}
	for _, entry := range toolCacheEnvSubdirs {
		want := filepath.Join(toolCachePolicy.Dir, entry.subdir)
		if got := observedToolCacheEnv[entry.env]; got != want {
			t.Fatalf("local runtime %s = %q, want isolated cache path %q; environment=%q", entry.env, got, want, toolCacheEnv)
		}
	}
}

// GITMOOT-IMPL: TestRemoteExecutionBackendShellImplementRoundTripE2E is the
// spend-free Slice D acceptance path. It proves the host receives exact bytes
// and, independently, that the billed sandbox is deleted after delivery.
func TestRemoteExecutionBackendShellImplementRoundTripE2E(t *testing.T) {
	ctx := context.Background()
	t.Setenv("HERDR_BIN", filepath.Join(t.TempDir(), "herdr-must-not-run"))
	harness := newRemoteLifecycleHarness(t)
	home, paths, store := heartbeatLoopE2EHome(t)
	writeRemoteLifecycleConfig(t, paths, harness.control.URL)

	const branch = "gitmoot-delegation-parent-remote-roundtrip"
	checkout := createDaemonWorkerGitCheckout(t, branch)
	baseHEAD := strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "HEAD"))
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	want := []byte{'b', 'a', 'c', 'k', 'e', 'n', 'd', 0, 'p', 'r', 'o', 'd', 'u', 'c', 'e', 'd', 0xff, 'b', 'y', 't', 'e', 's', '\n'}
	digest := sha256.Sum256(want)
	expectedSHA := filepath.Join(t.TempDir(), "backend.sha256")
	if err := os.WriteFile(expectedSHA, []byte(hex.EncodeToString(digest[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `if env | grep -Eq '^(UV_CACHE_DIR|PIP_CACHE_DIR|npm_config_cache|GOCACHE|GOMODCACHE)='; then
  printf 'host tool-cache environment reached remote runtime' >&2
  exit 41
fi
printf 'backend\000produced\377bytes\n' > artifact.bin
printf '%s' '{"gitmoot_result":{"decision":"implemented","summary":"remote backend bytes produced","findings":[],"changes_made":["created artifact.bin"],"tests_run":[],"needs":[],"delegations":[]}}'`
	seedDaemonWorkerAgent(t, store, "remote-coder", runtime.ShellRuntime, script, []string{"ask", "implement"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-remote-backend", RepoFullName: "owner/repo", GoalID: "goal-remote-backend",
		Title: "Remote backend", State: string(workflow.TaskImplementing), Branch: branch, WorktreePath: checkout,
	}); err != nil {
		t.Fatal(err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: branch, Owner: "remote-coder"}); err != nil || !acquired {
		t.Fatalf("AcquireLock acquired=%v err=%v", acquired, err)
	}
	mailbox := workflow.NewMailbox(store, workflow.PayloadDeliveryWorktreeResolver)
	parent, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "remote-backend-parent", Agent: "remote-coder", Action: "ask", Repo: "owner/repo", Instructions: "delegate the byte round-trip",
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "remote-backend-shell-implement", Agent: "remote-coder", Action: "implement",
		Repo: "owner/repo", Branch: branch, GoalID: "goal-remote-backend", TaskID: "task-remote-backend",
		TaskTitle: "Remote backend", WorktreePath: checkout, HeadSHA: baseHEAD,
		ParentJobID: parent.ID, DelegationID: "remote-roundtrip", DelegationDepth: 1, DelegatedBy: "remote-coder", RootJobID: parent.ID,
		Cockpit: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	finalizerCalled := false
	worker := executionBackendJobWorker(store, os.Stderr, home)
	worker.RemoteEnvdEndpointResolver = func(string, int) string { return harness.envd.URL }
	// GITMOOT-IMPL: executionBackendJobWorker installs a bound value-receiver;
	// rebind after injecting the offline resolver so no request can leave httptest.
	worker.ExecutionBackendFactory = worker.defaultExecutionBackend
	worker.PermissionPolicyEffectGit = func(string) permissionpolicy.EffectGit {
		return &permissionPolicyEffectGitFake{remote: map[string]struct{}{branch: {}}}
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{
			Store:                   store,
			ResolveDeliveryWorktree: workflow.PayloadDeliveryWorktreeResolver,
			ImplementationFinalizer: byteIdentityHostFinalizer{checkout: checkout, expectedSHA: expectedSHA, called: &finalizerCalled},
		}
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}
	if !finalizerCalled {
		t.Fatal("host finalizer did not run")
	}
	completed, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", completed.State)
	}
	payload, err := workflow.ParseJobPayload(completed.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ResultObservation == nil || fmt.Sprint(payload.ResultObservation.TouchedFiles) != "[artifact.bin]" {
		t.Fatalf("result observation = %+v, want imported artifact.bin", payload.ResultObservation)
	}
	show := exec.Command("git", "-C", checkout, "show", "HEAD:artifact.bin")
	got, err := show.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("committed artifact is not byte-identical: got %x want %x", got, want)
	}
	if status := strings.TrimSpace(runGitOutput(t, checkout, "status", "--porcelain")); status != "" {
		t.Fatalf("host finalizer left a dirty tree: %q", status)
	}

	creates, deleted, listCalls, runtimeEnv, runtimeStarted := harness.snapshot()
	if listCalls != 1 {
		t.Fatalf("provider startup reap list calls = %d, want 1", listCalls)
	}
	if len(creates) != 1 {
		t.Fatalf("provider create calls = %d, want 1", len(creates))
	}
	if create := creates[0]; create.TemplateID != "template-GITMOOT-IMPL" || create.Timeout <= 0 || !create.Secure || create.AutoPause || create.Metadata["job_id"] != job.ID {
		t.Fatalf("provider create request = %+v", create)
	}
	if fmt.Sprint(deleted) != "[sandbox-1]" {
		t.Fatalf("provider deletes = %v, want exactly [sandbox-1]", deleted)
	}
	attempt, err := store.GetExecBackendAttempt(ctx, db.ExecBackendAttemptKey{
		JobID: job.ID, Attempt: 1, LifecycleGeneration: job.LifecycleGeneration,
	})
	if err != nil {
		t.Fatalf("get remote execution ledger attempt: %v", err)
	}
	if attempt.State != db.ExecBackendAttemptStateDestroyed || attempt.SandboxID == nil || *attempt.SandboxID != "sandbox-1" {
		t.Fatalf("remote execution ledger attempt = %+v, want destroyed sandbox-1", attempt)
	}
	if _, err := os.Stat(harness.workspace); !os.IsNotExist(err) {
		t.Fatalf("sandbox workspace still exists after provider deletion: %v", err)
	}
	if !runtimeStarted {
		t.Fatal("remote runtime command was not observed")
	}
	for _, name := range []string{"UV_CACHE_DIR", "PIP_CACHE_DIR", "npm_config_cache", "GOCACHE", "GOMODCACHE"} {
		if value, ok := runtimeEnv[name]; ok {
			t.Fatalf("host tool-cache environment reached remote runtime: %s=%s", name, value)
		}
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	cockpitUnavailable := 0
	for _, event := range events {
		if event.Kind == "cockpit_unavailable" && strings.Contains(event.Message, "remote execution backend has no host worktree pane") {
			cockpitUnavailable++
		}
	}
	if cockpitUnavailable != 1 {
		t.Fatalf("remote cockpit_unavailable events = %d, want 1; events=%+v", cockpitUnavailable, events)
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
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "local-ask-ran")
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))
	jobID := execBackendDispatchAsk(t, home)
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}
	assertExecBackendLocalSucceeded(t, store, jobID, marker)
}

// GITMOOT-IMPL: non-implement work must fail before construction; otherwise a
// billed sandbox could run work whose result has no transport back to the host.
func TestRemoteExecutionBackendRefusesNonImplementBeforeProvision(t *testing.T) {
	ctx := context.Background()
	home, paths, store := heartbeatLoopE2EHome(t)
	writeRemoteLifecycleConfig(t, paths, "")
	checkout := createDaemonWorkerGitCheckout(t, "remote-refusal")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "remote-ask-agent", runtime.ShellRuntime, heartbeatShellResultScript, []string{"ask"}, "owner/repo")
	mailbox := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("must refuse before checkout"))
	job, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "remote-ask-must-refuse", Agent: "remote-ask-agent", Action: "ask", Repo: "owner/repo", Instructions: "must not provision",
	})
	if err != nil {
		t.Fatal(err)
	}
	factoryCalls := 0
	worker := defaultJobWorker(store, io.Discard, home)
	worker.ExecutionBackendFactory = func(_ execbackend.Backend, _ config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		factoryCalls++
		return nil, errors.New("factory must not be called")
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}
	if factoryCalls != 0 {
		t.Fatalf("execution backend factory calls = %d, want 0", factoryCalls)
	}
	completed, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed", completed.State)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if strings.Contains(event.Message, "only implement jobs transport changes back to the host") {
			found = true
		}
	}
	if !found {
		t.Fatalf("remote refusal event missing: %+v", events)
	}
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
	content := fmt.Sprintf("\n[remote_exec]\nbackend = \"remote\"\ne2b_api_key_file = %q\ne2b_template = \"template-GITMOOT-IMPL\"\n", keyFile)
	if baseURL != "" {
		content += fmt.Sprintf("e2b_base_url = %q\n", baseURL)
	}
	_, writeErr := io.WriteString(file, content)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}
