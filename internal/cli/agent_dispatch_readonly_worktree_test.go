package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// TestDispatchReadOnlyWorktreeEligible pins the gate for dispatch-time read-only
// worktree allocation (#739): only a BACKGROUND read-only (ask/review) job with no
// existing task worktree qualifies. A foreground ask (runs inline, never
// serializes), an implement job, and a read-only job that already carries a
// per-PR/task worktree (TaskID set) are all excluded.
func TestDispatchReadOnlyWorktreeEligible(t *testing.T) {
	cases := []struct {
		name string
		req  localAgentDispatchRequest
		want bool
	}{
		{"background ask", localAgentDispatchRequest{Background: true, Action: "ask"}, true},
		{"background review no task", localAgentDispatchRequest{Background: true, Action: "review"}, true},
		{"foreground ask untouched", localAgentDispatchRequest{Background: false, Action: "ask"}, false},
		{"background implement untouched", localAgentDispatchRequest{Background: true, Action: "implement"}, false},
		{"background run untouched", localAgentDispatchRequest{Background: true, Action: "run"}, false},
		{"background ask with task worktree", localAgentDispatchRequest{Background: true, Action: "ask", TaskID: "t1"}, false},
		{"background review with task worktree", localAgentDispatchRequest{Background: true, Action: "review", TaskID: "review-pr-1"}, false},
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
