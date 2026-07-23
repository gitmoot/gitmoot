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
		{"show-summary", WorkflowSummarySQL, []any{"release-42"}, "idx_jobs_workflow_id"},
		{"show-jobs", ListJobsByWorkflowSQL, []any{"release-42", 100}, "idx_jobs_workflow_id"},
		{"dashboard-graph-jobs", ListWorkflowGraphJobsSQL, []any{"release-42"}, "idx_jobs_workflow_id"},
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

func TestListWorkflowGraphJobsReturnsBoundedPayloadAndRootProjection(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	rootPayload := `{"workflow_id":"release-42","repo":"acme/widget","task_title":"root"}`
	childPayload := `{"workflow_id":"release-42","repo":"acme/widget","root_job_id":"root","parent_job_id":"root","deps":["prep"]}`
	if err := store.CreateJob(ctx, Job{ID: "root", Agent: "lead", Type: "orchestrate", State: "running", Payload: rootPayload}); err != nil {
		t.Fatalf("CreateJob(root): %v", err)
	}
	if err := store.CreateJob(ctx, Job{ID: "child", Agent: "worker", Type: "implement", State: "queued", Payload: childPayload, ParentJobID: "root", DelegationID: "ship", DelegationDepth: 1}); err != nil {
		t.Fatalf("CreateJob(child): %v", err)
	}
	seedWorkflowJob(t, store, "other", "other-label", "succeeded", "acme/other", 1, 1)

	jobs, err := store.ListWorkflowGraphJobs(ctx, "release-42")
	if err != nil {
		t.Fatalf("ListWorkflowGraphJobs: %v", err)
	}
	if len(jobs) != 2 || jobs[0].ID != "child" && jobs[0].ID != "root" {
		t.Fatalf("jobs = %+v", jobs)
	}
	byID := map[string]Job{}
	for _, job := range jobs {
		byID[job.ID] = job
	}
	if byID["root"].Payload != rootPayload || byID["root"].RootID != "root" {
		t.Fatalf("root projection = %+v", byID["root"])
	}
	if byID["child"].Payload != childPayload || byID["child"].RootID != "root" || byID["child"].ParentJobID != "root" {
		t.Fatalf("child projection = %+v", byID["child"])
	}
	if _, ok := byID["other"]; ok {
		t.Fatalf("projection leaked another workflow: %+v", jobs)
	}
	if !strings.Contains(ListWorkflowGraphJobsSQL, "payload") || !strings.Contains(ListWorkflowGraphJobsSQL, "workflow_id != ''") {
		t.Fatalf("projection must read payload only behind the partial-index predicate: %s", ListWorkflowGraphJobsSQL)
	}
}

func TestWorkflowNoteCountsGroupsRequestedLabels(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	for _, label := range []string{"release-42", "release-42", "other"} {
		if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: label, Body: "note"}); err != nil {
			t.Fatalf("InsertWorkflowNote(%s): %v", label, err)
		}
	}
	counts, err := store.WorkflowNoteCounts(ctx, []string{"release-42", "missing"})
	if err != nil {
		t.Fatalf("WorkflowNoteCounts: %v", err)
	}
	if counts["release-42"] != 2 || counts["missing"] != 0 {
		t.Fatalf("counts = %v", counts)
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

func TestWorkflowMetaLastWriteWinsAndObservationFailureRollsBack(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	first, err := store.InsertWorkflowNoteWithMeta(ctx,
		WorkflowNote{WorkflowID: "fable/dashboard-redesign", Author: "fable", Body: "started"},
		WorkflowMeta{Author: "fable", Pane: "pane-1", SessionID: "session-1", WorkDir: "/work/one"})
	if err != nil || first.ID == 0 {
		t.Fatalf("InsertWorkflowNoteWithMeta = (%+v, %v)", first, err)
	}
	second, err := store.InsertWorkflowNoteWithMeta(ctx,
		WorkflowNote{WorkflowID: "fable/dashboard-redesign", Author: "operator", Body: "handoff"},
		WorkflowMeta{Author: "operator", Pane: "pane-2", SessionID: "session-2", WorkDir: "/work/two"})
	if err != nil || second.ID <= first.ID {
		t.Fatalf("second InsertWorkflowNoteWithMeta = (%+v, %v)", second, err)
	}
	meta, err := store.GetWorkflowMeta(ctx, "fable/dashboard-redesign")
	if err != nil || meta.Author != "operator" || meta.Pane != "pane-2" || meta.SessionID != "session-2" || meta.WorkDir != "/work/two" {
		t.Fatalf("metadata = %+v, err=%v", meta, err)
	}
	third, err := store.InsertWorkflowNoteWithMeta(ctx,
		WorkflowNote{WorkflowID: "fable/dashboard-redesign", Author: "reviewer", Body: "no handoff change"},
		WorkflowMeta{Author: "reviewer"})
	if err != nil || third.ID <= second.ID {
		t.Fatalf("third InsertWorkflowNoteWithMeta = (%+v, %v)", third, err)
	}
	meta, err = store.GetWorkflowMeta(ctx, "fable/dashboard-redesign")
	if err != nil || meta.Author != "reviewer" || meta.Pane != "pane-2" || meta.SessionID != "session-2" || meta.WorkDir != "/work/two" {
		t.Fatalf("metadata after omitted optional flags = %+v, err=%v", meta, err)
	}

	_, _, err = store.InsertWorkflowNoteWithObservationAndMeta(ctx,
		WorkflowNote{WorkflowID: "fable/dashboard-redesign", Author: "bad", Body: "must roll back"},
		MemoryObservation{Content: "missing owner"},
		WorkflowMeta{Author: "bad", Pane: "bad", SessionID: "bad", WorkDir: "/bad"})
	if err == nil {
		t.Fatal("expected invalid observation to roll back note and metadata")
	}
	meta, err = store.GetWorkflowMeta(ctx, "fable/dashboard-redesign")
	if err != nil || meta.Author != "reviewer" || meta.Pane != "pane-2" || meta.SessionID != "session-2" || meta.WorkDir != "/work/two" {
		t.Fatalf("metadata changed after rollback: %+v, err=%v", meta, err)
	}
	notes, err := store.ListWorkflowNotes(ctx, "fable/dashboard-redesign", 0)
	if err != nil || len(notes) != 3 {
		t.Fatalf("notes after rollback = %+v, err=%v", notes, err)
	}
}

func TestWorkflowAutoNotePreservesCoordinatorAuthor(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	const workflowID = "fable/dashboard-redesign"
	if _, err := store.InsertWorkflowNoteWithMeta(ctx,
		WorkflowNote{WorkflowID: workflowID, Author: "fable", Body: "kickoff"},
		WorkflowMeta{Author: "fable", Pane: "Gitmoot2", SessionID: "full-session", WorkDir: "/work/fable"}); err != nil {
		t.Fatalf("InsertWorkflowNoteWithMeta kickoff: %v", err)
	}

	// Production auto-note writers update only status: Author is deliberately
	// empty so a daemon receipt cannot replace the coordinator identity.
	_, inserted, err := store.InsertWorkflowAutoNoteWithMeta(ctx,
		WorkflowNote{WorkflowID: workflowID, Author: WorkflowAutoNoteAuthor, Body: "[auto:pr:958:opened] PR #958 opened (feature/958)"},
		WorkflowMeta{WorkflowID: workflowID, Status: string(WorkflowStatusActive), StatusSet: true})
	if err != nil || !inserted {
		t.Fatalf("InsertWorkflowAutoNoteWithMeta = (inserted=%v, err=%v)", inserted, err)
	}

	meta, err := store.GetWorkflowMeta(ctx, workflowID)
	if err != nil || meta.Author != "fable" || meta.Pane != "Gitmoot2" || meta.SessionID != "full-session" || meta.WorkDir != "/work/fable" || meta.Status != string(WorkflowStatusActive) {
		t.Fatalf("metadata after production-shaped auto note = %+v, err=%v", meta, err)
	}
}

func TestWorkflowSummarySeparatesDaemonNotesFromHumanAcknowledgment(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	const workflowID = "release/958"
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: workflowID, Author: "coordinator", Body: "human handoff"}); err != nil {
		t.Fatalf("InsertWorkflowNote: %v", err)
	}
	if _, inserted, err := store.InsertWorkflowAutoNoteWithMeta(ctx,
		WorkflowNote{WorkflowID: workflowID, Author: WorkflowAutoNoteAuthor, Body: "[auto:pr:958:closed] PR #958 closed without merging"},
		WorkflowMeta{WorkflowID: workflowID, Status: string(WorkflowStatusActive), StatusSet: true}); err != nil || !inserted {
		t.Fatalf("InsertWorkflowAutoNoteWithMeta = (inserted=%v, err=%v)", inserted, err)
	}

	summary, err := store.WorkflowSummary(ctx, workflowID)
	if err != nil {
		t.Fatalf("WorkflowSummary: %v", err)
	}
	if summary.LastAuthor != WorkflowAutoNoteAuthor || summary.LastHumanAuthor != "coordinator" || summary.LastNoteAt == "" || summary.LastHumanNoteAt == "" {
		t.Fatalf("summary = %+v; want daemon last author and coordinator last human author", summary)
	}
}

func TestWorkflowSummaryExcludesOrgEscalateFromHumanAcknowledgment(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	const workflowID = "release/escalate"
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: workflowID, Author: "operator", Body: "[org:escalate to=owner from=operator wf=release/escalate] why failed?"}); err != nil {
		t.Fatalf("InsertWorkflowNote: %v", err)
	}
	summary, err := store.WorkflowSummary(ctx, workflowID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.LastHumanAuthor != "" || summary.LastHumanNoteAt != "" {
		t.Fatalf("escalation acknowledged failure: %+v", summary)
	}
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: workflowID, Author: "coordinator", Body: "human acknowledgment"}); err != nil {
		t.Fatalf("InsertWorkflowNote: %v", err)
	}
	summary, err = store.WorkflowSummary(ctx, workflowID)
	if err != nil || summary.LastHumanAuthor != "coordinator" || summary.LastHumanNoteAt == "" {
		t.Fatalf("ordinary note did not acknowledge: %+v err=%v", summary, err)
	}
}

func TestWorkflowSummaryTracksMergedDaemonReceipt(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	fixtures := []struct {
		workflowID, author, body string
		merged                   bool
	}{
		{workflowID: "release/merged", author: WorkflowAutoNoteAuthor, body: "[auto:pr:958:merged] PR #958 merged", merged: true},
		{workflowID: "release/closed", author: WorkflowAutoNoteAuthor, body: "[auto:pr:959:closed] PR #959 closed without merging"},
		{workflowID: "release/closed-tail", author: WorkflowAutoNoteAuthor, body: "[auto:pr:961:closed] reverted; see note re :merged] state"},
		{workflowID: "release/opened", author: WorkflowAutoNoteAuthor, body: "[auto:pr:960:opened] PR #960 opened"},
		{workflowID: "release/human", author: "coordinator", body: "human handoff"},
	}
	for _, fixture := range fixtures {
		if fixture.author == WorkflowAutoNoteAuthor {
			if _, inserted, err := store.InsertWorkflowAutoNoteWithMeta(ctx,
				WorkflowNote{WorkflowID: fixture.workflowID, Author: fixture.author, Body: fixture.body},
				WorkflowMeta{WorkflowID: fixture.workflowID, Status: string(WorkflowStatusActive), StatusSet: true}); err != nil || !inserted {
				t.Fatalf("InsertWorkflowAutoNoteWithMeta(%q) = (inserted=%v, err=%v)", fixture.workflowID, inserted, err)
			}
			continue
		}
		if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: fixture.workflowID, Author: fixture.author, Body: fixture.body}); err != nil {
			t.Fatalf("InsertWorkflowNote(%q): %v", fixture.workflowID, err)
		}
	}

	listed, err := store.ListWorkflowSummaries(ctx)
	if err != nil {
		t.Fatalf("ListWorkflowSummaries: %v", err)
	}
	listedByWorkflow := make(map[string]WorkflowSummary, len(listed))
	for _, summary := range listed {
		listedByWorkflow[summary.WorkflowID] = summary
	}
	for _, fixture := range fixtures {
		listedSummary, ok := listedByWorkflow[fixture.workflowID]
		if !ok {
			t.Fatalf("ListWorkflowSummaries missing %q: %+v", fixture.workflowID, listed)
		}
		shownSummary, err := store.WorkflowSummary(ctx, fixture.workflowID)
		if err != nil {
			t.Fatalf("WorkflowSummary(%q): %v", fixture.workflowID, err)
		}
		if got, want := listedSummary.LastMergedReceiptAt != "", fixture.merged; got != want {
			t.Fatalf("listed %q LastMergedReceiptAt = %q, want merged=%v", fixture.workflowID, listedSummary.LastMergedReceiptAt, want)
		}
		if got, want := shownSummary.LastMergedReceiptAt != "", fixture.merged; got != want {
			t.Fatalf("shown %q LastMergedReceiptAt = %q, want merged=%v", fixture.workflowID, shownSummary.LastMergedReceiptAt, want)
		}
	}
}

func TestWorkflowMetaTextSetPreserveClearAndLimit(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	workflowID := "fable/dashboard-redesign"
	write := func(body string, meta WorkflowMeta) {
		t.Helper()
		if _, err := store.InsertWorkflowNoteWithMeta(ctx,
			WorkflowNote{WorkflowID: workflowID, Author: "coord", Body: body}, meta); err != nil {
			t.Fatalf("InsertWorkflowNoteWithMeta(%q): %v", body, err)
		}
	}

	write("kickoff", WorkflowMeta{
		Author: "coord", Summary: "Ship the dashboard redesign.", SummarySet: true,
		Description: "Stable intent", DescriptionSet: true,
		Status: string(WorkflowStatusActive), StatusSet: true,
	})
	write("progress", WorkflowMeta{Author: "coord"})
	meta, err := store.GetWorkflowMeta(ctx, workflowID)
	if err != nil || meta.Summary != "Ship the dashboard redesign." || meta.Description != "Stable intent" || meta.Status != string(WorkflowStatusActive) {
		t.Fatalf("metadata after omitted update = %+v, err=%v", meta, err)
	}

	write("clear", WorkflowMeta{Author: "coord", SummarySet: true, DescriptionSet: true, StatusSet: true})
	meta, err = store.GetWorkflowMeta(ctx, workflowID)
	if err != nil || meta.Summary != "" || meta.Description != "" || meta.Status != "" {
		t.Fatalf("metadata after explicit clear = %+v, err=%v", meta, err)
	}

	for _, tc := range []struct {
		name string
		meta WorkflowMeta
	}{
		{name: "summary", meta: WorkflowMeta{Summary: strings.Repeat("s", WorkflowMetaTextMax+1), SummarySet: true}},
		{name: "description", meta: WorkflowMeta{Description: strings.Repeat("d", WorkflowMetaTextMax+1), DescriptionSet: true}},
	} {
		t.Run(tc.name+"_over_limit", func(t *testing.T) {
			if _, err := store.InsertWorkflowNoteWithMeta(ctx,
				WorkflowNote{WorkflowID: workflowID, Author: "coord", Body: tc.name}, tc.meta); err == nil || !strings.Contains(err.Error(), "at most 300 bytes") {
				t.Fatalf("over-limit %s error = %v", tc.name, err)
			}
		})
	}
}

func TestWorkflowMetaTextMigrations(t *testing.T) {
	store := openWorkflowTestStore(t)
	rows, err := store.db.QueryContext(context.Background(), `PRAGMA table_info(workflow_meta)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(workflow_meta): %v", err)
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "summary" || name == "description" || name == "status" {
			found[name] = columnType == "TEXT" && notNull == 1 && defaultValue.Valid && defaultValue.String == "''"
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"summary", "description", "status"} {
		if !found[name] {
			t.Fatalf("%s is missing TEXT NOT NULL DEFAULT '' in PRAGMA table_info(workflow_meta)", name)
		}
	}
}

func TestWorkflowDescriptionMigrationSeedsLegacySummary(t *testing.T) {
	ctx := context.Background()
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy-workflow-meta.db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	store := &Store{db: raw}
	descriptionMigration := -1
	for i, migration := range migrations {
		if strings.Contains(migration, "ADD COLUMN description") {
			descriptionMigration = i
			break
		}
	}
	if descriptionMigration < 0 {
		t.Fatal("workflow description migration not found")
	}
	for version, migration := range migrations[:descriptionMigration] {
		if err := store.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO workflow_meta(workflow_id, summary) VALUES ('release/legacy', 'Legacy human intent')`); err != nil {
		t.Fatalf("insert legacy meta: %v", err)
	}
	for offset, migration := range migrations[descriptionMigration:] {
		if err := store.applyMigration(ctx, descriptionMigration+1+offset, migration); err != nil {
			t.Fatalf("apply new workflow migration %d: %v", offset, err)
		}
	}
	meta, err := store.GetWorkflowMeta(ctx, "release/legacy")
	if err != nil || meta.Description != "Legacy human intent" || meta.Status != "" {
		t.Fatalf("migrated meta = %+v, err=%v", meta, err)
	}
}

func TestWorkflowDescriptionAutoSeedPriorityAndPreservation(t *testing.T) {
	tests := []struct {
		name       string
		workflowID string
		payload    string
		body       string
		human      string
		want       string
	}{
		{
			name: "issue title", workflowID: "release/#42",
			payload: `{"workflow_id":"release/#42","repo":"acme/widget","task_id":"issue-42"}`,
			body:    "Kickoff for #42. Then verify rollout.", want: "Repair login redirects",
		},
		{
			name: "first sentence", workflowID: "release/kickoff",
			payload: `{"workflow_id":"release/kickoff","repo":"acme/widget"}`,
			body:    "Ship the canary safely. Then watch metrics.", want: "Ship the canary safely.",
		},
		{
			name: "label campaign", workflowID: "release/canary-rollout",
			payload: `{"workflow_id":"release/canary-rollout","repo":"acme/widget"}`,
			body:    "", want: "canary-rollout",
		},
		{
			name: "human preserved", workflowID: "release/human",
			payload: `{"workflow_id":"release/human","repo":"acme/widget"}`,
			body:    "A later kickoff must not win.", human: "Human-set stable intent", want: "Human-set stable intent",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := openWorkflowTestStore(t)
			ctx := context.Background()
			if tc.name == "issue title" {
				if err := store.UpsertTask(ctx, Task{ID: "issue-42", RepoFullName: "acme/widget", Title: "Repair login redirects", State: "planned"}); err != nil {
					t.Fatalf("UpsertTask: %v", err)
				}
			}
			if err := store.CreateJob(ctx, Job{ID: "job", Agent: "worker", Type: "ask", State: "running", Payload: tc.payload}); err != nil {
				t.Fatalf("CreateJob: %v", err)
			}
			if tc.human != "" {
				if err := store.SetWorkflowDescription(ctx, tc.workflowID, tc.human); err != nil {
					t.Fatalf("SetWorkflowDescription: %v", err)
				}
			}
			body := tc.body
			author := "coord"
			if body == "" {
				body, author = "[auto:pr:7:opened] PR #7 opened (branch)", WorkflowAutoNoteAuthor
			}
			if _, err := store.InsertWorkflowNoteWithMeta(ctx,
				WorkflowNote{WorkflowID: tc.workflowID, Author: author, Body: body}, WorkflowMeta{Author: author}); err != nil {
				t.Fatalf("InsertWorkflowNoteWithMeta: %v", err)
			}
			meta, err := store.GetWorkflowMeta(ctx, tc.workflowID)
			if err != nil || meta.Description != tc.want {
				t.Fatalf("description = %q, want %q, err=%v", meta.Description, tc.want, err)
			}
		})
	}
}

func TestWorkflowDescriptionAutoSeedCapsUTF8Bytes(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	seedWorkflowJob(t, store, "job", "release/long", "running", "acme/widget", 0, 0)
	body := strings.Repeat("é", WorkflowMetaTextMax) // 600 bytes
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: "release/long", Body: body}); err != nil {
		t.Fatalf("InsertWorkflowNote: %v", err)
	}
	meta, err := store.GetWorkflowMeta(ctx, "release/long")
	if err != nil || len(meta.Description) > WorkflowMetaTextMax || !strings.HasPrefix(body, meta.Description) {
		t.Fatalf("capped description bytes=%d valid-prefix=%v err=%v", len(meta.Description), strings.HasPrefix(body, meta.Description), err)
	}
}

func TestWorkflowSummariesIncludeNoteOnlyLabelsAndNoteActivity(t *testing.T) {
	store := openWorkflowTestStore(t)
	ctx := context.Background()
	if _, err := store.InsertWorkflowNote(ctx, WorkflowNote{WorkflowID: "notes/only", Author: "coord", Body: "latest status"}); err != nil {
		t.Fatalf("InsertWorkflowNote: %v", err)
	}
	summaries, err := store.ListWorkflowSummaries(ctx)
	if err != nil {
		t.Fatalf("ListWorkflowSummaries: %v", err)
	}
	if len(summaries) != 1 || summaries[0].WorkflowID != "notes/only" || summaries[0].JobCount != 0 || summaries[0].NoteCount != 1 || summaries[0].LastNote != "latest status" || summaries[0].LastAuthor != "coord" || summaries[0].FirstAt == "" || summaries[0].LastAt == "" {
		t.Fatalf("note-only summaries = %+v", summaries)
	}
	summary, err := store.WorkflowSummary(ctx, "notes/only")
	if err != nil || summary.NoteCount != 1 || summary.JobCount != 0 {
		t.Fatalf("WorkflowSummary(note-only) = %+v, err=%v", summary, err)
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

// TestWorkflowIDForPullRequestPrefersPullNumberOverReusedBranch proves the
// documented "pull-request equality wins" guarantee: an exact PR-number match in
// the correct workflow must beat a NEWER branch-only job in a different workflow
// that reuses the same head branch name. A single early-return-on-either scan
// (ordered newest-first) would wrongly return the reused-branch workflow.
func TestWorkflowIDForPullRequestPrefersPullNumberOverReusedBranch(t *testing.T) {
	ctx := context.Background()
	store := openWorkflowTestStore(t)
	// Older job carries the PR number in the correct workflow.
	if err := store.CreateJob(ctx, Job{ID: "job-a", Agent: "worker", Type: "implement", State: "succeeded",
		Payload: `{"workflow_id":"wfA","repo":"owner/repo","branch":"shared","pull_request":42}`}); err != nil {
		t.Fatalf("CreateJob a: %v", err)
	}
	// Newer job reuses the same branch name in a different workflow, no PR number.
	if err := store.CreateJob(ctx, Job{ID: "job-b", Agent: "worker", Type: "implement", State: "succeeded",
		Payload: `{"workflow_id":"wfB","repo":"owner/repo","branch":"shared"}`}); err != nil {
		t.Fatalf("CreateJob b: %v", err)
	}

	got, err := store.WorkflowIDForPullRequest(ctx, "owner/repo", 42, "shared")
	if err != nil || got != "wfA" {
		t.Fatalf("WorkflowIDForPullRequest(42) = %q, err=%v; want wfA (PR number wins over newer reused branch)", got, err)
	}
	// With no PR-number match, the newest branch-only job is the correct fallback.
	got, err = store.WorkflowIDForPullRequest(ctx, "owner/repo", 999, "shared")
	if err != nil || got != "wfB" {
		t.Fatalf("WorkflowIDForPullRequest(999) branch fallback = %q, err=%v; want wfB", got, err)
	}
}
