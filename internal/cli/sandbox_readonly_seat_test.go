package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/github/githubtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/sandbox"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/transcript"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type sandboxAdapterCaptureRunner struct {
	stdout  string
	err     error
	dir     string
	command string
	args    []string
	env     []string
}

func (r *sandboxAdapterCaptureRunner) Run(_ context.Context, dir, command string, args ...string) (subprocess.Result, error) {
	r.dir = dir
	r.command = command
	r.args = append([]string(nil), args...)
	return subprocess.Result{Command: command, Args: args, Stdout: r.stdout}, r.err
}

func (r *sandboxAdapterCaptureRunner) RunEnv(_ context.Context, dir string, env []string, command string, args ...string) (subprocess.Result, error) {
	r.env = append([]string(nil), env...)
	return r.Run(context.Background(), dir, command, args...)
}

func (r *sandboxAdapterCaptureRunner) LookPath(file string) (string, error) { return file, nil }

type repairStateRunner struct {
	calls               int
	stateDir            string
	repairStateObserved bool
	toolCacheObserved   bool
}

func (r *repairStateRunner) Run(context.Context, string, string, ...string) (subprocess.Result, error) {
	return subprocess.Result{}, errors.New("repair-state runner requires explicit environment")
}

func (r *repairStateRunner) RunEnv(_ context.Context, _ string, env []string, command string, args ...string) (subprocess.Result, error) {
	r.calls++
	stateDir := envValue(env, "PI_CODING_AGENT_DIR")
	if stateDir == "" {
		return subprocess.Result{}, errors.New("missing isolated PI_CODING_AGENT_DIR")
	}
	// The job-scoped broker route is written into the isolated state before
	// every delivery; a repair turn that lost the state would lose it too.
	if _, err := os.ReadFile(filepath.Join(stateDir, "config.yml")); err != nil {
		return subprocess.Result{}, err
	}
	if cache := envValue(env, "GOCACHE"); strings.Contains(cache, filepath.Join("cache", "tools", "read-only")) {
		r.toolCacheObserved = true
	}
	marker := filepath.Join(stateDir, "repair.marker")
	switch r.calls {
	case 1:
		r.stateDir = stateDir
		if err := os.WriteFile(marker, []byte("first-delivery"), 0o600); err != nil {
			return subprocess.Result{}, err
		}
		return subprocess.Result{Command: command, Args: args, Stdout: readOnlySeatOmpStdout("review complete, no json")}, nil
	case 2:
		if stateDir != r.stateDir {
			return subprocess.Result{}, errors.New("repair delivery changed isolated state directory")
		}
		data, err := os.ReadFile(marker)
		if err != nil || string(data) != "first-delivery" {
			return subprocess.Result{}, errors.New("repair delivery lost first-delivery state")
		}
		r.repairStateObserved = true
		return subprocess.Result{Command: command, Args: args, Stdout: readOnlySeatOmpStdout(`{"gitmoot_result":{"decision":"approved","summary":"clean after repair","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`)}, nil
	default:
		return subprocess.Result{}, errors.New("unexpected extra repair delivery")
	}
}

func (r *repairStateRunner) LookPath(file string) (string, error) { return file, nil }

type renameRuntimeStateRunner struct {
	savedState string
}

func (r *renameRuntimeStateRunner) Run(context.Context, string, string, ...string) (subprocess.Result, error) {
	return subprocess.Result{}, errors.New("rename-state runner requires explicit environment")
}

func (r *renameRuntimeStateRunner) RunEnv(_ context.Context, _ string, env []string, command string, args ...string) (subprocess.Result, error) {
	stateDir := envValue(env, "PI_CODING_AGENT_DIR")
	if stateDir == "" {
		return subprocess.Result{}, errors.New("missing isolated PI_CODING_AGENT_DIR")
	}
	// <cache root>/runtime-state/.omp/agent: move the whole runtime-state tree.
	stateRoot := filepath.Dir(filepath.Dir(stateDir))
	cacheRoot := filepath.Dir(stateRoot)
	r.savedState = filepath.Join(cacheRoot, "saved-runtime-state")
	if err := os.Rename(stateRoot, r.savedState); err != nil {
		return subprocess.Result{}, fmt.Errorf("move runtime state inside writable cache: %w", err)
	}
	return subprocess.Result{
		Command: command,
		Args:    args,
		Stdout:  readOnlySeatOmpStdout(`{"gitmoot_result":{"decision":"approved","summary":"state moved","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`),
	}, nil
}

func (r *renameRuntimeStateRunner) LookPath(file string) (string, error) { return file, nil }

// ompStreamStdout renders text as the final assistant message of an omp
// --mode=json stream, the wire format OmpAdapter parses.
func readOnlySeatOmpStdout(text string) string {
	encoded, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return `{"type":"session","version":3,"id":"01920000-0000-7000-8000-000000000152","timestamp":"2026-08-15T00:00:00.000Z","cwd":"/repo"}` + "\n" +
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":` + string(encoded) + `}],"usage":{"input":1,"output":1,"totalTokens":2},"stopReason":"stop"}}` + "\n" +
		`{"type":"agent_end","messages":[],"isTerminal":true}` + "\n"
}

// readOnlySeatOmpBrokerEnv configures the credential broker an omp read-only
// seat requires before any staging, so seat setup reaches what a test checks.
func readOnlySeatOmpBrokerEnv(t *testing.T) {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "broker-token")
	if err := os.WriteFile(tokenFile, []byte("seat-broker-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_AUTH_BROKER_URL", "http://127.0.0.1:8765")
	t.Setenv("OMP_AUTH_BROKER_TOKEN", "")
	t.Setenv("OMP_AUTH_BROKER_TOKEN_FILE", tokenFile)
}

// readOnlySeatOmpModel is a provider-qualified model, which an omp seat needs
// to scope its broker.
const readOnlySeatOmpModel = "anthropic/claude-sonnet-4-5"

func TestWrapReadOnlySandboxAdapterUsesExplicitReadsAndIsolatedState(t *testing.T) {
	configHome := t.TempDir()
	checkout := filepath.Join(t.TempDir(), "review-worktree")
	hostProfile := filepath.Join(t.TempDir(), "omp-profile")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(hostProfile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostProfile, "settings.json"), []byte(`{"env":{"UNRELATED_SECRET":"must-not-copy"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	readOnlySeatOmpBrokerEnv(t)
	t.Setenv("GH_TOKEN", "must-not-reach-review")
	t.Setenv("OPENAI_API_KEY", "must-not-reach-review")
	agent := runtime.Agent{
		Runtime:          runtime.OmpRuntime,
		AutonomyPolicy:   runtime.AutonomyPolicyReadOnly,
		ReadOnlySeat:     true,
		RuntimeConfigDir: hostProfile,
	}
	wrapped, _, err := wrapReadOnlySandboxAdapter(configHome, agent, checkout, "", runtime.OmpAdapter{Runner: subprocess.GroupRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	stateAdapter, ok := wrapped.(readOnlyRuntimeAdapter)
	if !ok {
		t.Fatalf("wrapped adapter = %T, want readOnlyRuntimeAdapter", wrapped)
	}
	t.Cleanup(func() { _ = stateAdapter.cleanup() })
	stagedState := filepath.Join(stateAdapter.cleanupRoot, "runtime-state", ".omp", "agent")
	if _, err := os.Stat(filepath.Join(stagedState, "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated profile copied host settings: %v", err)
	}
	adapter, ok := stateAdapter.Adapter.(runtime.OmpAdapter)
	if !ok {
		t.Fatalf("inner adapter = %T, want runtime.OmpAdapter", stateAdapter.Adapter)
	}
	runner, ok := adapter.Runner.(subprocess.WrappingRunner)
	if !ok {
		t.Fatalf("wrapped runner = %T, want subprocess.WrappingRunner", adapter.Runner)
	}
	if !runner.ReadOnlyWorkdir {
		t.Fatal("read-only runner ReadOnlyWorkdir = false")
	}
	if !containsPath(runner.ReadablePaths, checkout) || !containsPath(runner.ReadablePaths, filepath.Join(checkout, ".git")) {
		t.Fatalf("explicit reads %v do not include checkout and git metadata", runner.ReadablePaths)
	}
	if containsPath(runner.ReadablePaths, "/") || containsPath(runner.ReadablePaths, hostProfile) {
		t.Fatalf("explicit reads %v expose the host root or source profile", runner.ReadablePaths)
	}
	if len(runner.WritablePaths) == 0 || runner.WritablePaths[0] == hostProfile || !strings.Contains(runner.WritablePaths[0], string(filepath.Separator)+"read-only"+string(filepath.Separator)) {
		t.Fatalf("writes = %v, want the per-worktree cache first and no source profile", runner.WritablePaths)
	}
	assertSeatTempDirGrant(t, runner.WritablePaths, stateAdapter.cleanupTemp, runner.Env)
	configEnv := envValue(runner.Env, "PI_CODING_AGENT_DIR")
	if configEnv == "" || configEnv == hostProfile || !strings.HasPrefix(configEnv, runner.WritablePaths[0]+string(filepath.Separator)) {
		t.Fatalf("PI_CODING_AGENT_DIR = %q, want isolated state under %q", configEnv, runner.WritablePaths[0])
	}
	if !containsEnvPrefix(runner.Env, "GOCACHE=") || !containsEnvPrefix(runner.Env, "TMPDIR=") {
		t.Fatalf("read-only env %v lacks Go cache or temp grants", runner.Env)
	}
	base, ok := runner.Inner.(subprocess.CuratedGroupRunner)
	if !ok {
		t.Fatalf("sandbox inner = %T, want CuratedGroupRunner", runner.Inner)
	}
	if containsEnvPrefix(base.BaseEnv, "GH_TOKEN=") || containsEnvPrefix(base.BaseEnv, "OPENAI_API_KEY=") {
		t.Fatalf("curated base environment leaked credentials: %v", base.BaseEnv)
	}
}

func TestReadOnlyRuntimeStateSurvivesRepairDeliveries(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "runtime-state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(stateRoot, "state")
	if err := os.WriteFile(stateFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &sandboxAdapterCaptureRunner{stdout: `done`}
	adapter := readOnlyRuntimeAdapter{
		Adapter:     runtime.ShellAdapter{Runner: runner},
		cleanupRoot: stateRoot,
	}
	agent := runtime.Agent{
		Name: "reviewer", Role: "reviewer", Runtime: runtime.ShellRuntime,
		RuntimeRef: "true", RepoScope: "owner/repo",
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
	}
	for delivery := 1; delivery <= 2; delivery++ {
		if _, err := adapter.Deliver(context.Background(), agent, runtime.Job{Prompt: "review"}); err != nil {
			t.Fatalf("delivery %d: %v", delivery, err)
		}
		if data, err := os.ReadFile(stateFile); err != nil || string(data) != "secret" {
			t.Fatalf("delivery %d lost repair state: data=%q err=%v", delivery, data, err)
		}
	}
	if err := adapter.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged runtime state survived job cleanup: %v", err)
	}
}

func TestWorkerReadOnlyRuntimeStateSurvivesMailboxRepair(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	readOnlySeatOmpBrokerEnv(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.OmpRuntime,
		"", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "repair-review", Agent: "reviewer", Action: "review", Repo: "owner/repo",
		WorktreePath: checkout, ReadOnlySeat: true, Model: readOnlySeatOmpModel,
	})
	runner := &repairStateRunner{}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return runtime.OmpAdapter{Runner: runner}, nil
	}
	job, err := store.GetJob(ctx, "repair-review")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(workflow.JobSucceeded) || runner.calls != 2 || !runner.repairStateObserved {
		t.Fatalf("repair job state=%q calls=%d stateObserved=%v payload=%s", stored.State, runner.calls, runner.repairStateObserved, stored.Payload)
	}
	if _, err := os.Stat(filepath.Dir(runner.stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated runtime state survived job boundary: %v", err)
	}
}

func TestWorkerReadOnlyCleanupRemovesRenamedRuntimeState(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	readOnlySeatOmpBrokerEnv(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.OmpRuntime,
		"", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "rename-state-review", Agent: "reviewer", Action: "review", Repo: "owner/repo",
		WorktreePath: checkout, ReadOnlySeat: true, Model: readOnlySeatOmpModel,
	})
	runner := &renameRuntimeStateRunner{}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return runtime.OmpAdapter{Runner: runner}, nil
	}
	job, err := store.GetJob(ctx, "rename-state-review")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(workflow.JobSucceeded) || runner.savedState == "" {
		t.Fatalf("rename-state job state=%q savedState=%q payload=%s", stored.State, runner.savedState, stored.Payload)
	}
	if _, err := os.Stat(runner.savedState); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed runtime state survived job boundary: %v", err)
	}
}

// streamingReviewRunner is STREAM-capable on purpose: TeeRunner writes through
// the StreamRunner seam, so a buffered-only fake makes the retained transcript
// look empty even when the tee composed correctly — a fixture limitation that
// reads exactly like a lost wrapper.
type streamingReviewRunner struct {
	stdout string
	env    []string
}

func (r *streamingReviewRunner) Run(ctx context.Context, dir, command string, args ...string) (subprocess.Result, error) {
	return r.RunStream(ctx, dir, nil, command, args...)
}

func (r *streamingReviewRunner) RunEnv(ctx context.Context, dir string, env []string, command string, args ...string) (subprocess.Result, error) {
	return r.RunEnvStream(ctx, dir, env, nil, command, args...)
}

func (r *streamingReviewRunner) RunStream(_ context.Context, _ string, out io.Writer, command string, args ...string) (subprocess.Result, error) {
	if out != nil {
		if _, err := io.WriteString(out, r.stdout); err != nil {
			return subprocess.Result{}, err
		}
	}
	return subprocess.Result{Command: command, Args: args, Stdout: r.stdout}, nil
}

func (r *streamingReviewRunner) RunEnvStream(ctx context.Context, dir string, env []string, out io.Writer, command string, args ...string) (subprocess.Result, error) {
	r.env = append([]string(nil), env...)
	return r.RunStream(ctx, dir, out, command, args...)
}

// The read-only review adapter composes a PID-capturing, env-injecting tee, so
// the fixture must serve that combined seam or delivery fails for a reason that
// has nothing to do with the wrappers under test.
func (r *streamingReviewRunner) RunStreamWithPID(ctx context.Context, dir string, out io.Writer, onPID subprocess.PIDCallback, command string, args ...string) (subprocess.Result, error) {
	if onPID != nil {
		onPID(os.Getpid())
	}
	return r.RunStream(ctx, dir, out, command, args...)
}

func (r *streamingReviewRunner) RunEnvStreamWithPID(ctx context.Context, dir string, env []string, out io.Writer, onPID subprocess.PIDCallback, command string, args ...string) (subprocess.Result, error) {
	if onPID != nil {
		onPID(os.Getpid())
	}
	return r.RunEnvStream(ctx, dir, env, out, command, args...)
}
func (r *streamingReviewRunner) LookPath(file string) (string, error) { return file, nil }

// TestWorkerReadOnlyReviewRewrapsToolCacheAndTranscript enters through the
// queued worker path: it must retain both post-sandbox wrappers around a
// stateful read-only adapter. Calling either helper directly would not prove
// the production composition order.
func TestWorkerReadOnlyReviewRewrapsToolCacheAndTranscript(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	readOnlySeatOmpBrokerEnv(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.OmpRuntime,
		"", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	const jobID = "read-only-rewrap-review"
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: jobID, Agent: "reviewer", Action: "review", Repo: "owner/repo",
		WorktreePath: checkout, ReadOnlySeat: true, Model: readOnlySeatOmpModel,
	})
	runner := &streamingReviewRunner{
		stdout: readOnlySeatOmpStdout(`{"gitmoot_result":{"decision":"approved","summary":"reviewer adapter rewrap probe","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`),
	}
	var workerOutput bytes.Buffer
	worker := defaultJobWorker(store, &workerOutput, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return runtime.OmpAdapter{Runner: runner}, nil
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != string(workflow.JobSucceeded) {
		t.Fatalf("review job state=%q payload=%s", stored.State, stored.Payload)
	}
	if cache := envValue(runner.env, "GOCACHE"); !strings.Contains(cache, filepath.Join("cache", "tools", "read-only")) {
		t.Fatalf("GOCACHE=%q, want isolated read-only tool cache", cache)
	}
	if output := workerOutput.String(); strings.Contains(output, "tool cache env inject failed") || strings.Contains(output, "transcript tee build failed") {
		t.Fatalf("worker lost a post-sandbox wrapper: %s", output)
	}
	log, err := os.ReadFile(transcript.JobLogPath(config.PathsForHome(home).Logs, jobID))
	if err != nil {
		t.Fatalf("read retained transcript: %v", err)
	}
	if !strings.Contains(string(log), "reviewer adapter rewrap probe") {
		t.Fatalf("retained transcript missing delivery output: %q", log)
	}
}

func TestForegroundReviewRuntimeStateSurvivesRepairAndCleansAtBoundary(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.OmpRuntime,
		"", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "implementer", runtime.ShellRuntime,
		"true", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
	readOnlySeatOmpBrokerEnv(t)
	head := readonlyWorktreeHead(t, checkout)
	runner := &repairStateRunner{}

	previousAdapterFactory := localAgentDispatchRuntimeAdapterFor
	localAgentDispatchRuntimeAdapterFor = func(configHome string, agent runtime.Agent, deliveryCheckout string) (runtime.Adapter, error) {
		delivery, _, err := wrapReadOnlySandboxAdapter(configHome, agent, deliveryCheckout, "", runtime.OmpAdapter{Runner: runner})
		if err != nil {
			return nil, err
		}
		adapter, ok := delivery.(runtime.Adapter)
		if !ok {
			return nil, errors.New("foreground read-only adapter does not implement runtime.Adapter")
		}
		return adapter, nil
	}
	t.Cleanup(func() { localAgentDispatchRuntimeAdapterFor = previousAdapterFactory })
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	t.Cleanup(func() { newAgentDispatchGitHubClient = previousGitHubFactory })

	output, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer",
		Action: "review", Instructions: "Review the exact head.", PullRequest: 7,
		Branch: "main", HeadSHA: head, Home: home, Model: readOnlySeatOmpModel,
	})
	if err != nil {
		t.Fatalf("foreground review: %v", err)
	}
	if output.Result == nil || output.Result.Decision != "approved" {
		t.Fatalf("foreground review result = %+v, want approved", output.Result)
	}
	// SCOPE, stated because it is easy to misread: toolCacheObserved pins that the
	// Landlock wrapper delivers the isolated cache env on the FOREGROUND path. It
	// does NOT pin the two rewrap switches — reverting those leaves this green,
	// verified by mutation. TestWorkerReadOnlyReviewRewrapsToolCacheAndTranscript
	// is the test that guards them.
	if runner.calls != 2 || !runner.repairStateObserved || !runner.toolCacheObserved {
		t.Fatalf("foreground repair calls=%d stateObserved=%v toolCacheObserved=%v", runner.calls, runner.repairStateObserved, runner.toolCacheObserved)
	}
	if _, err := os.Stat(filepath.Dir(runner.stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreground isolated runtime state survived job boundary: %v", err)
	}
}

func TestWrapReadOnlySandboxAdapterRejectsOmpBeforeStagingWithoutCredentialBroker(t *testing.T) {
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_AUTH_BROKER_URL", "")
	t.Setenv("OMP_AUTH_BROKER_TOKEN", "")
	t.Setenv("OMP_AUTH_BROKER_TOKEN_FILE", "")
	configHome := t.TempDir()
	_, _, err := wrapReadOnlySandboxAdapter(configHome, runtime.Agent{
		Runtime: runtime.OmpRuntime, ReadOnlySeat: true,
	}, checkout, "", runtime.OmpAdapter{})
	if err == nil ||
		!strings.Contains(err.Error(), "OMP_AUTH_BROKER_URL") ||
		!strings.Contains(err.Error(), "OMP_AUTH_BROKER_TOKEN_FILE") {
		t.Fatalf("omp read-only seat error = %v", err)
	}
	entries, readErr := os.ReadDir(configHome)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("broker refusal staged state under %q: entries=%v err=%v", configHome, entries, readErr)
	}
}

func TestWrapReadOnlySandboxAdapterUsesScopedBrokerAndPrivateOmpState(t *testing.T) {
	configHome := t.TempDir()
	checkout := filepath.Join(t.TempDir(), "review-worktree")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	hostProfile := filepath.Join(t.TempDir(), "operator-omp")
	if err := os.MkdirAll(hostProfile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostProfile, "agent.db"), []byte("host sessions and credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runtimeDir, runtime.OmpRuntime), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "broker-token")
	if err := os.WriteFile(tokenFile, []byte("seat-broker-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_AUTH_BROKER_URL", "http://127.0.0.1:8765")
	t.Setenv("OMP_AUTH_BROKER_TOKEN", "")
	t.Setenv("OMP_AUTH_BROKER_TOKEN_FILE", tokenFile)
	t.Setenv("OPENAI_API_KEY", "must-not-cross")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-cross")
	t.Setenv("OMP_PROFILE", "operator")
	t.Setenv("PI_PROFILE", "operator")
	t.Setenv("PI_CODING_AGENT_DIR", hostProfile)

	wrapped, _, err := wrapReadOnlySandboxAdapter(configHome, runtime.Agent{
		Runtime: runtime.OmpRuntime, Model: "kimi-code/k3", ReadOnlySeat: true, RuntimeConfigDir: hostProfile,
	}, checkout, "", runtime.OmpAdapter{Runner: subprocess.GroupRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	stateAdapter, ok := wrapped.(readOnlyRuntimeAdapter)
	if !ok {
		t.Fatalf("wrapped adapter = %T, want readOnlyRuntimeAdapter", wrapped)
	}
	stateDir := filepath.Join(stateAdapter.cleanupRoot, "runtime-state", ".omp", "agent")
	if entries, err := os.ReadDir(stateDir); err != nil || len(entries) != 0 {
		t.Fatalf("isolated OMP state before delivery = %q entries=%v err=%v, want empty", stateDir, entries, err)
	}
	if _, err := stateAdapter.ompSession.prepare("kimi-code/k3"); err != nil {
		t.Fatalf("prepare job-scoped OMP broker: %v", err)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.yml" {
		t.Fatalf("isolated OMP state = %q entries=%v err=%v, want only scoped config", stateDir, entries, err)
	}
	brokerURL := "http://" + stateAdapter.ompSession.broker.listener.Addr().String()
	brokerToken := stateAdapter.ompSession.broker.downstreamKey
	configPath := filepath.Join(stateDir, "config.yml")
	configData, err := os.ReadFile(configPath)
	if err != nil ||
		!strings.Contains(string(configData), brokerURL) ||
		!strings.Contains(string(configData), brokerToken) ||
		strings.Contains(string(configData), "seat-broker-token") ||
		strings.Contains(string(configData), "127.0.0.1:8765") {
		t.Fatalf("job-private OMP config did not contain only the scoped route: %q err=%v", configData, err)
	}
	if info, err := os.Stat(configPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("job-private OMP config mode = %v err=%v, want 0600", info, err)
	}
	ompAdapter, ok := stateAdapter.Adapter.(runtime.OmpAdapter)
	if !ok {
		t.Fatalf("inner adapter = %T, want runtime.OmpAdapter", stateAdapter.Adapter)
	}
	runner, ok := ompAdapter.Runner.(subprocess.WrappingRunner)
	if !ok {
		t.Fatalf("wrapped runner = %T, want subprocess.WrappingRunner", ompAdapter.Runner)
	}
	if !runner.ReadOnlyWorkdir || len(runner.WritablePaths) == 0 || runner.WritablePaths[0] != stateAdapter.cleanupRoot {
		t.Fatalf("OMP sandbox workdir/write grants = readOnly:%v writes:%v", runner.ReadOnlyWorkdir, runner.WritablePaths)
	}
	assertSeatTempDirGrant(t, runner.WritablePaths, stateAdapter.cleanupTemp, runner.Env)
	if containsPath(runner.ReadablePaths, hostProfile) || containsPath(runner.ReadableFiles, filepath.Join(hostProfile, "agent.db")) {
		t.Fatalf("OMP sandbox reads expose operator profile: dirs=%v files=%v", runner.ReadablePaths, runner.ReadableFiles)
	}
	if envValue(runner.Env, "PI_CODING_AGENT_DIR") != stateDir ||
		envValue(runner.Env, "PI_CONFIG_DIR") != filepath.Join("..", "runtime-state", ".omp") {
		t.Fatalf("OMP sandbox env lacks isolated state roots: %v", redactEnvNames(runner.Env))
	}
	for _, forbidden := range []string{
		"OPENAI_API_KEY=", "ANTHROPIC_API_KEY=", "OMP_PROFILE=", "PI_PROFILE=",
		"OMP_AUTH_BROKER_URL=", "OMP_AUTH_BROKER_TOKEN=", "OMP_AUTH_BROKER_TOKEN_FILE=",
	} {
		if containsEnvPrefix(runner.Env, forbidden) {
			t.Fatalf("OMP sandbox env contains forbidden %s", strings.TrimSuffix(forbidden, "="))
		}
	}
	base, ok := runner.Inner.(subprocess.CuratedGroupRunner)
	if !ok {
		t.Fatalf("sandbox inner = %T, want CuratedGroupRunner", runner.Inner)
	}
	for _, forbidden := range []string{"OMP_AUTH_BROKER_URL=", "OMP_AUTH_BROKER_TOKEN=", "OMP_AUTH_BROKER_TOKEN_FILE="} {
		if containsEnvPrefix(base.BaseEnv, forbidden) {
			t.Fatalf("OMP curated base exposes broker config: %v", redactEnvNames(base.BaseEnv))
		}
	}
	if probe := sandbox.SandboxProbe(); probe.Supported {
		for _, path := range []string{
			tokenFile,
			filepath.Join("/proc", strconv.Itoa(os.Getpid()), "root", tokenFile),
		} {
			readResult, readErr := runner.Run(context.Background(), checkout, "/usr/bin/cat", path)
			if readErr == nil || strings.Contains(readResult.Stdout, "seat-broker-token") {
				t.Fatalf("OMP sandbox read upstream broker token through %q: err=%v output=%q", path, readErr, readResult.Stdout)
			}
		}
	}
	var stagedOmp string
	for _, dir := range filepath.SplitList(envValue(runner.Env, "PATH")) {
		candidate := filepath.Join(dir, runtime.OmpRuntime)
		if info, statErr := os.Stat(candidate); statErr == nil && info.Mode().IsRegular() {
			stagedOmp = candidate
			break
		}
	}
	if stagedOmp == "" || pathWithin(stagedOmp, runtimeDir) {
		t.Fatalf("OMP PATH did not resolve an engine-owned staged copy: %q", stagedOmp)
	}
	if err := stateAdapter.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stateAdapter.cleanupRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated OMP state survived cleanup: %v", err)
	}
	client := http.Client{Timeout: 250 * time.Millisecond}
	response, requestErr := client.Get(brokerURL + "/v1/healthz")
	if requestErr == nil {
		response.Body.Close()
		t.Fatal("job-local OMP broker remained reachable after cleanup")
	}
}

func TestApplyReadOnlySeatClearsInheritedGrants(t *testing.T) {
	for _, test := range []struct {
		name       string
		marked     bool
		wantSeat   bool
		wantConfig string
	}{
		{name: "review seat", marked: true, wantSeat: true, wantConfig: "/profiles/reviewer"},
		{name: "ask seat", marked: true, wantSeat: true, wantConfig: "/profiles/reviewer"},
		{name: "ordinary job", marked: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := runtime.Agent{
				WritablePaths: []string{"/shared/cache"},
				ReadablePaths: []string{"/host"},
				ReadableFiles: []string{"/host/secret"},
			}
			if err := applyReadOnlySeat(test.marked, " /profiles/reviewer ", "job-grants", &agent); err != nil {
				t.Fatalf("applyReadOnlySeat: %v", err)
			}
			if agent.ReadOnlySeat != test.wantSeat || agent.RuntimeConfigDir != test.wantConfig {
				t.Fatalf("read-only marker = %v config = %q, want %v %q", agent.ReadOnlySeat, agent.RuntimeConfigDir, test.wantSeat, test.wantConfig)
			}
			if test.wantSeat && (len(agent.WritablePaths) != 0 || len(agent.ReadablePaths) != 0 || len(agent.ReadableFiles) != 0) {
				t.Fatalf("read-only seat retained configured grants: writes=%v readDirs=%v readFiles=%v", agent.WritablePaths, agent.ReadablePaths, agent.ReadableFiles)
			}
		})
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

// assertSeatTempDirGrant proves a read-only seat's write grants are its cache
// root plus exactly its own private temp dir (#2314): the one the adapter will
// remove, the one its TMPDIR names, a fresh gmr-<8 hex> directly under the seat
// temp parent. Where no short parent is writable (a seat of an older daemon)
// the temp dir is <cache root>/tmp instead, and the cache root is the only grant.
func assertSeatTempDirGrant(t *testing.T, writes []string, owned seatTempDir, env []string) {
	t.Helper()
	grant := owned.dir
	if grant == "" {
		t.Fatalf("the adapter owns no seat temp dir; writes = %v", writes)
	}
	if owned.inCacheRoot {
		if len(writes) != 1 || grant != filepath.Join(writes[0], "tmp") {
			t.Fatalf("writes = %v with cache-root temp %q, want only the cache root holding <cache root>/tmp", writes, grant)
		}
	} else {
		if len(writes) != 2 || writes[1] != grant {
			t.Fatalf("writes = %v, want the cache root and the adapter's own seat temp dir %q", writes, grant)
		}
		if filepath.Dir(grant) != seatTempParent() || !seatTempNamePattern.MatchString(filepath.Base(grant)) {
			t.Fatalf("seat temp write grant = %q, want %s/gmr-<8 hex>", grant, seatTempParent())
		}
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if got := envValue(env, name); got != grant {
			t.Fatalf("seat %s = %q, want its temp dir grant %q", name, got, grant)
		}
	}
	if info, err := os.Lstat(grant); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("seat temp dir %q = %v err=%v, want a 0700 directory", grant, info, err)
	}
}

func containsEnv(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}
func envValue(env []string, name string) string {
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func containsEnvPrefix(env []string, prefix string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
