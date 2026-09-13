package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// A role-authored reviewer is outside runtime-family comparison because a human
// session has no runtime family. Reviewer/implementer identity remains binding;
// unresolved agent families are reported as advisories rather than merge bars.

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

// An unregistered reviewer produces an unresolved-family advisory.
func TestUnregisteredAgentReviewerProducesFamilyAdvisory(t *testing.T) {
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
		t.Fatal("an unregistered agent reviewer did not produce a family advisory")
	}
	if !strings.Contains(reason, "gm-omp-nag") {
		t.Fatalf("advisory reason = %q, want the unresolvable reviewer named", reason)
	}
}

// A role-attributed implementer has no runtime family, so the reviewer remains
// eligible on distinct identity and the unavailable comparison is disclosed.
func TestRoleAttributedImplementerProducesFamilyAdvisory(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedFamilyAgent(t, store, "g7-review", "codex")
	gate := PolicyMergeGate{Store: store}

	same, reason, err := gate.sameRuntimeFamilyAsImplementer(ctx, "review-job", "g7-review", false, "codex",
		map[string]implementerIdentity{"gitmoot": {Name: "gitmoot", FromActingRole: true}})
	if err != nil {
		t.Fatalf("sameRuntimeFamilyAsImplementer: %v", err)
	}
	if !same || !strings.Contains(reason, "advisory") {
		t.Fatalf("role-attributed implementer did not produce the family advisory: same=%v reason=%q", same, reason)
	}
}

// An unregistered agent implementer also produces an advisory, but is not
// confused with a role identity.
func TestUnregisteredAgentImplementerProducesFamilyAdvisory(t *testing.T) {
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
		t.Fatal("an unregistered agent implementer did not produce a family advisory")
	}
	if !strings.Contains(reason, "gm-omp-nag") {
		t.Fatalf("advisory reason = %q, want the unresolvable implementer named", reason)
	}
}
