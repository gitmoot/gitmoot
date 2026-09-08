package cli

import (
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/org"
)

// #1702. The turn counter and its completion time were captured off the wire,
// parsed, modelled on RoleLiveState and rendered on the web dashboard, while
// org_overview.go built its row from that same `live` value with live.Activity
// in scope on the line and never read it. These tests pin the three facts that
// make reading it useful.

// NIL IS NOT ZERO, and this is the subtlety. org.RoleActivity's own comment is
// the contract: "A nil *RoleActivity means the provider did not report turn
// activity; callers must not treat that as a zero-valued or stale turn." A
// rendered 0 would invent a stalled seat out of a silent provider.
func TestOrgLastTurnKeepsNotReportedDistinctFromZero(t *testing.T) {
	if got := orgLastTurn(org.RoleLiveState{State: org.StateUnknown}); got != nil {
		t.Fatalf("a provider that reported no activity yielded turn %v, want nil", *got)
	}
	zero := orgLastTurn(org.RoleLiveState{Activity: &org.RoleActivity{Turn: 0}})
	if zero == nil || *zero != 0 {
		t.Fatalf("a REPORTED turn of 0 was lost: %v", zero)
	}
	if orgTurnText(nil) == orgTurnText(zero) {
		t.Fatalf("not-reported and turn 0 render identically as %q", orgTurnText(nil))
	}
	if orgTurnText(nil) != "-" {
		t.Fatalf("not-reported renders as %q, want the dash every other absent column uses", orgTurnText(nil))
	}
	if orgTurnText(zero) != "0" {
		t.Fatalf("a reported turn 0 renders as %q", orgTurnText(zero))
	}
	// The age half must be absent for the same input, never a duration measured
	// from a zero time (which would print as a two-thousand-year age).
	if age := orgTurnAge(org.RoleLiveState{State: org.StateUnknown}, time.Now()); age != "" {
		t.Fatalf("a silent provider produced turn age %q", age)
	}
	// A non-nil activity carrying a ZERO completion time is the same class. The
	// herdr provider cannot produce it - herdr_org.go:180-183 requires the
	// timestamp and yields a nil activity without it - but org.RoleLiveState is
	// a public type and any other provider can fill it, so the guard is real
	// rather than decorative and is pinned here instead of left unexercised.
	if age := orgTurnAge(org.RoleLiveState{Activity: &org.RoleActivity{Turn: 7}}, time.Now()); age != "" {
		t.Fatalf("a zero completion time produced turn age %q, want absent", age)
	}
}

func TestOrgLastTurnCarriesTheReportedValue(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	live := org.RoleLiveState{
		State:    org.StateWorking,
		Activity: &org.RoleActivity{Turn: 134, TurnEpoch: 1788691508180392015, CompletedAt: now.Add(-9*time.Minute - 29*time.Second)},
	}
	got := orgLastTurn(live)
	if got == nil || *got != 134 {
		t.Fatalf("turn = %v, want 134", got)
	}
	if orgTurnText(got) != "134" {
		t.Fatalf("rendered %q, want 134", orgTurnText(got))
	}
	if age := orgTurnAge(live, now); age != "9m29s" {
		t.Fatalf("turn age = %q, want 9m29s", age)
	}
}

// TestOrgTurnContradictsNoteAge is the justification, and it is a CONTRADICTION
// rather than a gap.
//
// #1702 rejects note age as a discriminator because a seat can be inside one
// long turn with an hour-old note and be perfectly healthy. Measured on this
// host from the built binary, the field the chart already printed did not merely
// fail to help - it returned the WRONG verdict:
//
//	among-friends-omp  working  turn=55   turn_age=37s        seen=45h38m10s
//	deimos             working  turn=117  turn_age=9m29s      seen=27h34m4s
//
// Both read as long dead by note age; both had completed a turn within ten
// minutes. `seen=` is note age, and it was lying about exactly the thing it is
// read for. This test pins that a fresh turn is reported as fresh no matter how
// old the note age is, so nobody later gates one on the other.
func TestOrgTurnContradictsNoteAge(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	// The note is 45 hours old; the turn completed 37 seconds ago.
	live := org.RoleLiveState{State: org.StateWorking, Activity: &org.RoleActivity{Turn: 55, CompletedAt: now.Add(-37 * time.Second)}}
	if age := orgTurnAge(live, now); age != "37s" {
		t.Fatalf("turn age = %q, want 37s regardless of note age", age)
	}
	turn := orgLastTurn(live)
	if turn == nil || *turn != 55 {
		t.Fatalf("a working seat's turn was not carried: %v", turn)
	}
	// A silent provider on a seat in the SAME state must still render absent
	// rather than borrowing anything from the age column.
	silent := org.RoleLiveState{State: org.StateWorking}
	if orgTurnText(orgLastTurn(silent)) != "-" {
		t.Fatalf("a working seat with no reported activity rendered %q", orgTurnText(orgLastTurn(silent)))
	}
}

// TestOrgTurnAgeSeparatesAStuckSeatFromABusyOne is the reading NEITHER existing
// column produced.
//
//	numbra  working  turn=14   turn_age=18h9m59s  seen=21h43m7s
//	deimos  working  turn=117  turn_age=9m29s     seen=27h34m4s
//
// Same state. numbra is the real suspect and nothing on that surface singled it
// out: a low turn number is normal for a young seat, and its 21h note age is
// unremarkable beside seats that are healthy at 27h and 45h. What separates them
// is not the turn NUMBER but how long ago that turn completed - and for a
// WORKING seat that is how long the current turn has been running.
//
// The number and the age must therefore be independent: a HIGH turn with an old
// age is stuck, and a LOW turn with a fresh age is fine.
func TestOrgTurnAgeSeparatesAStuckSeatFromABusyOne(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	stuck := org.RoleLiveState{State: org.StateWorking, Activity: &org.RoleActivity{Turn: 14, CompletedAt: now.Add(-18*time.Hour - 9*time.Minute - 59*time.Second)}}
	busy := org.RoleLiveState{State: org.StateWorking, Activity: &org.RoleActivity{Turn: 117, CompletedAt: now.Add(-9*time.Minute - 29*time.Second)}}
	if got := orgTurnAge(stuck, now); got != "18h9m59s" {
		t.Fatalf("stuck seat turn age = %q, want 18h9m59s", got)
	}
	if got := orgTurnAge(busy, now); got != "9m29s" {
		t.Fatalf("busy seat turn age = %q, want 9m29s", got)
	}
	// The LOWER turn number is the stuck one, which is why the number alone
	// cannot produce this verdict.
	if *orgLastTurn(stuck) >= *orgLastTurn(busy) {
		t.Fatalf("test setup lost the inversion: stuck=%d busy=%d", *orgLastTurn(stuck), *orgLastTurn(busy))
	}
	// A completion time in the future (clock skew between hosts) must clamp to
	// zero rather than print a negative duration.
	skewed := org.RoleLiveState{State: org.StateWorking, Activity: &org.RoleActivity{Turn: 9, CompletedAt: now.Add(5 * time.Minute)}}
	if got := orgTurnAge(skewed, now); got != "0s" {
		t.Fatalf("clock-skewed completion rendered %q, want 0s", got)
	}
}
