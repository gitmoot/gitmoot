package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1250. Native review fanout children were enqueued with NO org attribution —
// measured at 0 of 99 workflow-* jobs on the live store, a total gap rather than
// a thin one. Attribution is what event_sink/event_rule_sink use as the WAKE
// TARGET ROLE, so an unattributed job's blocked event has no owner to wake: the
// same machinery behind the dropped wake rows of #1347. #1250 and #1347 are one
// defect seen from two ends.
//
// READER 1 of 2: the in-process PR-open trigger. It takes the role from the
// branch lock — the single durable writer — so it and the daemon PR-watcher
// cannot drift apart the way two independently-populated constructors would.
func TestInProcessPROpenAttributesFanoutChildrenFromBranchLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "reviewer", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"reviewer"}

	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName:  "gitmoot/gitmoot",
		Branch:        "task-20",
		Owner:         "lead",
		ActingOrgRole: "gmc-fanout",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	insertCompletedJob(t, store, db.Job{
		ID:    "attributed-implement",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-20",
		PullRequest: 20,
		HeadSHA:     "head020",
		TaskID:      "task-20",
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "implemented", Summary: "opened PR"},
	})

	if err := engine.AdvanceJob(ctx, "attributed-implement"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	assertFanoutChildRole(t, ctx, store, "gmc-fanout")
}

// Legacy polarity, mandatory per ruling 20630 condition (1). A branch lock that
// predates the migration carries an EMPTY role. The reader must degrade to
// today's behaviour — the fanout still runs, the child is simply unattributed,
// and nothing crashes or invents an attribution it was never given.
func TestInProcessPROpenLeavesFanoutChildrenUnattributedForLegacyLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "reviewer", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"reviewer"}

	// No ActingOrgRole — exactly what a pre-migration row backfills to.
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: "gitmoot/gitmoot",
		Branch:       "task-21",
		Owner:        "lead",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	insertCompletedJob(t, store, db.Job{
		ID:    "legacy-implement",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-21",
		PullRequest: 21,
		HeadSHA:     "head021",
		TaskID:      "task-21",
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "implemented", Summary: "opened PR"},
	})

	if err := engine.AdvanceJob(ctx, "legacy-implement"); err != nil {
		t.Fatalf("legacy lock broke the advance instead of degrading: %v", err)
	}

	// The fanout still happens — behaviour is unchanged, only attribution is absent.
	assertFanoutChildRole(t, ctx, store, "")
}

// The single WRITER: attribution is stamped when the branch is taken, and is
// never rewritten afterwards. One writer is what makes the two readers unable to
// disagree, so this guards the property the whole design rests on.
func TestBranchLockStampsActingOrgRoleAtCreation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName:  "gitmoot/gitmoot",
		Branch:        "task-22",
		Owner:         "lead",
		ActingOrgRole: "gmc-fanout",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	lock, err := store.GetBranchLock(ctx, "gitmoot/gitmoot", "task-22")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if lock.ActingOrgRole != "gmc-fanout" {
		t.Fatalf("lock creation did not stamp the acting org role: %+v", lock)
	}
}

func assertFanoutChildRole(t *testing.T, ctx context.Context, store *db.Store, want string) {
	t.Helper()
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	found := false
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		found = true
		payload, err := ParseJobPayload(job.Payload)
		if err != nil {
			t.Fatalf("ParseJobPayload returned error: %v", err)
		}
		if payload.ActingOrgRole != want {
			t.Fatalf("fanout child acting_org_role = %q, want %q; an unattributed fanout child has no owner to wake (#1347): %+v", payload.ActingOrgRole, want, payload)
		}
	}
	if !found {
		t.Fatal("no review job was enqueued; cannot assert attribution")
	}
}

// #1250 finding 1 (g7-review). The "single writer" claim was FALSE as first
// written. Worktree allocation (AllocateTaskWorktree / AllocateDelegationWorktree)
// creates the branch lock BEFORE the role-aware ensureBranchLock runs on the same
// dispatch, and because CreateLock was INSERT OR IGNORE the blank attribution was
// frozen forever — the role-bearing caller could never repair it.
//
// The fix keeps ONE writer and lets it COMPLETE a row it already created:
// same-owner, blank-only fill. This models the real sequence.
func TestBlankLockFromWorktreeAllocationIsRepairedByTheRoleAwarePath(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)

	// Step 1: worktree allocation takes the branch with no role available.
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: "gitmoot/gitmoot", Branch: "task-23", Owner: "lead",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock (worktree) returned acquired=%v err=%v", acquired, err)
	}
	// Step 2: the role-aware path runs for the SAME dispatch and owner.
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: "gitmoot/gitmoot", Branch: "task-23", Owner: "lead", ActingOrgRole: "gmc-fanout",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock (role-aware) returned acquired=%v err=%v", acquired, err)
	}

	lock, err := store.GetBranchLock(ctx, "gitmoot/gitmoot", "task-23")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if lock.ActingOrgRole != "gmc-fanout" {
		t.Fatalf("fresh worktree lock preempted attribution: acting_org_role=%q, want gmc-fanout", lock.ActingOrgRole)
	}
	if lock.Owner != "lead" {
		t.Fatalf("repair changed the owner: %+v", lock)
	}
}

// The repair must never RELABEL: an existing non-blank role is not overwritten,
// and a different owner cannot fill another owner's blank. Without these the
// blank-fill would be a genuine second writer rather than the same one finishing.
func TestLockAttributionRepairNeverRelabels(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)

	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: "gitmoot/gitmoot", Branch: "task-24", Owner: "lead", ActingOrgRole: "gmc-fanout",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	// Same owner, different role: must NOT overwrite an existing attribution.
	if _, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: "gitmoot/gitmoot", Branch: "task-24", Owner: "lead", ActingOrgRole: "gmc-gate",
	}); err != nil {
		t.Fatalf("AcquireLock returned error: %v", err)
	}
	lock, err := store.GetBranchLock(ctx, "gitmoot/gitmoot", "task-24")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if lock.ActingOrgRole != "gmc-fanout" {
		t.Fatalf("an existing attribution was overwritten: %+v", lock)
	}

	// Different owner against a BLANK lock: must not fill it.
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: "gitmoot/gitmoot", Branch: "task-25", Owner: "lead",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if _, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: "gitmoot/gitmoot", Branch: "task-25", Owner: "other", ActingOrgRole: "gmc-gate",
	}); err != nil {
		t.Fatalf("AcquireLock returned error: %v", err)
	}
	other, err := store.GetBranchLock(ctx, "gitmoot/gitmoot", "task-25")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if other.ActingOrgRole != "" {
		t.Fatalf("a different owner relabelled a blank lock: %+v", other)
	}
}

// #1250 finding 3 (g7-review). The list projections omitted the column, so
// callers received a structurally valid but FALSELY BLANK lock — worse than a
// missing field, because it reads as "unattributed" rather than "not loaded".
func TestBranchLockListProjectionsCarryAttribution(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: "gitmoot/gitmoot", Branch: "task-list-probe", Owner: "lead", ActingOrgRole: "gmc-fanout",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	locks, err := store.ListBranchLocks(ctx, "gitmoot/gitmoot")
	if err != nil {
		t.Fatalf("ListBranchLocks returned error: %v", err)
	}
	if len(locks) != 1 || locks[0].ActingOrgRole != "gmc-fanout" {
		t.Fatalf("ListBranchLocks = %+v, want acting_org_role gmc-fanout", locks)
	}
	infos, err := store.ListBranchLocksWithAge(ctx, "gitmoot/gitmoot")
	if err != nil {
		t.Fatalf("ListBranchLocksWithAge returned error: %v", err)
	}
	if len(infos) != 1 || infos[0].ActingOrgRole != "gmc-fanout" {
		t.Fatalf("ListBranchLocksWithAge = %+v, want acting_org_role gmc-fanout", infos)
	}
}

// #1250 finding 2 (g7-review). The risk-tiered path diverts BEFORE the ordinary
// attributed request loop, so the whole high-risk branch was unattributed.
func TestHighRiskFanoutAttributesLensChildren(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "sec", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true

	event := highRiskEvent()
	event.ActingOrgRole = "gmc-fanout"
	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	lenses := 0
	for _, job := range jobs {
		if !strings.Contains(job.ID, "/delegation/") {
			continue
		}
		lenses++
		payload, err := ParseJobPayload(job.Payload)
		if err != nil {
			t.Fatalf("ParseJobPayload returned error: %v", err)
		}
		if payload.ActingOrgRole != "gmc-fanout" {
			t.Fatalf("high-risk lens %q acting_org_role = %q, want gmc-fanout", job.ID, payload.ActingOrgRole)
		}
	}
	if lenses == 0 {
		t.Fatal("no lens children were enqueued; cannot assert attribution")
	}
}

// #1250 finding 1, round 2 (g7-review). END-TO-END on the path that actually
// runs: allocator-created BLANK lock -> role-bearing job persisted by Mailbox ->
// executor preflight -> attributed lock.
//
// The repair arm alone was not enough. ensureJobExecutorAllowed RECONSTRUCTS a
// JobRequest to authorize the executor, and it omitted ActingOrgRole — so the
// sole writer received an empty role and correctly refused to fill. The
// attribution died one call short of the lock, on a payload carrying it the whole
// time. My own site audit had flagged that reconstruction as a miss and dismissed
// it as "preflight-only, harmless"; it is the only path that reaches the lock for
// task-run and isolated delegation work.
func TestExecutorPreflightAttributesTheAllocatorCreatedLock(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	engine := testEngine(store)

	// Step 1: the worktree allocator takes the branch with no role in hand.
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: "gitmoot/gitmoot", Branch: "task-26", Owner: "lead",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock (allocator) returned acquired=%v err=%v", acquired, err)
	}

	// Step 2: a role-bearing implement job, as Mailbox persists it.
	payload := JobPayload{
		Repo:          "gitmoot/gitmoot",
		Branch:        "task-26",
		TaskID:        "task-26",
		LeadAgent:     "lead",
		ActingOrgRole: "gmc-fanout",
	}
	job := db.Job{ID: "executor-preflight-job", Agent: "lead", Type: "implement"}

	// Step 3: the executor preflight — the path that actually reaches the writer.
	if err := engine.ensureJobExecutorAllowed(ctx, job, payload, taskRefFromPayload(payload)); err != nil {
		t.Fatalf("ensureJobExecutorAllowed returned error: %v", err)
	}

	lock, err := store.GetBranchLock(ctx, "gitmoot/gitmoot", "task-26")
	if err != nil {
		t.Fatalf("GetBranchLock returned error: %v", err)
	}
	if lock.ActingOrgRole != "gmc-fanout" {
		t.Fatalf("executor preflight left allocator lock unattributed: %+v", lock)
	}
}
