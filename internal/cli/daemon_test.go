package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func resetPreflightWarnState() {
	preflightWarnMu.Lock()
	preflightWarnByRepo = map[string]preflightWarnState{}
	preflightWarnMu.Unlock()
}

// daemonArgsContainFlag reports whether argv carries `--name value` as a
// separate flag/value pair.
func daemonArgsContainFlag(args []string, name string, value string) bool {
	flagName := "--" + name
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flagName && args[i+1] == value {
			return true
		}
	}
	return false
}

func mustListJobEvents(t *testing.T, store *db.Store, jobID string) []db.JobEvent {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	return events
}

// stubEnsureGitHub adopts a pre-existing open PR via EnsurePullRequest, the way
// the real GhClient does when GetOpenPullRequestByHead finds one. It records
// whether CreatePullRequest was called so the test can prove the finalizer took
// the idempotent adopt path instead of erroring on a 422 race.
type stubEnsureGitHub struct {
	github.NoopClient
	existing    github.PullRequest
	ensureCalls int
	createCalls int
	ensureErr   error
}

func (s *stubEnsureGitHub) EnsurePullRequest(context.Context, github.CreatePullRequestInput) (github.PullRequest, error) {
	s.ensureCalls++
	if s.ensureErr != nil {
		return github.PullRequest{}, s.ensureErr
	}
	return s.existing, nil
}

func (s *stubEnsureGitHub) CreatePullRequest(context.Context, github.CreatePullRequestInput) (github.PullRequest, error) {
	s.createCalls++
	return github.PullRequest{}, errors.New("CreatePullRequest should not be called when EnsurePullRequest adopts")
}

// stubSkipFlagAtOpenGitHub records whether the branch lock's skip flag was
// already persisted at the moment the PR is opened — the #390 invariant: the
// flag must be durable BEFORE the daemon-watched PR becomes observable, or the
// PR-watcher can fan out native reviews in the gap.
type stubSkipFlagAtOpenGitHub struct {
	github.NoopClient
	store      *db.Store
	repo       string
	branch     string
	pr         github.PullRequest
	opened     bool
	skipAtOpen bool
}

func (s *stubSkipFlagAtOpenGitHub) EnsurePullRequest(ctx context.Context, _ github.CreatePullRequestInput) (github.PullRequest, error) {
	s.opened = true
	if lock, err := s.store.GetBranchLock(ctx, s.repo, s.branch); err == nil {
		s.skipAtOpen = lock.SkipNativeReviewFanout
	}
	return s.pr, nil
}

// --- #394 PR1: opt-in continuous worker-pool scheduler (--scheduler=pool) ---

const poolSchedulerAskResult = `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

func poolSchedulerWorker(t *testing.T, store *db.Store, adapter workflow.DeliveryAdapter, usePool bool) jobWorker {
	t.Helper()
	worker := defaultJobWorker(store, io.Discard)
	worker.UsePool = usePool
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	return worker
}

// runMidFlightEnqueueScenario runs job-a, which enqueues job-b (a different repo,
// so a distinct checkout key) once dispatch has begun, then returns job-b's final
// state. The pool re-queries and runs it; the barrier never re-queries, so it
// stays queued — the #394 layer-1 behavior the pool fixes.
func runMidFlightEnqueueScenario(t *testing.T, usePool bool) string {
	t.Helper()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	if err := store.AllowAgentRepo(ctx, "audit", "owner/repo-b"); err != nil {
		t.Fatalf("AllowAgentRepo: %v", err)
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})

	var once sync.Once
	var enqErr error
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	adapter.onDeliver = func() {
		once.Do(func() {
			_, enqErr = (workflow.Mailbox{Store: store}).Enqueue(ctx, workflow.JobRequest{ID: "job-b", Agent: "audit", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2})
		})
	}
	worker := poolSchedulerWorker(t, store, adapter, usePool)

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo(usePool=%v): %v", usePool, err)
	}
	if enqErr != nil {
		t.Fatalf("mid-flight enqueue of job-b failed: %v", enqErr)
	}
	job, err := store.GetJob(ctx, "job-b")
	if err != nil {
		t.Fatalf("GetJob job-b: %v", err)
	}
	return job.State
}

type poolConcurrencyTracker struct {
	mu     sync.Mutex
	active int
	max    int
}

func (c *poolConcurrencyTracker) span() {
	c.mu.Lock()
	c.active++
	if c.active > c.max {
		c.max = c.active
	}
	c.mu.Unlock()
	time.Sleep(25 * time.Millisecond)
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
}

func (c *poolConcurrencyTracker) peak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.max
}

func runPoolConcurrencyScenario(t *testing.T, repoB string) int {
	t.Helper()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo-a")
	if repoB != "owner/repo-a" {
		seedDaemonWorkerRepo(t, store, repoB, t.TempDir())
		if err := store.AllowAgentRepo(ctx, "audit", repoB); err != nil {
			t.Fatalf("AllowAgentRepo: %v", err)
		}
	}
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-1", Agent: "audit", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-2", Agent: "audit", Action: "ask", Repo: repoB, Branch: "main", PullRequest: 2})

	tracker := &poolConcurrencyTracker{}
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	adapter.onDeliver = tracker.span
	worker := poolSchedulerWorker(t, store, adapter, true)

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo: %v", err)
	}
	return tracker.peak()
}

// runAdmissionConcurrencyScenario enqueues two jobs that would otherwise run in
// parallel (distinct repos ⇒ distinct checkout keys; distinct codex sessions ⇒
// distinct runtime keys, so neither the checkout nor the runtime lock serializes
// them) and runs them under the barrier or pool scheduler with the given
// admission budget attached. It returns the observed peak concurrent deliveries.
func runAdmissionConcurrencyScenario(t *testing.T, usePool bool, budget *admissionBudget) int {
	t.Helper()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo-a", t.TempDir())
	seedDaemonWorkerRepo(t, store, "owner/repo-b", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit-a", runtime.CodexRuntime, "session-a", []string{"ask"}, "owner/repo-a")
	seedDaemonWorkerAgent(t, store, "audit-b", runtime.CodexRuntime, "session-b", []string{"ask"}, "owner/repo-b")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-a", Agent: "audit-a", Action: "ask", Repo: "owner/repo-a", Branch: "main", PullRequest: 1})
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "job-b", Agent: "audit-b", Action: "ask", Repo: "owner/repo-b", Branch: "main", PullRequest: 2})

	tracker := &poolConcurrencyTracker{}
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	adapter.onDeliver = tracker.span
	worker := poolSchedulerWorker(t, store, adapter, usePool)
	worker.Admission = budget

	if err := runQueuedJobsForRepo(ctx, worker, 2, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo(usePool=%v): %v", usePool, err)
	}
	for _, id := range []string{"job-a", "job-b"} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q, want succeeded (a deferred job must still run, never drop)", id, job.State)
		}
	}
	return tracker.peak()
}

// blockedTTLTestStore builds a fresh store on a retained home so the blocked_ttl
// sweep tests can backdate a blocked job's updated_at through a second raw
// connection (there is no store setter for updated_at; the blocked transition
// stamps CURRENT_TIMESTAMP). It mirrors daemonWorkerStore but returns paths.
func blockedTTLTestStore(t *testing.T) (*db.Store, config.Paths) {
	t.Helper()
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store, paths
}

// backdateJobUpdatedAt rewrites a job's updated_at to an explicit SQLite-UTC
// timestamp through a raw connection, so a blocked job can be aged past the sweep
// window deterministically without a wall-clock sleep.
func backdateJobUpdatedAt(t *testing.T, dbPath, jobID string, when time.Time) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(context.Background(), `UPDATE jobs SET updated_at = ? WHERE id = ?`, when.UTC().Format("2006-01-02 15:04:05"), jobID); err != nil {
		t.Fatalf("backdate updated_at returned error: %v", err)
	}
}

// fakeReclaimWorktreeManager is a worktree manager used by the
// reclaim-skipped-worktrees test: it satisfies the cleanup's type-asserted
// interfaces (force-remove + branch delete + branch existence) and records calls.
type fakeReclaimWorktreeManager struct {
	removed  []string
	deleted  []string
	branches map[string]bool
	pruned   int
}

func (m *fakeReclaimWorktreeManager) AddWorktree(context.Context, string, string, string) error {
	return nil
}
func (m *fakeReclaimWorktreeManager) AddDetachedWorktree(context.Context, string, string) error {
	return nil
}
func (m *fakeReclaimWorktreeManager) RemoveWorktreeForce(_ context.Context, path string) error {
	m.removed = append(m.removed, path)
	return nil
}
func (m *fakeReclaimWorktreeManager) DeleteBranch(_ context.Context, branch string) error {
	m.deleted = append(m.deleted, branch)
	m.branches[branch] = false
	return nil
}
func (m *fakeReclaimWorktreeManager) BranchExists(_ context.Context, branch string) (bool, error) {
	return m.branches[branch], nil
}
func (m *fakeReclaimWorktreeManager) PruneWorktrees(context.Context) error {
	m.pruned++
	return nil
}

func delegationWorktreeCleanupPendingForTest(ctx context.Context, store tickCandidateStore, jobID string) (bool, error) {
	ids, err := store.JobIDsWithPendingDelegationWorktreeReclaim(ctx)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if id == jobID {
			return true, nil
		}
	}
	return false, nil
}

type daemonGitRunner struct {
	results []subprocess.Result
	errs    []error
	calls   [][]string
}

func (f *daemonGitRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	call := append([]string{command}, args...)
	f.calls = append(f.calls, call)
	index := len(f.calls) - 1
	result := subprocess.Result{Command: command, Args: args}
	if index < len(f.results) {
		result = f.results[index]
		result.Command = command
		result.Args = args
	}
	var err error
	if index < len(f.errs) {
		err = f.errs[index]
	}
	return result, err
}

func (f *daemonGitRunner) LookPath(string) (string, error) {
	return "", errors.New("not implemented")
}

func (f *daemonGitRunner) wantArgs(t *testing.T, index int, want ...string) {
	t.Helper()
	if index >= len(f.calls) {
		t.Fatalf("missing call %d; calls=%v", index, f.calls)
	}
	if !reflect.DeepEqual(f.calls[index], want) {
		t.Fatalf("call %d = %v, want %v", index, f.calls[index], want)
	}
}

type cliPollFakeGitHub struct {
	github.NoopClient
	pulls                 []github.PullRequest
	comments              map[int64][]github.IssueComment
	listErr               error
	postErr               error
	listPullRequestsCalls int
	posted                []cliPollPostedComment
	conditionalStats      github.ConditionalRequestStats
}

type cliETagRunner struct {
	mu    sync.Mutex
	calls int
}

func (r *cliETagRunner) Run(_ context.Context, _ string, command string, _ ...string) (subprocess.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if command != "gh" {
		return subprocess.Result{}, fmt.Errorf("unexpected command %s", command)
	}
	if r.calls == 1 {
		return subprocess.Result{Stdout: "HTTP/2 200 OK\nETag: \"cli-etag\"\n\n[]"}, nil
	}
	return subprocess.Result{Stdout: "HTTP/2.0 304 Not Modified\n\n"}, errors.New("exit status 1")
}

func (r *cliETagRunner) LookPath(string) (string, error) { return "/usr/bin/gh", nil }

func (f *cliPollFakeGitHub) ConditionalRequestStats() github.ConditionalRequestStats {
	return f.conditionalStats
}

type cliPollPostedComment struct {
	issueNumber int64
	body        string
}

func (f *cliPollFakeGitHub) ListPullRequests(context.Context, github.Repository, string) ([]github.PullRequest, error) {
	f.listPullRequestsCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]github.PullRequest(nil), f.pulls...), nil
}

func (f *cliPollFakeGitHub) ListIssues(context.Context, github.Repository, string) ([]github.Issue, error) {
	return nil, nil
}

func (f *cliPollFakeGitHub) ListIssueComments(_ context.Context, _ github.Repository, issueNumber int64) ([]github.IssueComment, error) {
	return append([]github.IssueComment(nil), f.comments[issueNumber]...), nil
}

func (f *cliPollFakeGitHub) ListRepoIssueComments(_ context.Context, _ github.Repository, _ time.Time) ([]github.IssueComment, error) {
	var out []github.IssueComment
	for number, list := range f.comments {
		for _, c := range list {
			c.IssueNumber = number
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *cliPollFakeGitHub) PostIssueComment(_ context.Context, _ github.Repository, issueNumber int64, body string) (github.IssueComment, error) {
	if f.postErr != nil {
		return github.IssueComment{}, f.postErr
	}
	f.posted = append(f.posted, cliPollPostedComment{issueNumber: issueNumber, body: body})
	return github.IssueComment{ID: int64(len(f.posted)), Body: body}, nil
}

func (f *cliPollFakeGitHub) GetUserPermission(context.Context, github.Repository, string) (github.UserPermission, error) {
	return github.UserPermission{Permission: "write", RoleName: "write"}, nil
}

type cliWorkerFakeAdapter struct {
	mu                   sync.Mutex
	output               string
	err                  error
	startRuntimeRef      string
	startErr             error
	startCalls           int
	startCheckouts       []string
	calls                int
	delivered            []string
	prompts              []string
	onStart              func()
	onDeliver            func()
	waitForContextCancel bool
	contextCancelled     bool
}

func (f *cliWorkerFakeAdapter) Name() string {
	return "fake"
}

func (f *cliWorkerFakeAdapter) Start(_ context.Context, _ runtime.StartRequest) (runtime.StartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	if f.startErr != nil {
		return runtime.StartResult{}, f.startErr
	}
	onStart := f.onStart
	ref := strings.TrimSpace(f.startRuntimeRef)
	if ref == "" {
		ref = "550e8400-e29b-41d4-a716-446655440000"
	}
	if onStart != nil {
		onStart()
	}
	return runtime.StartResult{RuntimeRef: ref}, nil
}

func (f *cliWorkerFakeAdapter) Validate(context.Context, runtime.Agent) error {
	return nil
}

func (f *cliWorkerFakeAdapter) Deliver(ctx context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	f.mu.Lock()
	f.calls++
	f.delivered = append(f.delivered, job.ID)
	f.prompts = append(f.prompts, job.Prompt)
	onDeliver := f.onDeliver
	waitForContextCancel := f.waitForContextCancel
	f.mu.Unlock()
	if onDeliver != nil {
		onDeliver()
	}
	if waitForContextCancel {
		<-ctx.Done()
		f.mu.Lock()
		f.contextCancelled = true
		f.mu.Unlock()
		return runtime.Result{}, ctx.Err()
	}
	if f.err != nil {
		return runtime.Result{}, f.err
	}
	return runtime.Result{Raw: f.output}, nil
}

func (f *cliWorkerFakeAdapter) Health(context.Context, runtime.Agent) error {
	return nil
}

func (f *cliWorkerFakeAdapter) Capabilities(context.Context) ([]string, error) {
	return nil, nil
}

func (f *cliWorkerFakeAdapter) observedContextCancel() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contextCancelled
}

type cliWorkerFakeMergeGate struct {
	calls    int
	decision workflow.MergeDecision
	err      error
}

func (f *cliWorkerFakeMergeGate) Evaluate(context.Context, workflow.MergeRequest) (workflow.MergeDecision, error) {
	f.calls++
	if f.err != nil {
		return workflow.MergeDecision{}, f.err
	}
	return f.decision, nil
}

func daemonWorkerStore(t *testing.T) *db.Store {
	t.Helper()
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	})
	return store
}

func createDaemonWorkerGitCheckout(t *testing.T, branch string) string {
	t.Helper()
	checkout := t.TempDir()
	runDaemonWorkerGit(t, checkout, "init", "-b", branch)
	runDaemonWorkerGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runDaemonWorkerGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile README returned error: %v", err)
	}
	runDaemonWorkerGit(t, checkout, "add", "README.md")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "initial")
	runDaemonWorkerGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	return checkout
}

func runDaemonWorkerGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func seedDaemonWorkerRepo(t *testing.T, store *db.Store, fullName string, checkout string) {
	t.Helper()
	repo, err := daemon.ParseRepository(fullName)
	if err != nil {
		t.Fatalf("ParseRepository returned error: %v", err)
	}
	if err := store.UpsertRepo(context.Background(), db.Repo{Owner: repo.Owner, Name: repo.Name, CheckoutPath: checkout, PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
}

func seedDaemonWorkerAgent(t *testing.T, store *db.Store, name string, runtimeName string, runtimeRef string, capabilities []string, repo string) {
	t.Helper()
	// Implement-capable workers need a write policy or the fail-closed dispatch
	// preflight blocks their jobs (#452); default everyone else to auto. Tests that
	// deliberately exercise a non-write implement policy use the WithPolicy variant.
	policy := runtime.AutonomyPolicyAuto
	if runtime.HasImplementCapability(capabilities) {
		policy = runtime.AutonomyPolicyWorkspaceWrite
	}
	seedDaemonWorkerAgentWithPolicy(t, store, name, runtimeName, runtimeRef, capabilities, repo, policy)
}

func seedDaemonWorkerAgentWithPolicy(t *testing.T, store *db.Store, name string, runtimeName string, runtimeRef string, capabilities []string, repo string, policy string) {
	t.Helper()
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           name,
		Role:           "worker",
		Runtime:        runtimeName,
		RuntimeRef:     runtimeRef,
		RepoScope:      repo,
		Capabilities:   capabilities,
		AutonomyPolicy: policy,
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
}

func enqueueDaemonWorkerJob(t *testing.T, store *db.Store, request workflow.JobRequest) {
	t.Helper()
	if _, err := (workflow.Mailbox{Store: store}).Enqueue(context.Background(), request); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
}

// writeRepoConcurrencyConfigHome initializes a config home and appends body to
// its default config, returning the raw --home. Used to drive the per-repo
// concurrency override (#576) through the real jobWorker config loaders.
func writeRepoConcurrencyConfigHome(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return home
}

// runRepoConcurrencyTickPeak drives the REAL per-repo run path
// (runDaemonWorkerTickTracked -> resolveRepoScheduler -> runQueuedJobsForRepo) for one
// repo and returns the observed peak concurrent deliveries. It seeds `jobs`
// parallelizable queued jobs (each with a DISTINCT worktree path, so their
// checkout keys differ and nothing but the scheduler's concurrency limit can
// serialize them) and runs the global pool at globalWorkers — which is exactly
// what a [repos."owner/repo"] max_parallel override overrides per repo.
func runRepoConcurrencyTickPeak(t *testing.T, configHome, repo string, jobs, globalWorkers int) int {
	t.Helper()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, repo, t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, repo)
	ids := make([]string, 0, jobs)
	for i := 0; i < jobs; i++ {
		id := "job-" + strconv.Itoa(i+1)
		ids = append(ids, id)
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: id, Agent: "audit", Action: "ask", Repo: repo, Branch: "main", PullRequest: i + 1, WorktreePath: filepath.Join(t.TempDir(), "wt-"+id)})
	}

	tracker := &poolConcurrencyTracker{}
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	adapter.onDeliver = tracker.span
	// Global config: pool scheduler at globalWorkers (would run all in parallel).
	worker := poolSchedulerWorker(t, store, adapter, true)
	worker.ConfigHome = configHome
	worker.ConfigHomeExplicit = true

	if err := runDaemonWorkerTickTracked(ctx, store, worker, globalWorkers, false, repo, "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
		t.Fatalf("runDaemonWorkerTickTracked(%s): %v", repo, err)
	}
	for _, id := range ids {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q, want succeeded", id, job.State)
		}
	}
	return tracker.peak()
}

func daemonWorkerHasEvent(events []db.JobEvent, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

// seedDelegationCoordinator inserts a completed coordinator parent job whose
// result carries the given delegations and advances it so the engine dispatches
// the top-level delegation children. It returns the worker wired to the same
// store. The coordinator runs an "ask" job; delegations use "review" so dispatch
// stays on the shared-checkout path (no per-delegation worktree allocation).
func seedDelegationCoordinator(t *testing.T, store *db.Store, parentID string, delegations []workflow.Delegation) jobWorker {
	t.Helper()
	ctx := context.Background()
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "coord", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	seenAgents := map[string]bool{"coord": true}
	for _, d := range delegations {
		if seenAgents[d.Agent] {
			continue
		}
		seenAgents[d.Agent] = true
		seedDaemonWorkerAgent(t, store, d.Agent, runtime.ShellRuntime, "unused", []string{d.Action}, "owner/repo")
	}

	if err := store.CreateJob(ctx, db.Job{ID: parentID, Agent: "coord", Type: "ask", State: string(workflow.JobRunning)}); err != nil {
		t.Fatalf("CreateJob(parent) returned error: %v", err)
	}
	payload := workflow.JobPayload{
		Repo:      "owner/repo",
		Branch:    "task-1",
		TaskID:    "task-1",
		TaskTitle: "Coordinator",
		Sender:    "coord",
		Result: &workflow.AgentResult{
			Decision:    "approved",
			Summary:     "delegated",
			Delegations: delegations,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal parent payload returned error: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, parentID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload(parent) returned error: %v", err)
	}
	if err := store.UpdateJobState(ctx, parentID, string(workflow.JobSucceeded)); err != nil {
		t.Fatalf("UpdateJobState(parent) returned error: %v", err)
	}

	worker := defaultJobWorker(store, io.Discard)
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		return daemonWorkflowEngine(store, github.NoopClient{}, checkout, "")
	}
	engine := worker.WorkflowFactory("")
	if err := engine.AdvanceJob(ctx, parentID); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	return worker
}

// markDelegationChildTimedOut simulates a per-delegation timeout kill: the child
// is left JobRunning with no stored result, exactly as Mailbox.Run leaves it when
// the run context deadline fires mid-delivery (the cancelled context aborts its
// own fail write).
func markDelegationChildTimedOut(t *testing.T, store *db.Store, childID string) {
	t.Helper()
	if err := store.UpdateJobState(context.Background(), childID, string(workflow.JobRunning)); err != nil {
		t.Fatalf("UpdateJobState(%s, running) returned error: %v", childID, err)
	}
}

// cliWorkerDelegationAdapter returns a per-job-id canned gitmoot_result, so a
// fan-out test can give the coordinator a delegating result and each child a
// plain approval.
type cliWorkerDelegationAdapter struct {
	mu            sync.Mutex
	outputs       map[string]string
	defaultOutput string
	delivered     []string
}

func (a *cliWorkerDelegationAdapter) Name() string { return "fake-delegation" }

func (a *cliWorkerDelegationAdapter) Validate(context.Context, runtime.Agent) error { return nil }

func (a *cliWorkerDelegationAdapter) Deliver(_ context.Context, _ runtime.Agent, job runtime.Job) (runtime.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.delivered = append(a.delivered, job.ID)
	if out, ok := a.outputs[job.ID]; ok {
		return runtime.Result{Raw: out}, nil
	}
	return runtime.Result{Raw: a.defaultOutput}, nil
}

func (a *cliWorkerDelegationAdapter) Health(context.Context, runtime.Agent) error { return nil }

func (a *cliWorkerDelegationAdapter) Capabilities(context.Context) ([]string, error) {
	return nil, nil
}

// --- #444: same-repo parallel-job discoverability ---

// queuePolicyBusyRuntimeStore builds a worker whose parallel-session policy is
// "queue" (so a runtime-busy job bounces errRuntimeSessionBusy instead of forking a
// temp worker) with the codex "audit" agent's runtime session runtime:codex:session-1
// already externally locked. Each returned dispatch attempt bounces busy. Used by
// the #598 spin/dedup tests.
func queuePolicyBusyRuntimeStore(t *testing.T) (*db.Store, jobWorker, *int64) {
	t.Helper()
	store := daemonWorkerStore(t)
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	content := strings.Replace(config.DefaultConfig(paths), `same_session = "fork_temp_session"`, `same_session = "queue"`, 1)
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile config returned error: %v", err)
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.CodexRuntime, "session-1", []string{"ask"}, "owner/repo")
	if acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: "runtime:codex:session-1",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	var attempts int64
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		atomic.AddInt64(&attempts, 1)
		return t.TempDir(), nil
	}
	return store, worker, &attempts
}

// resetRuntimeLockWaitEpisodes clears the #598 runtime_lock_wait dedup state. Test-
// only (it mutates the package-global map), which is why it lives here rather than in
// daemon.go: the map persists across tests in a package run, so a test that asserts a
// runtime_lock_wait row is written must reset it first (a neighboring test reusing the
// same job id could otherwise leave an open episode that suppresses the write).
func resetRuntimeLockWaitEpisodes() {
	runtimeLockWaitMu.Lock()
	defer runtimeLockWaitMu.Unlock()
	runtimeLockWaitEpisodes = map[string]time.Time{}
}

// countingCandidateStore wraps a real *db.Store and counts how many times each of
// the three per-tick candidate GROUP BY queries executes, delegating to the store
// so the query still runs (returning the real, empty result on a job-free DB). It
// backs TestTickCandidatesComputedOncePerTick's proof that the #619 hoist runs each
// candidate query once per tick, not once per enabled repo.
type countingCandidateStore struct {
	inner       *db.Store
	advance     int32
	comment     int32
	reclaim     int32
	agedReclaim int32
}

func (c *countingCandidateStore) JobIDsWithPendingAdvanceRetry(ctx context.Context) ([]string, error) {
	atomic.AddInt32(&c.advance, 1)
	return c.inner.JobIDsWithPendingAdvanceRetry(ctx)
}

func (c *countingCandidateStore) JobIDsWithPendingCommentRetry(ctx context.Context) ([]string, error) {
	atomic.AddInt32(&c.comment, 1)
	return c.inner.JobIDsWithPendingCommentRetry(ctx)
}

func (c *countingCandidateStore) JobIDsWithPendingDelegationWorktreeReclaim(ctx context.Context) ([]string, error) {
	atomic.AddInt32(&c.reclaim, 1)
	return c.inner.JobIDsWithPendingDelegationWorktreeReclaim(ctx)
}

func (c *countingCandidateStore) JobIDsWithAgedTerminalDelegationWorktree(ctx context.Context, cutoff time.Time) ([]string, error) {
	atomic.AddInt32(&c.agedReclaim, 1)
	return c.inner.JobIDsWithAgedTerminalDelegationWorktree(ctx, cutoff)
}

// flakyCandidateStore returns an error on the first call to each candidate query and
// the memoized success on every later call, counting total calls per query. It backs
// TestTickCandidatesRetriesOnError's proof that tickCandidates memoizes SUCCESSES
// only: a query that errors on one repo's pass must be re-attempted (not replayed as
// the same error) on the next pass within the same tick.
type flakyCandidateStore struct {
	advanceCalls     int32
	commentCalls     int32
	reclaimCalls     int32
	agedReclaimCalls int32
}

var errCandidateTransient = errors.New("transient store fault")

func (s *flakyCandidateStore) JobIDsWithPendingAdvanceRetry(context.Context) ([]string, error) {
	if atomic.AddInt32(&s.advanceCalls, 1) == 1 {
		return nil, errCandidateTransient
	}
	return []string{"advance-job"}, nil
}

func (s *flakyCandidateStore) JobIDsWithPendingCommentRetry(context.Context) ([]string, error) {
	if atomic.AddInt32(&s.commentCalls, 1) == 1 {
		return nil, errCandidateTransient
	}
	return []string{"comment-job"}, nil
}

func (s *flakyCandidateStore) JobIDsWithPendingDelegationWorktreeReclaim(context.Context) ([]string, error) {
	if atomic.AddInt32(&s.reclaimCalls, 1) == 1 {
		return nil, errCandidateTransient
	}
	return []string{"reclaim-job"}, nil
}

func (s *flakyCandidateStore) JobIDsWithAgedTerminalDelegationWorktree(context.Context, time.Time) ([]string, error) {
	if atomic.AddInt32(&s.agedReclaimCalls, 1) == 1 {
		return nil, errCandidateTransient
	}
	return []string{"aged-reclaim-job"}, nil
}
