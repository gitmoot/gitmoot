package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2531: a job keeps the repo name it ran under, so the cleanup obligations
// of a repo that was removed or renamed belong to no registered repo. No repo
// sweep visited them, and they stayed retryable forever.
const unregisteredCleanupRepo = "gone/repo"

// seedUnregisteredCleanupOwner seeds a final owner of a delegation worktree in
// unregisteredCleanupRepo, aged past the reclaim TTL, with the due retryable
// obligation the live rows have.
func seedUnregisteredCleanupOwner(t *testing.T, store *db.Store, id string, job db.Job, payload workflow.JobPayload) {
	t.Helper()
	ctx := context.Background()
	job.ID, job.Repo = id, unregisteredCleanupRepo
	payload.Repo = unregisteredCleanupRepo
	job.Payload = mustJobPayload(t, payload)
	seedCLIJob(t, store, job, "seed unregistered cleanup owner")
	backdateJobUpdatedAt(t, store.DatabasePath(), id, time.Now().Add(-100*24*time.Hour))
	past := time.Now().Add(-time.Hour)
	if _, err := store.DeferCleanupObligation(ctx, id, payload.WorktreePath, db.CleanupReasonTerminalDeferred, past, past); err != nil {
		t.Fatal(err)
	}
}

func unregisteredCleanupSeat(t *testing.T, home, id string) string {
	t.Helper()
	path, err := workflow.DelegationWorktreePath(filepath.Join(home, config.DirName), unregisteredCleanupRepo, id, "readonly-seat", 0)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func requireCleanupObligationRemoved(t *testing.T, store *db.Store, id, path, wantEvent string) {
	t.Helper()
	ctx := context.Background()
	resource := db.CleanupObligationResourceID(id, path)
	obligation, err := store.GetCleanupObligation(ctx, resource)
	if err != nil || obligation.State != db.CleanupObligationRemoved {
		t.Fatalf("obligation = %+v %v, want removed", obligation, err)
	}
	receipt, err := store.GetCleanupReceipt(ctx, resource, obligation.LifecycleGeneration)
	if err != nil || receipt.RemovedAt == "" {
		t.Fatalf("receipt = %+v %v, want a completed receipt", receipt, err)
	}
	events, err := store.ListJobEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(event db.JobEvent) bool { return event.Kind == wantEvent }) {
		t.Fatalf("no %s event: %+v", wantEvent, events)
	}
}

// A worktree that is already gone has no registered checkout and no .git
// pointer left; its obligation is closed as removed, with the receipt and
// event an ordinary removal records.
func TestReclaimLoopClosesAbsentWorktreeOfUnregisteredRepo(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	id := "local-ask-gone-1"
	path := unregisteredCleanupSeat(t, home, id)
	// A failed read-only ask, like most live rows: terminal labels alone do
	// not authorize deleting its worktree, but there is nothing left to delete.
	seedUnregisteredCleanupOwner(t, store, id, db.Job{
		Agent: "reader", Type: "ask", State: string(workflow.JobFailed),
	}, workflow.JobPayload{
		WorktreePath: path, ReadOnlyWorktree: true, ReadOnlySeat: true,
		Result: &workflow.AgentResult{Decision: "failed"},
	})
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture path exists: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	tracker := newInflightJobTracker(ctx)
	var stdout bytes.Buffer
	runWorktreeReclaimLoopPass(ctx, store, worker, tracker, "", fleetReclaimRepos, &stdout, time.Now().UTC(), time.Time{})
	requireCleanupObligationRemoved(t, store, id, path, "delegation_worktree_reclaimed_ttl")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the pass created the absent path: %v", err)
	}
}

// A worktree that still exists is removed through the checkout owning it,
// found from its .git pointer, with the ordinary proofs: here the checkout is
// registered under the repo's new name (a rename), so that repo's jobs are
// kept out during the attempt too.
func TestReclaimLoopRemovesWorktreeOfUnregisteredRepoThroughOwningCheckout(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "renamed/repo", checkout)
	head := strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "HEAD"))
	id := "local-review-gone-1"
	path := unregisteredCleanupSeat(t, home, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, checkout, "worktree", "add", "--detach", path, "HEAD")
	// A cancelled pinned PR review, like the live rows still on disk: it is
	// rebuildable, so aged cleanup may remove it.
	seedUnregisteredCleanupOwner(t, store, id, db.Job{
		Agent: "reviewer", Type: "review", State: string(workflow.JobCancelled),
	}, workflow.JobPayload{
		WorktreePath: path, ReadOnlyWorktree: true, ReadOnlySeat: true, PullRequest: 7, HeadSHA: head,
	})
	// The fixture owns every user of its worktree, so it answers liveness for
	// that path; an unrelated host process whose cwd an unprivileged run
	// cannot read is not part of this scenario. Any other path keeps the
	// strict scan.
	owned, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	tracker := newInflightJobTracker(ctx)
	var mu sync.Mutex
	var checkouts []string
	var renamedRepoStarted bool
	worker.WorkflowFactory = func(engineCheckout string) workflow.Engine {
		mu.Lock()
		checkouts = append(checkouts, engineCheckout)
		mu.Unlock()
		if admitted, _ := tracker.beginWithinLane(0, false, "renamed-probe", "renamed/repo", "repo:renamed/repo", ""); admitted {
			renamedRepoStarted = true
			tracker.end("renamed-probe")
		}
		engine := worker.defaultWorkflow(engineCheckout)
		engine.WorktreeLiveness = func(candidate string) (bool, bool) {
			if filepath.Clean(candidate) == owned || filepath.Clean(candidate) == filepath.Clean(path) {
				return false, true
			}
			return workflow.WorktreeLiveness(candidate)
		}
		return engine
	}
	var stdout bytes.Buffer
	runWorktreeReclaimLoopPass(ctx, store, worker, tracker, "", fleetReclaimRepos, &stdout, time.Now().UTC(), time.Time{})
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the worktree was not removed: %v; log=%q", err, stdout.String())
	}
	requireCleanupObligationRemoved(t, store, id, path, "delegation_worktree_reclaimed_ttl")
	if out := runGitOutput(t, checkout, "worktree", "list", "--porcelain"); strings.Contains(out, path) {
		t.Fatalf("the owning checkout still lists the worktree:\n%s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(checkouts) == 0 || !slices.Contains(checkouts, checkout) {
		t.Fatalf("engine checkouts = %v, want the owning checkout %s", checkouts, checkout)
	}
	if renamedRepoStarted {
		t.Fatal("a job of the repo registered on the owning checkout started during the attempt")
	}
}

// With no owning checkout to prove (the .git pointer is gone), nothing is
// removed and the obligation is quarantined once the retry budget is spent,
// with the reason recorded.
func TestReclaimLoopQuarantinesUnregisteredRepoWorktreeWithoutProvableOwner(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	head := strings.Repeat("a", 40)
	id := "local-review-orphan-1"
	path := unregisteredCleanupSeat(t, home, id)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "kept.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedUnregisteredCleanupOwner(t, store, id, db.Job{
		Agent: "reviewer", Type: "review", State: string(workflow.JobCancelled),
	}, workflow.JobPayload{
		WorktreePath: path, ReadOnlyWorktree: true, ReadOnlySeat: true, PullRequest: 7, HeadSHA: head,
	})
	worker := defaultJobWorker(store, io.Discard, home)
	tracker := newInflightJobTracker(ctx)
	resource := db.CleanupObligationResourceID(id, path)
	var stdout bytes.Buffer
	for pass := range delegationCleanupRetryBudget {
		runWorktreeReclaimLoopPass(ctx, store, worker, tracker, "", fleetReclaimRepos, &stdout, time.Now().UTC(), time.Time{})
		obligation, err := store.GetCleanupObligation(ctx, resource)
		if err != nil {
			t.Fatal(err)
		}
		if obligation.AttemptCount != pass+1 {
			t.Fatalf("pass %d: obligation = %+v, want %d attempts", pass, obligation, pass+1)
		}
		backdate := time.Now().Add(-time.Minute)
		if obligation.State == db.CleanupObligationRetryable {
			if _, err := store.DeferCleanupObligation(ctx, id, path, obligation.Reason, backdate, backdate); err != nil {
				t.Fatal(err)
			}
		}
	}
	obligation, err := store.GetCleanupObligation(ctx, resource)
	if err != nil || obligation.State != db.CleanupObligationQuarantined || obligation.Reason != db.CleanupReasonIdentityOrContainment ||
		!strings.Contains(obligation.LastError, "no checkout owning") {
		t.Fatalf("obligation = %+v %v, want quarantined for an unprovable owner", obligation, err)
	}
	if _, err := os.Stat(filepath.Join(path, "kept.txt")); err != nil {
		t.Fatalf("content without a provable owner was deleted: %v", err)
	}
}

// `job cleanup inspect` derives the decision for a job of an unregistered
// repo instead of failing on the repo lookup.
func TestJobCleanupInspectWorksForUnregisteredRepo(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	head := strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "HEAD"))
	present := "local-review-present-1"
	presentPath := unregisteredCleanupSeat(t, home, present)
	if err := os.MkdirAll(filepath.Dir(presentPath), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, checkout, "worktree", "add", "--detach", presentPath, "HEAD")
	seedUnregisteredCleanupOwner(t, store, present, db.Job{
		Agent: "reviewer", Type: "review", State: string(workflow.JobCancelled),
	}, workflow.JobPayload{
		WorktreePath: presentPath, ReadOnlyWorktree: true, ReadOnlySeat: true, PullRequest: 7, HeadSHA: head,
	})
	absent := "local-review-absent-1"
	absentPath := unregisteredCleanupSeat(t, home, absent)
	seedUnregisteredCleanupOwner(t, store, absent, db.Job{
		Agent: "reviewer", Type: "review", State: string(workflow.JobCancelled),
	}, workflow.JobPayload{
		WorktreePath: absentPath, ReadOnlyWorktree: true, ReadOnlySeat: true, PullRequest: 7, HeadSHA: head,
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	inspect := func(id string) workflow.CleanupInspection {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := runJobCleanupResource("inspect", []string{"--home", home, "--json", id}, &stdout, &stderr); code != 0 {
			t.Fatalf("inspect %s exit = %d stderr=%q", id, code, stderr.String())
		}
		var decision workflow.CleanupInspection
		if err := json.Unmarshal(stdout.Bytes(), &decision); err != nil {
			t.Fatalf("decode inspect %s: %v\n%s", id, err, stdout.String())
		}
		if decision.OwnerJobID != id || decision.Repo != unregisteredCleanupRepo || decision.ResourceID == "" {
			t.Fatalf("inspect %s = %+v", id, decision)
		}
		return decision
	}
	// The present worktree is inspected through its owning checkout: git
	// there proves it, so no checkout or git identity refusal.
	decision := inspect(present)
	for _, reason := range []string{"checkout_identity_unknown", "git_identity_unknown"} {
		if slices.Contains(decision.ReasonCodes, reason) {
			t.Fatalf("present worktree refused %s: %+v", reason, decision)
		}
	}
	// The absent one has no owner to inspect it through.
	if decision := inspect(absent); !slices.Contains(decision.ReasonCodes, "checkout_identity_unknown") {
		t.Fatalf("absent worktree decision = %+v", decision)
	}
}
