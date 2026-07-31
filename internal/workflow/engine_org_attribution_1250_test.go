package workflow

import (
	"context"
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
