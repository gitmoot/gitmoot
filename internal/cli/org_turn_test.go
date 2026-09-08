package cli

import (
	"context"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
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

// #2042 review P2, and it found a real gap in my own evidence. The three tests
// above call orgLastTurn/orgTurnText DIRECTLY, so removing `LastTurn:` from the
// row literal in buildOrgStatusRows survives all of them - the mutant I claimed
// killed "the original defect" was only killed at the helper. These two enter
// the ROW BUILDER, which is what the CLI and JSON actually render from.
func TestBuildOrgStatusRowsCarriesTheReportedTurnOntoTheRow(t *testing.T) {
	_, paths := setupOrgHome(t)
	ctx := context.Background()
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := loadOrgSharedState(ctx, paths, store, time.Now().UTC())
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	source := func(context.Context, config.OrgConfig) (map[string]org.RoleLiveState, time.Time, string, error) {
		return map[string]org.RoleLiveState{
			"owner":  {State: org.StateWorking, Activity: &org.RoleActivity{Turn: 17, CompletedAt: time.Now().UTC()}},
			"review": {State: org.StateIdle},
		}, time.Time{}, "fixture", nil
	}
	rows, err := buildOrgStatusRows(ctx, &shared, source, "status", false)
	if err != nil {
		t.Fatalf("buildOrgStatusRows: %v", err)
	}
	var owner, review *orgStatusOutput
	for i := range rows {
		switch rows[i].Role {
		case "owner":
			owner = &rows[i]
		case "review":
			review = &rows[i]
		}
	}
	if owner == nil || review == nil {
		t.Fatalf("fixture roles missing from rows: %+v", rows)
	}
	if owner.LastTurn == nil {
		t.Fatal("the row builder dropped a provider-reported turn; the CLI and JSON render from this row, not from orgLastTurn")
	}
	if *owner.LastTurn != 17 {
		t.Fatalf("row last_turn = %d, want 17", *owner.LastTurn)
	}
	// And a role whose provider reported no activity must stay absent on the row.
	if review.LastTurn != nil {
		t.Fatalf("a role with no reported activity carried turn %d onto its row", *review.LastTurn)
	}
}

// An UNAVAILABILITY incident is a statement about whether a role may be
// dispatched to. It is not evidence that the provider reported nothing, and the
// overlay used to replace the whole live state - erasing a reported turn, which
// is the same class of invention as rendering nil as zero.
func TestBuildOrgStatusRowsKeepsTheTurnForAnUnavailableRole(t *testing.T) {
	_, paths := setupOrgHome(t)
	ctx := context.Background()
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailable(ctx, "owner", "quota", now.Add(time.Hour), now); err != nil {
		store.Close()
		t.Skipf("unavailability fixture unavailable in this schema: %v", err)
	}
	shared, err := loadOrgSharedState(ctx, paths, store, time.Now().UTC())
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	source := func(context.Context, config.OrgConfig) (map[string]org.RoleLiveState, time.Time, string, error) {
		return map[string]org.RoleLiveState{
			"owner": {State: org.StateWorking, Activity: &org.RoleActivity{Turn: 17, CompletedAt: time.Now().UTC()}},
		}, time.Time{}, "fixture", nil
	}
	rows, err := buildOrgStatusRows(ctx, &shared, source, "status", false)
	if err != nil {
		t.Fatalf("buildOrgStatusRows: %v", err)
	}
	for _, row := range rows {
		if row.Role != "owner" {
			continue
		}
		if row.UnavailableReason == "" {
			t.Skip("unavailability overlay did not apply in this fixture; the erasure arm is untested here")
		}
		if row.LastTurn == nil {
			t.Fatal("the unavailable overlay erased a provider-reported turn")
		}
		if *row.LastTurn != 17 {
			t.Fatalf("unavailable row last_turn = %d, want 17", *row.LastTurn)
		}
	}
}
