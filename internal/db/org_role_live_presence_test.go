package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRoleLivePresenceUpsertListAndReap(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	var table string
	if err := store.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master
		WHERE type = 'table' AND name = 'org_role_live_presence'`).Scan(&table); err != nil {
		t.Fatalf("org_role_live_presence migration missing: %v", err)
	}
	first := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)

	if err := store.UpsertRoleLivePresence(ctx, " Owner ", "idle", first); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRoleLivePresence(ctx, "owner", "working", second); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertRoleLivePresence(ctx, "review", "done", second); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListRoleLivePresence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Role != "owner" || rows[0].State != "working" ||
		rows[0].ObservedAt != second.Format(time.RFC3339Nano) || rows[1].Role != "review" {
		t.Fatalf("rows = %+v", rows)
	}

	if err := store.DeleteRoleLivePresenceExcept(ctx, []string{"owner", "owner", ""}); err != nil {
		t.Fatal(err)
	}
	rows, err = store.ListRoleLivePresence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Role != "owner" {
		t.Fatalf("rows after reap = %+v", rows)
	}
	// An empty keep-set is a no-op, NOT a wipe: a transient empty/all-blank
	// snapshot must never erase presence for a still-live fleet.
	if err := store.DeleteRoleLivePresenceExcept(ctx, nil); err != nil {
		t.Fatal(err)
	}
	rows, err = store.ListRoleLivePresence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Role != "owner" {
		t.Fatalf("empty snapshot reap should be a no-op, rows = %+v", rows)
	}
}
