package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	dbtest "github.com/gitmoot/gitmoot/internal/db/dbtest"
)

// The helper parks exactly the archived seats' open directives — other roles'
// obligations keep sweeping — and a nil/empty archived set parks NOTHING: the
// helper is park-only, so absent observation can never suspend or resume
// anything on its own.
func TestParkOutstandingDirectivesForArchivedSeats(t *testing.T) {
	home := t.TempDir()
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	insert := func(body string) db.WorkflowNote {
		t.Helper()
		note, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{WorkflowID: "wave/park", Author: "owner", Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return note
	}
	archivedSeat := insert("[org:directive to=scout from=owner wf=wave/park] parked with the seat")
	liveSeat := insert("[org:directive to=keeper from=owner wf=wave/park] keeps sweeping")

	if n, err := parkOutstandingDirectivesForArchivedSeats(ctx, store, nil, time.Now()); err != nil || n != 0 {
		t.Fatalf("nil archived set parked %d err=%v, want 0", n, err)
	}

	at := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	archived := map[string]orgArchivedObservation{
		"scout": {At: at, By: "jarvis", Reason: "project paused", ObservedAt: at.Add(time.Minute)},
	}
	// Parking self-invalidates without the mirror row (#1643 round 5), so the
	// archived seat must be mirrored before its directives can park.
	if err := store.UpsertOrgRoleArchived(ctx, db.OrgRoleArchived{
		Role: "scout", ArchivedAt: at.Format(time.RFC3339Nano), ArchivedBy: "jarvis",
		ObservedAt: at.Add(time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	n, err := parkOutstandingDirectivesForArchivedSeats(ctx, store, archived, at.Add(time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("parked %d err=%v, want 1", n, err)
	}
	open, err := store.ListOpenOrgDirectiveObligations(ctx, 50)
	if err != nil || len(open) != 1 || open[0].ID != liveSeat.ID {
		t.Fatalf("open = %+v err=%v, want only keeper's %d", open, err, liveSeat.ID)
	}
	rows, err := store.ListParkedOrgDirectives(ctx, "scout")
	if err != nil || len(rows) != 1 || rows[0].ID != archivedSeat.ID {
		t.Fatalf("parked = %+v err=%v, want scout's %d", rows, err, archivedSeat.ID)
	}
	if want := "seat archived 2026-08-20T09:00:00Z by jarvis: project paused"; rows[0].ParkedReason != want {
		t.Fatalf("parked reason = %q, want %q", rows[0].ParkedReason, want)
	}
}

// After unpark, the evaluator must NOT find the directive due until a full TTL
// has elapsed from unpark time — a directive returning from parking with an
// already-blown TTL must not nag immediately (#1635 ruling). The unpark write
// sets directive_last_nudged_at to unpark time; this pins that the due-check
// honors that anchor.
func TestDirectiveTTLNotDueImmediatelyAfterUnpark(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[org]
directive_ack_ttl = "1h"
[org.roles."owner"]
scope = ["*"]
[org.roles."scout"]
parent = "owner"
scope = ["*"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	unparkAt := time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)
	item := db.OrgDirectiveObligation{
		WorkflowNote: db.WorkflowNote{ID: 7, CreatedAt: "2026-01-01 00:00:00"}, // TTL long blown while parked
		LastNudgedAt: unparkAt.Format(time.RFC3339Nano),                        // what UnparkOrgDirectivesForRole writes
	}
	if _, _, _, _, due := directiveTTLDue(item, cfg, unparkAt.Add(time.Minute)); due {
		t.Fatal("directive due one minute after unpark; the reset anchor is not honored")
	}
	if _, _, _, _, due := directiveTTLDue(item, cfg, unparkAt.Add(time.Hour+time.Minute)); !due {
		t.Fatal("directive not due one full TTL after unpark; the ladder never resumes")
	}
}
