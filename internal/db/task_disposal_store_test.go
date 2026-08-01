package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskDisposalMigrationIsAppendOnlyTail(t *testing.T) {
	if len(migrations) < 2 {
		t.Fatalf("migrations length = %d", len(migrations))
	}
	tail := migrations[len(migrations)-1]
	if !strings.Contains(tail, "disposal_tier") || !strings.Contains(tail, "idx_tasks_disposal_candidates") ||
		!strings.Contains(tail, "task_emit_count") || !strings.Contains(tail, "task_exhausted_at") {
		t.Fatalf("last migration is not #1344 task disposal migration")
	}
	if !strings.Contains(migrations[len(migrations)-2], "CREATE TABLE awaited_facts") {
		t.Fatalf("#1368 awaited-facts migration was not preserved immediately before the new tail")
	}
}

func TestTaskDisposalStatesCannotBeOverwrittenByConditionalUpsert(t *testing.T) {
	for _, terminal := range []string{"superseded", "stranded"} {
		t.Run(terminal, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
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
