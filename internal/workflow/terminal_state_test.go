package workflow

import "testing"

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
