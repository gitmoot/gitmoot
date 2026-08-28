package remote

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
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
)

const testAccessToken = "remote-envd-access-token-GITMOOT-IMPL"

func TestRemoteProvisionRequiresTTLAndRecordsOwnership(t *testing.T) {
	harness := newProviderHarness(t)
	backend := harness.backend(t)
	backend.bootID = "boot-test"
	backend.ownerPID = 1234
	backend.ownerStart = "5678"

	if instance, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: "job-zero"}); err == nil || instance != nil || !strings.Contains(err.Error(), "remote execution backend TTL must be positive") {
		t.Fatalf("Provision without TTL = %+v, %v; want refusal", instance, err)
	}
	if got := harness.createCount(); got != 0 {
		t.Fatalf("create calls after zero TTL = %d, want 0", got)
	}

	instance, err := backend.Provision(context.Background(), execbackend.JobScope{
		JobID:               "job-1",
		LifecycleGeneration: 9,
		TTL:                 90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != "sandbox-1" || instance.Workspace != workspacePath || instance.JobID != "job-1" || instance.LifecycleGeneration != 9 {
		t.Fatalf("instance = %+v", instance)
	}
	request := harness.lastCreate()
	if request.Timeout != 90 || request.Metadata[metadataJobID] != "job-1" || request.Metadata[metadataLifecycleGeneration] != "9" || request.Metadata[metadataBootID] != "boot-test" || request.Metadata[metadataOwnerPID] != "1234" || request.Metadata[metadataOwnerStartTime] != "5678" {
		t.Fatalf("create request = %+v", request)
	}
}

func TestRemoteExecRefusesRelativeDirAndDoesNotForwardOnStart(t *testing.T) {
	harness := newProviderHarness(t)
	backend := harness.backend(t)
	instance := provisionAndSync(t, backend, testSourceRepo(t))

	if stream, err := backend.Exec(context.Background(), instance, execbackend.Command{Name: "true", Dir: "relative"}); err == nil || stream != nil {
		t.Fatalf("relative Exec = %+v, %v; want refusal", stream, err)
	}
	started := false
	stream, err := backend.Exec(context.Background(), instance, execbackend.Command{
		Name:    "sh",
		Args:    []string{"-c", "printf remote-ok"},
		Dir:     workspacePath,
		OnStart: func(int) { started = true },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stream.Wait()
	if err != nil || result.Stdout != "remote-ok" {
		t.Fatalf("Wait = %+v, %v", result, err)
	}
	if started {
		t.Fatal("remote Exec forwarded sandbox PID to the host OnStart callback")
	}
}

func TestRemoteDestroyIsIdempotent(t *testing.T) {
	harness := newProviderHarness(t)
	backend := harness.backend(t)
	instance, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: "job-1", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Destroy(context.Background(), instance); err != nil {
		t.Fatal(err)
	}
	if err := backend.Destroy(context.Background(), instance); err != nil {
		t.Fatalf("second Destroy: %v", err)
	}
	if got := harness.deletedIDs(); !reflect.DeepEqual(got, []string{"sandbox-1"}) {
		t.Fatalf("deleted sandboxes = %v", got)
	}
}

func TestRemoteReapLeavesForeignBootSandboxAlone(t *testing.T) {
	harness := newProviderHarness(t)
	harness.setInventory([]e2b.Sandbox{
		{ID: "foreign", TemplateID: "template-test", State: "running", Metadata: map[string]string{
			metadataBootID: "boot-foreign", metadataOwnerPID: "999999", metadataOwnerStartTime: "1",
		}},
		{ID: "stale-local", TemplateID: "template-test", State: "paused", Metadata: map[string]string{
			metadataBootID: "boot-local", metadataOwnerPID: "999998", metadataOwnerStartTime: "2",
		}},
	})
	backend := harness.backend(t)
	backend.bootID = "boot-local"
	backend.ownerAlive = func(int, string, string) bool { return false }

	reaped, err := backend.Reap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reaped, []string{"stale-local"}) {
		t.Fatalf("reaped = %v", reaped)
	}
	if got := harness.deletedIDs(); !reflect.DeepEqual(got, []string{"stale-local"}) {
		t.Fatalf("deleted sandboxes = %v; foreign boot must remain", got)
	}
}

func TestRemoteCollectMatchesLocalCollect(t *testing.T) {
	ctx := context.Background()
	source := testSourceRepo(t)
	harness := newProviderHarness(t)
	remoteBackend := harness.backend(t)
	remoteInstance := provisionAndSync(t, remoteBackend, source)

	localBackend, err := execbackend.NewLocalBackend(filepath.Join(t.TempDir(), "local-instances"), nil)
	if err != nil {
		t.Fatal(err)
	}
	localInstance, err := localBackend.Provision(ctx, execbackend.JobScope{JobID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := localBackend.SyncIn(ctx, localInstance, execbackend.Materials{SourceWorktree: source}); err != nil {
		t.Fatal(err)
	}

	applyCollectionCorpus(t, localInstance.Workspace)
	applyCollectionCorpus(t, harness.workspace)
	localChanges, err := localBackend.Collect(ctx, localInstance)
	if err != nil {
		t.Fatalf("local Collect: %v", err)
	}
	remoteChanges, err := remoteBackend.Collect(ctx, remoteInstance)
	if err != nil {
		t.Fatalf("remote Collect: %v", err)
	}
	if !reflect.DeepEqual(remoteChanges, localChanges) {
		localJSON, _ := json.MarshalIndent(localChanges, "", "  ")
		remoteJSON, _ := json.MarshalIndent(remoteChanges, "", "  ")
		t.Fatalf("ChangeSet mismatch\nlocal:\n%s\nremote:\n%s", localJSON, remoteJSON)
	}
}

func provisionAndSync(t *testing.T, backend *Backend, source string) *execbackend.Instance {
	t.Helper()
	instance, err := backend.Provision(context.Background(), execbackend.JobScope{JobID: "job-1", LifecycleGeneration: 2, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.SyncIn(context.Background(), instance, execbackend.Materials{SourceWorktree: source}); err != nil {
		t.Fatal(err)
	}
	return instance
}

func testSourceRepo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "init", "-q")
	testGit(t, root, "config", "user.email", "tests@gitmoot.local")
	testGit(t, root, "config", "user.name", "Gitmoot Tests")
	testWriteFile(t, root, "tracked.txt", []byte("original\n"), 0o644)
	testWriteFile(t, root, "deleted.txt", []byte("delete me\n"), 0o644)
	testWriteFile(t, root, "script.sh", []byte("#!/bin/sh\n"), 0o644)
	testWriteFile(t, root, "input.txt", []byte("base input\n"), 0o644)
	if err := os.Symlink("deleted.txt", filepath.Join(root, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "add", "-A")
	testGit(t, root, "commit", "-q", "-m", "base")
	testWriteFile(t, root, "input.txt", []byte("host input change\n"), 0o644)
	testWriteFile(t, root, "input-untracked.txt", []byte("host untracked input\n"), 0o644)
	return root
}

func applyCollectionCorpus(t *testing.T, root string) {
	t.Helper()
	testWriteFile(t, root, "tracked.txt", []byte("changed\n"), 0o644)
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tracked.txt", filepath.Join(root, "tracked-link")); err != nil {
		t.Fatal(err)
	}
	testWriteFile(t, root, "nested/untracked.bin", []byte{'n', 'e', 'w', 0, 0xff, '\n'}, 0o644)
	if err := os.Symlink("untracked.bin", filepath.Join(root, "nested", "untracked-link")); err != nil {
		t.Fatal(err)
	}
	invalidName := "invalid-\xff-name.txt"
	testWriteFile(t, root, invalidName, []byte("byte-exact\n"), 0o644)
}

func testWriteFile(t *testing.T, root, name string, data []byte, mode os.FileMode) {
	t.Helper()
	filePath := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, data, mode); err != nil {
		t.Fatal(err)
	}
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

type createRequest struct {
	TemplateID string            `json:"templateID"`
	Timeout    int               `json:"timeout"`
	Metadata   map[string]string `json:"metadata"`
}

type providerHarness struct {
	t *testing.T

	mu        sync.Mutex
	creates   []createRequest
	deleted   []string
	inventory []e2b.Sandbox
	upload    []byte

	workspace string
	control   *httptest.Server
	envd      *httptest.Server
}

func newProviderHarness(t *testing.T) *providerHarness {
	t.Helper()
	harness := &providerHarness{t: t, workspace: filepath.Join(t.TempDir(), "sandbox-workspace")}
	harness.control = httptest.NewServer(http.HandlerFunc(harness.serveControl))
	harness.envd = httptest.NewServer(http.HandlerFunc(harness.serveEnvd))
	t.Cleanup(harness.control.Close)
	t.Cleanup(harness.envd.Close)
	return harness
}

func (h *providerHarness) backend(t *testing.T) *Backend {
	t.Helper()
	client, err := e2b.NewClient("api-key-GITMOOT-IMPL", e2b.Options{BaseURL: h.control.URL, HTTPClient: h.control.Client()})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := NewBackend(client, Options{
		TemplateID: "template-test",
		Envd: e2b.EnvdOptions{
			HTTPClient:       h.envd.Client(),
			EndpointResolver: func(string, int) string { return h.envd.URL },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func (h *providerHarness) serveControl(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
		var request createRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			h.t.Errorf("decode create request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.creates = append(h.creates, request)
		id := fmt.Sprintf("sandbox-%d", len(h.creates))
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sandboxID": id, "templateID": request.TemplateID, "envdAccessToken": testAccessToken, "domain": nil,
		})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sandboxes/"):
		id := strings.TrimPrefix(r.URL.Path, "/sandboxes/")
		h.mu.Lock()
		h.deleted = append(h.deleted, id)
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/v2/sandboxes":
		h.mu.Lock()
		inventory := append([]e2b.Sandbox(nil), h.inventory...)
		h.mu.Unlock()
		items := make([]map[string]any, 0, len(inventory))
		for _, sandbox := range inventory {
			items = append(items, listedSandboxJSON(sandbox))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
	default:
		h.t.Errorf("unexpected control request: %s %s", r.Method, r.URL.String())
		http.NotFound(w, r)
	}
}

func (h *providerHarness) serveEnvd(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("X-Access-Token"); got != testAccessToken {
		h.t.Errorf("X-Access-Token = %q", got)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/files":
		if got := r.URL.Query().Get("username"); got != "user" {
			h.t.Errorf("upload username = %q", got)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			h.t.Errorf("read upload: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.upload = data
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	case r.Method == http.MethodPost && r.URL.Path == "/process.Process/Start":
		h.serveStart(w, r)
	default:
		h.t.Errorf("unexpected envd request: %s %s", r.Method, r.URL.String())
		http.NotFound(w, r)
	}
}

func (h *providerHarness) serveStart(w http.ResponseWriter, r *http.Request) {
	_, body, err := readTestConnectFrame(r.Body)
	if err != nil {
		h.t.Errorf("read Start frame: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var request struct {
		Process struct {
			Command string            `json:"cmd"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"envs"`
			Dir     string            `json:"cwd"`
		} `json:"process"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		h.t.Errorf("decode Start request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stdout, stderr, exitCode, providerError := h.runProcess(r.Context(), request.Process.Command, request.Process.Args, request.Process.Dir, request.Process.Env)
	w.Header().Set("Content-Type", "application/connect+json")
	writeTestConnectJSON(h.t, w, 0, map[string]any{"event": map[string]any{"start": map[string]any{"pid": 42}}})
	if len(stdout) > 0 {
		writeTestConnectJSON(h.t, w, 0, map[string]any{"event": map[string]any{"data": map[string]any{"stdout": stdout}}})
	}
	if len(stderr) > 0 {
		writeTestConnectJSON(h.t, w, 0, map[string]any{"event": map[string]any{"data": map[string]any{"stderr": stderr}}})
	}
	status := fmt.Sprintf("exit status %d", exitCode)
	writeTestConnectJSON(h.t, w, 0, map[string]any{"event": map[string]any{"end": map[string]any{
		"exitCode": exitCode, "exited": true, "status": status, "error": providerError,
	}}})
	writeTestConnectJSON(h.t, w, 2, map[string]any{})
}

func (h *providerHarness) runProcess(ctx context.Context, command string, args []string, remoteDir string, env map[string]string) ([]byte, []byte, int, string) {
	if command == "sh" && reflect.DeepEqual(args, []string{"-c", syncWorkspaceScript}) {
		h.mu.Lock()
		archive := append([]byte(nil), h.upload...)
		h.mu.Unlock()
		staging := filepath.Join(filepath.Dir(h.workspace), "sync-staging")
		if err := os.RemoveAll(h.workspace); err != nil {
			return nil, nil, 1, err.Error()
		}
		if err := os.RemoveAll(staging); err != nil {
			return nil, nil, 1, err.Error()
		}
		if err := extractTestArchive(archive, staging); err != nil {
			return nil, nil, 1, err.Error()
		}
		if err := os.Rename(filepath.Join(staging, "workspace"), h.workspace); err != nil {
			return nil, nil, 1, err.Error()
		}
		for _, gitArgs := range [][]string{{"init", "-q"}, {"config", "user.name", "gitmoot"}, {"config", "user.email", "gitmoot@localhost"}, {"add", "-A"}, {"commit", "-q", "--allow-empty", "-m", "gitmoot sync base"}} {
			cmd := exec.CommandContext(ctx, "git", append([]string{"-C", h.workspace}, gitArgs...)...)
			if output, err := cmd.CombinedOutput(); err != nil {
				return nil, output, 1, err.Error()
			}
		}
		inputPatch, err := os.ReadFile(filepath.Join(staging, "changes.patch"))
		if err != nil {
			return nil, nil, 1, err.Error()
		}
		if len(inputPatch) > 0 {
			cmd := exec.CommandContext(ctx, "git", "-C", h.workspace, "apply", "--binary", "--whitespace=nowarn", "-")
			cmd.Stdin = bytes.NewReader(inputPatch)
			if output, err := cmd.CombinedOutput(); err != nil {
				return nil, output, 1, err.Error()
			}
		}
		if err := os.RemoveAll(staging); err != nil {
			return nil, nil, 1, err.Error()
		}
		cmd := exec.CommandContext(ctx, "git", "-C", h.workspace, "rev-parse", "HEAD")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, output, 1, err.Error()
		}
		return output, nil, 0, ""
	}

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = h.localDir(remoteDir)
	cmd.Env = os.Environ()
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		cmd.Env = append(cmd.Env, key+"="+env[key])
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), stderr.Bytes(), 0, ""
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.Bytes(), stderr.Bytes(), exitErr.ExitCode(), strings.TrimSpace(stderr.String())
	}
	return stdout.Bytes(), stderr.Bytes(), 1, err.Error()
}

func (h *providerHarness) localDir(remoteDir string) string {
	if remoteDir == workspacePath {
		return h.workspace
	}
	if strings.HasPrefix(remoteDir, workspacePath+"/") {
		return filepath.Join(h.workspace, filepath.FromSlash(strings.TrimPrefix(remoteDir, workspacePath+"/")))
	}
	return filepath.Dir(h.workspace)
}

func (h *providerHarness) setInventory(inventory []e2b.Sandbox) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inventory = append([]e2b.Sandbox(nil), inventory...)
}

func (h *providerHarness) createCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.creates)
}

func (h *providerHarness) lastCreate() createRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.creates[len(h.creates)-1]
}

func (h *providerHarness) deletedIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.deleted...)
}

func listedSandboxJSON(sandbox e2b.Sandbox) map[string]any {
	return map[string]any{
		"sandboxID": sandbox.ID, "templateID": sandbox.TemplateID,
		"startedAt": time.Unix(1, 0).UTC().Format(time.RFC3339),
		"endAt":     time.Unix(3601, 0).UTC().Format(time.RFC3339),
		"cpuCount":  1, "memoryMB": 128, "diskSizeMB": 512,
		"envdVersion": "test", "state": sandbox.State, "metadata": sandbox.Metadata,
	}
}

func readTestConnectFrame(reader io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	body := make([]byte, int(length))
	if _, err := io.ReadFull(reader, body); err != nil {
		return 0, nil, err
	}
	return header[0], body, nil
}

func writeTestConnectJSON(t *testing.T, writer io.Writer, flag byte, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var header [5]byte
	header[0] = flag
	binary.BigEndian.PutUint32(header[1:], uint32(len(body)))
	if _, err := writer.Write(append(header[:], body...)); err != nil {
		t.Fatal(err)
	}
}

func extractTestArchive(data []byte, destination string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
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
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path %q", header.Name)
		}
		filePath := filepath.Join(destination, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(filePath, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, filePath); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(filePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, tarReader)
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		}
	}
}
