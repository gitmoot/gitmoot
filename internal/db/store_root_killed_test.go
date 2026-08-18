package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestRootKilledRoundTrip pins the #341 store contract: a fresh job reads as not
// killed; SetRootJobKilled flips it; IsRootJobKilled and GetJob both observe the
// flag; and an unknown id reads as not killed (fails open) rather than erroring.
func TestRootKilledRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.CreateJobWithEvent(ctx, Job{ID: "root", Agent: "coord", Type: "ask", State: "succeeded"}, JobEvent{Kind: "succeeded", Message: "done"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	killed, err := store.IsRootJobKilled(ctx, "root")
	if err != nil {
		t.Fatalf("IsRootJobKilled returned error: %v", err)
	}
	if killed {
		t.Fatal("a fresh job must read as not killed")
	}
	if job, err := store.GetJob(ctx, "root"); err != nil || job.RootKilled {
		t.Fatalf("GetJob fresh RootKilled = %v (err %v), want false", job.RootKilled, err)
	}

	if err := store.SetRootJobKilled(ctx, "root"); err != nil {
		t.Fatalf("SetRootJobKilled returned error: %v", err)
	}

	killed, err = store.IsRootJobKilled(ctx, "root")
	if err != nil {
		t.Fatalf("IsRootJobKilled (after set) returned error: %v", err)
	}
	if !killed {
		t.Fatal("IsRootJobKilled should report true after SetRootJobKilled")
	}
	job, err := store.GetJob(ctx, "root")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if !job.RootKilled {
		t.Fatalf("GetJob RootKilled = false after kill, want true: %+v", job)
	}

	// Unknown ids fail open: not killed, no error.
	missing, err := store.IsRootJobKilled(ctx, "no-such-root")
	if err != nil {
		t.Fatalf("IsRootJobKilled(missing) returned error: %v", err)
	}
	if missing {
		t.Fatal("an unknown root id must read as not killed")
	}
}

// TestMigrateAppendsRootKilled pins that the root_killed ALTER applies on a
// pre-existing DB that predates the column, defaulting existing jobs to not
// killed, and that existing job scans keep working after the upgrade.
func TestMigrateAppendsRootKilled(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	store := &Store{db: raw}
	// Apply every migration EXCEPT the last (the root_killed ALTER), reproducing a
	// DB whose jobs table has no root_killed column.
	for version, migration := range migrationsBefore(t, "root_killed") {
		if err := store.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d) returned error: %v", version+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO jobs(id, agent, type, state, payload) VALUES ('legacy', 'coord', 'ask', 'succeeded', '{}')`); err != nil {
		t.Fatalf("insert legacy job returned error: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw Close returned error: %v", err)
	}

	upgraded, err := openRealTestStore(t, path)
	if err != nil {
		t.Fatalf("Open upgraded DB returned error: %v", err)
	}
	defer upgraded.Close()

	job, err := upgraded.GetJob(ctx, "legacy")
	if err != nil {
		t.Fatalf("GetJob legacy returned error: %v", err)
	}
	if job.RootKilled {
		t.Fatalf("legacy job RootKilled = true, want false default")
	}
	// Existing scans still work end to end after the upgrade.
	jobs, err := upgraded.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "legacy" || jobs[0].RootKilled {
		t.Fatalf("ListJobs after upgrade = %+v, want one un-killed legacy job", jobs)
	}
}
