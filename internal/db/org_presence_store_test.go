package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOrgRolePresenceMigrationAndUpsert(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	var table string
	if err := store.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name='org_role_presence'`).Scan(&table); err != nil {
		t.Fatalf("org_role_presence migration missing: %v", err)
	}
	if err := store.TouchOrgRolePresence(ctx, "owner", "org brief"); err != nil {
		t.Fatalf("TouchOrgRolePresence(insert) error = %v", err)
	}
	if err := store.TouchOrgRolePresence(ctx, "owner", "agent run"); err != nil {
		t.Fatalf("TouchOrgRolePresence(update) error = %v", err)
	}
	if err := store.TouchOrgRolePresence(ctx, "review", "org status"); err != nil {
		t.Fatalf("TouchOrgRolePresence(second role) error = %v", err)
	}
	presence, err := store.ListOrgRolePresence(ctx)
	if err != nil {
		t.Fatalf("ListOrgRolePresence() error = %v", err)
	}
	if len(presence) != 2 || presence[0].Role != "owner" || presence[0].LastCommand != "agent run" || presence[0].LastSeenAt == "" || presence[1].Role != "review" {
		t.Fatalf("presence = %+v", presence)
	}
	owner, found, err := store.GetOrgRolePresence(ctx, " owner ")
	if err != nil || !found || owner.Role != "owner" || owner.LastCommand != "agent run" || owner.LastSeenAt == "" {
		t.Fatalf("GetOrgRolePresence(owner) = %+v, %v, %v", owner, found, err)
	}
	missing, found, err := store.GetOrgRolePresence(ctx, "missing")
	if err != nil || found || missing != (OrgRolePresence{}) {
		t.Fatalf("GetOrgRolePresence(missing) = %+v, %v, %v", missing, found, err)
	}
}

func TestOrgRolePresenceKeyIsCanonicalized(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	// A raw case-variant --org-role (e.g. "OWNER") must land on the canonical
	// row so recycle enforcement's canonical read cannot miss it (the off->block
	// case-staleness class).
	if err := store.TouchOrgRolePresence(ctx, "OWNER", "agent run"); err != nil {
		t.Fatalf("TouchOrgRolePresence(OWNER) error = %v", err)
	}
	rows, err := store.ListOrgRolePresence(ctx)
	if err != nil || len(rows) != 1 || rows[0].Role != "owner" {
		t.Fatalf("presence keyed non-canonically: %+v (err %v)", rows, err)
	}
	for _, lookup := range []string{"owner", "OWNER", " Owner "} {
		got, found, err := store.GetOrgRolePresence(ctx, lookup)
		if err != nil || !found || got.Role != "owner" {
			t.Fatalf("GetOrgRolePresence(%q) = %+v, %v, %v; want canonical owner", lookup, got, found, err)
		}
	}
}

func TestTouchOrgRolePresenceRejectsEmptyRole(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	if err := store.TouchOrgRolePresence(context.Background(), " ", "org brief"); err == nil {
		t.Fatal("TouchOrgRolePresence() error = nil, want validation error")
	}
}
