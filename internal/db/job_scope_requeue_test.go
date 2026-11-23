package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// A state-only transition that moves a row into queued re-projects its repo
// and root_id from the payload (#2735): the scheduler's scoped listing trusts
// both columns, and a terminal row, which the startup repair skips, can be
// queued again by any of these transitions without a payload write. Each row
// starts failed with columns an older version left stale (written through a
// second connection), as TestLifecycleGenerationBumpsOnlyOnEntryToQueued's
// failed -> queued retry does, and every state-only writer is probed.
func TestStateOnlyRequeueReprojectsTheScopeColumns(t *testing.T) {
	ctx := context.Background()
	store := openLifecycleGenerationStore(t)
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// A read error yields -1, which no row has, so the anchored transition
	// reports that it did not move the row.
	generation := func(id string) int64 {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			return -1
		}
		return job.LifecycleGeneration
	}
	moved := func(ok bool, err error) error {
		if err == nil && !ok {
			return errors.New("transition did not move the row")
		}
		return err
	}
	retry := JobEvent{Kind: "queued", Message: "retry"}
	requeue := map[string]func(id string) error{
		"UpdateJobState": func(id string) error { return store.UpdateJobState(ctx, id, "queued") },
		"TransitionJobState": func(id string) error {
			return moved(store.TransitionJobState(ctx, id, "failed", "queued"))
		},
		"TransitionJobStateAtGeneration": func(id string) error {
			return moved(store.TransitionJobStateAtGeneration(ctx, id, "failed", generation(id), "queued"))
		},
		"TransitionJobStateWithEvent": func(id string) error {
			return moved(store.TransitionJobStateWithEvent(ctx, id, "failed", "queued", retry))
		},
		"TransitionJobStateWithEventAtGeneration": func(id string) error {
			return moved(store.TransitionJobStateWithEventAtGeneration(ctx, id, "failed", generation(id), "queued", retry))
		},
		"TransitionJobStateWithPayloadAndEvents": func(id string) error {
			return moved(store.TransitionJobStateWithPayloadAndEvents(ctx, id, "failed", "queued", nil, retry))
		},
	}
	for name, run := range requeue {
		t.Run(name, func(t *testing.T) {
			id := "job-" + name
			if err := store.CreateJob(ctx, Job{ID: id, Agent: "lead", Type: "implement", State: "failed",
				Payload: `{"repo":"owner/b","root_job_id":"session-b"}`}); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.ExecContext(ctx, `UPDATE jobs SET repo = 'owner/stale', root_id = 'stale-root' WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
			if err := run(id); err != nil {
				t.Fatal(err)
			}
			var state, repo, root string
			if err := raw.QueryRowContext(ctx, `SELECT state, repo, root_id FROM jobs WHERE id = ?`, id).Scan(&state, &repo, &root); err != nil {
				t.Fatal(err)
			}
			if state != "queued" || repo != "owner/b" || root != "session-b" {
				t.Fatalf("after %s: state=%q repo=%q root_id=%q, want queued owner/b session-b", name, state, repo, root)
			}
		})
	}
}
