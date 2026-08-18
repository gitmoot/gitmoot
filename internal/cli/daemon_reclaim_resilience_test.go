package cli

import (
	"bytes"
	"context"
	"errors"
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
