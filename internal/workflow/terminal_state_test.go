package workflow

import (
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// TestSettledVsFinalTruthTables pins the canonical two-predicate contract (#632):
// `blocked` is SETTLED (barrier semantics) but NOT FINAL (resumability
// semantics); every other end state agrees, and non-end states are neither.
func TestSettledVsFinalTruthTables(t *testing.T) {
	cases := []struct {
		state       string
		wantSettled bool
		wantFinal   bool
	}{
		{string(JobSucceeded), true, true},
		{string(JobFailed), true, true},
		{string(JobCancelled), true, true},
		// The deliberate split: blocked is settled but resumable (not final).
		{string(JobBlocked), true, false},
		{string(JobQueued), false, false},
		{string(JobRunning), false, false},
		{"", false, false},
		{"weird", false, false},
	}
	for _, c := range cases {
		if got := IsSettledJobState(c.state); got != c.wantSettled {
			t.Errorf("IsSettledJobState(%q) = %v, want %v", c.state, got, c.wantSettled)
		}
		if got := IsFinalJobState(c.state); got != c.wantFinal {
			t.Errorf("IsFinalJobState(%q) = %v, want %v", c.state, got, c.wantFinal)
		}
	}
}

// TestPinDelegationBarrierProceedsPastBlockedChild pins the engine's observable
// barrier semantics (#632): a delegation whose child is `blocked` counts as
// resolved, so the coordinator continuation is not stalled by it. This is the
// "settled includes blocked" behavior encoded via allDelegationsResolved and
// must hold identically before and after the predicate refactor.
func TestPinDelegationBarrierProceedsPastBlockedChild(t *testing.T) {
	delegations := []Delegation{{ID: "a"}, {ID: "b"}}
	children := map[string]db.Job{
		"a": {ID: "root/delegation/a", State: string(JobSucceeded)},
		"b": {ID: "root/delegation/b", State: string(JobBlocked)},
	}
	if !allDelegationsResolved(delegations, children, nil) {
		t.Fatal("allDelegationsResolved with a blocked child = false, want true (barrier must proceed past blocked)")
	}

	// A still-running child, by contrast, is NOT resolved: the barrier waits.
	children["b"] = db.Job{ID: "root/delegation/b", State: string(JobRunning)}
	if allDelegationsResolved(delegations, children, nil) {
		t.Fatal("allDelegationsResolved with a running child = true, want false (barrier must wait on running)")
	}
}
