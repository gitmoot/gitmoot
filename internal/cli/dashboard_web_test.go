package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// seedWebDashboardTree seeds a coordinator with two delegation children (one
// depending on the other) plus a continuation job, so the DataSource has a real
// delegation graph to build from.
func seedWebDashboardTree(t *testing.T, home string) {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.UpsertAgent(ctx, db.Agent{Name: "project-lead", Runtime: "codex"}); err != nil {
		t.Fatalf("UpsertAgent lead: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: "builder", Runtime: "claude"}); err != nil {
		t.Fatalf("UpsertAgent builder: %v", err)
	}

	// Coordinator (originating root) whose settled result declares two
	// delegations: "export" depends on "search".
	coordPayload := workflow.JobPayload{
		Repo:      "jerryfane/noted",
		TaskTitle: "add search + export",
		Result: &workflow.AgentResult{
			Decision: "delegated",
			Delegations: []workflow.Delegation{
				{ID: "d-search", Agent: "builder", Action: "implement: search"},
				{ID: "d-export", Agent: "builder", Action: "implement: export", Deps: []string{"d-search"}},
			},
		},
	}
	mustCreateJob(t, store, db.Job{ID: "coord", Agent: "project-lead", Type: "orchestrate", State: "succeeded", Payload: mustJSON(t, coordPayload)}, "delegation_enqueued", "fanned out 2 delegations")

	searchPayload := workflow.JobPayload{Repo: "jerryfane/noted", TaskTitle: "implement: search", PullRequest: 12}
	mustCreateJob(t, store, db.Job{ID: "child-search", Agent: "builder", Type: "implement", State: "succeeded", Payload: mustJSON(t, searchPayload), ParentJobID: "coord", DelegationID: "d-search", DelegationDepth: 1}, "worker_started", "picked up d-search")

	exportPayload := workflow.JobPayload{Repo: "jerryfane/noted", TaskTitle: "implement: export"}
	mustCreateJob(t, store, db.Job{ID: "child-export", Agent: "builder", Type: "implement", State: "running", Payload: mustJSON(t, exportPayload), ParentJobID: "coord", DelegationID: "d-export", DelegationDepth: 1}, "worker_started", "picked up d-export")

	// Continuation job carries no delegation id.
	contPayload := workflow.JobPayload{Repo: "jerryfane/noted", TaskTitle: "synthesize"}
	mustCreateJob(t, store, db.Job{ID: "coord-cont", Agent: "project-lead", Type: "orchestrate", State: "queued", Payload: mustJSON(t, contPayload), ParentJobID: "coord"}, "", "")
}

// TestWebDataSourceNotFound asserts the DataSource maps an unknown job/run to the
// dashboard sentinels (so the API returns 404, not 500), while a real run resolves.
func TestWebDataSourceNotFound(t *testing.T) {
	home := dashboardTestHome(t)
	seedWebDashboardTree(t, home)
	ds := &webDataSource{home: home}
	ctx := context.Background()

	if _, err := ds.Job(ctx, "no-such-job"); !errors.Is(err, dashboard.ErrJobNotFound) {
		t.Fatalf("Job(unknown) err = %v, want ErrJobNotFound", err)
	}
	if _, err := ds.State(ctx, "no-such-run"); !errors.Is(err, dashboard.ErrRunNotFound) {
		t.Fatalf("State(unknown) err = %v, want ErrRunNotFound", err)
	}
	if _, err := ds.State(ctx, "coord"); err != nil {
		t.Fatalf("State(coord) err = %v, want nil", err)
	}
}

func mustCreateJob(t *testing.T, store *db.Store, job db.Job, eventKind, eventMsg string) {
	t.Helper()
	if err := store.CreateJob(context.Background(), job); err != nil {
		t.Fatalf("CreateJob %s: %v", job.ID, err)
	}
	if eventKind != "" {
		if err := store.AddJobEvent(context.Background(), db.JobEvent{JobID: job.ID, Kind: eventKind, Message: eventMsg}); err != nil {
			t.Fatalf("AddJobEvent %s: %v", job.ID, err)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(b)
}

func nodeByID(nodes []dashboard.Node, id string) (dashboard.Node, bool) {
	for _, n := range nodes {
		if n.ID == id {
			return n, true
		}
	}
	return dashboard.Node{}, false
}

func TestWebDataSourceBuildsGraphFromStore(t *testing.T) {
	home := dashboardTestHome(t)
	seedWebDashboardTree(t, home)

	ds := &webDataSource{home: home}
	ctx := context.Background()

	// Runs: one run rooted at the coordinator, showing live (running) work.
	runs, err := ds.Runs(ctx)
	if err != nil {
		t.Fatalf("Runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d: %+v", len(runs), runs)
	}
	if runs[0].RunID != "coord" {
		t.Fatalf("run id = %q, want coord", runs[0].RunID)
	}
	if runs[0].State != "running" {
		t.Fatalf("run state = %q, want running", runs[0].State)
	}
	if runs[0].Title != "add search + export" {
		t.Fatalf("run title = %q", runs[0].Title)
	}

	// State (active run): four nodes, correct parentage, dep edge, runtime.
	state, err := ds.State(ctx, "")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.RunID != "coord" {
		t.Fatalf("state run id = %q, want coord", state.RunID)
	}
	if len(state.Nodes) != 4 {
		t.Fatalf("expected 4 nodes, got %d: %+v", len(state.Nodes), state.Nodes)
	}

	search, ok := nodeByID(state.Nodes, "child-search")
	if !ok {
		t.Fatalf("child-search node missing")
	}
	if search.ParentID != "coord" {
		t.Fatalf("child-search parent = %q, want coord", search.ParentID)
	}
	if search.Runtime != "claude" {
		t.Fatalf("child-search runtime = %q, want claude", search.Runtime)
	}
	if search.State != "succeeded" {
		t.Fatalf("child-search state = %q, want succeeded", search.State)
	}
	if search.Title != "implement: search" {
		t.Fatalf("child-search title = %q", search.Title)
	}
	if search.PRURL != "https://github.com/jerryfane/noted/pull/12" {
		t.Fatalf("child-search prUrl = %q", search.PRURL)
	}
	if len(search.Events) == 0 {
		t.Fatalf("child-search should carry worker events")
	}

	export, ok := nodeByID(state.Nodes, "child-export")
	if !ok {
		t.Fatalf("child-export node missing")
	}
	if len(export.Deps) != 1 || export.Deps[0] != "child-search" {
		t.Fatalf("child-export deps = %v, want [child-search]", export.Deps)
	}
	if export.State != "running" {
		t.Fatalf("child-export state = %q, want running", export.State)
	}

	coord, ok := nodeByID(state.Nodes, "coord")
	if !ok {
		t.Fatalf("coord node missing")
	}
	if coord.Runtime != "codex" {
		t.Fatalf("coord runtime = %q, want codex", coord.Runtime)
	}

	// Job: single node lookup resolves the same dep edge as State.
	node, err := ds.Job(ctx, "child-export")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if len(node.Deps) != 1 || node.Deps[0] != "child-search" {
		t.Fatalf("Job child-export deps = %v, want [child-search]", node.Deps)
	}
	if node.Title != "implement: export" {
		t.Fatalf("Job child-export title = %q", node.Title)
	}
}

// setJobTimes overwrites a seeded job's created_at/updated_at (which CreateJob
// stamps with CURRENT_TIMESTAMP) so timing assertions have deterministic values.
// It opens a throwaway connection because the seeding store is already closed.
func setJobTimes(t *testing.T, home, jobID, createdAt, updatedAt string) {
	t.Helper()
	conn, err := sql.Open("sqlite", config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`, createdAt, updatedAt, jobID); err != nil {
		t.Fatalf("UPDATE jobs times: %v", err)
	}
	if _, err := conn.Exec(`UPDATE job_events SET created_at = ? WHERE job_id = ?`, createdAt, jobID); err != nil {
		t.Fatalf("UPDATE job_events times: %v", err)
	}
}

// TestWebDataSourceNodeTiming asserts the DataSource reads created_at/updated_at
// off the row (via ListJobs/GetJob) and stamps StartedAt always, EndedAt only on
// terminal jobs, and Event.T from the event's created_at.
func TestWebDataSourceNodeTiming(t *testing.T) {
	home := dashboardTestHome(t)
	seedWebDashboardTree(t, home)

	const created = "2026-05-23 06:45:00"
	const updated = "2026-05-23 07:00:00"
	setJobTimes(t, home, "child-search", created, updated) // succeeded => terminal
	setJobTimes(t, home, "child-export", created, updated) // running => not terminal

	wantStart := parseJobTimeMillis(created)
	wantEnd := parseJobTimeMillis(updated)
	if wantStart == 0 || wantEnd == 0 {
		t.Fatalf("test timestamps did not parse: start=%d end=%d", wantStart, wantEnd)
	}

	ds := &webDataSource{home: home}
	ctx := context.Background()

	state, err := ds.State(ctx, "coord")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	search, ok := nodeByID(state.Nodes, "child-search")
	if !ok {
		t.Fatalf("child-search node missing")
	}
	if search.StartedAt != wantStart {
		t.Fatalf("child-search StartedAt = %d, want %d", search.StartedAt, wantStart)
	}
	if search.EndedAt != wantEnd {
		t.Fatalf("child-search EndedAt = %d, want %d (terminal job should stamp EndedAt)", search.EndedAt, wantEnd)
	}
	if len(search.Events) == 0 {
		t.Fatalf("child-search should carry events")
	}
	if search.Events[0].T != wantStart {
		t.Fatalf("child-search Events[0].T = %d, want %d (from event created_at)", search.Events[0].T, wantStart)
	}

	// A non-terminal (running) job stamps StartedAt but leaves EndedAt unset.
	export, ok := nodeByID(state.Nodes, "child-export")
	if !ok {
		t.Fatalf("child-export node missing")
	}
	if export.StartedAt != wantStart {
		t.Fatalf("child-export StartedAt = %d, want %d", export.StartedAt, wantStart)
	}
	if export.EndedAt != 0 {
		t.Fatalf("child-export EndedAt = %d, want 0 (running job is not terminal)", export.EndedAt)
	}

	// /api/job (GetJob) timing must match /api/state (GetJob now selects created_at/updated_at).
	node, err := ds.Job(ctx, "child-search")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if node.StartedAt != wantStart || node.EndedAt != wantEnd {
		t.Fatalf("Job timing = (%d,%d), want (%d,%d)", node.StartedAt, node.EndedAt, wantStart, wantEnd)
	}
}

// TestNodeTitleDescriptive covers the descriptive-title preference order beyond
// the plain task-title case: a humanized delegation id and an instructions line.
func TestNodeTitleDescriptive(t *testing.T) {
	// Humanized "task-N-..." delegation id (no task title).
	got := nodeTitle(workflow.JobPayload{}, db.Job{DelegationID: "task-3-pairing-agent-auth", Type: "implement"}, "implement")
	if got != "Task 3: pairing agent auth" {
		t.Fatalf("delegation title = %q, want %q", got, "Task 3: pairing agent auth")
	}

	// First non-empty instructions line wins over the bare action, and is capped.
	long := "   \n" + "Wire the pairing agent into the auth handshake so sessions resume cleanly across restarts and reconnects"
	got = nodeTitle(workflow.JobPayload{Instructions: long}, db.Job{Type: "ask"}, "ask")
	if got == "ask" || got == "" {
		t.Fatalf("instructions title fell through to action: %q", got)
	}
	if r := []rune(got); len(r) > 61 { // 60 + ellipsis
		t.Fatalf("instructions title not capped: %d runes (%q)", len(r), got)
	}

	// An explicit task title still wins over everything.
	got = nodeTitle(workflow.JobPayload{TaskTitle: "Add search", Instructions: "ignored"}, db.Job{DelegationID: "task-1-x"}, "implement")
	if got != "Add search" {
		t.Fatalf("task title = %q, want %q", got, "Add search")
	}

	// Empty inputs fall back to the job id, never panic.
	got = nodeTitle(workflow.JobPayload{}, db.Job{ID: "job-xyz"}, "")
	if got != "job-xyz" {
		t.Fatalf("fallback title = %q, want job-xyz", got)
	}
}

// TestSummarizeRunsSortedAndCapped asserts the run list puts active runs first,
// then most-recent, and caps at maxRunSummaries.
func TestSummarizeRunsSortedAndCapped(t *testing.T) {
	var jobs []db.Job
	// 70 terminal (succeeded) roots with increasing updated_at, plus one older
	// active (running) root. The active root must lead despite its older time,
	// and the list must cap at maxRunSummaries.
	for i := 0; i < 70; i++ {
		jobs = append(jobs, db.Job{
			ID:        fmt.Sprintf("done-%02d", i),
			State:     "succeeded",
			UpdatedAt: fmt.Sprintf("2026-05-%02d 10:00:00", (i%27)+1),
		})
	}
	jobs = append(jobs, db.Job{ID: "active-root", State: "running", UpdatedAt: "2026-01-01 00:00:00"})

	runs := summarizeRuns(jobs)
	if len(runs) != maxRunSummaries {
		t.Fatalf("run count = %d, want %d (capped)", len(runs), maxRunSummaries)
	}
	if runs[0].RunID != "active-root" {
		t.Fatalf("first run = %q, want active-root (active leads even when older)", runs[0].RunID)
	}
	if runs[0].State != "running" {
		t.Fatalf("first run state = %q, want running", runs[0].State)
	}
	// Terminal runs after the active one are newest-activity first.
	for i := 2; i < len(runs); i++ {
		if runs[i-1].Updated < runs[i].Updated {
			t.Fatalf("runs not newest-first at %d: %d < %d", i, runs[i-1].Updated, runs[i].Updated)
		}
	}
}

func TestParseRunKindAgent(t *testing.T) {
	cases := []struct {
		rootID     string
		root       db.Job
		wantKind   string
		wantAgent  string
	}{
		{"local-ask-project-lead-18bde5e13a42d5a7", db.Job{}, "ask", "project-lead"},
		{"local-review-acme-reviewer-abcdef123456", db.Job{}, "review", "acme-reviewer"},
		{"gh-implement-worker-0123456789ab", db.Job{}, "implement", "worker"},
		// no hash suffix / not the local-<kind>-<agent>-<hash> shape => fall back to
		// the root job's Type + Agent columns.
		{"task-294-presence-docs", db.Job{Type: "Coordination", Agent: "presence-docs"}, "coordination", "presence-docs"},
		// a multi-token internal action (parts[1] not a known kind) must NOT be
		// mis-split into kind/agent; it falls back to the root Type/Agent columns.
		{"local-skillopt-train-candidate-review-sess-abcdef123456", db.Job{Type: "Train", Agent: "skillopt-worker"}, "train", "skillopt-worker"},
	}
	for _, c := range cases {
		gotKind, gotAgent := parseRunKindAgent(c.rootID, c.root)
		if gotKind != c.wantKind || gotAgent != c.wantAgent {
			t.Errorf("parseRunKindAgent(%q) = (%q,%q), want (%q,%q)", c.rootID, gotKind, gotAgent, c.wantKind, c.wantAgent)
		}
	}
}

func TestSummarizeRunsEnriched(t *testing.T) {
	root := "local-ask-project-lead-18bde5e13a42d5a7"
	jobs := []db.Job{
		{ID: root, State: "succeeded", DelegationDepth: 0, CreatedAt: "2026-06-01 10:00:00", UpdatedAt: "2026-06-01 10:05:00"},
		{ID: "c1", ParentJobID: root, State: "succeeded", DelegationDepth: 1, CreatedAt: "2026-06-01 10:01:00", UpdatedAt: "2026-06-01 10:04:00"},
		{ID: "c2", ParentJobID: root, State: "running", DelegationDepth: 1, CreatedAt: "2026-06-01 10:01:00", UpdatedAt: "2026-06-01 10:02:00"},
	}
	runs := summarizeRuns(jobs)
	if len(runs) != 1 {
		t.Fatalf("run count = %d, want 1", len(runs))
	}
	r := runs[0]
	if r.Significance != "orchestration" {
		t.Errorf("significance = %q, want orchestration", r.Significance)
	}
	if r.NodeCount != 3 {
		t.Errorf("nodeCount = %d, want 3", r.NodeCount)
	}
	if r.Depth != 2 {
		t.Errorf("depth = %d, want 2 (max delegation depth 1 + 1 level)", r.Depth)
	}
	if r.DoneCount != 2 {
		t.Errorf("doneCount = %d, want 2 (two terminal jobs)", r.DoneCount)
	}
	if r.Kind != "ask" || r.Agent != "project-lead" {
		t.Errorf("kind/agent = %q/%q, want ask/project-lead", r.Kind, r.Agent)
	}
	// a single-node run is a one-shot, not an orchestration.
	solo := summarizeRuns([]db.Job{{ID: "local-ask-x-abcabcabcabc", State: "succeeded"}})
	if solo[0].Significance != "one-shot" || solo[0].NodeCount != 1 {
		t.Errorf("solo run = %q/%d, want one-shot/1", solo[0].Significance, solo[0].NodeCount)
	}
}
