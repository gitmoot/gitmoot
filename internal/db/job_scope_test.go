package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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
		for _, class := range []string{"TEXT", "BLOB"} {
			t.Run(name+"/"+class, func(t *testing.T) {
				id := "job-" + name + "-" + class
				if err := store.CreateJob(ctx, Job{ID: id, Agent: "lead", Type: "implement", State: "failed",
					Payload: `{"repo":"owner/b","root_job_id":"session-b"}`}); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.ExecContext(ctx, `UPDATE jobs SET payload = CAST(payload AS `+class+`), repo = 'owner/stale', root_id = 'stale-root' WHERE id = ?`, id); err != nil {
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
}

// legacyScopeRows writes the four stale live rows the startup repair must fix:
// a repo the #1066 json_extract backfill projected from the first of two
// duplicate keys, and a root_id an older payload update left behind, each with
// its payload stored as TEXT and as BLOB (an older []byte writer bound one).
// It returns each row's id with the repo and root_id it must end with.
func legacyScopeRows(t *testing.T, ctx context.Context, store *Store, raw *sql.DB) map[string][2]string {
	t.Helper()
	want := map[string][2]string{}
	for _, class := range []string{"TEXT", "BLOB"} {
		rows := []struct{ id, payload, stale, repo, root string }{
			{"repo-" + class, `{"repo":"owner/a","Repo":"owner/b"}`, `repo = 'owner/a'`, "owner/b", "repo-" + class},
			{"root-" + class, `{"repo":"owner/c","root_job_id":"session-b"}`, `root_id = 'stale-root'`, "owner/c", "session-b"},
		}
		for _, row := range rows {
			if err := store.CreateJob(ctx, Job{ID: row.id, Agent: "lead", Type: "implement", State: "queued", Payload: row.payload}); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.ExecContext(ctx, `UPDATE jobs SET payload = CAST(payload AS `+class+`), `+row.stale+` WHERE id = ?`, row.id); err != nil {
				t.Fatal(err)
			}
			want[row.id] = [2]string{row.repo, row.root}
		}
	}
	return want
}

// The startup repair compares each payload in its own storage class, so it
// fixes TEXT and BLOB rows alike and leaves the payload as stored (#2735
// round 3, P2). Bound as a Go string, the BLOB rows' compare-and-set matched
// nothing, and the repair reported success over two stale rows.
func TestReprojectLiveJobScopeRepairsTextAndBlobPayloads(t *testing.T) {
	ctx := context.Background()
	store := openLifecycleGenerationStore(t)
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	want := legacyScopeRows(t, ctx, store, raw)
	changed, err := store.ReprojectLiveJobScope(ctx)
	if err != nil || changed != len(want) {
		t.Fatalf("ReprojectLiveJobScope = %d, %v; want %d, nil", changed, err, len(want))
	}
	for id, cols := range want {
		var repo, root, class string
		if err := raw.QueryRowContext(ctx, `SELECT repo, root_id, typeof(payload) FROM jobs WHERE id = ?`, id).Scan(&repo, &root, &class); err != nil {
			t.Fatal(err)
		}
		if repo != cols[0] || root != cols[1] {
			t.Errorf("%s: repo=%q root_id=%q, want %q %q", id, repo, root, cols[0], cols[1])
		}
		if wantClass := strings.ToLower(id[strings.IndexByte(id, '-')+1:]); class != wantClass {
			t.Errorf("%s: payload stored as %s after the repair, want %s", id, class, wantClass)
		}
	}
	if changed, err := store.ReprojectLiveJobScope(ctx); err != nil || changed != 0 {
		t.Fatalf("second ReprojectLiveJobScope = %d, %v; want 0, nil", changed, err)
	}
}

// A row the repair must rewrite but whose rewrite matches nothing fails the
// repair, so its caller retries instead of publishing a verified scope over a
// stale row. A trigger that ignores the row's update stands in for a writer
// that changed it between the repair's read and its write; once it is gone,
// the retry repairs every row.
func TestReprojectLiveJobScopeFailsWhenARowIsNotRewritten(t *testing.T) {
	ctx := context.Background()
	store := openLifecycleGenerationStore(t)
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	want := legacyScopeRows(t, ctx, store, raw)
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER hold_scope_repair BEFORE UPDATE OF repo ON jobs
		WHEN OLD.id = 'repo-BLOB' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.ReprojectLiveJobScope(ctx); err == nil || !strings.Contains(err.Error(), "repo-BLOB") {
		t.Fatalf("ReprojectLiveJobScope with repo-BLOB held = %d, %v; want an error naming repo-BLOB", changed, err)
	}
	if _, err := raw.ExecContext(ctx, `DROP TRIGGER hold_scope_repair`); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.ReprojectLiveJobScope(ctx); err != nil || changed != len(want) {
		t.Fatalf("retried ReprojectLiveJobScope = %d, %v; want %d, nil", changed, err, len(want))
	}
}

// The cleanup publications compare the owner snapshot the caller read with the
// stored payload as TEXT, so a terminal row whose payload is stored as a BLOB
// still takes its artifacts and diff (#2735 round 3): compared as stored, the
// BLOB never equalled the caller's text and every publication looked stale.
func TestCleanupPublicationMatchesABlobStoredPayload(t *testing.T) {
	ctx := context.Background()
	store := openLifecycleGenerationStore(t)
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := store.CreateJob(ctx, Job{ID: "producer", Agent: "lead", Type: "ask", State: "succeeded", Payload: `{"repo":"owner/a"}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE jobs SET payload = CAST(payload AS BLOB) WHERE id = 'producer'`); err != nil {
		t.Fatal(err)
	}
	owner, err := store.GetJob(ctx, "producer")
	if err != nil {
		t.Fatal(err)
	}
	if written, err := store.RecordCleanupArtifactSnapshot(ctx, owner, `{"resource_id":"r"}`); err != nil || !written {
		t.Fatalf("artifact publication on a BLOB payload: written=%t err=%v", written, err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE jobs SET payload = CAST(payload AS BLOB) WHERE id = 'producer'`); err != nil {
		t.Fatal(err)
	}
	if owner, err = store.GetJob(ctx, "producer"); err != nil {
		t.Fatal(err)
	}
	if written, err := store.RecordReadOnlyCleanupDiff(ctx, owner, "diff", false, ""); err != nil || !written {
		t.Fatalf("diff publication on a BLOB payload: written=%t err=%v", written, err)
	}
}
