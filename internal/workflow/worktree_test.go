package workflow

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
)

func TestTaskWorktreePath(t *testing.T) {
	path, err := TaskWorktreePath("/home/gitmoot", "owner/repo", "task-1")
	if err != nil {
		t.Fatalf("TaskWorktreePath returned error: %v", err)
	}
	want := filepath.Join("/home/gitmoot", "worktrees", "owner--repo", "task-1")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	for _, tc := range []struct {
		name string
		repo string
		task string
	}{
		{name: "empty repo", repo: "", task: "task-1"},
		{name: "nested repo", repo: "owner/repo/extra", task: "task-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := TaskWorktreePath("/home/gitmoot", tc.repo, tc.task); err == nil {
				t.Fatal("TaskWorktreePath accepted invalid input")
			}
		})
	}

	// A task id that is not a plain path segment (e.g. "../task") is now sanitized
	// into a safe, traversal-safe segment rather than rejected.
	tp, err := TaskWorktreePath("/home/gitmoot", "owner/repo", "../task")
	if err != nil {
		t.Fatalf("TaskWorktreePath should sanitize unsafe task id, got error: %v", err)
	}
	troot := filepath.Join("/home/gitmoot", "worktrees", "owner--repo")
	if rel, err := filepath.Rel(troot, tp); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("sanitized task path %q escaped the worktrees root (rel=%q err=%v)", tp, rel, err)
	}
}

func TestAcquireCheckoutMutationLockWithWaitBudgetTimesOutWhenLocked(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	checkout := t.TempDir()
	key, err := checkoutMutationLockKey(checkout)
	if err != nil {
		t.Fatalf("checkoutMutationLockKey returned error: %v", err)
	}
	if acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  "task:other",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}

	_, _, err = acquireCheckoutMutationLockWithWaitBudget(ctx, store, checkout, "worktree:task-1", time.Now().UTC(), 20*time.Millisecond, 5*time.Millisecond)

	var blocked BlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "Waited up to") {
		t.Fatalf("error = %v, want checkout wait timeout BlockedError", err)
	}
}

func TestDelegationWorktreePath(t *testing.T) {
	path, err := DelegationWorktreePath("/home/gitmoot", "owner/repo", "job-1", "d1", 0)
	if err != nil {
		t.Fatalf("DelegationWorktreePath returned error: %v", err)
	}
	want := filepath.Join("/home/gitmoot", "worktrees", "owner--repo", "delegations", "job-1", "d1")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	// A retry attempt gets an isolated /retry/<n> subdirectory so it never
	// collides with the failed original attempt's worktree.
	retryPath, err := DelegationWorktreePath("/home/gitmoot", "owner/repo", "job-1", "d1", 2)
	if err != nil {
		t.Fatalf("DelegationWorktreePath (retry) returned error: %v", err)
	}
	wantRetry := filepath.Join("/home/gitmoot", "worktrees", "owner--repo", "delegations", "job-1", "d1", "retry", "2")
	if retryPath != wantRetry {
		t.Fatalf("retry path = %q, want %q", retryPath, wantRetry)
	}
	if retryPath == want {
		t.Fatalf("retry path %q collides with original attempt path", retryPath)
	}
	for _, tc := range []struct {
		name       string
		home       string
		repo       string
		parentJob  string
		delegation string
	}{
		{name: "empty home", home: "", repo: "owner/repo", parentJob: "job-1", delegation: "d1"},
		{name: "empty repo", home: "/home/gitmoot", repo: "", parentJob: "job-1", delegation: "d1"},
		{name: "nested repo", home: "/home/gitmoot", repo: "owner/repo/extra", parentJob: "job-1", delegation: "d1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DelegationWorktreePath(tc.home, tc.repo, tc.parentJob, tc.delegation, 0); err == nil {
				t.Fatal("DelegationWorktreePath accepted invalid input")
			}
		})
	}

	// Parent/delegation ids that are not a plain path segment -- a "/"-bearing
	// continuation id, or a "../" attempt -- are now SANITIZED into a safe segment
	// rather than rejected, so the multi-round coordinator can dispatch an
	// implement delegation from a continuation. The result must stay traversal-safe
	// (never escape the delegations root).
	for _, tc := range []struct {
		name       string
		parentJob  string
		delegation string
	}{
		{name: "slashed parent (continuation)", parentJob: "job-1/continuation/continuation", delegation: "d1"},
		{name: "dotdot parent", parentJob: "../job", delegation: "d1"},
		{name: "dotdot delegation", parentJob: "job-1", delegation: "../d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := DelegationWorktreePath("/home/gitmoot", "owner/repo", tc.parentJob, tc.delegation, 0)
			if err != nil {
				t.Fatalf("DelegationWorktreePath should sanitize, got error: %v", err)
			}
			root := filepath.Join("/home/gitmoot", "worktrees", "owner--repo", "delegations")
			rel, err := filepath.Rel(root, p)
			if err != nil || strings.HasPrefix(rel, "..") {
				t.Fatalf("sanitized path %q escaped the delegations root (rel=%q err=%v)", p, rel, err)
			}
		})
	}
}

func TestEngineReclaimTerminalTaskWorktreeKeepsLiveDirtyAdhocAtAnyAge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("MkdirAll checkout: %v", err)
	}
	runWorktreeGit(t, checkout, "init", "-b", "main")
	runWorktreeGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runWorktreeGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".gitignore"), []byte("GOALS/\nCLAUDE.local.md\n"), 0o644); err != nil {
		t.Fatalf("WriteFile .gitignore: %v", err)
	}
	runWorktreeGit(t, checkout, "add", "base.txt", ".gitignore")
	runWorktreeGit(t, checkout, "commit", "-m", "base")

	home := filepath.Join(root, "home")
	path, err := TaskWorktreePath(home, "owner/repo", "adhoc-old-live")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll worktree parent: %v", err)
	}
	runWorktreeGit(t, checkout, "worktree", "add", "-b", "adhoc-old-live", path, "HEAD")
	untrackedPath := filepath.Join(path, "uncommitted.txt")
	if err := os.WriteFile(untrackedPath, []byte("preserve me\n"), 0o644); err != nil {
		t.Fatalf("WriteFile uncommitted change: %v", err)
	}

	liveProcess := exec.Command("sleep", "30")
	liveProcess.Dir = path
	if err := liveProcess.Start(); err != nil {
		t.Fatalf("start live worktree process: %v", err)
	}
	t.Cleanup(func() {
		_ = liveProcess.Process.Kill()
		_ = liveProcess.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		live, known := WorktreeLiveness(path)
		if live && known {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("live worktree process was not observable: live=%v known=%v", live, known)
		}
		time.Sleep(10 * time.Millisecond)
	}

	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "adhoc-old-live",
		RepoFullName: "owner/repo",
		State:        string(TaskDismissed),
		Branch:       "adhoc-old-live",
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	manager := gitutil.NewHostClient(checkout)
	engine := testEngine(store)
	engine.WorktreeHasLiveProcess = nil

	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, "adhoc-old-live", manager)
	if err != nil {
		t.Fatalf("live ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimLiveProcess {
		t.Fatalf("live outcome = %+v, want retained live-process task", outcome)
	}
	if live, known := WorktreeLiveness(path); !live || !known {
		t.Fatalf("KEEP fixture process after reclaim: live=%v known=%v", live, known)
	}
	if content, err := os.ReadFile(untrackedPath); err != nil || string(content) != "preserve me\n" {
		t.Fatalf("uncommitted work after live reclaim = %q, err=%v", content, err)
	}

	if err := liveProcess.Process.Kill(); err != nil {
		t.Fatalf("kill live worktree process: %v", err)
	}
	_ = liveProcess.Wait()
	// The live-process arm above uses the real /proc scan. After the child exits,
	// make the no-live premise deterministic so an unrelated unreadable host PID
	// cannot prevent this same test from reaching the content guards.
	engine.WorktreeLiveness = func(string) (bool, bool) { return false, true }
	outcome, err = engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, "adhoc-old-live", manager)
	if err != nil {
		t.Fatalf("dirty ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimDirty {
		t.Fatalf("dirty outcome = %+v, want retained dirty task", outcome)
	}
	if clean, err := manager.WorktreeCleanAt(ctx, path); err != nil || clean {
		t.Fatalf("dirty WorktreeCleanAt = %v, err=%v, want false nil", clean, err)
	}

	if err := os.Remove(untrackedPath); err != nil {
		t.Fatalf("remove untracked fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, "GOALS"), 0o755); err != nil {
		t.Fatalf("MkdirAll ignored content: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "GOALS", "plan.md"), []byte("preserve ignored plan\n"), 0o644); err != nil {
		t.Fatalf("WriteFile ignored plan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "CLAUDE.local.md"), []byte("preserve ignored instructions\n"), 0o644); err != nil {
		t.Fatalf("WriteFile ignored instructions: %v", err)
	}
	runWorktreeGit(t, path, "check-ignore", "GOALS/plan.md", "CLAUDE.local.md")
	outcome, err = engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, "adhoc-old-live", manager)
	if err != nil {
		t.Fatalf("ignored-content ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimDirty {
		t.Fatalf("ignored-content outcome = %+v, want retained dirty task", outcome)
	}
	if pristine, err := manager.WorktreePristineAt(ctx, path); err != nil || pristine {
		t.Fatalf("ignored-content WorktreePristineAt = %v, err=%v, want false nil", pristine, err)
	}
	task, err := store.GetTask(ctx, "adhoc-old-live")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.WorktreePath != path {
		t.Fatalf("worktree path = %q, want preserved %q", task.WorktreePath, path)
	}
	for _, ignored := range []string{"GOALS/plan.md", "CLAUDE.local.md"} {
		if _, err := os.Stat(filepath.Join(path, ignored)); err != nil {
			t.Fatalf("ignored content %s was removed: %v", ignored, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(path, "GOALS")); err != nil {
		t.Fatalf("RemoveAll ignored GOALS: %v", err)
	}
	if err := os.Remove(filepath.Join(path, "CLAUDE.local.md")); err != nil {
		t.Fatalf("remove ignored instructions: %v", err)
	}
	runWorktreeGit(t, path, "checkout", "--detach")
	if err := os.WriteFile(filepath.Join(path, "detached.txt"), []byte("detached commit\n"), 0o644); err != nil {
		t.Fatalf("WriteFile detached commit: %v", err)
	}
	runWorktreeGit(t, path, "add", "detached.txt")
	runWorktreeGit(t, path, "commit", "-m", "detached local commit")
	if pristine, err := manager.WorktreePristineAt(ctx, path); err != nil || !pristine {
		t.Fatalf("detached committed WorktreePristineAt = %v, err=%v, want true nil", pristine, err)
	}
	detachedHead, err := manager.HeadSHAAt(ctx, path)
	if err != nil {
		t.Fatalf("detached HeadSHAAt: %v", err)
	}
	branchHead, err := manager.RevParse(ctx, "refs/heads/adhoc-old-live")
	if err != nil {
		t.Fatalf("task branch RevParse: %v", err)
	}
	if reachable, err := manager.IsAncestor(ctx, detachedHead, branchHead); err != nil || reachable {
		t.Fatalf("detached head reachable from task branch = %v, err=%v, want false nil", reachable, err)
	}
	outcome, err = engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, "adhoc-old-live", manager)
	if err != nil {
		t.Fatalf("detached-head ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimHeadUnreachable {
		t.Fatalf("detached-head outcome = %+v, want retained unreachable head", outcome)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("detached-head worktree was removed: %v", err)
	}
	task, err = store.GetTask(ctx, "adhoc-old-live")
	if err != nil || task.WorktreePath != path {
		t.Fatalf("detached-head task = %+v, err=%v, want preserved path %q", task, err, path)
	}
}

func TestEngineReclaimTerminalTaskWorktreeReclaimsTaskKinds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    string
		state TaskState
	}{
		{name: "adhoc dismissed", id: "adhoc-clean", state: TaskDismissed},
		{name: "review pr merged", id: "review-pr-17-clean", state: TaskMerged},
		{name: "task superseded", id: "task-clean", state: TaskSuperseded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			home := t.TempDir()
			checkout := t.TempDir()
			path, err := TaskWorktreePath(home, "owner/repo", tc.id)
			if err != nil {
				t.Fatalf("TaskWorktreePath: %v", err)
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("MkdirAll worktree: %v", err)
			}
			if err := store.UpsertTask(ctx, db.Task{
				ID:           tc.id,
				RepoFullName: "owner/repo",
				State:        string(tc.state),
				Branch:       tc.id,
				WorktreePath: path,
			}); err != nil {
				t.Fatalf("UpsertTask: %v", err)
			}
			manager := &fakeWorktreeManager{existingBranches: map[string]bool{tc.id: true}}
			engine := testEngine(store)

			outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, tc.id, manager)
			if err != nil {
				t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
			}
			if !outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimReclaimed {
				t.Fatalf("outcome = %+v, want reclaimed", outcome)
			}
			if len(manager.cleanCalls) != 2 || len(manager.removed) != 1 || manager.removed[0] != path {
				t.Fatalf("manager safety/removal calls: clean=%v removed=%v", manager.cleanCalls, manager.removed)
			}
			if len(manager.deletedBranches) != 0 {
				t.Fatalf("terminal task branch was deleted: %v", manager.deletedBranches)
			}
			task, err := store.GetTask(ctx, tc.id)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.WorktreePath != "" || task.Branch != tc.id {
				t.Fatalf("task after reclaim = %+v, want empty path and preserved branch", task)
			}
			events, err := store.ListTaskEvents(ctx, tc.id)
			if err != nil {
				t.Fatalf("ListTaskEvents: %v", err)
			}
			if len(events) != 1 || events[0].Kind != "terminal_worktree_reclaimed" || events[0].Reason != path {
				t.Fatalf("task events = %+v", events)
			}
		})
	}
}

func TestEngineReclaimTerminalTaskWorktreeKeepsUnsafeCandidates(t *testing.T) {
	for _, tc := range []struct {
		name           string
		live           bool
		known          bool
		clean          bool
		want           TaskWorktreeReclaimClassification
		wantCleanCalls int
	}{
		{name: "live process", live: true, known: true, clean: true, want: TaskWorktreeReclaimLiveProcess},
		{name: "unknown process table", known: false, clean: true, want: TaskWorktreeReclaimLivenessUnknown},
		{name: "dirty worktree", known: true, clean: false, want: TaskWorktreeReclaimDirty, wantCleanCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			home := t.TempDir()
			path, err := TaskWorktreePath(home, "owner/repo", "adhoc-terminal")
			if err != nil {
				t.Fatalf("TaskWorktreePath: %v", err)
			}
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatalf("MkdirAll worktree: %v", err)
			}
			if err := store.UpsertTask(ctx, db.Task{
				ID:           "adhoc-terminal",
				RepoFullName: "owner/repo",
				State:        string(TaskDismissed),
				WorktreePath: path,
			}); err != nil {
				t.Fatalf("UpsertTask: %v", err)
			}
			manager := &fakeWorktreeManager{cleanSet: true, clean: tc.clean}
			engine := testEngine(store)
			engine.WorktreeHasLiveProcess = nil
			engine.WorktreeLiveness = func(gotPath string) (bool, bool) {
				if gotPath != path {
					t.Fatalf("liveness path = %q, want %q", gotPath, path)
				}
				return tc.live, tc.known
			}

			outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, t.TempDir(), "adhoc-terminal", manager)
			if err != nil {
				t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
			}
			if outcome.Reclaimed || outcome.Classification != tc.want {
				t.Fatalf("outcome = %+v, want retained %s", outcome, tc.want)
			}
			if len(manager.cleanCalls) != tc.wantCleanCalls || len(manager.removed) != 0 {
				t.Fatalf("unsafe worktree was over-inspected or removed: clean=%v removed=%v", manager.cleanCalls, manager.removed)
			}
			task, err := store.GetTask(ctx, "adhoc-terminal")
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.WorktreePath != path {
				t.Fatalf("worktree path = %q, want preserved %q", task.WorktreePath, path)
			}
		})
	}
}

// A preserved task branch that no longer exists means safety is unprovable, not
// that the pass is broken: it classifies instead of erroring on every tick.
func TestEngineReclaimTerminalTaskWorktreeClassifiesMissingBranch(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	const taskID = "review-pr-42-missing-branch"
	path, err := TaskWorktreePath(home, "owner/repo", taskID)
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           taskID,
		RepoFullName: "owner/repo",
		State:        string(TaskMerged),
		Branch:       taskID,
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	manager := &fakeWorktreeManager{existingBranches: map[string]bool{}}
	engine := testEngine(store)

	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, t.TempDir(), taskID, manager)
	if err != nil {
		t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimHeadUnreachable {
		t.Fatalf("outcome = %+v, want head_unreachable retention", outcome)
	}
	if len(manager.removed) != 0 {
		t.Fatalf("worktree was removed without a reachable branch: %v", manager.removed)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("worktree was deleted: %v", statErr)
	}
}

type fakeTerminalWorktreeRemovalError struct{}

func (fakeTerminalWorktreeRemovalError) Error() string {
	return "registered root does not own worktree"
}

func (fakeTerminalWorktreeRemovalError) TerminalWorktreeRemoval() bool {
	return true
}

func TestEngineReclaimTerminalTaskWorktreeClassifiesUnremovableOnce(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	path, err := TaskWorktreePath(home, "owner/repo", "review-pr-unremovable")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "review-pr-unremovable",
		RepoFullName: "owner/repo",
		State:        string(TaskMerged),
		Branch:       "review-pr-unremovable",
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	manager := &fakeWorktreeManager{
		removeErr:        fakeTerminalWorktreeRemovalError{},
		existingBranches: map[string]bool{"review-pr-unremovable": true},
	}
	engine := testEngine(store)

	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, t.TempDir(), "review-pr-unremovable", manager)
	if err != nil {
		t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimUnremovable {
		t.Fatalf("outcome = %+v, want terminal-unremovable", outcome)
	}
	ids, err := store.TaskIDsWithTerminalWorktree(ctx)
	if err != nil {
		t.Fatalf("TaskIDsWithTerminalWorktree: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("classified candidate remained retryable: %v", ids)
	}
	events, err := store.ListTaskEvents(ctx, "review-pr-unremovable")
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "terminal_worktree_unremovable" || events[0].Reason != path {
		t.Fatalf("task events = %+v", events)
	}
}

func TestEngineReclaimTerminalTaskWorktreeClassifiesMissingGitAdmin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("MkdirAll checkout: %v", err)
	}
	runWorktreeGit(t, checkout, "init", "-b", "main")
	runWorktreeGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runWorktreeGit(t, checkout, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(checkout, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("WriteFile base: %v", err)
	}
	runWorktreeGit(t, checkout, "add", "base.txt")
	runWorktreeGit(t, checkout, "commit", "-m", "base")

	home := filepath.Join(root, "home")
	path, err := TaskWorktreePath(home, "owner/repo", "adhoc-missing-admin")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll worktree parent: %v", err)
	}
	runWorktreeGit(t, checkout, "worktree", "add", "-b", "adhoc-missing-admin", path, "HEAD")
	gitFile, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		t.Fatalf("ReadFile .git: %v", err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(gitFile)), "\n")
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		t.Fatalf("worktree .git pointer = %q", line)
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	if err := os.RemoveAll(filepath.Clean(gitDir)); err != nil {
		t.Fatalf("remove isolated worktree admin: %v", err)
	}

	store := openEngineStore(t)
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "adhoc-missing-admin",
		RepoFullName: "owner/repo",
		State:        string(TaskDismissed),
		Branch:       "adhoc-missing-admin",
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	engine := testEngine(store)
	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, checkout, "adhoc-missing-admin", gitutil.NewHostClient(checkout))
	if err != nil {
		t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimUnremovable {
		t.Fatalf("outcome = %+v, want terminal-unremovable", outcome)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unprovable worktree was removed: %v", err)
	}
	ids, err := store.TaskIDsWithTerminalWorktree(ctx)
	if err != nil {
		t.Fatalf("TaskIDsWithTerminalWorktree: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("missing-admin candidate remained retryable: %v", ids)
	}
}

func TestEngineReclaimTerminalTaskWorktreeKeepsBranchLockOwner(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	path, err := TaskWorktreePath(home, "owner/repo", "task-locked")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-locked",
		RepoFullName: "owner/repo",
		State:        string(TaskMerged),
		Branch:       "task-locked",
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	created, err := store.CreateLock(ctx, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-locked", Owner: "live-owner"})
	if err != nil || !created {
		t.Fatalf("CreateLock created=%v err=%v", created, err)
	}
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)

	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, t.TempDir(), "task-locked", manager)
	if err != nil {
		t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimActiveOwner {
		t.Fatalf("outcome = %+v, want active owner keep", outcome)
	}
	if len(manager.cleanCalls) != 0 || len(manager.removed) != 0 {
		t.Fatalf("owned worktree was inspected or removed: clean=%v removed=%v", manager.cleanCalls, manager.removed)
	}
}

func TestEngineReclaimTerminalTaskWorktreeRejectsUnmanagedPath(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	unmanaged := t.TempDir()
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "task-unmanaged",
		RepoFullName: "owner/repo",
		State:        string(TaskDismissed),
		WorktreePath: unmanaged,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)

	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, t.TempDir(), "task-unmanaged", manager)
	if err != nil {
		t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimPathMismatch {
		t.Fatalf("outcome = %+v, want path mismatch keep", outcome)
	}
	if len(manager.cleanCalls) != 0 || len(manager.removed) != 0 {
		t.Fatalf("unmanaged path was inspected or removed: clean=%v removed=%v", manager.cleanCalls, manager.removed)
	}
	if _, err := os.Stat(unmanaged); err != nil {
		t.Fatalf("unmanaged path changed: %v", err)
	}
}

func TestEngineReclaimTerminalTaskWorktreeKeepsBlockedJobOwner(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	home := t.TempDir()
	path, err := TaskWorktreePath(home, "owner/repo", "review-pr-active")
	if err != nil {
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID:           "review-pr-active",
		RepoFullName: "owner/repo",
		State:        string(TaskMerged),
		WorktreePath: path,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	payload, err := marshalPayload(JobPayload{TaskID: "review-pr-active", WorktreePath: path})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "active-owner", Agent: "reviewer", Type: "review", State: string(JobBlocked), Payload: payload,
	}, db.JobEvent{Kind: string(JobBlocked), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	manager := &fakeWorktreeManager{}
	engine := testEngine(store)

	outcome, err := engine.ReclaimTerminalTaskWorktreeOutcome(ctx, home, t.TempDir(), "review-pr-active", manager)
	if err != nil {
		t.Fatalf("ReclaimTerminalTaskWorktreeOutcome: %v", err)
	}
	if outcome.Reclaimed || outcome.Classification != TaskWorktreeReclaimActiveOwner {
		t.Fatalf("outcome = %+v, want active owner keep", outcome)
	}
	if len(manager.cleanCalls) != 0 || len(manager.removed) != 0 {
		t.Fatalf("active job worktree was inspected or removed: clean=%v removed=%v", manager.cleanCalls, manager.removed)
	}
}

func TestNestedGitObjectDatabaseRejectsCorruptGitShapedCaches(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{
			name: "loose object declaring more bytes than it carries",
			setup: func(t *testing.T, root string) {
				writeZlibFile(t, filepath.Join(root, "cache", "ab", strings.Repeat("c", 38)), "blob 999999\x00x")
			},
		},
		{
			name: "loose object whose bytes do not hash to its name",
			setup: func(t *testing.T, root string) {
				writeZlibFile(t, filepath.Join(root, "cache", "ab", strings.Repeat("c", 38)), "blob 5\x00hello")
			},
		},
		{
			name: "pack file holding only the magic",
			setup: func(t *testing.T, root string) {
				name := filepath.Join(root, "cache", "pack", "pack-"+strings.Repeat("a", 40)+".pack")
				if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
					t.Fatalf("MkdirAll pack directory: %v", err)
				}
				if err := os.WriteFile(name, []byte("PACK"), 0o644); err != nil {
					t.Fatalf("write pack: %v", err)
				}
			},
		},
		{
			name: "pack file with no index beside it",
			setup: func(t *testing.T, root string) {
				name := filepath.Join(root, "cache", "pack", "pack-"+strings.Repeat("b", 40)+".pack")
				if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
					t.Fatalf("MkdirAll pack directory: %v", err)
				}
				body := append([]byte("PACK"), 0, 0, 0, 2, 0, 0, 0, 1)
				body = append(body, make([]byte, 32)...)
				if err := os.WriteFile(name, body, 0o644); err != nil {
					t.Fatalf("write pack: %v", err)
				}
			},
		},
		{
			name: "indexed pack with an unsupported version",
			setup: func(t *testing.T, root string) {
				writePackFile(t, root, strings.Repeat("c", 40), 7, 1)
			},
		},
		{
			name: "indexed pack carrying no objects",
			setup: func(t *testing.T, root string) {
				writePackFile(t, root, strings.Repeat("d", 40), 2, 0)
			},
		},
		{
			name: "pack whose index names a different pack",
			setup: func(t *testing.T, root string) {
				// Both files verify on their own; only the cross-check catches this.
				writeVerifiablePack(t, root, strings.Repeat("e", 40), false)
			},
		},
		{
			name: "pack with a corrupt trailing checksum",
			setup: func(t *testing.T, root string) {
				writeVerifiablePack(t, root, strings.Repeat("0", 40), true)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			nested, err := nestedGitObjectDatabase(ctx, root, gitPackVerifierForTest(t))
			if err != nil {
				t.Fatalf("nestedGitObjectDatabase: %v", err)
			}
			if nested != "" {
				t.Fatalf("corrupt Git-shaped cache classified as object database %q", nested)
			}
		})
	}
}

// writePackFile lays down an INDEXED pack of the requested version and object
// count, sized past the header-plus-checksum floor, so the only thing under test
// is the structural check rather than the size or index preconditions.
func writePackFile(t *testing.T, root, hashName string, version, objects uint32) {
	t.Helper()
	name := filepath.Join(root, "cache", "pack", "pack-"+hashName+".pack")
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatalf("MkdirAll pack directory: %v", err)
	}
	body := make([]byte, 0, 12+1+20)
	body = append(body, "PACK"...)
	body = binary.BigEndian.AppendUint32(body, version)
	body = binary.BigEndian.AppendUint32(body, objects)
	body = append(body, make([]byte, 1+20)...)
	if err := os.WriteFile(name, body, 0o644); err != nil {
		t.Fatalf("write pack: %v", err)
	}
	if err := os.WriteFile(strings.TrimSuffix(name, ".pack")+".idx", []byte("idx"), 0o644); err != nil {
		t.Fatalf("write pack index: %v", err)
	}
}

// writeVerifiablePack lays down a pack and index whose OWN checksums verify, so
// the only thing under test is the cross-check between them (and, with
// corruptPack, the pack self-check).
func writeVerifiablePack(t *testing.T, root, hashName string, corruptPack bool) {
	t.Helper()
	name := filepath.Join(root, "cache", "pack", "pack-"+hashName+".pack")
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatalf("MkdirAll pack directory: %v", err)
	}
	body := make([]byte, 0, 12+1)
	body = append(body, "PACK"...)
	body = binary.BigEndian.AppendUint32(body, 2)
	body = binary.BigEndian.AppendUint32(body, 1)
	body = append(body, 0x00)
	packDigest := sha1.Sum(body)
	pack := append(append([]byte{}, body...), packDigest[:]...)
	if corruptPack {
		pack[len(pack)-1] ^= 0xff
	}
	if err := os.WriteFile(name, pack, 0o644); err != nil {
		t.Fatalf("write pack: %v", err)
	}
	// A v2 index that verifies but records a DIFFERENT pack digest.
	index := make([]byte, 0, 8+1024+40)
	index = append(index, 0xff, 0x74, 0x4f, 0x63)
	index = binary.BigEndian.AppendUint32(index, 2)
	index = append(index, make([]byte, 1024)...)
	recorded := sha1.Sum([]byte("a different pack"))
	if corruptPack {
		// Record the pack's ACTUAL (corrupted) trailer, so the cross-check agrees
		// and only the pack's own checksum can reject the file.
		copy(recorded[:], pack[len(pack)-sha1.Size:])
	}
	index = append(index, recorded[:]...)
	indexDigest := sha1.Sum(index)
	index = append(index, indexDigest[:]...)
	if err := os.WriteFile(strings.TrimSuffix(name, ".pack")+".idx", index, 0o644); err != nil {
		t.Fatalf("write pack index: %v", err)
	}
}

func writeZlibFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	var deflated bytes.Buffer
	writer := zlib.NewWriter(&deflated)
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatalf("deflate %s: %v", path, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close deflater: %v", err)
	}
	if err := os.WriteFile(path, deflated.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A malformed loose-object candidate must not be able to size the daemon's
// allocation: the header read is bounded, while a VALID object of any size still
// streams through the hash.
func TestLooseObjectHeaderReadIsBounded(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	// 1 MiB of NUL-free content behind a hex-fanout name: no header terminator
	// inside the bound, so recognition rejects it without buffering the stream.
	writeZlibFile(t, filepath.Join(root, "cache", "ab", strings.Repeat("c", 38)), strings.Repeat("x", 1<<20))
	nested, err := nestedGitObjectDatabase(ctx, root, gitPackVerifierForTest(t))
	if err != nil {
		t.Fatalf("nestedGitObjectDatabase: %v", err)
	}
	if nested != "" {
		t.Fatalf("unterminated header classified as object database %q", nested)
	}
	// The bound is a RESOURCE guard, so the observable is how much it consumed: a
	// reader that drains a NUL-free stream is exactly the failure mode.
	stream := strings.NewReader(strings.Repeat("x", 1<<20))
	if _, err := readBoundedGitObjectHeader(bufio.NewReader(stream)); err == nil {
		t.Fatal("readBoundedGitObjectHeader accepted an unterminated header")
	}
	// One bufio refill (4 KiB) is the floor any buffered reader pays; draining the
	// whole megabyte is the defect. 64 KiB separates the two without pinning the
	// buffer size.
	const consumedLimit = 64 << 10
	if consumed := int64(1<<20) - int64(stream.Len()); consumed > consumedLimit {
		t.Fatalf("header read consumed %d bytes of a NUL-free stream, want at most %d", consumed, consumedLimit)
	}

	// A large VALID blob still streams: the bound applies to the header only.
	body := strings.Repeat("payload\n", 1<<14)
	header := fmt.Sprintf("blob %d\x00", len(body))
	digest := sha1.Sum([]byte(header + body))
	name := hex.EncodeToString(digest[:])
	writeZlibFile(t, filepath.Join(root, "objects", name[:2], name[2:]), header+body)
	nested, err = nestedGitObjectDatabase(ctx, root, gitPackVerifierForTest(t))
	if err != nil {
		t.Fatalf("nestedGitObjectDatabase after the valid object: %v", err)
	}
	if nested != "objects" {
		t.Fatalf("valid streamed object database = %q, want objects", nested)
	}
}

func runWorktreeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func runWorktreeGitEnvOutput(t *testing.T, dir string, env []string, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

type fakeWorktreeManager struct {
	err               error
	onAdd             func()
	onAddCtx          func(context.Context)
	existingBranches  map[string]bool
	fetchedRemotes    []string
	pathHeads         map[string]string
	revHeads          map[string]string
	ancestor          bool
	ancestorSet       bool
	clean             bool
	cleanSet          bool
	cleanErr          error
	remoteURL         string
	remoteURLErr      error
	refreshErr        error
	requireDeadline   bool
	refreshedPaths    []string
	refreshedURLs     []string
	cloneOnly         map[string]string // path -> unpublished sha ("" proves published)
	cloneOnlyDefault  string            // answer for paths absent from cloneOnly
	cloneOnlyErr      error
	verifiedPacks     []string
	verifyPackErr     error
	verifyPackInvalid bool
	cloneOnlyCalls    []string
	cloneOnlyHook     func(path string) // mutate the clone between proof rounds
	cleanCalls        []string
	ancestorCalls     [][2]string
	calls             []worktreeCall
	existingCalls     []worktreeCall
	detachedCalls     []worktreeCall // AddDetachedWorktree: path in .path, ref in .base
	removed           []string       // RemoveWorktree paths
	removedForce      []string       // RemoveWorktreeForce paths
	removeErr         error
	deletedBranches   []string // DeleteBranch branches
	deleteErr         error
	mergeCalls        []mergeCall // MergeBranches calls
	mergeErr          error
	committedDirs     []string // CommitWorktree dirs
	commitMade        bool     // value CommitWorktree returns for "committed"
	commitErr         error
}

type mergeCall struct {
	dir      string
	branches []string
}

type worktreeCall struct {
	branch string
	path   string
	base   string
}

func (f *fakeWorktreeManager) AddWorktree(ctx context.Context, branch string, path string, base string) error {
	// onAddCtx receives the allocation's OWN context, which is what a test needs to
	// observe the heartbeat cancelling an in-flight pre-effect (#1673).
	if f.onAddCtx != nil {
		f.onAddCtx(ctx)
	}
	if f.onAdd != nil {
		f.onAdd()
	}
	f.calls = append(f.calls, worktreeCall{branch: branch, path: path, base: base})
	if f.pathHeads != nil {
		f.pathHeads[path] = base
	}
	return f.err
}

func (f *fakeWorktreeManager) AddExistingBranchWorktree(_ context.Context, branch string, path string) error {
	if f.onAdd != nil {
		f.onAdd()
	}
	f.existingCalls = append(f.existingCalls, worktreeCall{branch: branch, path: path})
	return f.err
}

func (f *fakeWorktreeManager) BranchExists(_ context.Context, branch string) (bool, error) {
	return f.existingBranches[branch], nil
}

func (f *fakeWorktreeManager) FetchRemote(_ context.Context, remote string) error {
	f.fetchedRemotes = append(f.fetchedRemotes, remote)
	return nil
}

func (f *fakeWorktreeManager) HeadSHAAt(_ context.Context, path string) (string, error) {
	if head := f.pathHeads[path]; head != "" {
		return head, nil
	}
	return "existing-head", nil
}

func (f *fakeWorktreeManager) RevParse(_ context.Context, rev string) (string, error) {
	if head := f.revHeads[rev]; head != "" {
		return head, nil
	}
	return rev + "-head", nil
}

func (f *fakeWorktreeManager) IsAncestor(_ context.Context, ancestor, descendant string) (bool, error) {
	f.ancestorCalls = append(f.ancestorCalls, [2]string{ancestor, descendant})
	if f.ancestorSet {
		return f.ancestor, nil
	}
	return true, nil
}

func (f *fakeWorktreeManager) WorktreeCleanAt(_ context.Context, path string) (bool, error) {
	f.cleanCalls = append(f.cleanCalls, path)
	if f.cleanErr != nil {
		return false, f.cleanErr
	}
	if f.cleanSet {
		return f.clean, nil
	}
	return true, nil
}

func (f *fakeWorktreeManager) WorktreePristineAt(ctx context.Context, path string) (bool, error) {
	return f.WorktreeCleanAt(ctx, path)
}

func (f *fakeWorktreeManager) RemoteURL(_ context.Context, remote string) (string, error) {
	if f.remoteURLErr != nil {
		return "", f.remoteURLErr
	}
	if f.remoteURL != "" {
		return f.remoteURL, nil
	}
	return "https://example.invalid/" + remote + ".git", nil
}

func (f *fakeWorktreeManager) RefreshCloneProofRefs(ctx context.Context, path string, remoteURL string) error {
	if f.requireDeadline {
		if _, ok := ctx.Deadline(); !ok {
			return errors.New("trusted remote refresh has no deadline")
		}
	}
	f.refreshedPaths = append(f.refreshedPaths, path)
	f.refreshedURLs = append(f.refreshedURLs, remoteURL)
	return f.refreshErr
}

// VerifyPackIndex mirrors Git's pack validation. The fake accepts by default and
// records what it was asked to verify, so a test can prove the production path
// consults it and can make it refuse.
func (f *fakeWorktreeManager) VerifyPackIndex(_ context.Context, indexPath string, objectFormat string) (bool, error) {
	f.verifiedPacks = append(f.verifiedPacks, indexPath+" "+objectFormat)
	if f.verifyPackErr != nil {
		return false, f.verifyPackErr
	}
	return !f.verifyPackInvalid, nil
}

func (f *fakeWorktreeManager) CloneOnlyCommit(_ context.Context, path string) (string, error) {
	f.cloneOnlyCalls = append(f.cloneOnlyCalls, path)
	if f.cloneOnlyHook != nil {
		f.cloneOnlyHook(path)
	}
	if f.cloneOnlyErr != nil {
		return "", f.cloneOnlyErr
	}
	if sha, ok := f.cloneOnly[path]; ok {
		return sha, nil
	}
	return f.cloneOnlyDefault, nil
}

func (f *fakeWorktreeManager) AddDetachedWorktree(ctx context.Context, path string, ref string) error {
	// onAddCtx receives the allocation's OWN context, which is what a test needs to
	// observe the heartbeat cancelling an in-flight pre-effect (#1673). Detached
	// allocation is the only external pre-effect left since #2203 removed the
	// implement leg's writable worktree, so it carries the hook too.
	if f.onAddCtx != nil {
		f.onAddCtx(ctx)
	}
	if f.onAdd != nil {
		f.onAdd()
	}
	f.detachedCalls = append(f.detachedCalls, worktreeCall{path: path, base: ref})
	return f.err
}

func (f *fakeWorktreeManager) MergeBranches(_ context.Context, dir string, branches []string, _ string) error {
	f.mergeCalls = append(f.mergeCalls, mergeCall{dir: dir, branches: branches})
	return f.mergeErr
}

func (f *fakeWorktreeManager) CommitWorktree(_ context.Context, dir string, _ string) (bool, error) {
	f.committedDirs = append(f.committedDirs, dir)
	return f.commitMade, f.commitErr
}

func (f *fakeWorktreeManager) RemoveWorktreeForce(_ context.Context, path string) error {
	f.removedForce = append(f.removedForce, path)
	return f.removeErr
}

func (f *fakeWorktreeManager) RemoveWorktree(_ context.Context, path string) error {
	f.removed = append(f.removed, path)
	return f.removeErr
}

func (f *fakeWorktreeManager) DeleteBranch(_ context.Context, branch string) error {
	f.deletedBranches = append(f.deletedBranches, branch)
	return f.deleteErr
}

func TestTaskWorktreePathSegmentSanitizesSlashedIDs(t *testing.T) {
	// Backward-compatible: already-safe values are returned unchanged so existing
	// worktree paths never move.
	for _, v := range []string{"local-ask-lead-abc123", "task-1", "owner_repo", "a.b-c_d"} {
		got, err := taskWorktreePathSegment(v, "x")
		if err != nil {
			t.Fatalf("safe value %q errored: %v", v, err)
		}
		if got != v {
			t.Fatalf("safe value %q changed to %q (must be byte-identical)", v, got)
		}
	}

	// A coordinator continuation parent id (contains '/') must no longer error;
	// it sanitizes to a single, path-safe, traversal-safe, deterministic segment.
	id := "local-ask-lead-abc123/continuation/continuation"
	got, err := taskWorktreePathSegment(id, "parent job id")
	if err != nil {
		t.Fatalf("slashed continuation id errored (the bug this fixes): %v", err)
	}
	if strings.ContainsAny(got, `/\`) {
		t.Fatalf("sanitized segment %q still contains a path separator", got)
	}
	if got == "." || got == ".." || strings.HasPrefix(got, ".") {
		t.Fatalf("sanitized segment %q is not traversal-safe", got)
	}
	if again, _ := taskWorktreePathSegment(id, "parent job id"); got != again {
		t.Fatalf("not deterministic: %q vs %q", got, again)
	}

	// DelegationWorktreePath (the real caller) now succeeds for a slashed parent.
	p, err := DelegationWorktreePath("/h", "o/r", id, "impl", 0)
	if err != nil {
		t.Fatalf("DelegationWorktreePath rejected slashed parent (the bug): %v", err)
	}
	if !strings.Contains(p, got) {
		t.Fatalf("path %q missing sanitized parent segment %q", p, got)
	}

	// Distinct unsafe ids that collapse to the same prefix must NOT collide.
	a, _ := taskWorktreePathSegment("x/y", "p")
	b, _ := taskWorktreePathSegment("x:y", "p")
	if a == b {
		t.Fatalf("distinct unsafe ids collided: both -> %q", a)
	}

	// "." and ".." sanitize to a safe segment rather than being usable as traversal.
	for _, dotted := range []string{".", ".."} {
		seg, err := taskWorktreePathSegment(dotted, "p")
		if err != nil {
			t.Fatalf("%q errored: %v", dotted, err)
		}
		if seg == "." || seg == ".." || strings.ContainsAny(seg, `/\`) {
			t.Fatalf("%q produced unsafe segment %q", dotted, seg)
		}
	}

	// Empty / whitespace still errors.
	if _, err := taskWorktreePathSegment("   ", "p"); err == nil {
		t.Fatalf("blank value should still error")
	}
}

// gitPackVerifierForTest runs the REAL git verification the production path uses,
// so a recognition test is not silently weaker than the daemon. Without git on the
// host it returns nil, which is the conservative fallback production also takes.
func gitPackVerifierForTest(t *testing.T) packIndexVerifier {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	client := gitutil.NewHostClient(t.TempDir())
	return client.VerifyPackIndex
}

// seedWorktreeOwner creates a job that records worktreePath, used to build the
// "another row holds this path" situation the reclaim guard exists for.
func seedWorktreeOwner(t *testing.T, store *db.Store, jobID, state, worktreePath string) {
	t.Helper()
	payload, err := marshalPayload(JobPayload{
		Repo:         "owner/repo",
		WorktreePath: worktreePath,
		DelegationID: "seed-owner",
		Branch:       "seed-branch",
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID:      jobID,
		Agent:   "fixer",
		Type:    "implement",
		State:   state,
		Repo:    "owner/repo",
		Payload: payload,
	}, db.JobEvent{Kind: state, Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", jobID, err)
	}
}

func TestEngineReclaimSkipsDegenerateWorktreePath(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedWorktreeOwner(t, store, "fix-degenerate", string(JobSucceeded), ".")

	reclaimed, err := testEngine(store).ReclaimAgedTerminalDelegationWorktreeOutcome(
		ctx, "fix-degenerate", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("degenerate candidate returned an operational error: %v", err)
	}
	if reclaimed {
		t.Fatal("degenerate candidate reported a reclaimed worktree")
	}
}

// TestEngineReclaimRefusesWhenAnotherJobStillHoldsThePath is the guard's whole
// purpose, and it was UNTESTED: a mutant that ignored a non-final co-owner
// survived the suite.
//
// A deterministic worktree path recurs across historical rows. If an aged
// terminal row reclaims a path a live job still holds, it deletes a running
// job's checkout. #2149 changed how the co-owners are FOUND - the payloads are
// no longer decoded - so the property has to be pinned before that change can
// be trusted, not after.
