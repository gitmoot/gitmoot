package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestDispatchReadOnlyWorktreeEligible pins the scope split: every review gets a
// per-job exact-head worktree, while only a background taskless ask gets the
// existing committed-tip isolation. Implement, foreground ask, and task-bearing
// ask behavior stays unchanged.
func TestDispatchReadOnlyWorktreeEligible(t *testing.T) {
	cases := []struct {
		name string
		req  localAgentDispatchRequest
		want bool
	}{
		{"background ask", localAgentDispatchRequest{Background: true, Action: "ask"}, true},
		{"background review no task", localAgentDispatchRequest{Background: true, Action: "review"}, true},
		{"foreground review", localAgentDispatchRequest{Background: false, Action: "review"}, true},
		{"foreground ask untouched", localAgentDispatchRequest{Background: false, Action: "ask"}, false},
		{"background implement untouched", localAgentDispatchRequest{Background: true, Action: "implement"}, false},
		{"background run untouched", localAgentDispatchRequest{Background: true, Action: "run"}, false},
		{"background ask with task worktree", localAgentDispatchRequest{Background: true, Action: "ask", TaskID: "t1"}, false},
		{"background review with task identity", localAgentDispatchRequest{Background: true, Action: "review", TaskID: "review-pr-1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dispatchReadOnlyWorktreeEligible(tc.req); got != tc.want {
				t.Fatalf("dispatchReadOnlyWorktreeEligible = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDispatchBackgroundAskAllocatesReadOnlyWorktree drives the REAL dispatch path
// for a background ask (a moot seat / chat-task / autorespond / `agent ask
// --background` shape) and proves the #739 fix: the enqueued job is born with a
// detached committed-tip worktree, so queuedJobCheckoutKey keys it off
// worktree:<path> and it runs beside same-repo seats instead of serializing on the
// shared repo:<repo> key.
func TestDispatchBackgroundAskAllocatesReadOnlyWorktree(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	// Shell runtime, ask-only: a background dispatch enqueues and returns before any
	// delivery, so the command body is irrelevant here.
	seedDaemonWorkerAgent(t, store, "responder", runtime.ShellRuntime, "printf '%s' '{}'", []string{"ask"}, "owner/repo")

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag:     "owner/repo",
		Agent:        "responder",
		Action:       "ask",
		Instructions: "hello",
		Background:   true,
		Home:         home,
	})
	if err != nil {
		t.Fatalf("dispatchLocalAgentJob returned error: %v", err)
	}
	job, err := store.GetJob(ctx, out.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload: %v", err)
	}
	if strings.TrimSpace(payload.WorktreePath) == "" {
		t.Fatal("background ask payload has no WorktreePath; expected a dispatch-time read-only worktree (#739)")
	}
	if !payload.ReadOnlyWorktree {
		t.Fatal("background ask payload ReadOnlyWorktree = false, want true (disposal marker for a top-level read-only worktree)")
	}
	if payload.HeadSHA != "" {
		t.Fatalf("background ask payload HeadSHA = %q, want cleared (validate against the fresh worktree HEAD)", payload.HeadSHA)
	}
	if _, statErr := os.Stat(payload.WorktreePath); statErr != nil {
		t.Fatalf("read-only worktree dir missing on disk: %v", statErr)
	}
	// The #654 context note points the isolated committed-tip job at the canonical
	// checkout for gitignored/uncommitted paths.
	if !strings.Contains(payload.Instructions, checkout) {
		t.Fatal("background ask instructions missing the canonical-checkout context note (#654)")
	}
	// The job is keyed off its detached worktree, NOT the shared repo checkout — the
	// whole point of #739.
	wantPath, err := normalizeTaskWorktreePath(payload.WorktreePath)
	if err != nil {
		t.Fatalf("normalizeTaskWorktreePath: %v", err)
	}
	if got := queuedJobCheckoutKey(ctx, store, job); got != "worktree:"+wantPath {
		t.Fatalf("queuedJobCheckoutKey = %q, want worktree:%s (must NOT be repo:owner/repo)", got, wantPath)
	}
}

func TestDispatchReviewAllocatesDistinctExactHeadWorktrees(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout, firstHead, secondHead := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})

	type scannerInput struct {
		dir  string
		head string
	}
	var scannerInputs []scannerInput
	originalScanner := dispatchPromptHeadContradictionWarnings
	dispatchPromptHeadContradictionWarnings = func(_ context.Context, git gitutil.Client, _ string, head string) []string {
		scannerInputs = append(scannerInputs, scannerInput{dir: git.Dir, head: head})
		return nil
	}
	t.Cleanup(func() { dispatchPromptHeadContradictionWarnings = originalScanner })

	dispatch := func(head string) (db.Job, workflow.JobPayload) {
		out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
			RepoFlag: "owner/repo", Agent: "reviewer", LeadAgent: "implementer",
			Action: "review", Instructions: "Review this exact head.", Background: true,
			PullRequest: 12, Branch: "feature/review", HeadSHA: head, Home: home,
		})
		if err != nil {
			t.Fatalf("dispatchLocalAgentJob(%s): %v", head, err)
		}
		job, err := store.GetJob(ctx, out.JobID)
		if err != nil {
			t.Fatalf("GetJob(%s): %v", out.JobID, err)
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			t.Fatalf("daemonJobPayload(%s): %v", out.JobID, err)
		}
		return job, payload
	}

	firstJob, first := dispatch(firstHead)
	firstPath := first.WorktreePath
	secondJob, second := dispatch(secondHead)
	if firstJob.ID == secondJob.ID || firstPath == second.WorktreePath {
		t.Fatalf("review jobs/worktrees not distinct: first=%s %q second=%s %q", firstJob.ID, firstPath, secondJob.ID, second.WorktreePath)
	}
	for _, got := range []struct {
		name    string
		payload workflow.JobPayload
		head    string
	}{
		{name: "first", payload: first, head: firstHead},
		{name: "second", payload: second, head: secondHead},
	} {
		if !got.payload.ReadOnlyWorktree || strings.TrimSpace(got.payload.WorktreePath) == "" {
			t.Fatalf("%s payload lacks owned read-only worktree: %+v", got.name, got.payload)
		}
		if got.payload.HeadSHA != got.head {
			t.Fatalf("%s payload HeadSHA=%q, want preserved %q", got.name, got.payload.HeadSHA, got.head)
		}
		if actual := readonlyWorktreeHead(t, got.payload.WorktreePath); actual != got.head {
			t.Fatalf("%s worktree HEAD=%q, want %q", got.name, actual, got.head)
		}
	}
	if first.TaskID == "" || first.TaskID != second.TaskID {
		t.Fatalf("review task identity first=%q second=%q, want one stable task", first.TaskID, second.TaskID)
	}
	if actual := readonlyWorktreeHead(t, firstPath); actual != firstHead {
		t.Fatalf("first worktree changed after second dispatch: HEAD=%q, want %q", actual, firstHead)
	}
	task, err := store.GetTask(ctx, first.TaskID)
	if err != nil {
		t.Fatalf("GetTask(%s): %v", first.TaskID, err)
	}
	if task.WorktreePath != "" {
		t.Fatalf("review task WorktreePath=%q, want lifecycle row without shared checkout", task.WorktreePath)
	}
	if len(scannerInputs) != 2 {
		t.Fatalf("review scanner calls=%d, want 2: %+v", len(scannerInputs), scannerInputs)
	}
	for i, want := range []scannerInput{{dir: first.WorktreePath, head: firstHead}, {dir: second.WorktreePath, head: secondHead}} {
		if scannerInputs[i] != want {
			t.Fatalf("review scanner input[%d]=%+v, want %+v", i, scannerInputs[i], want)
		}
	}
}

func TestDispatchAskScannerKeepsCanonicalCheckoutAndInheritedHead(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")
	wantHead := strings.Repeat("a", 40)
	var gotDir, gotHead string
	originalScanner := dispatchPromptHeadContradictionWarnings
	dispatchPromptHeadContradictionWarnings = func(_ context.Context, git gitutil.Client, _ string, head string) []string {
		gotDir, gotHead = git.Dir, head
		return nil
	}
	t.Cleanup(func() { dispatchPromptHeadContradictionWarnings = originalScanner })

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "responder", Action: "ask", Background: true,
		Instructions: "Inspect the checkout.", HeadSHA: wantHead, Home: home,
	})
	if err != nil {
		t.Fatalf("dispatchLocalAgentJob: %v", err)
	}
	if gotDir != checkout || gotHead != wantHead {
		t.Fatalf("ask scanner dir=%q head=%q, want canonical dir=%q inherited head=%q", gotDir, gotHead, checkout, wantHead)
	}
	job, err := store.GetJob(ctx, out.JobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	if payload.HeadSHA != "" || payload.WorktreePath == "" {
		t.Fatalf("ask payload=%+v, want post-scan head clear and isolated worktree", payload)
	}
}

func TestDispatchTaskBearingAskDoesNotAllocateReviewWorktree(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")
	if err := store.UpsertTask(ctx, db.Task{ID: "task-ask", RepoFullName: "owner/repo", State: string(workflow.TaskBlocked), WorktreePath: checkout}); err != nil {
		t.Fatal(err)
	}
	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "responder", Action: "ask", Background: true,
		TaskID: "task-ask", Instructions: "Inspect this task.", Home: home,
	})
	if err != nil {
		t.Fatalf("dispatchLocalAgentJob: %v", err)
	}
	job, err := store.GetJob(ctx, out.JobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ReadOnlyWorktree || payload.WorktreePath != "" {
		t.Fatalf("task-bearing ask unexpectedly allocated per-job worktree: %+v", payload)
	}
	wantPath, err := normalizeTaskWorktreePath(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if got := queuedJobCheckoutKey(ctx, store, job); got != "worktree:"+wantPath {
		t.Fatalf("task-bearing ask checkout key=%q, want existing task worktree:%s", got, wantPath)
	}
}

func TestDispatchReviewCapacityAndAllocationFailuresFailClosed(t *testing.T) {
	t.Run("capacity checked before task mutation", func(t *testing.T) {
		ctx := context.Background()
		store, home := blockerE2EHome(t)
		checkout, head, _ := readonlyReviewWorktreeGitCheckout(t)
		seedReviewDispatchFixture(t, store, checkout)
		replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
			return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 1 << 30}, nil
		})
		_, err := dispatchLocalAgentJob(ctx, store, reviewDispatchRequest(home, head))
		if err == nil || !strings.Contains(err.Error(), "requires at least") {
			t.Fatalf("capacity error=%v", err)
		}
		if tasks, listErr := store.ListTasks(ctx); listErr != nil || len(tasks) != 0 {
			t.Fatalf("tasks after capacity refusal=%+v err=%v, want none", tasks, listErr)
		}
		if jobs, listErr := store.ListJobs(ctx); listErr != nil || len(jobs) != 0 {
			t.Fatalf("jobs after capacity refusal=%+v err=%v, want none", jobs, listErr)
		}
	})

	t.Run("allocation failure refuses enqueue", func(t *testing.T) {
		ctx := context.Background()
		store, home := blockerE2EHome(t)
		checkout, head, _ := readonlyReviewWorktreeGitCheckout(t)
		seedReviewDispatchFixture(t, store, checkout)
		replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
			return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
		})
		originalAllocate := allocateDispatchReadOnlyWorktree
		allocateDispatchReadOnlyWorktree = func(context.Context, *db.Store, string, string, string, string, string, int, string, time.Duration, workflow.ReadOnlyWorktreeManager) (string, error) {
			return "", errors.New("injected allocation failure")
		}
		t.Cleanup(func() { allocateDispatchReadOnlyWorktree = originalAllocate })
		originalFetch := fetchDispatchReviewPullRequest
		fetchDispatchReviewPullRequest = func(context.Context, gitutil.Client, int) error { return nil }
		t.Cleanup(func() { fetchDispatchReviewPullRequest = originalFetch })

		_, err := dispatchLocalAgentJob(ctx, store, reviewDispatchRequest(home, head))
		if err == nil || !strings.Contains(err.Error(), "injected allocation failure") {
			t.Fatalf("allocation error=%v", err)
		}
		if jobs, listErr := store.ListJobs(ctx); listErr != nil || len(jobs) != 0 {
			t.Fatalf("jobs after allocation refusal=%+v err=%v, want none", jobs, listErr)
		}
	})
}

// TestDispatchForegroundAndImplementLeaveCheckoutKeyShared confirms the contrast:
// neither a foreground ask nor an implement job gets a dispatch-time read-only
// worktree, so an equivalent payload with no worktree still keys repo:<repo>.
func TestDispatchImplementUntouchedKeysRepo(t *testing.T) {
	ctx := context.Background()
	store, _ := blockerE2EHome(t)
	// An implement job the dispatch would have prepared carries a task worktree, so
	// eligibility is false; a bare read-only payload with no worktree keys repo:<repo>.
	if dispatchReadOnlyWorktreeEligible(localAgentDispatchRequest{Background: true, Action: "implement"}) {
		t.Fatal("implement dispatch must not be read-only-worktree eligible")
	}
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "responder", runtime.ShellRuntime, "printf '%s' '{}'", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:           "ask-no-worktree",
		Agent:        "responder",
		Action:       "ask",
		Repo:         "owner/repo",
		Sender:       "local",
		Instructions: "hi",
	})
	jobs, err := store.ListQueuedJobs(ctx)
	if err != nil {
		t.Fatalf("ListQueuedJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("queued jobs = %d, want 1", len(jobs))
	}
	if got := queuedJobCheckoutKey(ctx, store, jobs[0]); got != "repo:owner/repo" {
		t.Fatalf("queuedJobCheckoutKey for a no-worktree job = %q, want repo:owner/repo", got)
	}
}

func readonlyWorktreeGitCheckout(t *testing.T, fullName string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "branch", "-m", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "gitmoot test")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/"+fullName+".git")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func readonlyReviewWorktreeGitCheckout(t *testing.T) (string, string, string) {
	t.Helper()
	dir := readonlyWorktreeGitCheckout(t, "owner/repo")
	runGit(t, dir, "switch", "-c", "feature/review")
	writeFile(t, filepath.Join(dir, "review.txt"), "round one\n")
	runGit(t, dir, "add", "review.txt")
	runGit(t, dir, "commit", "-m", "review round one")
	firstHead := readonlyWorktreeHead(t, dir)
	writeFile(t, filepath.Join(dir, "review.txt"), "round two\n")
	runGit(t, dir, "add", "review.txt")
	runGit(t, dir, "commit", "-m", "review round two")
	secondHead := readonlyWorktreeHead(t, dir)
	runGit(t, dir, "switch", "main")
	return dir, firstHead, secondHead
}

func readonlyWorktreeHead(t *testing.T, dir string) string {
	t.Helper()
	head, err := (gitutil.Client{Dir: dir}).HeadSHA(context.Background())
	if err != nil {
		t.Fatalf("HeadSHA(%s): %v", dir, err)
	}
	return head
}

func seedReviewDispatchFixture(t *testing.T, store *db.Store, checkout string) {
	t.Helper()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "implementer", runtime.ShellRuntime, "true", []string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)
}

func reviewDispatchRequest(home, head string) localAgentDispatchRequest {
	return localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "reviewer", LeadAgent: "implementer",
		Action: "review", Instructions: "Review this exact head.", Background: true,
		PullRequest: 12, Branch: "feature/review", HeadSHA: head, Home: home,
	}
}
