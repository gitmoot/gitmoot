package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2004 acceptance 3, ruling (b): a role-authored reviewer is OUTSIDE the
// runtime-family predicate's domain rather than an unresolved value inside it.
//
// The three arms below are one property, and arm 2 is the one that earns arm 1.
// An exclusion that switched off the family check would be indistinguishable
// from an exemption if nothing else still bound a role. Arm 2 shows something
// does: a role approving its own implementation is refused on IDENTITY, before
// the family check is ever reached. Arm 3 shows the exclusion did not leak to
// the case that must keep failing closed - an unregistered AGENT also resolves
// to no family, and inferring the exclusion from an empty family rather than
// from the plumbed flag would collapse the two.

func seedRoleDomainImplementer(t *testing.T, store *db.Store, agent string, runtime string) {
	t.Helper()
	if runtime != "" {
		seedFamilyAgent(t, store, agent, runtime)
	}
	insertCompletedJob(t, store, db.Job{ID: "implement-" + agent, Agent: agent, Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 9,
	})
}

// Arm 1: the family check does not apply to a role, so a role-authored approval
// by someone who did not implement is not blocked. A human session has no
// runtime family to share.
func TestRoleAuthoredApprovalIsOutsideTheFamilyCheck(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedRoleDomainImplementer(t, store, "wave-impl", "codex")
	gate := PolicyMergeGate{Store: store}

	same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "reviewer", true, "",
		map[string]implementerIdentity{"wave-impl": {Name: "wave-impl", RecordedRuntime: "codex"}})
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if same {
		t.Fatalf("a role-authored approval was blocked on runtime family: %q; a role has no family to share", reason)
	}
}

// ARM 2 IS NOT WRITTEN HERE, ON PURPOSE. It already exists and already drives
// the real Evaluate path: TestPolicyMergeGateReachesIndependenceForARoleAuthoredApproval
// carries the table case "the SAME role that implemented cannot approve its own
// work", expecting zero merges with the reason "the implementing agent". That is
// the arm that makes arm 1 a domain exclusion instead of an exemption, and its
// reason string is the proof the refusal comes from IDENTITY rather than from a
// family a role does not have.
//
// Re-asserting it here would be a second, weaker copy: my first draft checked the
// same implementer map twice and never reached Evaluate, which would have passed
// against a gate that let a role self-approve a merge. What matters is that the
// pre-existing case still passes WITH the exclusion applied, and it does.

// Arm 3: the exclusion is keyed on the plumbed flag, never on an empty family.
// An unregistered AGENT produces exactly the same empty family and must keep
// failing closed.
func TestUnregisteredAgentReviewerStillFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedRoleDomainImplementer(t, store, "wave-impl", "codex")
	gate := PolicyMergeGate{Store: store}

	same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "gm-omp-nag", false, "",
		map[string]implementerIdentity{"wave-impl": {Name: "wave-impl", RecordedRuntime: "codex"}})
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if !same {
		t.Fatal("an unregistered agent reviewer did not fail closed; its family is as empty as a role's and it is not a role")
	}
	if !strings.Contains(reason, "gm-omp-nag") {
		t.Fatalf("block reason = %q, want the unresolvable reviewer named", reason)
	}
}

// The implementer side of arm 1, which CI found and my filtered run could not:
// TestRoleImplementedTaskIsAttributable lives outside every pattern I ran. A
// role-attributed implementer (#1916) is a human session with no runtime family,
// so an agent reviewing its work is independent and must not be blocked.
func TestRoleAttributedImplementerIsOutsideTheFamilyCheck(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedFamilyAgent(t, store, "g7-review", "codex")
	gate := PolicyMergeGate{Store: store}

	same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "g7-review", false, "codex",
		map[string]implementerIdentity{"gitmoot": {Name: "gitmoot", FromActingRole: true}})
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if same {
		t.Fatalf("an agent reviewer was blocked against a ROLE implementer: %q; a role has no family to share", reason)
	}
}

// And its control: an implementer that is an unregistered AGENT, not a role,
// produces the identical empty family and must still fail closed. Without this
// the arm above is satisfied by skipping every unresolvable implementer.
func TestUnregisteredAgentImplementerStillFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedFamilyAgent(t, store, "g7-review", "codex")
	gate := PolicyMergeGate{Store: store}

	same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "g7-review", false, "codex",
		map[string]implementerIdentity{"gm-omp-nag": {Name: "gm-omp-nag"}})
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if !same {
		t.Fatal("an unregistered agent implementer did not fail closed; it is not a role and its family is unknown")
	}
	if !strings.Contains(reason, "gm-omp-nag") {
		t.Fatalf("block reason = %q, want the unresolvable implementer named", reason)
	}
}
