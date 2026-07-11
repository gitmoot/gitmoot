package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func openWorkflowTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedWorkflowJob(t *testing.T, store *Store, id, workflowID, state, repo string, in, out int) {
	t.Helper()
	payload := `{"repo":"` + repo + `","workflow_id":"` + workflowID + `"}`
	if err := store.CreateJob(context.Background(), Job{ID: id, Agent: "worker", Type: "ask", State: state, Payload: payload}); err != nil {
		t.Fatalf("CreateJob(%s): %v", id, err)
	}
	if err := store.UpdateJobUsage(context.Background(), id, in, out); err != nil {
		t.Fatalf("UpdateJobUsage(%s): %v", id, err)
	}
}

func TestWorkflowStoreAggregatesAndFiltersByIndexedColumn(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	seedWorkflowJob(t, store, "a", "release-42", "queued", "acme/widget", 2, 3)
	seedWorkflowJob(t, store, "b", "release-42", "succeeded", "acme/widget", 5, 7)
	seedWorkflowJob(t, store, "c", "other", "failed", "acme/other", 100, 100)
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: "release-42", Body: "checkpoint"}); err != nil {
		t.Fatalf("InsertWorkflowNote: %v", err)
	}
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: "release-42", Body: "checkpoint two"}); err != nil {
		t.Fatalf("InsertWorkflowNote: %v", err)
	}

	summary, err := store.WorkflowSummary(ctx, "release-42")
	if err != nil {
		t.Fatalf("WorkflowSummary: %v", err)
	}
	if summary.JobCount != 2 || summary.Queued != 1 || summary.Succeeded != 1 || summary.NoteCount != 2 || summary.InputTokens != 7 || summary.OutputTokens != 10 {
		t.Fatalf("summary = %+v", summary)
	}
	jobs, err := store.ListJobsByWorkflow(ctx, "release-42", 0)
	if err != nil {
		t.Fatalf("ListJobsByWorkflow: %v", err)
	}
	if len(jobs) != 2 || jobs[0].ID != "a" || jobs[1].ID != "b" || jobs[0].Payload != "" || jobs[0].Repo != "acme/widget" {
		t.Fatalf("jobs = %+v", jobs)
	}
	limitedJobs, err := store.ListJobsByWorkflow(ctx, "release-42", 1)
	if err != nil || len(limitedJobs) != 1 || limitedJobs[0].ID != "a" {
		t.Fatalf("limited jobs=%+v err=%v", limitedJobs, err)
	}
	limitedNotes, err := store.ListWorkflowNotes(ctx, "release-42", 1)
	if err != nil || len(limitedNotes) != 1 || limitedNotes[0].Body != "checkpoint" {
		t.Fatalf("limited notes=%+v err=%v", limitedNotes, err)
	}
	repos, err := store.WorkflowRepos(ctx, "release-42")
	if err != nil || len(repos) != 1 || repos[0] != "acme/widget" {
		t.Fatalf("repos=%v err=%v", repos, err)
	}
	if strings.Contains(strings.ToLower(ListJobsByWorkflowSQL), "payload") || strings.Contains(strings.ToLower(WorkflowReposSQL), "payload") {
		t.Fatal("workflow scalar queries must not read or parse payload")
	}
}

func TestWorkflowProductionQueriesUseIndexes(t *testing.T) {
	store := openWorkflowTestStore(t)
	queries := []struct {
		name  string
		query string
		args  []any
		index string
	}{
		{"list", ListWorkflowSummariesSQL, nil, "idx_jobs_workflow_id"},
		{"show-summary", WorkflowSummarySQL, []any{"release-42", "release-42", "release-42"}, "idx_jobs_workflow_id"},
		{"show-jobs", ListJobsByWorkflowSQL, []any{"release-42", 100}, "idx_jobs_workflow_id"},
		{"show-notes", ListWorkflowNotesSQL, []any{"release-42", 100}, "idx_workflow_notes_wid"},
		{"filter", CountJobsByWorkflowSQL, []any{"release-42"}, "idx_jobs_workflow_id"},
		{"repo-inference", WorkflowReposSQL, []any{"release-42"}, "idx_jobs_workflow_id"},
	}
	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := store.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+tc.query, tc.args...)
			if err != nil {
				t.Fatalf("EXPLAIN production query: %v", err)
			}
			defer rows.Close()
			var plan strings.Builder
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatalf("scan plan: %v", err)
				}
				plan.WriteString(detail)
			}
			if !strings.Contains(plan.String(), tc.index) {
				t.Fatalf("plan does not use %s: %s", tc.index, plan.String())
			}
		})
	}
}

func TestWorkflowSummaryUnknownReturnsNoRows(t *testing.T) {
	store := openWorkflowTestStore(t)
	if _, err := store.WorkflowSummary(context.Background(), "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("WorkflowSummary(missing) error=%v, want sql.ErrNoRows", err)
	}
}

func TestWorkflowIDImmutableAcrossPayloadUpdatePaths(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	seed := func(id, state, workflowID string) string {
		payload := `{"repo":"acme/widget","pull_request":7,"workflow_id":"` + workflowID + `"}`
		if err := store.CreateJob(ctx, Job{ID: id, Agent: "from", Type: "ask", State: state, Payload: payload}); err != nil {
			t.Fatalf("CreateJob(%s): %v", id, err)
		}
		return payload
	}
	assertUnchanged := func(id, wantPayload, wantState, wantAgent string) {
		t.Helper()
		var payload, workflowID, state, agent string
		if err := store.db.QueryRowContext(ctx, `SELECT payload, workflow_id, state, agent FROM jobs WHERE id = ?`, id).Scan(&payload, &workflowID, &state, &agent); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if payload != wantPayload || workflowID != "release-42" || state != wantState || agent != wantAgent {
			t.Fatalf("job %s payload=%q workflow=%q state=%q agent=%q", id, payload, workflowID, state, agent)
		}
	}
	assertImmutable := func(err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "workflow id is immutable") {
			t.Fatalf("error=%v, want immutable workflow rejection", err)
		}
	}

	seed("update", "running", "release-42")
	if err := store.UpdateJobPayload(ctx, "update", `{"repo":"acme/new","pull_request":9,"workflow_id":"release-42"}`); err != nil {
		t.Fatalf("same-label UpdateJobPayload: %v", err)
	}
	var repo string
	var pr int
	if err := store.db.QueryRowContext(ctx, `SELECT repo, pull_request FROM jobs WHERE id = 'update'`).Scan(&repo, &pr); err != nil || repo != "acme/new" || pr != 9 {
		t.Fatalf("updated scalar projection repo=%q pr=%d err=%v", repo, pr, err)
	}
	current, _ := store.GetJob(ctx, "update")
	assertImmutable(store.UpdateJobPayload(ctx, "update", `{"repo":"evil/repo","workflow_id":"other"}`))
	assertUnchanged("update", current.Payload, "running", "from")

	transitionOriginal := seed("transition", "queued", "release-42")
	ok, err := store.TransitionJobStatePayloadWithEvent(ctx, "transition", "queued", "running", `{"repo":"acme/widget"}`, JobEvent{Kind: "running", Message: "must not write"})
	if ok {
		t.Fatal("mismatched transition succeeded")
	}
	assertImmutable(err)
	assertUnchanged("transition", transitionOriginal, "queued", "from")
	events, err := store.ListJobEvents(ctx, "transition")
	if err != nil || len(events) != 0 {
		t.Fatalf("mismatched transition events=%+v err=%v", events, err)
	}

	delegateOriginal := seed("delegate", "queued", "release-42")
	ok, err = store.DelegateQueuedJob(ctx, "delegate", "from", "to", `{"repo":"acme/widget","workflow_id":"other"}`, JobEvent{Kind: "delegated"})
	if ok {
		t.Fatal("mismatched delegation succeeded")
	}
	assertImmutable(err)
	assertUnchanged("delegate", delegateOriginal, "queued", "from")

	unlabelled := seed("unlabelled", "running", "")
	err = store.UpdateJobPayload(ctx, "unlabelled", `{"repo":"acme/widget","workflow_id":"release-42"}`)
	assertImmutable(err)
	var gotPayload, gotWorkflow string
	if err := store.db.QueryRowContext(ctx, `SELECT payload, workflow_id FROM jobs WHERE id = 'unlabelled'`).Scan(&gotPayload, &gotWorkflow); err != nil || gotPayload != unlabelled || gotWorkflow != "" {
		t.Fatalf("unlabelled job mutated payload=%q workflow=%q err=%v", gotPayload, gotWorkflow, err)
	}
}

func TestWorkflowIDDerivedAtEveryJobInsertPath(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	jobs := []struct {
		id   string
		open func() error
	}{
		{"plain", func() error {
			return store.CreateJob(ctx, Job{ID: "plain", Agent: "a", Type: "ask", State: "queued", Payload: `{"workflow_id":"batch-one"}`})
		}},
		{"event", func() error {
			return store.CreateJobWithEvent(ctx, Job{ID: "event", Agent: "a", Type: "ask", State: "queued", Payload: `{"workflow_id":"batch-one"}`}, JobEvent{Kind: "queued"})
		}},
		{"external", func() error {
			return store.CreateExternallyDrivenJobWithEvent(ctx, Job{ID: "external", Agent: "a", Type: "ask", State: "running", Payload: `{"workflow_id":"batch-one"}`}, JobEvent{Kind: "running"})
		}},
	}
	for _, tc := range jobs {
		if err := tc.open(); err != nil {
			t.Fatalf("insert %s: %v", tc.id, err)
		}
		var got string
		if err := store.db.QueryRowContext(ctx, `SELECT workflow_id FROM jobs WHERE id = ?`, tc.id).Scan(&got); err != nil || got != "batch-one" {
			t.Fatalf("workflow_id(%s) = %q, err=%v", tc.id, got, err)
		}
	}
}

func TestWorkflowNoteAndObservationAreAtomic(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	_, _, err := store.InsertWorkflowNoteWithObservation(ctx,
		WorkflowNote{WorkflowID: "release-42", Body: "fact"},
		MemoryObservation{Content: "fact"})
	if err == nil {
		t.Fatal("expected missing observation owner to fail")
	}
	var notes int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_notes`).Scan(&notes); err != nil || notes != 0 {
		t.Fatalf("notes after rollback = %d, err=%v", notes, err)
	}
	note, obs, err := store.InsertWorkflowNoteWithObservation(ctx,
		WorkflowNote{WorkflowID: "release-42", Author: "human", Body: "arm64 CI is flaky", Repo: "acme/widget"},
		MemoryObservation{Owner: MemoryOwner{Kind: "shared", Ref: "shared"}, AuthorRef: "human", Repo: "acme/widget", Scope: "repo", Content: "arm64 CI is flaky", TrustMark: "low"})
	if err != nil {
		t.Fatalf("InsertWorkflowNoteWithObservation: %v", err)
	}
	id := strconv.FormatInt(note.ID, 10)
	if note.MemoryObservationID != obs.ID || obs.Key != "workflow-release-42-"+id || obs.Provenance != "workflow:release-42#"+id {
		t.Fatalf("note=%+v obs=%+v", note, obs)
	}
}

func TestWorkflowMigrationDefaultsExistingJobsToUnlabelled(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	store := &Store{db: raw}
	for version, migration := range migrations[:len(migrations)-1] {
		if err := store.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	const payload = `{"repo":"acme/widget","instructions":"legacy"}`
	if _, err := raw.ExecContext(ctx, `INSERT INTO jobs(id, agent, type, state, payload) VALUES ('legacy', 'a', 'ask', 'succeeded', ?)`, payload); err != nil {
		t.Fatalf("insert legacy job: %v", err)
	}
	if err := store.applyMigration(ctx, len(migrations), migrations[len(migrations)-1]); err != nil {
		t.Fatalf("apply workflow migration: %v", err)
	}
	var gotPayload, workflowID, repo string
	var pullRequest int
	if err := raw.QueryRowContext(ctx, `SELECT payload, workflow_id, repo, pull_request FROM jobs WHERE id = 'legacy'`).Scan(&gotPayload, &workflowID, &repo, &pullRequest); err != nil {
		t.Fatalf("read upgraded job: %v", err)
	}
	if gotPayload != payload || workflowID != "" || repo != "" || pullRequest != 0 {
		t.Fatalf("upgraded job payload=%q workflow_id=%q repo=%q pull_request=%d", gotPayload, workflowID, repo, pullRequest)
	}
	_ = raw.Close()
}
