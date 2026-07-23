package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationAddsUsageColumns_Idempotent(t *testing.T) {
	ctx := context.Background()
	// Locate the usage migration by content, not by tail position: versions are
	// positional and later migrations legitimately append after this one.
	usageIndex := -1
	for i, m := range migrations {
		if strings.Contains(m, "ADD COLUMN injected_count") {
			usageIndex = i
			break
		}
	}
	if usageIndex < 0 {
		t.Fatal("usage-columns migration not found")
	}
	usageMigration := migrations[usageIndex]
	for _, column := range []string{"injected_count", "last_injected_at", "recalled_count", "last_recalled_at"} {
		if !strings.Contains(usageMigration, "ADD COLUMN "+column) {
			t.Fatalf("usage migration missing %s", column)
		}
	}
	path := filepath.Join(t.TempDir(), "pre-usage.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	raw.SetMaxIdleConns(1)
	if err := configureWritableSQLite(ctx, raw); err != nil {
		t.Fatal(err)
	}
	store := &Store{db: raw}
	t.Cleanup(func() { _ = store.Close() })
	for i, migration := range migrations[:usageIndex] {
		if err := store.applyMigration(ctx, i+1, migration); err != nil {
			t.Fatalf("apply pre-usage migration %d: %v", i+1, err)
		}
	}
	id := mustUpsert(t, store, ConfirmedMemory{
		Owner: agentOwner("builder"), Repo: "acme/widget", Scope: "repo", Key: "legacy", Content: "legacy fact",
	})
	if err := store.applyMigration(ctx, usageIndex+1, usageMigration); err != nil {
		t.Fatalf("apply usage migration: %v", err)
	}
	var injected int64
	var lastInjected string
	var recalled int64
	var lastRecalled string
	if err := store.db.QueryRowContext(ctx, `SELECT injected_count, last_injected_at, recalled_count, last_recalled_at FROM confirmed_memories WHERE id = ?`, id).
		Scan(&injected, &lastInjected, &recalled, &lastRecalled); err != nil {
		t.Fatal(err)
	}
	if injected != 0 || lastInjected != "" || recalled != 0 || lastRecalled != "" {
		t.Fatalf("usage defaults = %d/%q %d/%q", injected, lastInjected, recalled, lastRecalled)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestUpdateInjectedCounters(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	first := mustUpsert(t, store, ConfirmedMemory{Owner: agentOwner("builder"), Scope: "general", Key: "first", Content: "first fact"})
	second := mustUpsert(t, store, ConfirmedMemory{Owner: agentOwner("builder"), Scope: "general", Key: "second", Content: "second fact"})
	if err := store.UpdateInjectedCounters(ctx, nil); err != nil {
		t.Fatalf("empty update: %v", err)
	}
	if err := store.UpdateInjectedCounters(ctx, []int64{first, first}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateInjectedCounters(ctx, []int64{first}); err != nil {
		t.Fatal(err)
	}
	firstRecord, err := store.GetConfirmedMemoryByID(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	secondRecord, err := store.GetConfirmedMemoryByID(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if firstRecord.InjectedCount != 2 || firstRecord.LastInjectedAt == "" || firstRecord.RecalledCount != 0 {
		t.Fatalf("first usage = %+v", firstRecord.ConfirmedMemory)
	}
	if secondRecord.InjectedCount != 0 || secondRecord.LastInjectedAt != "" {
		t.Fatalf("second usage = %+v", secondRecord.ConfirmedMemory)
	}
}

func TestUpdateRecalledCounters(t *testing.T) {
	ctx := context.Background()
	store := openMemTestStore(t)
	id := mustUpsert(t, store, ConfirmedMemory{Owner: agentOwner("builder"), Scope: "general", Key: "fact", Content: "one fact"})
	if err := store.UpdateRecalledCounters(ctx, []int64{id, id}); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetConfirmedMemoryByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if record.RecalledCount != 1 || record.LastRecalledAt == "" || record.InjectedCount != 0 || record.LastInjectedAt != "" {
		t.Fatalf("usage = %+v", record.ConfirmedMemory)
	}
}
