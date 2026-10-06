package cli

import (
	"bytes"
	"context"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestRetryRemovedReadonlySeatReallocatesExactHead is the #2286 lifecycle:
// a blocked remote review whose dispatch seat was removed, with a review task
// id and no review round or reviewers. gitmoot job retry must drop the dead
// path, keep the exact head and the ownership marker, and the existing
// allocator must recreate the readonly-seat from the registered checkout.
func TestRetryRemovedReadonlySeatReallocatesExactHead(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	shared, head, _ := readonlyReviewWorktreeGitCheckout(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", shared)
	paths := config.PathsForHome(home)
	const jobID = "local-review-removed-seat"
	seat, err := workflow.DelegationWorktreePath(paths.Home, "owner/repo", jobID, "readonly-seat", 0)
	if err != nil {
		t.Fatalf("DelegationWorktreePath: %v", err)
	}
	if err := os.RemoveAll(seat); err != nil {
		t.Fatalf("remove stale seat: %v", err)
	}
	// Incident shape: no review_round, no reviewers.
	payload := `{"repo":"owner/repo","branch":"feature/review","pull_request":1101,"task_id":"review-pr-1101","read_only_worktree":true,"read_only_seat":true,"exec_backend":"remote","worktree_path":"` + seat + `","head_sha":"` + head + `"}`
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: jobID, Agent: "reviewer", Type: "review", State: string(workflow.JobBlocked), Payload: payload,
	}, db.JobEvent{Kind: string(workflow.JobBlocked), Message: "blocked"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runJobRetry([]string{jobID, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job retry exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob after retry: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("state after retry = %q, want queued", job.State)
	}
	parsed, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload: %v", err)
	}
	if parsed.WorktreePath != "" {
		t.Fatalf("retry kept removed seat %q", parsed.WorktreePath)
	}
	if parsed.HeadSHA != head {
		t.Fatalf("retry HeadSHA = %q, want %s", parsed.HeadSHA, head)
	}
	if !parsed.ReadOnlyWorktree {
		t.Fatal("retry cleared ReadOnlyWorktree; the allocator needs that ownership marker")
	}
	if parsed.ReviewRound != "" || len(parsed.Reviewers) != 0 {
		t.Fatalf("fixture gained round/reviewers: %+v", parsed)
	}

	worker := defaultJobWorker(store, io.Discard, home)
	prepared, err := worker.prepareNativeReviewWorktreeForRunner(ctx, job, parsed, subprocess.ExecRunner{})
	if err != nil {
		t.Fatalf("prepareNativeReviewWorktreeForRunner: %v", err)
	}
	if prepared.WorktreePath != seat {
		t.Fatalf("allocated %q, want canonical readonly-seat %q", prepared.WorktreePath, seat)
	}
	if prepared.WorktreePath == shared {
		t.Fatalf("allocator returned the registered checkout %s", shared)
	}
	if prepared.HeadSHA != head {
		t.Fatalf("allocated payload head = %q, want %s", prepared.HeadSHA, head)
	}
	info, err := os.Stat(prepared.WorktreePath)
	if err != nil || !info.IsDir() {
		t.Fatalf("replacement seat stat = %v err=%v", info, err)
	}
	if got := readonlyWorktreeHead(t, prepared.WorktreePath); got != head {
		t.Fatalf("replacement seat HEAD = %s, want exact head %s", got, head)
	}
	checkout, err := worker.checkoutForJob(ctx, job, prepared, runtime.Agent{}, subprocess.ExecRunner{})
	if err != nil {
		t.Fatalf("checkoutForJob: %v", err)
	}
	if checkout != seat {
		t.Fatalf("checkoutForJob = %q, want replacement seat %q", checkout, seat)
	}
}

// TestRetryPresentReadonlySeatIsConsumed keeps a still-present owned review
// seat and runs checkout against that path. Recreating it would fail with
// "already exists". No review round or reviewers, matching the incident shape.
func TestRetryPresentReadonlySeatIsConsumed(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	shared, head, _ := readonlyReviewWorktreeGitCheckout(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", shared)
	paths := config.PathsForHome(home)
	const jobID = "local-review-present-seat"
	seat, err := workflow.DelegationWorktreePath(paths.Home, "owner/repo", jobID, "readonly-seat", 0)
	if err != nil {
		t.Fatalf("DelegationWorktreePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(seat), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, shared, "worktree", "add", "--detach", seat, head)
	payload := `{"repo":"owner/repo","branch":"feature/review","pull_request":1101,"task_id":"review-pr-1101","read_only_worktree":true,"read_only_seat":true,"exec_backend":"remote","worktree_path":"` + seat + `","head_sha":"` + head + `"}`
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: jobID, Agent: "reviewer", Type: "review", State: string(workflow.JobBlocked), Payload: payload,
	}, db.JobEvent{Kind: string(workflow.JobBlocked), Message: "blocked"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runJobRetry([]string{jobID, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job retry exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.WorktreePath != seat || parsed.HeadSHA != head || !parsed.ReadOnlyWorktree {
		t.Fatalf("present seat after retry = %+v, want path %s head %s", parsed, seat, head)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	prepared, err := worker.prepareNativeReviewWorktreeForRunner(ctx, job, parsed, subprocess.ExecRunner{})
	if err != nil {
		t.Fatalf("prepare must not recreate a present seat: %v", err)
	}
	if prepared.WorktreePath != seat {
		t.Fatalf("prepare path = %q, want existing seat %q", prepared.WorktreePath, seat)
	}
	checkout, err := worker.checkoutForJob(ctx, job, prepared, runtime.Agent{}, subprocess.ExecRunner{})
	if err != nil {
		t.Fatalf("checkoutForJob: %v", err)
	}
	if checkout != seat {
		t.Fatalf("checkoutForJob = %q, want existing seat %q", checkout, seat)
	}
	if got := readonlyWorktreeHead(t, checkout); got != head {
		t.Fatalf("consumed seat HEAD = %s, want %s", got, head)
	}
}
