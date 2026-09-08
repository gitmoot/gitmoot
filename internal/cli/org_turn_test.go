package cli

import (
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/org"
)

// #1702. The turn counter was captured off the wire, parsed, modelled on
// RoleLiveState and rendered on the web dashboard, while org.go's row builder
// had live.Activity in scope on the same line and never read it. These tests
// pin the two facts that make reading it useful.

// NIL IS NOT ZERO, and this is the whole subtlety. org.RoleActivity's own
// comment states it: "A nil *RoleActivity means the provider did not report
// turn activity; callers must not treat that as a zero-valued or stale turn."
// A rendered 0 would invent a stalled seat out of a silent provider, which is
// worse than showing nothing.
func TestOrgLastTurnKeepsNotReportedDistinctFromZero(t *testing.T) {
	if got := orgLastTurn(org.RoleLiveState{State: org.StateUnknown}); got != nil {
		t.Fatalf("a provider that reported no activity yielded turn %v, want nil", *got)
	}
	zero := orgLastTurn(org.RoleLiveState{Activity: &org.RoleActivity{Turn: 0}})
	if zero == nil || *zero != 0 {
		t.Fatalf("a REPORTED turn of 0 was lost: %v", zero)
	}
	// The two must render differently, or the distinction dies at the surface.
	if orgTurnText(nil) == orgTurnText(zero) {
		t.Fatalf("not-reported and turn 0 render identically as %q", orgTurnText(nil))
	}
	if orgTurnText(nil) != "-" {
		t.Fatalf("not-reported renders as %q, want the same dash every other absent column uses", orgTurnText(nil))
	}
	if orgTurnText(zero) != "0" {
		t.Fatalf("a reported turn 0 renders as %q", orgTurnText(zero))
	}
}

func TestOrgLastTurnCarriesTheReportedValue(t *testing.T) {
	live := org.RoleLiveState{
		State:    org.StateWorking,
		Activity: &org.RoleActivity{Turn: 134, TurnEpoch: 1788691508180392015, CompletedAt: time.Now()},
	}
	got := orgLastTurn(live)
	if got == nil || *got != 134 {
		t.Fatalf("turn = %v, want 134", got)
	}
	if orgTurnText(got) != "134" {
		t.Fatalf("rendered %q, want 134", orgTurnText(got))
	}
}

// TestOrgTurnIsIndependentOfLastSeenAge is the justification, as a test.
//
// #1702's own argument is that note age is no substitute, because a seat can be
// inside one long turn with an hour-old note and be perfectly healthy. The same
// defect applies to the field the chart already prints: LastSeenAge measures
// RECENCY and the turn measures PROGRESS, so a seat can be working with a stale
// age, and only the turn separates that from a seat that stopped.
//
// Observed on this host while this was written, from `gitmoot org chart`:
//
//	deimos · working · turn=117 · seen=27h26m24s
//	among-friends-omp · working · turn=52 · seen=45h30m30s
//
// Both working, both with ages that would read as long-dead. This test pins that
// the two fields are independent so nobody later derives one from the other.
func TestOrgTurnIsIndependentOfLastSeenAge(t *testing.T) {
	working := org.RoleLiveState{State: org.StateWorking, Activity: &org.RoleActivity{Turn: 117}}
	turn := orgLastTurn(working)
	if turn == nil || *turn != 117 {
		t.Fatalf("a working seat's turn was not carried: %v", turn)
	}
	// A silent provider on a seat in the SAME state must still render absent
	// rather than borrowing anything from the age column.
	silent := org.RoleLiveState{State: org.StateWorking}
	if orgTurnText(orgLastTurn(silent)) != "-" {
		t.Fatalf("a working seat with no reported activity rendered %q", orgTurnText(orgLastTurn(silent)))
	}
}
