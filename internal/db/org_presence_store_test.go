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
