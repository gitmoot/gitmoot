package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func insertParkTestDirective(t *testing.T, store *Store, workflowID, body string) WorkflowNote {
	t.Helper()
	note, err := store.InsertWorkflowNote(context.Background(), WorkflowNote{
		WorkflowID: workflowID, Author: "owner", Body: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	return note
}

// archiveWorkerInMirror satisfies parking's self-invalidation clause (#1643
// round 5): a park only lands while the role's mirror row exists.
func archiveWorkerInMirror(t *testing.T, store *Store) {
	t.Helper()
	if err := store.UpsertOrgRoleArchived(context.Background(), OrgRoleArchived{
		Role: "worker", ArchivedAt: "2026-08-26T12:00:00Z", ArchivedBy: "herdr-app",
		ObservedAt: "2026-08-26T12:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
}

// Parking a role suspends its OPEN obligations: they leave the live sweep
// (list AND count, which must share one open-predicate) and appear in the
// parked listing with their stamp and reason. Done directives carry no ladder
// and are untouched; other roles' obligations are untouched.
func TestParkOrgDirectivesSuspendsOpenSweep(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	first := insertParkTestDirective(t, store, "wave/park", "[org:directive to=worker from=owner wf=wave/park] first")
	second := insertParkTestDirective(t, store, "wave/park", "[org:directive to=worker from=owner wf=wave/park] second")
	other := insertParkTestDirective(t, store, "wave/park", "[org:directive to=peer from=owner wf=wave/park] keep sweeping")
	done := insertParkTestDirective(t, store, "wave/park", "[org:directive to=worker from=owner wf=wave/park] already done")
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "wave/park", Author: "worker",
		Body: fmt.Sprintf("[org:directive-done id=%d by=worker]", done.ID),
	}); err != nil {
		t.Fatal(err)
	}

	archiveWorkerInMirror(t, store)
	parkedAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	parked, err := store.ParkOpenOrgDirectivesForRole(ctx, "worker", parkedAt, "seat archived")
	if err != nil || parked != 2 {
		t.Fatalf("park = %d, %v; want 2 rows (open only, done untouched)", parked, err)
	}

	open, err := store.ListOpenOrgDirectiveObligations(ctx, 50)
	if err != nil || len(open) != 1 || open[0].ID != other.ID {
		t.Fatalf("open after park = %+v err=%v, want only peer's %d", open, err, other.ID)
	}
	count, err := store.CountOpenOrgDirectiveObligations(ctx)
	if err != nil || count != len(open) {
		t.Fatalf("count=%d err=%v, want %d — count and list must share the open-predicate", count, err, len(open))
	}

	stamp := parkedAt.Format(time.RFC3339Nano)
	rows, err := store.ListParkedOrgDirectives(ctx, "worker")
	if err != nil || len(rows) != 2 || rows[0].ID != first.ID || rows[1].ID != second.ID {
		t.Fatalf("parked listing = %+v err=%v, want [%d %d]", rows, err, first.ID, second.ID)
	}
	for _, row := range rows {
		if row.ParkedAt != stamp || row.ParkedReason != "seat archived" {
			t.Fatalf("parked row = %+v, want stamp %q reason %q", row, stamp, "seat archived")
		}
	}
	all, err := store.ListParkedOrgDirectives(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all-roles parked listing = %+v err=%v, want 2", all, err)
	}

	// Idempotent: already-parked rows are not re-stamped.
	again, err := store.ParkOpenOrgDirectivesForRole(ctx, "worker", parkedAt.Add(time.Hour), "later")
	if err != nil || again != 0 {
		t.Fatalf("re-park = %d, %v; want 0", again, err)
	}
}

// A parked row refuses nudge marks at the write site, so a sweep that listed
// an obligation just before it was parked cannot nudge it afterwards.
func TestParkedDirectiveRefusesNudgeMarks(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	unacked := insertParkTestDirective(t, store, "wave/park", "[org:directive to=worker from=owner wf=wave/park] unacked")
	acked := insertParkTestDirective(t, store, "wave/park", "[org:directive to=worker from=owner wf=wave/park] acked")
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{
		WorkflowID: "wave/park", Author: "worker",
		Body: fmt.Sprintf("[org:directive-ack id=%d by=worker]", acked.ID),
	}); err != nil {
		t.Fatal(err)
	}
	archiveWorkerInMirror(t, store)
	if parked, err := store.ParkOpenOrgDirectivesForRole(ctx, "worker", time.Now(), "seat archived"); err != nil || parked != 2 {
		t.Fatalf("park = %d, %v", parked, err)
	}
	now := time.Now().UTC()
	if _, claimed, err := store.MarkOrgDirectiveNudged(ctx, unacked.ID, 0, "", now); err != nil || claimed {
		t.Fatalf("ack-phase nudge on parked row claimed=%v err=%v, want refused", claimed, err)
	}
	if _, claimed, err := store.MarkOrgDirectiveDoneNudged(ctx, acked.ID, 0, "", now); err != nil || claimed {
		t.Fatalf("done-phase nudge on parked row claimed=%v err=%v, want refused", claimed, err)
	}
}

// Unpark returns the obligation to the live sweep — park is SUSPENSION, not
// done/cancel, which never come back — and resets the nudge anchor to unpark
// time so a TTL that elapsed while archived does not nag immediately.
func TestUnparkRestoresObligationAndResetsNudgeAnchor(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	old := insertParkTestDirective(t, store, "wave/park", "[org:directive to=worker from=owner wf=wave/park] stale ttl")
	if _, err := store.db.ExecContext(ctx, `UPDATE workflow_notes SET created_at = ? WHERE id = ?`, "2026-01-01 00:00:00", old.ID); err != nil {
		t.Fatal(err)
	}
	archiveWorkerInMirror(t, store)
	if parked, err := store.ParkOpenOrgDirectivesForRole(ctx, "worker", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), "seat archived"); err != nil || parked != 1 {
		t.Fatalf("park = %d, %v", parked, err)
	}
	unparkAt := time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)
	unparked, err := store.UnparkOrgDirectivesForRole(ctx, "worker", unparkAt)
	if err != nil || unparked != 1 {
		t.Fatalf("unpark = %d, %v; want 1", unparked, err)
	}
	open, err := store.ListOpenOrgDirectiveObligations(ctx, 50)
	if err != nil || len(open) != 1 || open[0].ID != old.ID {
		t.Fatalf("open after unpark = %+v err=%v, want the unparked row", open, err)
	}
	if got, want := open[0].LastNudgedAt, unparkAt.Format(time.RFC3339Nano); got != want {
		t.Fatalf("nudge anchor after unpark = %q, want unpark stamp %q — a returning seat must get a fresh TTL", got, want)
	}
	if rows, err := store.ListParkedOrgDirectives(ctx, "worker"); err != nil || len(rows) != 0 {
		t.Fatalf("parked listing after unpark = %+v err=%v, want empty", rows, err)
	}
	// Nothing left to unpark.
	if n, err := store.UnparkOrgDirectivesForRole(ctx, "worker", unparkAt); err != nil || n != 0 {
		t.Fatalf("re-unpark = %d, %v; want 0", n, err)
	}
}

// The transition is one transaction: a failure at ANY point (the hook fires
// just before commit) rolls back BOTH halves — directives stay parked, the
// mirror row survives. Mutant P2 (unpark committed in its own transaction
// before the delete) dies here: the hook failure would then leave directives
// unparked with the row still present, the exact partial state #1643 round 3
// proved a later omission strands forever.
func TestUnarchiveOrgSeatTransitionRollsBackAtomically(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	directive := insertParkTestDirective(t, store, "wave/atomic", "[org:directive to=worker from=owner wf=wave/atomic] parked")
	if err := store.UpsertOrgRoleArchived(ctx, OrgRoleArchived{
		Role: "worker", ArchivedAt: "2026-08-27T08:00:00Z", ArchivedBy: "herdr-app",
		ObservedAt: "2026-08-27T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if parked, err := store.ParkOpenOrgDirectivesForRole(ctx, "worker", time.Date(2026, 8, 27, 8, 0, 0, 0, time.UTC), "seat archived"); err != nil || parked != 1 {
		t.Fatalf("park fixture = %d, %v", parked, err)
	}

	orgUnarchiveTransitionTestHook = func() error { return errors.New("injected pre-commit failure") }
	t.Cleanup(func() { orgUnarchiveTransitionTestHook = nil })
	at := time.Date(2026, 8, 27, 8, 5, 0, 0, time.UTC)
	if _, err := store.UnarchiveOrgSeatTransition(ctx, "worker", at); err == nil {
		t.Fatal("hooked transition succeeded; the hook is not firing")
	}
	if parked, err := store.ListParkedOrgDirectives(ctx, "worker"); err != nil || len(parked) != 1 || parked[0].ID != directive.ID {
		t.Fatalf("parked after rollback = %+v err=%v, want STILL PARKED — the unpark half must roll back too", parked, err)
	}
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 1 || rows[0].Role != "worker" {
		t.Fatalf("mirror after rollback = %+v err=%v, want row preserved", rows, err)
	}

	orgUnarchiveTransitionTestHook = nil
	unparked, err := store.UnarchiveOrgSeatTransition(ctx, "worker", at)
	if err != nil || unparked != 1 {
		t.Fatalf("clean transition = %d, %v", unparked, err)
	}
	if rows, err := store.ListOrgRolesArchived(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("mirror after clean transition = %+v err=%v, want gone", rows, err)
	}
	open, err := store.ListOpenOrgDirectiveObligations(ctx, 50)
	if err != nil || len(open) != 1 || open[0].ID != directive.ID || open[0].LastNudgedAt != at.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("directive after clean transition = %+v err=%v, want unparked with the transition anchor", open, err)
	}
}

// The #1643 round-5 adversary (kimi): a park landing from a STALE snapshot
// after the unarchive transition deleted the mirror row would re-park
// directives no later tick could ever unpark — the mirror row is the unpark
// retry key. The EXISTS clause makes such a park SELF-INVALIDATE at the write:
// no mirror row, no park, regardless of what the caller believed. Mutant K-M
// (drop the EXISTS clause) dies only here.
func TestParkSelfInvalidatesWithoutMirrorRow(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	directive := insertParkTestDirective(t, store, "wave/stale", "[org:directive to=worker from=owner wf=wave/stale] open work")
	// No mirror row: the caller's snapshot is stale (the transition deleted
	// it). The park must land NOTHING.
	if parked, err := store.ParkOpenOrgDirectivesForRole(ctx, "worker", time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC), "stale snapshot"); err != nil || parked != 0 {
		t.Fatalf("stale-snapshot park = %d, %v; want 0 — self-invalidation at the write", parked, err)
	}
	if open, err := store.ListOpenOrgDirectiveObligations(ctx, 10); err != nil || len(open) != 1 || open[0].ID != directive.ID {
		t.Fatalf("directive after stale park = %+v err=%v, want still OPEN", open, err)
	}
	// With the row present the same park lands — the clause gates on the
	// mirror, not on anything about the directive.
	archiveWorkerInMirror(t, store)
	if parked, err := store.ParkOpenOrgDirectivesForRole(ctx, "worker", time.Date(2026, 8, 27, 10, 1, 0, 0, time.UTC), "seat archived"); err != nil || parked != 1 {
		t.Fatalf("mirrored park = %d, %v; want 1", parked, err)
	}
}

// The archive mirror re-parks every archived seat every minute and almost
// always parks nothing, so the park statement's plan IS its steady-state cost
// (#2530). The shipped text scanned every note body and, per unparked closed
// directive, rescanned every same-workflow sibling through
// idx_workflow_notes_wid: about 5s of daemon CPU per pass on the live store, to
// park 0 rows.
//
// As in #2138 this asserts the shape, not a duration. The outer statement must
// walk only the directive partial index, and each terminal-marker probe must be
// a RANGE on the marker index's 64-character prefix: `workflow_id=?` alone
// still walks every marker note in the workflow.
func TestParkOpenOrgDirectivesForRoleIsIndexBounded(t *testing.T) {
	store := openWorkflowTestStore(t)
	plan := explainPlan(t, store, parkOpenOrgDirectivesForRoleSQL, "stamp", "reason", "worker", "worker", "worker")
	if !strings.Contains(plan, "SCAN workflow_notes USING INDEX idx_workflow_notes_directive_oldest") {
		t.Fatalf("park does not restrict its outer scan to directive notes, so every pass reads every note body:\n%s", plan)
	}
	const markerRange = "SEARCH r USING INDEX idx_workflow_notes_directive_markers (workflow_id=? AND <expr>>? AND <expr><?)"
	if got := strings.Count(plan, markerRange); got != 2 {
		t.Fatalf("park has %d range-bounded terminal-marker probes, want 2 (cancel and done):\n%s", got, plan)
	}
	if strings.Contains(plan, "idx_workflow_notes_wid") {
		t.Fatalf("park probes every same-workflow sibling through idx_workflow_notes_wid:\n%s", plan)
	}
}

// The index-shaped rewrite matches terminal markers by a 64-character prefix
// RANGE instead of an exact per-row comparison, so these are the cases where a
// wrong bound silently parks a closed directive or skips an open one. Each is
// a wrong answer, not a slowdown.
func TestParkOpenOrgDirectivesForRoleTerminalMarkerScoping(t *testing.T) {
	for _, test := range []struct {
		name       string
		marker     func(id int64) (workflow string, body string)
		wantParked int64
	}{
		{
			name: "done in the same workflow keeps it unparked",
			marker: func(id int64) (string, string) {
				return "wave/scope", fmt.Sprintf("[org:directive-done id=%d by=worker] finished", id)
			},
			wantParked: 0,
		},
		{
			name: "cancel in the same workflow keeps it unparked",
			marker: func(id int64) (string, string) {
				return "wave/scope", fmt.Sprintf("[org:directive-cancel id=%d by=owner] withdrawn", id)
			},
			wantParked: 0,
		},
		{
			// The indexed prefix is 64 characters; the marker's own prefix is
			// shorter, so a long body must still match through the range.
			name: "done with a body past the indexed 64 characters keeps it unparked",
			marker: func(id int64) (string, string) {
				return "wave/scope", fmt.Sprintf("[org:directive-done id=%d by=worker] %s", id, strings.Repeat("x", 200))
			},
			wantParked: 0,
		},
		{
			name: "done in a FOREIGN workflow does not close it",
			marker: func(id int64) (string, string) {
				return "unrelated/workflow", fmt.Sprintf("[org:directive-done id=%d by=worker] finished", id)
			},
			wantParked: 1,
		},
		{
			name: "marker with no space after the id does not close it",
			marker: func(id int64) (string, string) {
				return "wave/scope", fmt.Sprintf("[org:directive-done id=%d]", id)
			},
			wantParked: 1,
		},
		{
			// id=71 sorts inside [id=7 , id=7!) only if the upper bound is wrong.
			name: "marker for a longer id sharing the prefix does not close it",
			marker: func(id int64) (string, string) {
				return "wave/scope", fmt.Sprintf("[org:directive-done id=%d1 by=worker] finished", id)
			},
			wantParked: 1,
		},
		{
			name: "zero-padded id does not close the unpadded directive",
			marker: func(id int64) (string, string) {
				return "wave/scope", fmt.Sprintf("[org:directive-cancel id=0%d by=owner] withdrawn", id)
			},
			wantParked: 1,
		},
		{
			name: "ack is not a terminal marker",
			marker: func(id int64) (string, string) {
				return "wave/scope", fmt.Sprintf("[org:directive-ack id=%d by=worker]", id)
			},
			wantParked: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openWorkflowTestStore(t)
			ctx := context.Background()
			directive := insertParkTestDirective(t, store, "wave/scope", "[org:directive to=worker from=owner wf=wave/scope] do the thing")
			workflowID, body := test.marker(directive.ID)
			if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: workflowID, Author: "worker", Body: body}); err != nil {
				t.Fatal(err)
			}
			archiveWorkerInMirror(t, store)
			parked, err := store.ParkOpenOrgDirectivesForRole(ctx, "worker", time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), "seat archived")
			if err != nil || parked != test.wantParked {
				t.Fatalf("park = %d, %v; want %d for marker %q in %q", parked, err, test.wantParked, body, workflowID)
			}
		})
	}
}
