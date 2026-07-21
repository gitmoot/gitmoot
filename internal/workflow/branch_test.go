package workflow

import (
	"testing"
)

func TestDelegationBranchNameAlwaysNamespaced(t *testing.T) {
	short := parentShort("parent-job")
	wantPrefix := "gitmoot-delegation-" + short + "-"

	// No worktree hint: the branch is the bare namespaced form.
	plain := delegationBranchName(Delegation{ID: "d1"}, "parent-job", "d1", 0)
	if plain != wantPrefix+"d1" {
		t.Fatalf("plain branch = %q, want %q", plain, wantPrefix+"d1")
	}

	// A worktree hint is appended only as a suffix, never replacing the namespace.
	hinted := delegationBranchName(Delegation{ID: "d1", Worktree: "Feature Login"}, "parent-job", "d1", 0)
	if hinted != wantPrefix+"d1-feature-login" {
		t.Fatalf("hinted branch = %q, want %q", hinted, wantPrefix+"d1-feature-login")
	}

	// Two sibling delegations that share an identical worktree hint must still get
	// distinct branches because each carries its own delegation id in the name.
	siblingA := delegationBranchName(Delegation{ID: "api", Worktree: "shared"}, "parent-job", "api", 0)
	siblingB := delegationBranchName(Delegation{ID: "ui", Worktree: "shared"}, "parent-job", "ui", 0)
	if siblingA == siblingB {
		t.Fatalf("siblings with identical worktree hint share branch %q", siblingA)
	}

	// Two siblings with empty worktree hints are likewise distinct.
	emptyA := delegationBranchName(Delegation{ID: "api"}, "parent-job", "api", 0)
	emptyB := delegationBranchName(Delegation{ID: "ui"}, "parent-job", "ui", 0)
	if emptyA == emptyB {
		t.Fatalf("siblings with empty worktree hint share branch %q", emptyA)
	}

	// A retry of the same delegation gets a distinct -retry-<n> branch so it never
	// reuses the failed original attempt's still-checked-out branch.
	original := delegationBranchName(Delegation{ID: "d1"}, "parent-job", "d1", 0)
	retry := delegationBranchName(Delegation{ID: "d1"}, "parent-job", "d1", 2)
	if retry != original+"-retry-2" {
		t.Fatalf("retry branch = %q, want %q", retry, original+"-retry-2")
	}
	if retry == original {
		t.Fatalf("retry branch %q collides with original attempt", retry)
	}

	// Determinism: the same inputs always produce the same branch.
	if again := delegationBranchName(Delegation{ID: "d1", Worktree: "Feature Login"}, "parent-job", "d1", 0); again != hinted {
		t.Fatalf("delegationBranchName not deterministic: %q != %q", again, hinted)
	}
}
