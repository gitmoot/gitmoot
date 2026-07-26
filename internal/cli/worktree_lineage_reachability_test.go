package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type dirtyTaskLineageFixture struct {
	home     string
	checkout string
	worktree string
	task     db.Task
	store    *db.Store
}

func newDirtyTaskLineageFixture(t *testing.T, offLineage bool) dirtyTaskLineageFixture {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	checkout := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatalf("MkdirAll checkout: %v", err)
	}
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot Test")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	writeFile(t, filepath.Join(checkout, "README.md"), "initial\n")
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")

	store := openCLIJobStore(t, home)
	paths := config.PathsForHome(home)
	worktree, err := workflow.TaskWorktreePath(paths.Home, "owner/repo", "task-lineage")
	if err != nil {
		store.Close()
		t.Fatalf("TaskWorktreePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		store.Close()
		t.Fatalf("MkdirAll worktree parent: %v", err)
	}
	runGit(t, checkout, "worktree", "add", "-b", "feature/lineage", worktree, "main")
	if offLineage {
		writeFile(t, filepath.Join(checkout, "base-moved.txt"), "new base\n")
		runGit(t, checkout, "add", "base-moved.txt")
		runGit(t, checkout, "commit", "-m", "advance base")
	}
	writeFile(t, filepath.Join(worktree, "salvage.txt"), "uncommitted salvage\n")

	task := db.Task{
		ID:           "task-lineage",
		RepoFullName: "owner/repo",
		GoalID:       "goal-lineage",
		Title:        "Lineage reachability",
		State:        string(workflow.TaskImplementing),
		Branch:       "feature/lineage",
		WorktreePath: worktree,
	}
	if err := store.UpsertTask(ctx, task); err != nil {
		store.Close()
		t.Fatalf("UpsertTask: %v", err)
	}
	if err := store.UpsertRepo(ctx, db.Repo{
		Owner:         "owner",
		Name:          "repo",
		DefaultBranch: "main",
		RemoteURL:     "https://github.com/owner/repo.git",
		CheckoutPath:  checkout,
		PollInterval:  "30s",
	}); err != nil {
		store.Close()
		t.Fatalf("UpsertRepo: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "lead",
		Runtime:        "shell",
		RuntimeRef:     "true",
		RepoScope:      "owner/repo",
		Capabilities:   []string{"implement"},
		AutonomyPolicy: "workspace-write",
		HealthStatus:   "ok",
	}); err != nil {
		store.Close()
		t.Fatalf("UpsertAgent: %v", err)
	}
	return dirtyTaskLineageFixture{
		home:     home,
		checkout: checkout,
		worktree: worktree,
		task:     task,
		store:    store,
	}
}

func TestPrepareLocalImplementDispatchRequestReconcilesDirtyWorktreeLineage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		offLineage  bool
		wantBlocked bool
	}{
		{name: "on-lineage keeps existing recovery guidance"},
		{name: "off-lineage blocks and journals", offLineage: true, wantBlocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newDirtyTaskLineageFixture(t, tc.offLineage)
			defer fixture.store.Close()

			_, _, err := prepareLocalImplementDispatchRequest(
				context.Background(),
				fixture.store,
				db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: fixture.checkout},
				github.Repository{Owner: "owner", Name: "repo"},
				localAgentDispatchRequest{
					Home:          fixture.home,
					Agent:         "lead",
					Action:        "implement",
					Instructions:  "Continue implementation.",
					Branch:        fixture.task.Branch,
					ImplementBase: "HEAD",
				},
			)
			if err == nil {
				t.Fatal("prepareLocalImplementDispatchRequest succeeded for dirty worktree")
			}

			stored, getErr := fixture.store.GetTask(context.Background(), fixture.task.ID)
			if getErr != nil {
				t.Fatalf("GetTask: %v", getErr)
			}
			events, eventsErr := fixture.store.ListTaskEvents(context.Background(), fixture.task.ID)
			if eventsErr != nil {
				t.Fatalf("ListTaskEvents: %v", eventsErr)
			}
			if tc.wantBlocked {
				var blocked workflow.BlockedError
				if !errors.As(err, &blocked) {
					t.Fatalf("error = %v, want BlockedError", err)
				}
				if stored.State != string(workflow.TaskBlocked) {
					t.Fatalf("task state = %q, want blocked", stored.State)
				}
				if len(events) != 1 || events[0].Kind != "stale_worktree_dirty_blocked" {
					t.Fatalf("task events = %+v", events)
				}
				for _, want := range []string{"is stale", "uncommitted changes", "manually salvage"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("blocked error %q missing %q", err, want)
					}
				}
				if strings.Contains(err.Error(), "gitmoot task recover") {
					t.Fatalf("off-lineage error points at stale-branch recovery: %v", err)
				}
			} else {
				if !strings.Contains(err.Error(), "gitmoot task recover task-lineage") {
					t.Fatalf("on-lineage error changed existing recovery guidance: %v", err)
				}
				if stored.State != fixture.task.State {
					t.Fatalf("on-lineage task state = %q, want %q", stored.State, fixture.task.State)
				}
				if len(events) != 0 {
					t.Fatalf("on-lineage task events = %+v, want none", events)
				}
			}
			if content, readErr := os.ReadFile(filepath.Join(fixture.worktree, "salvage.txt")); readErr != nil || string(content) != "uncommitted salvage\n" {
				t.Fatalf("salvage file = %q, %v", content, readErr)
			}
			if _, lockErr := fixture.store.GetBranchLock(context.Background(), "owner/repo", fixture.task.Branch); !errors.Is(lockErr, sql.ErrNoRows) {
				t.Fatalf("dirty preflight created branch lock: %v", lockErr)
			}
		})
	}
}

func TestRunTaskRunReconcilesDirtyWorktreeLineage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		offLineage  bool
		wantBlocked bool
	}{
		{name: "on-lineage keeps existing recovery guidance"},
		{name: "off-lineage blocks and journals", offLineage: true, wantBlocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newDirtyTaskLineageFixture(t, tc.offLineage)
			if err := fixture.store.Close(); err != nil {
				t.Fatalf("close seed store: %v", err)
			}
			withWorkingDirectory(t, fixture.checkout)

			var stdout, stderr bytes.Buffer
			code := Run([]string{
				"task", "run", fixture.task.ID,
				"--home", fixture.home,
				"--repo", "owner/repo",
				"--owner", "lead",
				"--base", "HEAD",
			}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("task run code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}

			store := openCLIJobStore(t, fixture.home)
			defer store.Close()
			stored, err := store.GetTask(context.Background(), fixture.task.ID)
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			events, err := store.ListTaskEvents(context.Background(), fixture.task.ID)
			if err != nil {
				t.Fatalf("ListTaskEvents: %v", err)
			}
			if tc.wantBlocked {
				if stored.State != string(workflow.TaskBlocked) {
					t.Fatalf("task state = %q, want blocked", stored.State)
				}
				if len(events) != 1 || events[0].Kind != "stale_worktree_dirty_blocked" {
					t.Fatalf("task events = %+v", events)
				}
				for _, want := range []string{"is stale", "uncommitted changes", "manually salvage"} {
					if !strings.Contains(stderr.String(), want) {
						t.Fatalf("blocked stderr %q missing %q", stderr.String(), want)
					}
				}
				if strings.Contains(stderr.String(), "gitmoot task recover") {
					t.Fatalf("off-lineage stderr points at stale-branch recovery: %s", stderr.String())
				}
			} else {
				if !strings.Contains(stderr.String(), "gitmoot task recover task-lineage") {
					t.Fatalf("on-lineage stderr changed existing recovery guidance: %s", stderr.String())
				}
				if stored.State != fixture.task.State {
					t.Fatalf("on-lineage task state = %q, want %q", stored.State, fixture.task.State)
				}
				if len(events) != 0 {
					t.Fatalf("on-lineage task events = %+v, want none", events)
				}
			}
			if content, readErr := os.ReadFile(filepath.Join(fixture.worktree, "salvage.txt")); readErr != nil || string(content) != "uncommitted salvage\n" {
				t.Fatalf("salvage file = %q, %v", content, readErr)
			}
			if _, lockErr := store.GetBranchLock(context.Background(), "owner/repo", fixture.task.Branch); !errors.Is(lockErr, sql.ErrNoRows) {
				t.Fatalf("dirty preflight created branch lock: %v", lockErr)
			}
		})
	}
}
