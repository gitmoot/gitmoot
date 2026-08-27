package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

// An unreadable stored poll stamp must surface as an ERROR, never as
// "never succeeded" (#1643 round 9 checklist walk): ok=false is a claim that
// no stamp exists, and asserting it about a stamp that exists-but-cannot-be-
// read flattens three states into two. The doctor's stamp-unreadable branch
// depends on the error being surfaced. Mutant S9-M (restore `return zero,
// false, nil` on parse failure) dies here.
func TestOrgArchivePollLastSuccessUnreadableStampFailsLoud(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()

	// Control: a well-formed stamp round-trips.
	at := time.Date(2026, 8, 27, 5, 0, 0, 0, time.UTC)
	if err := store.RecordOrgArchivePollSuccess(ctx, at); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.OrgArchivePollLastSuccess(ctx)
	if err != nil || !ok || !got.Equal(at) {
		t.Fatalf("well-formed stamp = %v ok=%v err=%v, want %v", got, ok, err, at)
	}

	// Corrupt the stored stamp directly (no production path writes one, so
	// the route in is manual repair / partial restore — the fail DIRECTION is
	// what this pins).
	if _, err := store.db.ExecContext(ctx, `UPDATE org_archive_poll SET last_success_at = 'not-a-timestamp' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	_, ok, err = store.OrgArchivePollLastSuccess(ctx)
	if err == nil || ok {
		t.Fatalf("unreadable stamp returned ok=%v err=%v; an existing-but-unreadable stamp must not read as never-succeeded", ok, err)
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("error %q does not name the unreadable stamp", err)
	}
}
