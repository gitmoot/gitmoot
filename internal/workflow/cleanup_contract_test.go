package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
)

func TestCleanupPreservesResumableConsumerUntilSucceeded(t *testing.T) {
	for _, aged := range []bool{false, true} {
		name := "ordinary"
		if aged {
			name = "aged"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			checkout := t.TempDir()
			runWorktreeGit(t, checkout, "init", "--initial-branch=main")
			runWorktreeGit(t, checkout, "config", "user.email", "test@example.com")
			runWorktreeGit(t, checkout, "config", "user.name", "Test")
			if err := os.WriteFile(filepath.Join(checkout, "result.txt"), []byte("producer result\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runWorktreeGit(t, checkout, "add", "result.txt")
			runWorktreeGit(t, checkout, "commit", "-m", "producer result")
			manager := gitutil.NewHostClient(checkout)
			engine := Engine{
				Store: store, Home: t.TempDir(), DelegationCheckout: checkout,
				DelegationWorktrees: manager,
				WorktreeLiveness:    func(string) (bool, bool) { return false, true },
			}
			const parent = "cleanup-parent"
			const producer = parent + "/delegation/produce"
			const consumer = parent + "/delegation/integrate"
			const branch = "gitmoot-delegation-cleanup-produce"
			path, err := DelegationWorktreePath(engine.Home, "owner/repo", parent, "produce", 0)
			if err != nil {
				t.Fatal(err)
			}
			runWorktreeGit(t, checkout, "worktree", "add", "-b", branch, path, "HEAD")
			insertCompletedJob(t, store, db.Job{ID: parent, Agent: "coord", Type: "ask"}, JobPayload{
				Repo: "owner/repo", Result: &AgentResult{Decision: "approved", Delegations: []Delegation{
					{ID: "produce", Action: "implement", Prompt: "produce"},
					{ID: "integrate", Action: "review", Prompt: "integrate", Deps: []string{"produce"}},
				}},
			})
			payload := JobPayload{Repo: "owner/repo", ParentJobID: parent, DelegationID: "produce",
				WorktreePath: path, Branch: branch, Result: &AgentResult{Decision: "implemented", Summary: "producer result"}}
			insertCompletedJob(t, store, db.Job{ID: producer, Agent: "producer", Type: "implement", ParentJobID: parent, DelegationID: "produce"}, payload)
			insertCompletedJob(t, store, db.Job{ID: consumer, Agent: "consumer", Type: "review", ParentJobID: parent, DelegationID: "integrate"}, JobPayload{
				Repo: "owner/repo", ParentJobID: parent, DelegationID: "integrate", Deps: []string{"produce"},
				Result: &AgentResult{Decision: "approved"},
			})
			cleanup := func() {
				t.Helper()
				var err error
				if aged {
					_, err = engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, producer, time.Now().Add(time.Hour))
				} else {
					err = engine.cleanupImplementDelegationWorktree(ctx, producer, "implement", payload)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, state := range []JobState{JobBlocked, JobQueued} {
				if err := store.UpdateJobState(ctx, consumer, string(state)); err != nil {
					t.Fatal(err)
				}
				cleanup()
				data, err := os.ReadFile(filepath.Join(path, "result.txt"))
				if err != nil || string(data) != "producer result\n" {
					t.Fatalf("%s consumer lost producer contents: %q, %v", state, data, err)
				}
				if exists, err := manager.BranchExists(ctx, branch); err != nil || !exists {
					t.Fatalf("%s consumer lost producer branch: exists=%v err=%v", state, exists, err)
				}
			}
			if err := store.UpdateJobState(ctx, consumer, string(JobSucceeded)); err != nil {
				t.Fatal(err)
			}
			cleanup()
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("completed consumer should release producer tree: %v", err)
			}
			if exists, err := manager.BranchExists(ctx, branch); err != nil || exists {
				t.Fatalf("completed consumer should release producer branch: exists=%v err=%v", exists, err)
			}
		})
	}
}
