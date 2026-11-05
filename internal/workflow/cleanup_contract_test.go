package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
			assertRetained := func(state string) {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(path, "result.txt"))
				if err != nil || string(data) != "producer result\n" {
					t.Fatalf("%s consumer lost producer contents: %q, %v", state, data, err)
				}
				if exists, err := manager.BranchExists(ctx, branch); err != nil || !exists {
					t.Fatalf("%s consumer lost producer branch: exists=%v err=%v", state, exists, err)
				}
			}
			cleanup()
			assertRetained("undispatched")
			if _, err := store.GetJob(ctx, consumer); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("cleanup changed the undispatched consumer: %v", err)
			}
			insertCompletedJob(t, store, db.Job{ID: consumer, Agent: "consumer", Type: "review", ParentJobID: parent, DelegationID: "integrate"}, JobPayload{
				Repo: "owner/repo", ParentJobID: parent, DelegationID: "integrate", Deps: []string{"produce"},
				Result: &AgentResult{Decision: "approved"},
			})
			for _, state := range []JobState{JobBlocked, JobQueued} {
				if err := store.UpdateJobState(ctx, consumer, string(state)); err != nil {
					t.Fatal(err)
				}
				cleanup()
				assertRetained(string(state))
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

func TestCleanupRefusesUnreadableRuntimeOwner(t *testing.T) {
	for _, aged := range []bool{false, true} {
		name := "ordinary"
		if aged {
			name = "aged"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			world := newProducerCleanupFixture(t, false)
			policy := CleanupPolicy{}
			if aged {
				policy = CleanupPolicy{Mode: "aged", cutoff: time.Now().Add(time.Hour)}
			}
			decision, err := world.engine.InspectProducerCleanup(ctx, world.jobID, policy)
			if err != nil || !decision.Eligible {
				t.Fatalf("healthy owner is not eligible: %+v %v", decision, err)
			}
			raw, err := sql.Open("sqlite", world.store.DatabasePath())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = raw.Close() })
			key, token := db.RuntimeSessionLockKeyPrefix+"cleanup-query-fault", "owner-token"
			acquired, err := world.store.AcquireResourceLock(ctx, db.ResourceLock{
				ResourceKey: key, OwnerJobID: world.jobID, OwnerToken: token,
				ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
			}, time.Now().UTC())
			if err != nil || !acquired {
				t.Fatalf("runtime lock fixture: acquired=%t err=%v", acquired, err)
			}
			// SQLite accepts a text value in this INTEGER column, but the runtime
			// liveness query cannot scan it into the PID. Other store paths work.
			if _, err := raw.ExecContext(ctx, `UPDATE resource_locks SET owner_pid = 'unreadable' WHERE resource_key = ?`, key); err != nil {
				t.Fatal(err)
			}
			decision, err = world.engine.InspectProducerCleanup(ctx, world.jobID, policy)
			if err != nil || decision.Eligible || !slices.Contains(decision.ReasonCodes, "runtime_owner_unknown") {
				t.Fatalf("unreadable runtime owner was not refused: %+v %v", decision, err)
			}
			cleanup := func() {
				t.Helper()
				var err error
				if aged {
					_, err = world.engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, world.jobID, policy.cutoff)
				} else {
					err = world.engine.cleanupImplementDelegationWorktree(ctx, world.jobID, "implement", world.payload)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			cleanup()
			world.assertPreserved(t)
			if released, err := world.store.ReleaseResourceLock(ctx, key, world.jobID, token); err != nil || !released {
				t.Fatalf("clear runtime fault: released=%t err=%v", released, err)
			}
			now := time.Now().Add(2 * time.Minute)
			world.engine.Now = func() time.Time { return now }
			cleanup()
			world.assertRemoved(t)
		})
	}
}

func TestCleanupRetainsServiceProducerWithoutConsumerEvidence(t *testing.T) {
	for _, aged := range []bool{false, true} {
		name := "ordinary"
		if aged {
			name = "aged"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			world := newProducerCleanupFixture(t, false)
			// Service stage jobs need not carry PipelineName; their canonical
			// sender still requires durable pipeline consumer evidence.
			world.payload.Sender = PipelineJobSender
			world.payload.RootJobID = "psr-0123456789abcdef0123456789abcdef"
			encoded, err := json.Marshal(world.payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := world.store.UpdateJobPayload(ctx, world.jobID, string(encoded)); err != nil {
				t.Fatal(err)
			}
			policy := CleanupPolicy{}
			if aged {
				policy = CleanupPolicy{Mode: "aged", cutoff: time.Now().Add(time.Hour)}
			}
			inspection, err := world.engine.InspectProducerCleanup(ctx, world.jobID, policy)
			if err != nil || inspection.Eligible || !slices.Contains(inspection.ReasonCodes, "pipeline_consumers_unknown") {
				t.Fatalf("missing service consumer evidence was not refused: %+v %v", inspection, err)
			}
			if aged {
				_, err = world.engine.ReclaimAgedTerminalDelegationWorktreeOutcome(ctx, world.jobID, policy.cutoff)
			} else {
				err = world.engine.cleanupImplementDelegationWorktree(ctx, world.jobID, "implement", world.payload)
			}
			if err != nil {
				t.Fatal(err)
			}
			world.assertPreserved(t)
		})
	}
}

// producerCleanupFixture uses production ownership validation and real Git
// resources. Each test owns every workspace user; runtime tests vary that proof.
type producerCleanupFixture struct {
	store     *db.Store
	engine    Engine
	manager   gitutil.Client
	jobID     string
	path      string
	branch    string
	preserved string
	payload   JobPayload
}

func newProducerCleanupFixture(t *testing.T, readOnly bool) producerCleanupFixture {
	t.Helper()
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
	engine := testEngine(store)
	engine.Home = t.TempDir()
	engine.DelegationCheckout, engine.DelegationWorktrees = checkout, manager
	engine.cleanupTargetValidator = nil
	engine.WorktreeHasLiveProcess = nil
	engine.WorktreeLiveness = func(string) (bool, bool) { return false, true }
	// A FIXTURE cap for the budget-gated consumer path (#2360 v2); never a live default.
	engine.CleanupBudget = fixtureCleanupBudget(1 << 30)
	world := producerCleanupFixture{store: store, engine: engine, manager: manager,
		jobID: "cleanup-parent/delegation/produce", branch: "gitmoot-delegation-cleanup-produce",
		preserved: filepath.Join(t.TempDir(), "preserved-result.txt")}
	action, owner, segment := "implement", "cleanup-parent", "produce"
	world.payload = JobPayload{Repo: "owner/repo", ParentJobID: owner, DelegationID: segment,
		Branch: world.branch, Result: &AgentResult{Decision: "implemented", Summary: "producer result"}}
	if readOnly {
		action, owner, segment = "review", "cleanup-review", "readonly-seat"
		world.jobID, world.branch = owner, ""
		world.payload = JobPayload{Repo: "owner/repo", PullRequest: 7,
			ReadOnlyWorktree: true, ReadOnlySeat: true,
			Result: &AgentResult{Decision: "approved", Summary: "producer result"}}
		head, err := manager.RevParse(ctx, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		world.payload.HeadSHA = head
	} else {
		insertCompletedJob(t, store, db.Job{ID: owner, Agent: "coordinator", Type: "ask"},
			JobPayload{Repo: "owner/repo", Result: &AgentResult{Decision: "approved"}})
	}
	path, err := DelegationWorktreePath(engine.Home, "owner/repo", owner, segment, 0)
	if err != nil {
		t.Fatal(err)
	}
	world.path, world.payload.WorktreePath = path, path
	if readOnly {
		runWorktreeGit(t, checkout, "worktree", "add", "--detach", path, "HEAD")
	} else {
		runWorktreeGit(t, checkout, "worktree", "add", "-b", world.branch, path, "HEAD")
	}
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: checkout}); err != nil {
		t.Fatal(err)
	}
	insertCompletedJob(t, store, db.Job{ID: world.jobID, Agent: "producer", Type: action,
		ParentJobID: world.payload.ParentJobID, DelegationID: world.payload.DelegationID}, world.payload)
	if readOnly {
		preserved := world.preserved
		world.engine.BeforeReadOnlyWorktreeCleanup = func(_ context.Context, _, _ string, payload JobPayload) error {
			content, err := os.ReadFile(filepath.Join(payload.WorktreePath, "result.txt"))
			if err != nil {
				return err
			}
			return os.WriteFile(preserved, content, 0600)
		}
	}
	return world
}

func (world producerCleanupFixture) assertPreserved(t *testing.T) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(world.path, "result.txt"))
	if err != nil || string(content) != "producer result\n" {
		t.Fatalf("producer contents changed: %q %v", content, err)
	}
	if world.branch != "" {
		if exists, err := world.manager.BranchExists(context.Background(), world.branch); err != nil || !exists {
			t.Fatalf("producer branch lost: exists=%t err=%v", exists, err)
		}
	}
}

func (world producerCleanupFixture) assertRemoved(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(world.path); !os.IsNotExist(err) {
		t.Fatalf("producer tree still exists: %v", err)
	}
	if world.branch != "" {
		if exists, err := world.manager.BranchExists(context.Background(), world.branch); err != nil || exists {
			t.Fatalf("producer branch remains: exists=%t err=%v", exists, err)
		}
	} else {
		content, err := os.ReadFile(world.preserved)
		if err != nil || string(content) != "producer result\n" {
			t.Fatalf("preserved output unavailable after disposal: %q %v", content, err)
		}
	}
	obligation, err := world.store.GetCleanupObligation(context.Background(), db.CleanupObligationResourceID(world.jobID, world.path))
	if err != nil || obligation.State != db.CleanupObligationRemoved {
		t.Fatalf("cleanup outcome not durable: %+v %v", obligation, err)
	}
}

// fixtureCleanupBudget is a test-only approved cap with explicit sizing bounds (#2360 v2).
func fixtureCleanupBudget(limit int64) func(string) (*ApprovedCleanupSizeBudget, *CleanupSizeLimits, error) {
	return func(string) (*ApprovedCleanupSizeBudget, *CleanupSizeLimits, error) {
		return &ApprovedCleanupSizeBudget{Unit: CleanupSizeUnitAllocatedBytes, LimitBytes: limit, Owner: "fixture-owner", Approval: "fixture-only"},
			&CleanupSizeLimits{MaxEntries: 100_000, MaxDepth: 64, TimeoutMS: 30_000}, nil
	}
}

// #2525: an ordinary cleanup of a review that did not succeed is refused as
// retry_disposition_unknown whatever the worktree holds, but the slow
// pre-cleanup evidence capture (10-45 s on the sshfs seats) ran first, and the
// reclaim pass retried a minute later, so the same refused rows were captured
// over and over and held back the rows that could be removed. The refusal must
// be decided before any capture, and the retry pushed back.
func TestRefusedCleanupSkipsTheCaptureAndBacksOff(t *testing.T) {
	world := newProducerCleanupFixture(t, true)
	ctx := context.Background()
	if err := world.store.UpdateJobState(ctx, world.jobID, string(JobFailed)); err != nil {
		t.Fatal(err)
	}
	if _, err := world.store.EnsureCleanupObligation(ctx, world.jobID, world.path, time.Now().UTC().Add(-8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	captured := 0
	world.engine.BeforeReadOnlyWorktreeCleanup = func(context.Context, string, string, JobPayload) error {
		captured++
		return nil
	}
	removed, err := world.engine.ReclaimTerminalDelegationWorktreeOutcome(ctx, world.jobID)
	if err != nil || removed {
		t.Fatalf("ordinary cleanup of a failed review: removed=%t err=%v, want kept", removed, err)
	}
	if captured != 0 {
		t.Fatalf("the capture ran %d times for a cleanup that is always refused", captured)
	}
	if _, err := os.Lstat(world.path); err != nil {
		t.Fatalf("the worktree was touched: %v", err)
	}
	obligation, err := world.store.GetCleanupObligation(ctx, db.CleanupObligationResourceID(world.jobID, world.path))
	if err != nil {
		t.Fatal(err)
	}
	next, err := time.Parse(time.RFC3339Nano, obligation.NextAttemptAt)
	if err != nil {
		t.Fatal(err)
	}
	if wait := time.Until(next); wait < 90*time.Minute {
		t.Fatalf("retry in %s after an 8-hour-old refused cleanup, want about 2h (a quarter of its age)", wait.Round(time.Minute))
	}
}
