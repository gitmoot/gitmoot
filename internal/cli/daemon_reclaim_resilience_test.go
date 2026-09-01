package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
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

func managedReclaimTestPath(t *testing.T, home, name string) string {
	t.Helper()
	path := filepath.Join(home, config.DirName, "worktrees", "owner--repo", "delegations", name, "pool-isolation")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func managedDelegationReclaimTestPath(t *testing.T, home, parentID, delegationID string) string {
	t.Helper()
	path, err := workflow.DelegationWorktreePath(filepath.Join(home, config.DirName), "owner/repo", parentID, delegationID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func seedReclaimResilienceCandidateForRepo(t *testing.T, store *db.Store, id string, repo string, path string, now time.Time, pendingMarker bool, badRunner bool) {
	t.Helper()
	payload := workflow.JobPayload{
		Repo: repo, WorktreePath: path, ReadOnlyWorktree: true,
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
	calls    int
}

func (m *selectiveReclaimManager) RemoveWorktreeForce(_ context.Context, path string) error {
	m.calls++
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
				poisonPath := managedReclaimTestPath(t, home, "a-poison")
				goodPath := managedReclaimTestPath(t, home, "b-good")
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
					return workflow.Engine{Store: store, Home: worker.workflowHome(), DelegationCheckout: t.TempDir(), DelegationWorktrees: manager}
				}

				cand := newTickCandidates(store)
				var err error
				if mode == "aged" {
					err = reclaimAgedTerminalDelegationWorktrees(ctx, worker, "", "", nil, cand, now, 72*time.Hour)
				} else {
					err = reclaimSkippedDelegationWorktrees(ctx, worker, "", "", nil, cand, now)
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
	path := filepath.Join(t.TempDir(), "a-poison")
	var output bytes.Buffer
	for attempt := 0; attempt < delegationReclaimFailureLogLimit+2; attempt++ {
		logDelegationReclaimFailure(&output, "aged", "reclaim", "a-poison", path, errors.New("persistent poison"))
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

func TestTerminalTaskReclaimRepeatedFailureStopsRelogging(t *testing.T) {
	resetDelegationReclaimAccountingForTest(t)
	path := filepath.Join(t.TempDir(), "task-poison")
	var output bytes.Buffer
	for range delegationReclaimFailureLogLimit + 2 {
		logTaskWorktreeReclaimFailure(&output, "task-poison", path, errors.New("persistent poison"))
	}
	if got := strings.Count(output.String(), "terminal task worktree reclaim failed"); got != delegationReclaimFailureLogLimit {
		t.Fatalf("failure log count = %d, want %d before suppression:\n%s", got, delegationReclaimFailureLogLimit, output.String())
	}
	if !strings.Contains(output.String(), "further identical-path failures are suppressed") {
		t.Fatalf("missing suppression notice: %s", output.String())
	}
}

func TestTerminalTaskRetentionStopsReloggingAndNamesMalformedOwner(t *testing.T) {
	resetDelegationReclaimAccountingForTest(t)
	path := filepath.Join(t.TempDir(), "task-retained")
	var output bytes.Buffer
	for range delegationReclaimFailureLogLimit + 2 {
		logTaskWorktreeRetention(&output, "task-retained", path, workflow.TaskWorktreeReclaimActiveOwner, "malformed-active")
	}
	if got := strings.Count(output.String(), "terminal task worktree retained"); got != delegationReclaimFailureLogLimit {
		t.Fatalf("retention log count = %d, want %d before suppression:\n%s", got, delegationReclaimFailureLogLimit, output.String())
	}
	if !strings.Contains(output.String(), "malformed_non_final_job=malformed-active") {
		t.Fatalf("retention log omitted malformed owner: %s", output.String())
	}
	if !strings.Contains(output.String(), "further identical retention messages are suppressed") {
		t.Fatalf("missing retention suppression notice: %s", output.String())
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
	firstPath := managedReclaimTestPath(t, home, "a-poison")
	secondPath := managedReclaimTestPath(t, home, "b-good")
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
			Store: store, Home: worker.workflowHome(), DelegationCheckout: checkout,
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

// TestDelegationCleanupUnknownFailureQuarantinesAndSurvivesRestart is the
// durable-budget guard for #1572. Mutant killed: raising or removing
// delegationCleanupRetryBudget leaves the obligation retryable after the third
// unclassified failure, so it remains selectable and this test fails.
func TestDelegationCleanupUnknownFailureQuarantinesAndSurvivesRestart(t *testing.T) {
	const wantBudget = 3
	resetDelegationReclaimAccountingForTest(t)
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	dbPath := store.DatabasePath()
	path := managedReclaimTestPath(t, home, "unknown-poison")
	realNow := time.Now().UTC()
	seedReclaimResilienceCandidate(t, store, "unknown-poison", path, realNow, false, false)
	manager := &selectiveReclaimManager{
		fakeReclaimWorktreeManager: fakeReclaimWorktreeManager{branches: map[string]bool{}},
		failPath:                   path,
		err:                        errors.New("unclassified git worktree refusal"),
	}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: worker.Store, Home: worker.workflowHome(), DelegationCheckout: t.TempDir(), DelegationWorktrees: manager}
	}

	// Keep each persisted next_attempt_at behind wall clock time while advancing
	// the logical attempt time. This exercises the production due-query without a
	// sleep or a test-only selector.
	for attempt := 0; attempt < wantBudget; attempt++ {
		now := realNow.Add(-10*time.Minute + time.Duration(attempt)*2*time.Minute)
		if err := reclaimAgedTerminalDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(store), now, 72*time.Hour); err != nil {
			t.Fatalf("attempt %d: %v", attempt+1, err)
		}
	}

	resourceID := db.CleanupObligationResourceID("unknown-poison", path)
	obligation, err := store.GetCleanupObligation(ctx, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	if obligation.State != db.CleanupObligationQuarantined || obligation.Reason != "unknown" || obligation.AttemptCount != wantBudget {
		t.Fatalf("obligation = %+v, want unknown quarantine at budget %d", obligation, wantBudget)
	}
	if ids, err := store.JobIDsWithAgedTerminalDelegationWorktree(ctx, realNow.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	} else if slices.Contains(ids, "unknown-poison") {
		t.Fatalf("quarantined job remained selectable: %v", ids)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = db.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	worker.Store = store
	obligation, err = store.GetCleanupObligation(ctx, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	if obligation.State != db.CleanupObligationQuarantined || obligation.AttemptCount != wantBudget {
		t.Fatalf("restarted obligation = %+v", obligation)
	}
	if manager.calls != wantBudget {
		t.Fatalf("cleanup calls = %d, want budget %d", manager.calls, wantBudget)
	}
	before := manager.calls
	if err := reclaimAgedTerminalDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(store), realNow, 72*time.Hour); err != nil {
		t.Fatal(err)
	}
	if manager.calls != before {
		t.Fatalf("quarantined cleanup retried after restart: before=%d after=%d", before, manager.calls)
	}
}

func TestDelegationCleanupSkippedFailureConsumesBudget(t *testing.T) {
	const wantBudget = 3
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	path := managedReclaimTestPath(t, home, "skipped-poison")
	realNow := time.Now().UTC()
	seedReclaimResilienceCandidate(t, store, "skipped-poison", path, realNow, true, false)
	manager := &selectiveReclaimManager{
		fakeReclaimWorktreeManager: fakeReclaimWorktreeManager{branches: map[string]bool{}},
		failPath:                   path,
		err:                        errors.New("unclassified skipped cleanup refusal"),
	}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, Home: worker.workflowHome(), DelegationCheckout: t.TempDir(), DelegationWorktrees: manager}
	}
	for attempt := 0; attempt < wantBudget; attempt++ {
		now := realNow.Add(-10*time.Minute + time.Duration(attempt)*2*time.Minute)
		if err := reclaimSkippedDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(store), now); err != nil {
			t.Fatal(err)
		}
	}
	obligation, err := store.GetCleanupObligation(ctx, db.CleanupObligationResourceID("skipped-poison", path))
	if err != nil {
		t.Fatal(err)
	}
	if obligation.State != db.CleanupObligationQuarantined || obligation.AttemptCount != wantBudget || manager.calls != wantBudget {
		t.Fatalf("skipped cleanup obligation=%+v calls=%d", obligation, manager.calls)
	}
}

func TestDelegationCleanupRefusesUncontainedTarget(t *testing.T) {
	resetDelegationReclaimAccountingForTest(t)
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	path := t.TempDir()
	realNow := time.Now().UTC()
	seedReclaimResilienceCandidate(t, store, "unsafe-target", path, realNow, false, false)
	manager := &selectiveReclaimManager{fakeReclaimWorktreeManager: fakeReclaimWorktreeManager{branches: map[string]bool{}}}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, Home: worker.workflowHome(), DelegationCheckout: t.TempDir(), DelegationWorktrees: manager}
	}
	for attempt := 0; attempt < delegationCleanupRetryBudget; attempt++ {
		now := realNow.Add(-10*time.Minute + time.Duration(attempt)*2*time.Minute)
		if err := reclaimAgedTerminalDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(store), now, 72*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if manager.calls != 0 {
		t.Fatalf("uncontained target reached remover %d times", manager.calls)
	}
	obligation, err := store.GetCleanupObligation(ctx, db.CleanupObligationResourceID("unsafe-target", path))
	if err != nil {
		t.Fatal(err)
	}
	if obligation.State != db.CleanupObligationQuarantined || obligation.Reason != "identity_or_containment" {
		t.Fatalf("unsafe obligation = %+v", obligation)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("uncontained target was changed: %v", err)
	}
}

// TestDelegationCleanupAccountingFailureFailsClosed kills the mutant that logs
// a cleanup-obligation persistence failure and reports a candidate-local skip.
func TestDelegationCleanupAccountingFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	path := managedReclaimTestPath(t, home, "accounting-failure")
	payload := workflow.JobPayload{Repo: "other/repo", WorktreePath: path, ReadOnlyWorktree: true}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	job := db.Job{ID: "accounting-failure", Type: "ask", Payload: string(encoded)}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`
CREATE TRIGGER fail_cleanup_obligation_accounting
BEFORE UPDATE ON cleanup_obligations
BEGIN
  SELECT RAISE(ABORT, 'injected cleanup accounting failure');
END;`); err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	if _, _, err := prepareDelegationCleanup(ctx, worker, "aged", job, path, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "injected cleanup accounting failure") {
		t.Fatalf("prepareDelegationCleanup error = %v, want persisted accounting failure", err)
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
	worktreePath := managedReclaimTestPath(t, home, "aged-terminal")
	seedCLIJob(t, store, db.Job{
		ID: "aged-terminal", Agent: "reader", Type: "ask", State: string(workflow.JobFailed),
		Repo: "owner/repo",
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo: "owner/repo", WorktreePath: worktreePath, ReadOnlyWorktree: true,
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
	if tracker.worktreeReclaimDue(now.Add(worktreeReclaimInterval - time.Nanosecond)) {
		t.Fatal("failed reclaim remained immediately due; want bounded retry cadence")
	}
	if !tracker.worktreeReclaimDue(now.Add(worktreeReclaimInterval)) {
		t.Fatal("failed reclaim was not eligible at the next bounded cadence")
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

func TestReclaimTerminalTaskWorktreesRemovesCleanDismissedTask(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard, home)
	livenessCalls := 0
	baseWorkflowFactory := worker.defaultWorkflow
	worker.WorkflowFactory = func(checkout string) workflow.Engine {
		engine := baseWorkflowFactory(checkout)
		engine.WorktreeLiveness = func(string) (bool, bool) {
			livenessCalls++
			return false, true
		}
		return engine
	}
	path, err := workflow.TaskWorktreePath(worker.workflowHome(), "owner/repo", "adhoc-finished")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll worktree parent: %v", err)
	}
	if err := (gitutil.NewHostClient(checkout)).AddWorktree(ctx, "adhoc-finished", path, "HEAD"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "adhoc-finished",
		RepoFullName: "owner/repo",
		State:        string(workflow.TaskDismissed),
		Branch:       "adhoc-finished",
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	if err := reclaimTerminalTaskWorktrees(ctx, worker, "owner/repo", "root-job", nil, newTickCandidates(store), io.Discard); err != nil {
		t.Fatalf("session-scoped reclaimTerminalTaskWorktrees: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session-scoped daemon removed terminal task worktree: %v", err)
	}
	if livenessCalls != 0 {
		t.Fatalf("session-scoped daemon ran %d task worktree liveness checks, want 0", livenessCalls)
	}
	if err := reclaimTerminalTaskWorktrees(ctx, worker, "owner/repo", "", nil, newTickCandidates(store), io.Discard); err != nil {
		t.Fatalf("reclaimTerminalTaskWorktrees: %v", err)
	}
	if livenessCalls != 2 {
		t.Fatalf("worktree liveness checks = %d, want 2", livenessCalls)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("terminal task worktree remains: stat err=%v", err)
	}
	task, err := store.GetTask(ctx, "adhoc-finished")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.WorktreePath != "" || task.Branch != "adhoc-finished" {
		t.Fatalf("task after reclaim = %+v, want empty path and preserved branch", task)
	}
}

// The candidate list is host-wide, so every repo's pass walks all of it. A pass
// that completes for a small repo must not reset a large repo's rotation window,
// which is what a single shared resume marker did: repo A's tail never ran.
func TestTerminalTaskWorktreeReclaimResumeIsPerRepo(t *testing.T) {
	t.Cleanup(func() {
		setTerminalTaskWorktreeReclaimResume("owner/big", "")
		setTerminalTaskWorktreeReclaimResume("owner/small", "")
	})
	ids := []string{"a01", "a02", "a03", "b01"}

	setTerminalTaskWorktreeReclaimResume("owner/big", "a03")
	// The small repo's pass walks the whole list and completes, clearing only its
	// own marker.
	setTerminalTaskWorktreeReclaimResume("owner/small", "")

	rotated := rotateTerminalTaskWorktreeCandidates("owner/big", ids)
	if len(rotated) == 0 || rotated[0] != "a03" {
		t.Fatalf("big repo window = %v, want it to resume at a03", rotated)
	}
	if got := rotateTerminalTaskWorktreeCandidates("owner/small", ids); got[0] != "a01" {
		t.Fatalf("small repo window = %v, want it to start at a01", got)
	}
	// A completed pass for the big repo clears its own marker and starts over.
	setTerminalTaskWorktreeReclaimResume("owner/big", "")
	if got := rotateTerminalTaskWorktreeCandidates("owner/big", ids); got[0] != "a01" {
		t.Fatalf("big repo window after completion = %v, want it to start at a01", got)
	}
}

// The scheduler must only close an obligation when the engine explicitly
// reports a completed removal. Path absence is ambiguous: a fix clone may have
// been set aside, and no sibling scan can prove an operator intended deletion.
func TestFinishDelegationCleanupAttemptDoesNotInferRemovalFromAbsence(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Now().UTC()
	path := filepath.Join(home, "worktrees", "owner--repo", "fix", "job-1")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll parent: %v", err)
	}
	quarantine := path + ".ttl-reclaiming-0123456789abcdef"
	if err := os.MkdirAll(quarantine, 0o755); err != nil {
		t.Fatalf("MkdirAll quarantine: %v", err)
	}

	if err := finishDelegationCleanupAttempt(ctx, worker, "job-1", path, false, now); err != nil {
		t.Fatalf("finishDelegationCleanupAttempt: %v", err)
	}
	obligation, err := store.GetCleanupObligation(ctx, db.CleanupObligationResourceID("job-1", path))
	if err != nil {
		t.Fatalf("GetCleanupObligation: %v", err)
	}
	if obligation.State == db.CleanupObligationRemoved {
		t.Fatalf("obligation = %+v, want it still open while a quarantine survives", obligation)
	}

	// Even with the sibling gone, absence alone is not durable removal evidence.
	if err := os.RemoveAll(quarantine); err != nil {
		t.Fatalf("RemoveAll quarantine: %v", err)
	}
	if err := finishDelegationCleanupAttempt(ctx, worker, "job-1", path, false, now); err != nil {
		t.Fatalf("finishDelegationCleanupAttempt after quarantine removal: %v", err)
	}
	obligation, err = store.GetCleanupObligation(ctx, db.CleanupObligationResourceID("job-1", path))
	if err != nil {
		t.Fatalf("GetCleanupObligation: %v", err)
	}
	if obligation.State == db.CleanupObligationRemoved {
		t.Fatalf("obligation = %+v, want it open without an explicit reclaimed outcome", obligation)
	}
}

// This enters through the daemon's skipped-cleanup pass. Re-enabling deletion in
// cleanupFixWorktree loses owned.txt and fails this production-path regression.
func TestReclaimSkippedFixClonePreservesStandaloneObjectDatabase(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	worker := defaultJobWorker(store, io.Discard, home)
	jobID := "fix-skipped"
	path, err := workflow.FixWorktreePath(worker.workflowHome(), "owner/repo", jobID)
	if err != nil {
		t.Fatalf("FixWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "owned.txt"), []byte("only copy\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	payload, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/repo", Branch: "feature/fix", WorktreePath: path, FixWorktree: true,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	seedCLIJob(t, store, db.Job{
		ID: jobID, Agent: "fixer", Type: "implement", State: string(workflow.JobSucceeded),
		Repo: "owner/repo", Payload: string(payload),
	}, "seed terminal fix")
	if err := store.AddJobEvent(ctx, db.JobEvent{
		JobID: jobID, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved",
	}); err != nil {
		t.Fatalf("AddJobEvent: %v", err)
	}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{Store: store, Home: worker.workflowHome()}
	}

	if err := reclaimSkippedDelegationWorktrees(ctx, worker, "", "", nil, newTickCandidates(store), time.Now().UTC()); err != nil {
		t.Fatalf("reclaimSkippedDelegationWorktrees: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(path, "owned.txt")); err != nil || string(got) != "only copy\n" {
		t.Fatalf("daemon cleanup did not preserve clone bytes: %q, %v", got, err)
	}
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == "delegation_worktree_removed" {
			t.Fatalf("daemon emitted removal for retained fix clone: %+v", events)
		}
	}
	obligation, err := store.GetCleanupObligation(ctx, db.CleanupObligationResourceID(jobID, path))
	if err != nil {
		t.Fatalf("GetCleanupObligation: %v", err)
	}
	if obligation.State == db.CleanupObligationRemoved {
		t.Fatalf("obligation = %+v, want retained clone visible", obligation)
	}
}

func TestSkippedReclaimPersistsFairnessPastFilteredHostWindow(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Now().UTC()

	for i := 0; i < 256; i++ {
		id := fmt.Sprintf("a-disabled-%03d", i)
		path, err := workflow.DelegationWorktreePath(worker.workflowHome(), "owner/disabled", "parent", id, 0)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(workflow.JobPayload{
			Repo: "owner/disabled", DelegationID: id, WorktreePath: path, ReadOnlyWorktree: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		seedCLIJob(t, store, db.Job{
			ID: id, Agent: "reader", Type: "ask", State: string(workflow.JobFailed),
			Repo: "owner/disabled", ParentJobID: "parent", DelegationID: id, Payload: string(payload),
		}, "seed filtered reclaim candidate")
		if err := store.AddJobEvent(ctx, db.JobEvent{
			JobID: id, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved",
		}); err != nil {
			t.Fatal(err)
		}
	}
	targetID := "z-target"
	targetPath, err := workflow.DelegationWorktreePath(worker.workflowHome(), "owner/target", "parent", targetID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetPath, 0o755); err != nil {
		t.Fatal(err)
	}
	targetPayload, err := json.Marshal(workflow.JobPayload{
		Repo: "owner/target", DelegationID: targetID, WorktreePath: targetPath, ReadOnlyWorktree: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	seedCLIJob(t, store, db.Job{
		ID: targetID, Agent: "reader", Type: "ask", State: string(workflow.JobFailed),
		Repo: "owner/target", ParentJobID: "parent", DelegationID: targetID, Payload: string(targetPayload),
	}, "seed target reclaim candidate")
	if err := store.AddJobEvent(ctx, db.JobEvent{
		JobID: targetID, Kind: "delegation_worktree_cleanup_skipped", Message: "preserved",
	}); err != nil {
		t.Fatal(err)
	}
	manager := &fakeReclaimWorktreeManager{branches: map[string]bool{}}
	worker.WorkflowFactory = func(string) workflow.Engine {
		return workflow.Engine{
			Store: store, Home: worker.workflowHome(), DelegationCheckout: t.TempDir(), DelegationWorktrees: manager,
		}
	}

	if err := reclaimSkippedDelegationWorktrees(ctx, worker, "owner/target", "", nil, newTickCandidates(store), now); err != nil {
		t.Fatalf("first reclaim pass: %v", err)
	}
	if len(manager.removed) != 0 {
		t.Fatalf("first pass removed %v, want target beyond the bounded window", manager.removed)
	}
	next, err := store.JobIDsWithPendingDelegationWorktreeReclaim(ctx)
	if err != nil {
		t.Fatalf("query next persistent window: %v", err)
	}
	if len(next) != 1 || next[0] != targetID {
		t.Fatalf("next persistent window = %v, want [%s]", next, targetID)
	}
}

// Lock contention is not failure. Counting it spends the three-attempt retry
// budget and quarantines an obligation that would have succeeded once the lock
// cleared, so a contended pass must DEFER instead of recording a failure.
func TestDeferDelegationCleanupContentionKeepsRetryBudget(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	worker := defaultJobWorker(store, io.Discard, home)
	now := time.Now().UTC()
	path := filepath.Join(home, "worktrees", "owner--repo", "fixes", "job-contended")
	contended := fmt.Errorf("lock checkout for TTL fix clone reclaim: %w",
		workflow.BlockedError{Reason: "This checkout is already being mutated by another Gitmoot task."})
	if !delegationCleanupContended(contended) {
		t.Fatal("wrapped BlockedError was not classified as contention")
	}
	if delegationCleanupContended(errors.New("remove failed")) {
		t.Fatal("an ordinary failure was classified as contention")
	}

	for attempt := 0; attempt < delegationCleanupRetryBudget+1; attempt++ {
		if err := deferDelegationCleanupContention(ctx, worker, "aged", "job-contended", path, contended, now); err != nil {
			t.Fatalf("deferDelegationCleanupContention: %v", err)
		}
	}
	obligation, err := store.GetCleanupObligation(ctx, db.CleanupObligationResourceID("job-contended", path))
	if err != nil {
		t.Fatalf("GetCleanupObligation: %v", err)
	}
	if obligation.State != db.CleanupObligationRetryable {
		t.Fatalf("obligation state = %q, want retryable after repeated contention", obligation.State)
	}
	if obligation.AttemptCount != 0 {
		t.Fatalf("attempt count = %d, want 0: contention must not spend the retry budget", obligation.AttemptCount)
	}
	if obligation.Reason != db.CleanupReasonCheckoutLock {
		t.Fatalf("reason = %q, want %q", obligation.Reason, db.CleanupReasonCheckoutLock)
	}
}

func TestReclaimTerminalTaskWorktreesNamesMalformedGlobalPin(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard, home)
	path, err := workflow.TaskWorktreePath(worker.workflowHome(), "owner/repo", "globally-pinned")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll worktree parent: %v", err)
	}
	if err := (gitutil.NewHostClient(checkout)).AddWorktree(ctx, "globally-pinned", path, "HEAD"); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "globally-pinned",
		RepoFullName: "owner/repo",
		State:        string(workflow.TaskDismissed),
		Branch:       "globally-pinned",
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "malformed-global-pin", Agent: "agent", Type: "implement", State: "queued", Payload: `not json`,
	}, db.JobEvent{Kind: "queued", Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent malformed global pin: %v", err)
	}

	var output bytes.Buffer
	if err := reclaimTerminalTaskWorktrees(ctx, worker, "owner/repo", "", nil, newTickCandidates(store), &output); err != nil {
		t.Fatalf("reclaimTerminalTaskWorktrees: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("globally pinned worktree was removed: %v", err)
	}
	if !strings.Contains(output.String(), "classification=active_owner") ||
		!strings.Contains(output.String(), "malformed_non_final_job=malformed-global-pin") {
		t.Fatalf("malformed global pin was not explained: %s", output.String())
	}
}

func TestTerminalTaskReclaimMemoizesMalformedOwnerAcrossRepos(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	if err := store.UpsertTask(ctx, db.Task{
		ID: "cross-repo-candidate", RepoFullName: "owner/other", State: string(workflow.TaskDismissed), WorktreePath: "/worktrees/cross-repo-candidate",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "cross-repo-malformed", Agent: "agent", Type: "implement", State: "queued", Payload: `not json`,
	}, db.JobEvent{Kind: "queued", Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	counter := &countingCandidateStore{inner: store}
	candidates := newTickCandidates(counter)
	worker := defaultJobWorker(store, io.Discard, home)
	for _, repo := range []string{"owner/one", "owner/two"} {
		if err := reclaimTerminalTaskWorktrees(ctx, worker, repo, "", nil, candidates, io.Discard); err != nil {
			t.Fatalf("reclaimTerminalTaskWorktrees(%s): %v", repo, err)
		}
	}
	if got := atomic.LoadInt32(&counter.taskReclaim); got != 1 {
		t.Fatalf("terminal task query ran %d times across repos, want 1", got)
	}
	if got := atomic.LoadInt32(&counter.malformedOwner); got != 1 {
		t.Fatalf("malformed owner query ran %d times across repos, want 1", got)
	}
}

func TestRemoveWorktreeUsesOwningCheckout(t *testing.T) {
	ctx := context.Background()
	owner := createDaemonWorkerGitCheckout(t, "main")
	unrelated := createDaemonWorkerGitCheckout(t, "main")
	worktreePath := filepath.Join(t.TempDir(), "foreign-owner-clean")
	if err := (gitutil.NewHostClient(owner)).AddDetachedWorktree(ctx, worktreePath, "HEAD"); err != nil {
		t.Fatalf("create owner worktree: %v", err)
	}

	cmd := exec.CommandContext(ctx, "git", "worktree", "remove", worktreePath)
	cmd.Dir = unrelated
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(strings.ToLower(string(output)), "is not a working tree") {
		t.Fatalf("unrelated-checkout control arm err=%v output=%q, want not-a-working-tree failure", err, output)
	}

	if err := (gitutil.NewHostClient(unrelated)).RemoveWorktree(ctx, worktreePath); err != nil {
		t.Fatalf("remove through owning checkout: %v", err)
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
