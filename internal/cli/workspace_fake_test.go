package cli

import (
	"bytes"
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

// An offline, spend-free stand-in for E2B (#2678): an HTTP control plane and
// an envd data plane that run every guest process as a real local process
// under one directory (the "guest home"). It speaks the same wire protocol
// as the provider, so the CLI under test uses its real E2B client, remote
// backend, sync script and exec supervisor. Like real envd, a process keeps
// running when the request that started it goes away; only the supervisor's
// own watchdog ends it.

const (
	workspaceTestAPIKey = "workspace-test-api-key"

	// workspaceCLIChildEnv flips the re-exec'd test binary into a real
	// `gitmoot ...` process wired to the fake (see TestMain).
	workspaceCLIChildEnv       = "GITMOOT_TEST_WORKSPACE_CLI_CHILD"
	workspaceChildGuestHomeEnv = "GITMOOT_TEST_WORKSPACE_GUEST_HOME"
	workspaceChildEnvdURLEnv   = "GITMOOT_TEST_WORKSPACE_ENVD_URL"
)

// workspaceTestHeartbeat and workspaceTestGrace shorten the exec session so
// the watchdog test ends in about a second.
const (
	workspaceTestHeartbeat = 50 * time.Millisecond
	workspaceTestGrace     = time.Second
)

func runWorkspaceCLIChild(args []string) int {
	workspaceGuestHome = os.Getenv(workspaceChildGuestHomeEnv)
	envdURL := os.Getenv(workspaceChildEnvdURLEnv)
	workspaceEnvdResolver = func(id string, _ int) string { return envdURL + "/" + id }
	workspaceHeartbeat = workspaceTestHeartbeat
	workspaceWatchdogGrace = workspaceTestGrace
	return Run(args, os.Stdout, os.Stderr)
}

type fakeWorkspaceCloud struct {
	t         *testing.T
	guestHome string
	control   *httptest.Server
	envd      *httptest.Server

	mu      sync.Mutex
	created int
	// createLost makes a create allocate a sandbox and then answer 503, the
	// lost-answer case; listLost also hides such sandboxes from the list.
	createLost bool
	listLost   bool
	lost       map[string]bool
	sandboxes  map[string]*fakeWorkspaceSandbox
	procs      map[int]*fakeWorkspaceProc
	events     []string
}

type fakeWorkspaceSandbox struct {
	deleted  bool
	timeouts []int
	metadata map[string]string
	template string
}

type fakeWorkspaceProc struct {
	name   string
	args   []string
	env    map[string]string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	done   chan struct{}
	stderr bytes.Buffer
}

func newFakeWorkspaceCloud(t *testing.T) *fakeWorkspaceCloud {
	t.Helper()
	cloud := &fakeWorkspaceCloud{
		t:         t,
		guestHome: filepath.Join(t.TempDir(), "guest"),
		sandboxes: map[string]*fakeWorkspaceSandbox{},
		procs:     map[int]*fakeWorkspaceProc{},
		lost:      map[string]bool{},
	}
	if err := os.MkdirAll(cloud.guestHome, 0o700); err != nil {
		t.Fatal(err)
	}
	cloud.control = httptest.NewServer(http.HandlerFunc(cloud.serveControl))
	cloud.envd = httptest.NewServer(http.HandlerFunc(cloud.serveEnvd))
	t.Cleanup(func() {
		cloud.mu.Lock()
		procs := make([]*fakeWorkspaceProc, 0, len(cloud.procs))
		for _, proc := range cloud.procs {
			procs = append(procs, proc)
		}
		cloud.mu.Unlock()
		for _, proc := range procs {
			_ = syscall.Kill(-proc.cmd.Process.Pid, syscall.SIGKILL)
			_ = proc.cmd.Process.Kill()
			<-proc.done
		}
		cloud.envd.Close()
		cloud.control.Close()
	})
	return cloud
}

func (c *fakeWorkspaceCloud) event(format string, args ...any) {
	c.mu.Lock()
	c.events = append(c.events, fmt.Sprintf(format, args...))
	c.mu.Unlock()
}

func (c *fakeWorkspaceCloud) snapshotEvents() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func (c *fakeWorkspaceCloud) sandbox(id string) fakeWorkspaceSandbox {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sandbox := c.sandboxes[id]; sandbox != nil {
		return fakeWorkspaceSandbox{deleted: sandbox.deleted, timeouts: append([]int(nil), sandbox.timeouts...), metadata: sandbox.metadata, template: sandbox.template}
	}
	return fakeWorkspaceSandbox{}
}

// expire retires a sandbox the way the provider does at its deadline: every
// later per-ID request answers 404.
func (c *fakeWorkspaceCloud) expire(id string) {
	c.mu.Lock()
	if sandbox := c.sandboxes[id]; sandbox != nil {
		sandbox.deleted = true
	}
	c.mu.Unlock()
}

func (c *fakeWorkspaceCloud) createdCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.created
}

// supervisors returns the exec supervisor processes started so far.
func (c *fakeWorkspaceCloud) supervisors() []*fakeWorkspaceProc {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*fakeWorkspaceProc
	for _, proc := range c.procs {
		if proc.name == "python3" && len(proc.args) > 1 && proc.args[1] == workspaceSupervisorScript {
			out = append(out, proc)
		}
	}
	return out
}

func (c *fakeWorkspaceCloud) serveControl(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("X-API-Key"); got != workspaceTestAPIKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/sandboxes/")
	suffix := ""
	if before, after, ok := strings.Cut(id, "/"); ok {
		id, suffix = before, "/"+after
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
		var request struct {
			TemplateID string            `json:"templateID"`
			Timeout    int               `json:"timeout"`
			Metadata   map[string]string `json:"metadata"`
			Env        map[string]string `json:"envVars"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad create", http.StatusBadRequest)
			return
		}
		if len(request.Env) != 0 {
			c.t.Errorf("sandbox create carried environment %v", request.Env)
		}
		c.mu.Lock()
		c.created++
		id = fmt.Sprintf("sbx-%d", c.created)
		c.sandboxes[id] = &fakeWorkspaceSandbox{timeouts: []int{request.Timeout}, metadata: request.Metadata, template: request.TemplateID}
		c.events = append(c.events, "create:"+id)
		lostAnswer := c.createLost
		if lostAnswer {
			c.lost[id] = true
		}
		c.mu.Unlock()
		if lostAnswer {
			http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": id, "templateID": request.TemplateID, "envdAccessToken": "token-" + id, "domain": nil})
	case r.Method == http.MethodGet && r.URL.Path == "/v2/sandboxes":
		c.mu.Lock()
		items := []map[string]any{}
		for sandboxID, sandbox := range c.sandboxes {
			if sandbox.deleted || (c.listLost && c.lost[sandboxID]) {
				continue
			}
			items = append(items, map[string]any{"sandboxID": sandboxID, "templateID": "tmpl", "envdVersion": "0.1.0",
				"startedAt": "2026-11-19T12:00:00Z", "endAt": "2026-11-19T13:00:00Z", "cpuCount": 2, "memoryMB": 1024,
				"diskSizeMB": 0, "metadata": sandbox.metadata, "state": "running"})
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(items)
	case r.Method == http.MethodGet && suffix == "":
		c.mu.Lock()
		sandbox := c.sandboxes[id]
		c.mu.Unlock()
		if sandbox == nil || sandbox.deleted {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxID": id, "templateID": "tmpl", "envdAccessToken": "token-" + id, "domain": nil})
	case r.Method == http.MethodPost && suffix == "/timeout":
		var request struct {
			Timeout int `json:"timeout"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		c.mu.Lock()
		sandbox := c.sandboxes[id]
		if sandbox != nil && !sandbox.deleted {
			sandbox.timeouts = append(sandbox.timeouts, request.Timeout)
			c.events = append(c.events, "timeout:"+id)
		}
		c.mu.Unlock()
		if sandbox == nil || sandbox.deleted {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && suffix == "":
		c.mu.Lock()
		sandbox := c.sandboxes[id]
		alreadyGone := sandbox == nil || sandbox.deleted
		if !alreadyGone {
			sandbox.deleted = true
			c.events = append(c.events, "delete:"+id)
		}
		c.mu.Unlock()
		if alreadyGone {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		c.t.Errorf("unexpected control request %s %s", r.Method, r.URL.RequestURI())
		http.NotFound(w, r)
	}
}

func (c *fakeWorkspaceCloud) serveEnvd(w http.ResponseWriter, r *http.Request) {
	id, route, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	c.mu.Lock()
	sandbox := c.sandboxes[id]
	c.mu.Unlock()
	if sandbox == nil || sandbox.deleted || r.Header.Get("X-Access-Token") != "token-"+id {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch route {
	case "files":
		target := filepath.Clean(r.URL.Query().Get("path"))
		if !strings.HasPrefix(target, c.guestHome+string(filepath.Separator)) {
			c.t.Errorf("upload outside the guest home: %s", target)
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err == nil {
			err = os.WriteFile(target, data, 0o600)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c.event("upload:%s", filepath.Base(target))
		_, _ = io.WriteString(w, "[]")
	case "process.Process/Start":
		c.serveStart(w, r)
	case "process.Process/SendInput":
		var request struct {
			Process struct {
				PID int `json:"pid"`
			} `json:"process"`
			Input struct {
				Stdin []byte `json:"stdin"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad input", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		proc := c.procs[request.Process.PID]
		c.mu.Unlock()
		if proc == nil || proc.stdin == nil {
			http.Error(w, "no such process", http.StatusNotFound)
			return
		}
		if _, err := proc.stdin.Write(request.Input.Stdin); err != nil {
			http.Error(w, err.Error(), http.StatusGone)
			return
		}
		_, _ = io.WriteString(w, "{}")
	case "process.Process/SendSignal":
		var request struct {
			Process struct {
				PID int `json:"pid"`
			} `json:"process"`
			Signal string `json:"signal"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad signal", http.StatusBadRequest)
			return
		}
		sig := syscall.SIGTERM
		if request.Signal == "SIGNAL_SIGKILL" {
			sig = syscall.SIGKILL
		}
		c.event("signal:%d:%s", request.Process.PID, request.Signal)
		_ = syscall.Kill(request.Process.PID, sig)
		_, _ = io.WriteString(w, "{}")
	default:
		c.t.Errorf("unexpected envd request %s %s", r.Method, r.URL.RequestURI())
		http.NotFound(w, r)
	}
}

func (c *fakeWorkspaceCloud) serveStart(w http.ResponseWriter, r *http.Request) {
	var header [5]byte
	if _, err := io.ReadFull(r.Body, header[:]); err != nil {
		http.Error(w, "bad frame", http.StatusBadRequest)
		return
	}
	payload := make([]byte, binary.BigEndian.Uint32(header[1:]))
	if _, err := io.ReadFull(r.Body, payload); err != nil {
		http.Error(w, "bad frame", http.StatusBadRequest)
		return
	}
	var request struct {
		Process struct {
			Cmd  string            `json:"cmd"`
			Args []string          `json:"args"`
			Envs map[string]string `json:"envs"`
			Cwd  string            `json:"cwd"`
		} `json:"process"`
		Stdin bool `json:"stdin"`
	}
	if err := json.Unmarshal(payload, &request); err != nil {
		http.Error(w, "bad start", http.StatusBadRequest)
		return
	}
	// envd's own defaults, never the test process's environment.
	cmd := exec.Command(request.Process.Cmd, request.Process.Args...)
	cmd.Dir = request.Process.Cwd
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + c.guestHome, "USER=user"}
	for name, value := range request.Process.Envs {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	proc := &fakeWorkspaceProc{name: request.Process.Cmd, args: request.Process.Args, env: request.Process.Envs, cmd: cmd, done: make(chan struct{})}
	var stdinPipe io.WriteCloser
	var err error
	if request.Stdin {
		if stdinPipe, err = cmd.StdinPipe(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	proc.stdin = stdinPipe
	pid := cmd.Process.Pid
	c.mu.Lock()
	c.procs[pid] = proc
	label := request.Process.Cmd
	if len(request.Process.Args) > 2 && request.Process.Args[0] == "-c" {
		label += " " + request.Process.Args[2]
	}
	c.events = append(c.events, "start:"+label)
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/connect+json")
	w.WriteHeader(http.StatusOK)
	var writeMu sync.Mutex
	send := func(flag byte, value any) {
		encoded, _ := json.Marshal(value)
		frame := make([]byte, 5+len(encoded))
		frame[0] = flag
		binary.BigEndian.PutUint32(frame[1:5], uint32(len(encoded)))
		copy(frame[5:], encoded)
		writeMu.Lock()
		defer writeMu.Unlock()
		// A gone client is ignored: like envd, the process keeps running.
		if _, err := w.Write(frame); err == nil {
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}
	send(0, map[string]any{"event": map[string]any{"start": map[string]any{"pid": pid}}})
	var pumps sync.WaitGroup
	pump := func(source io.Reader, key string, capture *bytes.Buffer) {
		defer pumps.Done()
		buffer := make([]byte, 32<<10)
		for {
			count, readErr := source.Read(buffer)
			if count > 0 {
				chunk := append([]byte(nil), buffer[:count]...)
				if capture != nil {
					c.mu.Lock()
					capture.Write(chunk)
					c.mu.Unlock()
				}
				send(0, map[string]any{"event": map[string]any{"data": map[string]any{key: chunk}}})
			}
			if readErr != nil {
				return
			}
		}
	}
	pumps.Add(2)
	go pump(stdout, "stdout", nil)
	go pump(stderr, "stderr", &proc.stderr)
	pumps.Wait()
	waitErr := cmd.Wait()
	end := map[string]any{"exitCode": 0, "exited": true, "status": "exit status 0"}
	if state := cmd.ProcessState; state != nil {
		if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			end = map[string]any{"exitCode": -1, "exited": false, "status": "signal: " + status.Signal().String()}
		} else {
			end = map[string]any{"exitCode": state.ExitCode(), "exited": true, "status": state.String()}
		}
	} else if waitErr != nil {
		end = map[string]any{"exitCode": 0, "exited": false, "status": waitErr.Error()}
	}
	c.event("end:%d", pid)
	close(proc.done)
	send(0, map[string]any{"event": map[string]any{"end": end}})
	send(2, map[string]any{})
}

// workspaceTestEnv is one gitmoot home wired to the fake cloud, with a
// registered repository whose origin is a local bare repository.
type workspaceTestEnv struct {
	t        *testing.T
	cloud    *fakeWorkspaceCloud
	home     string
	paths    config.Paths
	origin   string
	checkout string
	baseSHA  string
	pulls    map[int64]github.PullRequest
	pullsMu  sync.Mutex
	now      time.Time
	nowMu    sync.Mutex
}

type workspaceTestConfig struct {
	rate, maxTotal, maxWorkspace float64
	idle                         string
	// local selects [workspaces] provider = "local".
	local bool
}

func newWorkspaceTestEnv(t *testing.T, settings workspaceTestConfig) *workspaceTestEnv {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required for the workspace exec supervisor")
	}
	if settings.rate == 0 {
		settings.rate = 1
	}
	if settings.maxTotal == 0 {
		settings.maxTotal = 50
	}
	if settings.maxWorkspace == 0 {
		settings.maxWorkspace = 10
	}
	if settings.idle == "" {
		settings.idle = "4h"
	}
	cloud := newFakeWorkspaceCloud(t)
	env := &workspaceTestEnv{t: t, cloud: cloud, home: t.TempDir(), pulls: map[int64]github.PullRequest{}, now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	env.paths = config.PathsForHome(env.home)
	if err := config.Initialize(env.paths); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "e2b-key")
	if err := os.WriteFile(keyFile, []byte(workspaceTestAPIKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendWorkspaceConfig(t, env.paths, fmt.Sprintf(`
[remote_exec]
e2b_api_key_file = %q
e2b_template = "tmpl"
e2b_base_url = %q
cost_per_hour_usd = %v

[workspaces]
enabled = true
max_total_usd = %v
max_workspace_usd = %v
idle_timeout = %q
`, keyFile, cloud.control.URL, settings.rate, settings.maxTotal, settings.maxWorkspace, settings.idle))
	if settings.local {
		appendWorkspaceConfig(t, env.paths, "provider = \"local\"\n")
	}

	root := t.TempDir()
	env.origin = filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	env.checkout = filepath.Join(root, "checkout")
	runWorkspaceTestGit(t, root, "init", "--quiet", "--bare", "--initial-branch=main", env.origin)
	runWorkspaceTestGit(t, root, "init", "--quiet", "--initial-branch=main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("widget\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runWorkspaceTestGit(t, seed, "add", "README.md")
	runWorkspaceTestGit(t, seed, "commit", "--quiet", "-m", "initial")
	runWorkspaceTestGit(t, seed, "push", "--quiet", env.origin, "main")
	env.baseSHA = runWorkspaceTestGit(t, seed, "rev-parse", "HEAD")
	runWorkspaceTestGit(t, root, "clone", "--quiet", env.origin, env.checkout)
	if err := withStore(env.home, func(store *db.Store) error {
		return store.UpsertRepo(context.Background(), db.Repo{Owner: "acme", Name: "widget", DefaultBranch: "main", RemoteURL: env.origin, CheckoutPath: env.checkout})
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GITMOOT_ORG_ROLE", "intern")
	t.Setenv("HERDR_PANE_ID", "")
	previous := struct {
		home      string
		resolver  func(string, int) string
		gh        func(string) github.Client
		now       func() time.Time
		heartbeat time.Duration
		grace     time.Duration
	}{workspaceGuestHome, workspaceEnvdResolver, workspaceGitHubClient, workspaceNow, workspaceHeartbeat, workspaceWatchdogGrace}
	workspaceGuestHome = cloud.guestHome
	workspaceEnvdResolver = func(id string, _ int) string { return cloud.envd.URL + "/" + id }
	workspaceGitHubClient = func(string) github.Client { return workspaceTestGitHub{env: env} }
	workspaceNow = env.clock
	workspaceHeartbeat = workspaceTestHeartbeat
	workspaceWatchdogGrace = workspaceTestGrace
	workspaceSweepState = &workspaceSweepGate{}
	t.Cleanup(func() {
		workspaceGuestHome, workspaceEnvdResolver, workspaceGitHubClient, workspaceNow = previous.home, previous.resolver, previous.gh, previous.now
		workspaceHeartbeat, workspaceWatchdogGrace = previous.heartbeat, previous.grace
	})
	return env
}

func appendWorkspaceConfig(t *testing.T, paths config.Paths, content string) {
	t.Helper()
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := io.WriteString(file, content)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}

func runWorkspaceTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (e *workspaceTestEnv) clock() time.Time {
	e.nowMu.Lock()
	defer e.nowMu.Unlock()
	return e.now
}

func (e *workspaceTestEnv) advance(d time.Duration) {
	e.nowMu.Lock()
	e.now = e.now.Add(d)
	e.nowMu.Unlock()
}

func (e *workspaceTestEnv) setPull(pull github.PullRequest) {
	e.pullsMu.Lock()
	e.pulls[pull.Number] = pull
	e.pullsMu.Unlock()
}

// addPullRequest pushes a branch with one commit and exposes it as an open
// pull request (origin's refs/pull/N/head included).
func (e *workspaceTestEnv) addPullRequest(number int64, branch string) string {
	e.t.Helper()
	work := filepath.Join(e.t.TempDir(), "pr")
	runWorkspaceTestGit(e.t, filepath.Dir(work), "clone", "--quiet", e.origin, work)
	runWorkspaceTestGit(e.t, work, "checkout", "--quiet", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	runWorkspaceTestGit(e.t, work, "add", "feature.txt")
	runWorkspaceTestGit(e.t, work, "commit", "--quiet", "-m", "feature")
	head := runWorkspaceTestGit(e.t, work, "rev-parse", "HEAD")
	runWorkspaceTestGit(e.t, work, "push", "--quiet", "origin", branch, fmt.Sprintf("HEAD:refs/pull/%d/head", number))
	e.setPull(github.PullRequest{Number: number, State: "open", HeadRef: branch, HeadSHA: head, BaseRef: "main", HeadRepoFullName: "acme/widget"})
	return head
}

type workspaceTestGitHub struct {
	github.Client
	env *workspaceTestEnv
}

func (g workspaceTestGitHub) GetPullRequest(_ context.Context, _ github.Repository, number int64) (github.PullRequest, error) {
	g.env.pullsMu.Lock()
	defer g.env.pullsMu.Unlock()
	pull, ok := g.env.pulls[number]
	if !ok {
		return github.PullRequest{}, fmt.Errorf("pull request #%d not found", number)
	}
	return pull, nil
}

type workspaceCLIResult struct {
	code   int
	stdout string
	stderr string
}

func (e *workspaceTestEnv) run(args ...string) workspaceCLIResult {
	e.t.Helper()
	return e.runWithStdin(nil, args...)
}

func (e *workspaceTestEnv) runWithStdin(stdin io.Reader, args ...string) workspaceCLIResult {
	e.t.Helper()
	var stdout, stderr bytes.Buffer
	full := append([]string(nil), args...)
	full = append(full[:2:2], append([]string{"--home", e.home}, full[2:]...)...)
	var code int
	switch {
	case args[0] == "workspace" && args[1] == "exec":
		code = runWorkspaceExec(full[2:], stdin, &stdout, &stderr)
	case args[0] == "workspace" && args[1] == "write":
		code = runWorkspaceWrite(full[2:], stdin, &stdout, &stderr)
	case args[0] == "workspace" && args[1] == "push":
		code = runWorkspacePush(full[2:], stdin, &stdout, &stderr)
	default:
		code = Run(full, &stdout, &stderr)
	}
	return workspaceCLIResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func (e *workspaceTestEnv) open(args ...string) workspaceJSON {
	e.t.Helper()
	result := e.run(append([]string{"workspace", "open", "--repo", "acme/widget", "--json"}, args...)...)
	if result.code != 0 {
		e.t.Fatalf("workspace open %v exited %d\nstdout: %s\nstderr: %s", args, result.code, result.stdout, result.stderr)
	}
	var opened workspaceJSON
	if err := json.Unmarshal([]byte(result.stdout), &opened); err != nil {
		e.t.Fatalf("workspace open printed %q: %v", result.stdout, err)
	}
	return opened
}

func (e *workspaceTestEnv) record(id string) db.Workspace {
	e.t.Helper()
	var workspace db.Workspace
	if err := withStore(e.home, func(store *db.Store) error {
		var err error
		workspace, err = store.GetWorkspace(context.Background(), id)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return workspace
}

func (e *workspaceTestEnv) sweep() string {
	e.t.Helper()
	var out bytes.Buffer
	if err := withStoreAndPaths(e.home, func(paths config.Paths, store *db.Store) error {
		return sweepWorkspaces(context.Background(), store, paths, &out)
	}); err != nil {
		e.t.Fatalf("sweep: %v\n%s", err, out.String())
	}
	return out.String()
}

// errorCode reads the pinned error shape from a stream.
func workspaceErrorCode(t *testing.T, stream string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(stream), "\n") {
		var shape struct {
			Error *workspaceError `json:"error"`
		}
		if json.Unmarshal([]byte(line), &shape) == nil && shape.Error != nil {
			return shape.Error.Code
		}
	}
	t.Fatalf("no error JSON in %q", stream)
	return ""
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func processGone(pid int) bool {
	err := syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

func readPIDFile(t *testing.T, file string) int {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}
