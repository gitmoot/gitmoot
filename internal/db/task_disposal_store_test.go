package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestTaskDisposalMigrationIsAppendOnlyTail pins that the #1344 disposal migration was
// APPENDED and has never been REORDERED. Migrations are positional, so moving an existing
// entry re-numbers every schema version after it and corrupts partially-migrated databases.
//
// It used to assert the disposal migration was migrations[len-1] -- literally the last one.
// That is not the invariant. It is an accident of that migration having been written last,
// and it made the guard fail the moment any later migration appended a new tail, which is
// exactly what append-only PERMITS. #1407's lifecycle_generation migration is what tripped
// it. Re-pointing this test at THAT migration would only re-arm the same trap for whoever
// appends next.
//
// The durable form locates the disposal migration BY CONTENT and asserts its position
// relative to its predecessor. That is strictly stronger for the stated purpose -- it still
// fails if the entry is moved, duplicated, or removed -- and it does not decay on the next
// append.
//
// internal/db has other len(migrations)-1 sites that say "apply every migration except the
// last" and quietly mean something different after each new tail. They fail SILENTLY rather
// than loudly, they predate this change, and they are not touched here; tracked in gitmoot#1435.
func TestTaskDisposalMigrationIsAppendOnlyTail(t *testing.T) {
	if len(migrations) < 2 {
		t.Fatalf("migrations length = %d", len(migrations))
	}
	disposal := -1
	for i, migration := range migrations {
		if strings.Contains(migration, "disposal_tier") && strings.Contains(migration, "idx_tasks_disposal_candidates") &&
			strings.Contains(migration, "task_emit_count") && strings.Contains(migration, "task_exhausted_at") {
			if disposal >= 0 {
				t.Fatalf("#1344 task disposal migration appears at BOTH index %d and %d; a duplicated migration re-runs its DDL", disposal, i)
			}
			disposal = i
		}
	}
	if disposal < 0 {
		t.Fatal("#1344 task disposal migration is no longer present in the slice")
	}
	if disposal == 0 {
		t.Fatal("#1344 task disposal migration is at index 0; it was appended, not prepended, and migrations are positional")
	}
	if !strings.Contains(migrations[disposal-1], "CREATE TABLE awaited_facts") {
		t.Fatalf("the migration before the #1344 disposal entry (index %d) is not the #1368 awaited-facts migration; the slice was reordered, which re-numbers every schema version after it", disposal)
	}
}

func TestTaskDisposalStatesCannotBeOverwrittenByConditionalUpsert(t *testing.T) {
	for _, terminal := range []string{"superseded", "stranded"} {
		t.Run(terminal, func(t *testing.T) {
			store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			if err := store.UpsertTask(ctx, Task{ID: "task-terminal", State: terminal}); err != nil {
				t.Fatal(err)
			}
			changed, err := store.UpsertTaskUnlessStates(ctx, Task{ID: "task-terminal", State: "reviewing"}, []string{"dismissed", "superseded", "stranded"})
			if err != nil || changed {
				t.Fatalf("conditional upsert changed=%v err=%v", changed, err)
			}
			got, _ := store.GetTask(ctx, "task-terminal")
			if got.State != terminal {
				t.Fatalf("task state = %q, want %q", got.State, terminal)
			}
		})
	}
}

// TestUpsertTaskUnlessStatesDecidesAtWriteTimeAcrossConnections pins the property
// the workflow engine's merged-regression guard leans on (#1673): the forbidden-state
// predicate is evaluated by the UPDATE itself, so a state written by ANOTHER
// connection after the caller's read still wins.
//
// Two Store handles on the same file stand in for two daemons. Handle A reads
// `reviewing` — the read a pre-write advisory check would have approved — then
// handle B lands `merged`, and only then does A issue its stale conditional write.
// A `merged` row surviving that is the whole difference between an atomic guard and
// an advisory one; against a plain UpsertTask the stale `blocked` lands.
func TestUpsertTaskUnlessStatesDecidesAtWriteTimeAcrossConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	engineSide, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer engineSide.Close()
	otherDaemon, err := openCachedTestStore(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer otherDaemon.Close()

	ctx := context.Background()
	seed := Task{ID: "task-7", RepoFullName: "owner/repo", Title: "Parent", State: "reviewing", Branch: "task-7"}
	if err := engineSide.UpsertTask(ctx, seed); err != nil {
		t.Fatal(err)
	}

	stale, err := engineSide.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if stale.State != "reviewing" {
		t.Fatalf("pre-read state = %q, want reviewing (the window must open on a permitted state)", stale.State)
	}

	merged := seed
	merged.State = "merged"
	if err := otherDaemon.UpsertTask(ctx, merged); err != nil {
		t.Fatalf("other daemon's merged write returned error: %v", err)
	}

	stale.State = "blocked"
	changed, err := engineSide.UpsertTaskUnlessStates(ctx, stale, []string{"merged"})
	if err != nil {
		t.Fatalf("conditional upsert returned error: %v", err)
	}
	if changed {
		t.Fatal("conditional upsert reported changed=true; it wrote over a row that had merged since the read")
	}
	got, err := otherDaemon.GetTask(ctx, "task-7")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if got.State != "merged" {
		t.Fatalf("task state = %q, want merged: the stale write raced the merge and won", got.State)
	}
}
