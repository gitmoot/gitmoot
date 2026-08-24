package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func resetDelegationReclaimAccountingForTest(t *testing.T) {
	t.Helper()
	delegationReclaimAccounting.Lock()
	delegationReclaimAccounting.failures = map[string]delegationReclaimFailure{}
	delegationReclaimAccounting.Unlock()
}

func seedReclaimResilienceCandidateForRepo(t *testing.T, store *db.Store, id string, repo string, path string, now time.Time, pendingMarker bool, badRunner bool) {
	t.Helper()
	payload := workflow.JobPayload{
		Repo: repo, DelegationID: id, WorktreePath: path, ReadOnlyWorktree: true,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if badRunner {
		var envelope map[string]any
		if err := json.Unmarshal(encoded, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope["exec_backend"] = ""
		encoded, err = json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
	}
	seedCLIJob(t, store, db.Job{
		ID: id, Agent: "reader", Type: "ask", State: string(workflow.JobFailed),
		Repo: repo, ParentJobID: "parent", DelegationID: id, Payload: string(encoded),
	}, "seed reclaim resilience candidate")
	backdateJobUpdatedAt(t, store.DatabasePath(), id, now.Add(-73*time.Hour))
	if pendingMarker {
		if err := store.AddJobEvent(context.Background(), db.JobEvent{
			JobID: id, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved",
		}); err != nil {
			t.Fatal(err)
		}
	}
}

type selectiveReclaimManager struct {
	fakeReclaimWorktreeManager
	failPath string
	err      error
}

func (m *selectiveReclaimManager) RemoveWorktreeForce(_ context.Context, path string) error {
	if filepath.Clean(path) == filepath.Clean(m.failPath) {
		return m.err
	}
	m.removed = append(m.removed, path)
	return nil
}

func seedReclaimResilienceCandidate(t *testing.T, store *db.Store, id string, path string, now time.Time, pendingMarker bool, badRunner bool) {
	t.Helper()
	seedReclaimResilienceCandidateForRepo(t, store, id, "owner/repo", path, now, pendingMarker, badRunner)
}

func mutateReclaimCandidateColumn(t *testing.T, store *db.Store, statement string, args ...any) {
	t.Helper()
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(context.Background(), statement, args...); err != nil {
		t.Fatal(err)
	}
}

// TestDelegationReclaimPoisonPillContinues mutation-pins every candidate-local
// failure arm in both reclaim passes. Each subtest orders the poison candidate
// first and proves that the second candidate still reaches real cleanup.
func TestDelegationReclaimPoisonPillContinues(t *testing.T) {
	for _, mode := range []string{"aged", "skipped"} {
		for _, phase := range []string{"get_job", "runner", "reclaim"} {
			t.Run(mode+"/"+phase, func(t *testing.T) {
				resetDelegationReclaimAccountingForTest(t)
				ctx := context.Background()
				home := t.TempDir()
				store := openCLIJobStore(t, home)
				defer store.Close()
				now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
				poisonPath := t.TempDir()
				goodPath := t.TempDir()
				seedReclaimResilienceCandidate(t, store, "a-poison", poisonPath, now, mode == "skipped", phase == "runner")
				seedReclaimResilienceCandidate(t, store, "b-good", goodPath, now, mode == "skipped", false)

				manager := &selectiveReclaimManager{
					fakeReclaimWorktreeManager: fakeReclaimWorktreeManager{branches: map[string]bool{}},
				}
				if mode == "aged" && phase == "reclaim" {
					manager.failPath = poisonPath
					manager.err = errors.New("poison reclaim failure")
				}
				var output bytes.Buffer
				worker := defaultJobWorker(store, &output, home)
				if phase == "get_job" {
					worker.ReclaimJobLookup = func(ctx context.Context, jobID string) (db.Job, error) {
						if jobID == "a-poison" {
							return db.Job{}, errors.New("injected candidate row scan failure")
						}
						return store.GetJob(ctx, jobID)
					}
				}
				factoryCalls := 0
				worker.WorkflowFactory = func(string) workflow.Engine {
					factoryCalls++
					if mode == "skipped" && phase == "reclaim" && factoryCalls == 1 {
						mutateReclaimCandidateColumn(t, store, `UPDATE jobs SET payload = '{' WHERE id = ?`, "a-poison")
					}
					return workflow.Engine{Store: store, DelegationCheckout: t.TempDir(), DelegationWorktrees: manager}
				}

				cand := newTickCandidates(store)
				var err error
				if mode == "aged" {
					err = reclaimAgedTerminalDelegationWorktrees(ctx, worker, "", "", nil, cand, now, 72*time.Hour)
				} else {
					err = reclaimSkippedDelegationWorktrees(ctx, worker, "", "", nil, cand)
				}
				if err != nil {
					t.Fatalf("%s reclaim returned poison error: %v", mode, err)
				}
				if len(manager.removed) != 1 || filepath.Clean(manager.removed[0]) != filepath.Clean(goodPath) {
					t.Fatalf("second candidate was not reclaimed after %s failure: removed=%v", phase, manager.removed)
				}
			})
		}
	}
}

func TestDelegationReclaimRepeatedFailureStopsRelogging(t *testing.T) {
	resetDelegationReclaimAccountingForTest(t)
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	path := t.TempDir()
	seedReclaimResilienceCandidate(t, store, "a-poison", path, now, false, false)
	manager := &selectiveReclaimManager{
		fakeReclaimWorktreeManager: fakeReclaimWorktreeManager{branches: map[string]bool{}},
		failPath:                   path,
		err:                        errors.New("persistent poison"),
	}
	var output bytes.Buffer
	worker := defaultJobWorker(store, &output, home)
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, DelegationCheckout: t.TempDir(), DelegationWorktrees: manager}
	}
	for attempt := 0; attempt < delegationReclaimFailureLogLimit+2; attempt++ {
		if err := reclaimAgedTerminalDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(store), now, 72*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(output.String(), "phase=reclaim"); got != delegationReclaimFailureLogLimit {
		t.Fatalf("failure log count = %d, want %d before suppression:\n%s", got, delegationReclaimFailureLogLimit, output.String())
	}
	if !strings.Contains(output.String(), "further identical-path failures are suppressed") {
		t.Fatalf("missing suppression notice: %s", output.String())
	}
	delegationReclaimAccounting.Lock()
	count := delegationReclaimAccounting.failures[filepath.Clean(path)].count
	delegationReclaimAccounting.Unlock()
	if count != delegationReclaimFailureLogLimit+2 {
		t.Fatalf("recorded path failure count = %d, want %d", count, delegationReclaimFailureLogLimit+2)
	}
}

func TestDelegationReclaimStoreFailureStillAborts(t *testing.T) {
	resetDelegationReclaimAccountingForTest(t)
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	seedReclaimResilienceCandidate(t, store, "a-candidate", t.TempDir(), now, false, false)
	cand := newTickCandidates(store)
	if _, err := cand.agedDelegationReclaimCandidates(ctx, now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	err := reclaimAgedTerminalDelegationWorktrees(ctx, worker, "", "", nil, cand, now, 72*time.Hour)
	if err == nil || !strings.Contains(err.Error(), "path lookup also failed") {
		t.Fatalf("store-wide failure = %v, want fatal path-lookup distinction", err)
	}
}

type destinationReclaimManager struct {
	client   gitutil.Client
	failPath string
}

func (m destinationReclaimManager) AddWorktree(ctx context.Context, branch string, path string, base string) error {
	return m.client.AddWorktree(ctx, branch, path, base)
}
func (m destinationReclaimManager) AddDetachedWorktree(ctx context.Context, path string, ref string) error {
	return m.client.AddDetachedWorktree(ctx, path, ref)
}
func (m destinationReclaimManager) RemoveWorktreeForce(ctx context.Context, path string) error {
	if filepath.Clean(path) == filepath.Clean(m.failPath) {
		return errors.New("injected unreclaimable first destination")
	}
	return m.client.RemoveWorktreeForce(ctx, path)
}
func (m destinationReclaimManager) PruneWorktrees(ctx context.Context) error {
	return m.client.PruneWorktrees(ctx)
}

func TestAgedDelegationReclaimDestinationRemovesSecondDirectory(t *testing.T) {
	resetDelegationReclaimAccountingForTest(t)
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	client := gitutil.NewHostClient(checkout)
	firstPath := filepath.Join(t.TempDir(), "a-poison")
	secondPath := filepath.Join(t.TempDir(), "b-good")
	for _, path := range []string{firstPath, secondPath} {
		if err := client.AddDetachedWorktree(ctx, path, "HEAD"); err != nil {
			t.Fatalf("add detached worktree %s: %v", path, err)
		}
	}
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	seedReclaimResilienceCandidate(t, store, "a-poison", firstPath, now, false, false)
	seedReclaimResilienceCandidate(t, store, "b-good", secondPath, now, false, false)
	worker := defaultJobWorker(store, io.Discard, home)
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{
			Store: store, DelegationCheckout: checkout,
			DelegationWorktrees: destinationReclaimManager{client: client, failPath: firstPath},
		}
	}
	if err := reclaimAgedTerminalDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(store), now, 72*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(firstPath); err != nil {
		t.Fatalf("unreclaimable first directory unexpectedly changed: %v", err)
	}
	if _, err := os.Stat(secondPath); !os.IsNotExist(err) {
		t.Fatalf("second directory still exists after destination reclaim: %v", err)
	}
}

type failingAgedReclaimManager struct {
	fakeReclaimWorktreeManager
	err error
}

func (m *failingAgedReclaimManager) RemoveWorktreeForce(_ context.Context, path string) error {
	m.removed = append(m.removed, path)
	return m.err
}

func TestAgedWorktreeReclaimFailureDoesNotBlockDispatch(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)

	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	worktreePath := t.TempDir()
	seedCLIJob(t, store, db.Job{
		ID: "aged-terminal", Agent: "reader", Type: "ask", State: string(workflow.JobFailed),
		Repo: "owner/repo", ParentJobID: "parent", DelegationID: "aged",
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "owner/repo", DelegationID: "aged", WorktreePath: worktreePath,
		}),
	}, "seed aged terminal delegation")
	backdateJobUpdatedAt(t, store.DatabasePath(), "aged-terminal", now.Add(-73*time.Hour))

	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "queued-after-reclaim", Agent: "audit", Action: "ask",
		Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})

	var stdout bytes.Buffer
	worker := defaultJobWorker(store, &stdout, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	adapter := newWedgeBlockingAdapter("queued-after-reclaim")
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	reclaimErr := errors.New("injected aged worktree reclaim failure")
	manager := &failingAgedReclaimManager{
		fakeReclaimWorktreeManager: fakeReclaimWorktreeManager{branches: map[string]bool{}},
		err:                        reclaimErr,
	}
	baseWorkflowFactory := worker.defaultWorkflow
	worker.WorkflowFactory = func(parentCheckout string) workflow.Engine {
		engine := baseWorkflowFactory(parentCheckout)
		engine.DelegationCheckout = checkout
		engine.DelegationWorktrees = manager
		return engine
	}

	tracker := newInflightJobTracker(ctx)
	t.Cleanup(func() {
		close(adapter.release)
		tracker.drain(os.Stderr, 5*time.Second)
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	if err := runDaemonWorkerTickTracked(ctx, store, worker, 1, false, "owner/repo", "", &stdout, now, tracker, nil); err != nil {
		t.Fatalf("runDaemonWorkerTickTracked returned reclaim error before dispatch: %v", err)
	}
	if !waitForCondition(t, 5*time.Second, adapter.stillBlocked) {
		t.Fatalf("queued job was not admitted after reclaim failure; deliveries=%v log=%q", adapter.deliveredJobs(), stdout.String())
	}
	if !strings.Contains(stdout.String(), reclaimErr.Error()) {
		t.Fatalf("reclaim failure was not logged: %q", stdout.String())
	}
	if !tracker.agedDelegationWorktreeReclaimDue(now) {
		t.Fatal("failed reclaim advanced the success cadence; want immediate retry eligibility")
	}
}

func TestForceRemoveWorktreeUsesOwningCheckout(t *testing.T) {
	ctx := context.Background()
	owner := createDaemonWorkerGitCheckout(t, "main")
	unrelated := createDaemonWorkerGitCheckout(t, "main")
	worktreePath := filepath.Join(t.TempDir(), "foreign-owner")
	if err := (gitutil.NewHostClient(owner)).AddDetachedWorktree(ctx, worktreePath, "HEAD"); err != nil {
		t.Fatalf("create owner worktree: %v", err)
	}

	cmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", worktreePath)
	cmd.Dir = unrelated
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(strings.ToLower(string(output)), "is not a working tree") {
		t.Fatalf("unrelated-checkout control arm err=%v output=%q, want not-a-working-tree failure", err, output)
	}

	if err := (gitutil.NewHostClient(unrelated)).RemoveWorktreeForce(ctx, worktreePath); err != nil {
		t.Fatalf("force remove through owning checkout: %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("foreign-owned worktree remains after recovery: stat err=%v", err)
	}
}

type fixedResultRunner struct {
	result subprocess.Result
	err    error
}

func (r fixedResultRunner) Run(context.Context, string, string, ...string) (subprocess.Result, error) {
	return r.result, r.err
}

func (fixedResultRunner) LookPath(string) (string, error) {
	return "", errors.New("not implemented")
}

func TestGitClientErrorIncludesBoundedStderr(t *testing.T) {
	const cause = "fatal: diagnostic root cause"
	runner := fixedResultRunner{
		result: subprocess.Result{Stderr: "\n  " + cause + strings.Repeat("x", 10_000) + "\n"},
		err:    errors.New("exit status 128"),
	}
	err := (gitutil.NewClient("/repo", runner)).RemoveWorktreeForce(context.Background(), "/worktree")
	if err == nil || !strings.Contains(err.Error(), cause) {
		t.Fatalf("git error = %v, want trimmed stderr cause", err)
	}
	if len(err.Error()) > 5000 {
		t.Fatalf("git error length = %d, want bounded stderr", len(err.Error()))
	}
}
