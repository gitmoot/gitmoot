package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/buildinfo"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/update"
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

// TestPinDashboardBlockedDoesNotStampEndedAt pins the web dashboard's terminal
// semantics (#632): buildDashboardNode stamps EndedAt only for FINAL states
// (succeeded/failed/cancelled) and NOT for `blocked`, because a blocked job can
// still resume. This exclusion must hold identically before and after the
// predicate refactor.
func TestPinDashboardBlockedDoesNotStampEndedAt(t *testing.T) {
	const updated = "2026-05-23 07:00:00"
	wantEnd := parseJobTimeMillis(updated)
	if wantEnd == 0 {
		t.Fatalf("test timestamp did not parse")
	}

	// A blocked job carries an UpdatedAt but must NOT get EndedAt stamped.
	blocked := buildDashboardNode(
		db.Job{ID: "j-blocked", Agent: "a", Type: "implement", State: "blocked", UpdatedAt: updated},
		workflow.JobPayload{}, nil, nil, nil, "implement")
	if blocked.EndedAt != 0 {
		t.Fatalf("blocked node EndedAt = %d, want 0 (blocked is not final; can resume)", blocked.EndedAt)
	}

	// A failed job, by contrast, is final and does get EndedAt stamped.
	failed := buildDashboardNode(
		db.Job{ID: "j-failed", Agent: "a", Type: "implement", State: "failed", UpdatedAt: updated},
		workflow.JobPayload{}, nil, nil, nil, "implement")
	if failed.EndedAt != wantEnd {
		t.Fatalf("failed node EndedAt = %d, want %d (failed is final)", failed.EndedAt, wantEnd)
	}
}

func TestEventLabelFailureMessageCap(t *testing.T) {
	message := strings.Repeat("x", 300)
	tests := []struct {
		name  string
		event db.JobEvent
		want  string
	}{
		{
			name:  "failed event retains 300 byte message",
			event: db.JobEvent{Kind: "failed", Message: message},
			want:  "failed: " + message,
		},
		{
			name:  "non failed event keeps 120 byte cap",
			event: db.JobEvent{Kind: "worker_error", Message: message},
			want:  "worker_error: " + message[:120] + "…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := eventLabel(tt.event); got != tt.want {
				t.Fatalf("eventLabel() = %q, want %q", got, tt.want)
			}
		})
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

// TestWebDataSourceGraph asserts the galaxy Graph unions all jobs, adds repo +
// agent hub nodes, wires parent/dep/repo/agent links, and that a repo filter
// narrows the visible nodes while Repos stays the full list.
func TestWebDataSourceGraph(t *testing.T) {
	home := dashboardTestHome(t)
	seedWebDashboardTree(t, home)
	// A second job in a different repo, so the repo filter has something to hide
	// and Repos has more than one entry.
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	otherPayload := workflow.JobPayload{Repo: "jerryfane/other", TaskTitle: "other work"}
	if err := store.CreateJob(context.Background(), db.Job{ID: "other-root", Agent: "project-lead", Type: "ask", State: "succeeded", Payload: mustJSON(t, otherPayload)}); err != nil {
		t.Fatalf("CreateJob other: %v", err)
	}
	store.Close()

	ds := &webDataSource{home: home}
	ctx := context.Background()

	g, err := ds.Graph(ctx, "")
	if err != nil {
		t.Fatalf("Graph: %v", err)
	}

	graphNode := func(nodes []dashboard.GraphNode, id string) (dashboard.GraphNode, bool) {
		for _, n := range nodes {
			if n.ID == id {
				return n, true
			}
		}
		return dashboard.GraphNode{}, false
	}
	hasLink := func(links []dashboard.GraphLink, src, tgt, kind string) bool {
		for _, l := range links {
			if l.Source == src && l.Target == tgt && l.Kind == kind {
				return true
			}
		}
		return false
	}
	kindCount := func(links []dashboard.GraphLink, kind string) int {
		n := 0
		for _, l := range links {
			if l.Kind == kind {
				n++
			}
		}
		return n
	}

	// Job nodes: all five seeded jobs are present as type "job".
	for _, id := range []string{"coord", "child-search", "child-export", "coord-cont", "other-root"} {
		n, ok := graphNode(g.Nodes, id)
		if !ok {
			t.Fatalf("job node %q missing", id)
		}
		if n.Type != "job" {
			t.Fatalf("node %q type = %q, want job", id, n.Type)
		}
	}
	if n, _ := graphNode(g.Nodes, "coord"); n.Run != "coord" {
		t.Fatalf("coord Run = %q, want coord", n.Run)
	}
	if n, _ := graphNode(g.Nodes, "child-search"); n.Run != "coord" {
		t.Fatalf("child-search Run = %q, want coord (root)", n.Run)
	}

	// Repo hub + agent hubs.
	if n, ok := graphNode(g.Nodes, "repo::jerryfane/noted"); !ok || n.Type != "repo" || n.Repo != "jerryfane/noted" {
		t.Fatalf("repo hub node bad: %+v ok=%v", n, ok)
	}
	if n, ok := graphNode(g.Nodes, "agent::builder"); !ok || n.Type != "agent" || n.Agent != "builder" {
		t.Fatalf("agent hub builder bad: %+v ok=%v", n, ok)
	}
	if _, ok := graphNode(g.Nodes, "agent::project-lead"); !ok {
		t.Fatalf("agent hub project-lead missing")
	}

	// Links: parent + sibling-mesh (dep) + repo + agent all present.
	if !hasLink(g.Links, "coord", "child-search", "parent") {
		t.Fatalf("missing parent link coord->child-search")
	}
	// child-search and child-export share (root=coord, parent=coord) => a dep link.
	if !hasLink(g.Links, "child-export", "child-search", "dep") && !hasLink(g.Links, "child-search", "child-export", "dep") {
		t.Fatalf("missing sibling-mesh dep link between child-search and child-export")
	}
	if !hasLink(g.Links, "child-search", "repo::jerryfane/noted", "repo") {
		t.Fatalf("missing repo spoke for child-search")
	}
	if !hasLink(g.Links, "child-search", "agent::builder", "agent") {
		t.Fatalf("missing agent spoke for child-search")
	}
	if kindCount(g.Links, "dep") == 0 {
		t.Fatalf("expected at least one sibling-mesh dep link")
	}

	// Repos: full distinct set, sorted.
	if len(g.Repos) != 2 || g.Repos[0] != "jerryfane/noted" || g.Repos[1] != "jerryfane/other" {
		t.Fatalf("Repos = %v, want [jerryfane/noted jerryfane/other]", g.Repos)
	}

	// Repo filter narrows the job nodes to the one repo but keeps the full Repos list.
	filtered, err := ds.Graph(ctx, "jerryfane/noted")
	if err != nil {
		t.Fatalf("Graph(filter): %v", err)
	}
	if _, ok := graphNode(filtered.Nodes, "other-root"); ok {
		t.Fatalf("filtered graph should not contain other-root")
	}
	if _, ok := graphNode(filtered.Nodes, "repo::jerryfane/other"); ok {
		t.Fatalf("filtered graph should not contain the other repo hub")
	}
	if _, ok := graphNode(filtered.Nodes, "coord"); !ok {
		t.Fatalf("filtered graph should still contain coord")
	}
	if len(filtered.Repos) != 2 {
		t.Fatalf("filtered Repos = %v, want full 2-entry list", filtered.Repos)
	}
	for _, l := range filtered.Links {
		if l.Target == "repo::jerryfane/other" || l.Source == "other-root" || l.Target == "other-root" {
			t.Fatalf("filtered graph leaked a link to hidden node: %+v", l)
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

// jobByID returns the first JobSummary with the given id.
func jobSummaryByID(jobs []dashboard.JobSummary, id string) (dashboard.JobSummary, bool) {
	for _, j := range jobs {
		if j.ID == id {
			return j, true
		}
	}
	return dashboard.JobSummary{}, false
}

// agentByName returns the first AgentSummary with the given name.
func agentSummaryByName(agents []dashboard.AgentSummary, name string) (dashboard.AgentSummary, bool) {
	for _, a := range agents {
		if a.Name == name {
			return a, true
		}
	}
	return dashboard.AgentSummary{}, false
}

// TestWebDataSourceJobs asserts Jobs() flattens every job (no cap), sorts newest
// activity first with an id tie-break, and maps each field: title/repo/PR/kind/
// depth/run/state/runtime and token usage.
func TestWebDataSourceJobs(t *testing.T) {
	home := dashboardTestHome(t)
	seedWebDashboardTree(t, home)

	// Give one job real token usage (CreateJob defaults tokens to 0).
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.UpdateJobUsage(context.Background(), "child-search", 2000, 500); err != nil {
		t.Fatalf("UpdateJobUsage: %v", err)
	}
	store.Close()

	// Deterministic times so the sort and duration are exact.
	setJobTimes(t, home, "coord", "2026-05-23 10:00:00", "2026-05-23 10:00:00")
	setJobTimes(t, home, "coord-cont", "2026-05-23 10:05:00", "2026-05-23 10:10:00")
	setJobTimes(t, home, "child-export", "2026-05-23 10:05:00", "2026-05-23 10:20:00")
	setJobTimes(t, home, "child-search", "2026-05-23 10:05:00", "2026-05-23 10:30:00")

	ds := &webDataSource{home: home}
	jobs, err := ds.Jobs(context.Background())
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	// All four seeded jobs, no cap.
	if len(jobs) != 4 {
		t.Fatalf("job count = %d, want 4: %+v", len(jobs), jobs)
	}
	// Newest activity first: child-search(10:30) > child-export(10:20) >
	// coord-cont(10:10) > coord(10:00).
	wantOrder := []string{"child-search", "child-export", "coord-cont", "coord"}
	for i, id := range wantOrder {
		if jobs[i].ID != id {
			t.Fatalf("jobs[%d].ID = %q, want %q (order %v)", i, jobs[i].ID, id,
				[]string{jobs[0].ID, jobs[1].ID, jobs[2].ID, jobs[3].ID})
		}
	}

	search, _ := jobSummaryByID(jobs, "child-search")
	if search.Agent != "builder" {
		t.Errorf("child-search agent = %q, want builder", search.Agent)
	}
	if search.Runtime != "claude" {
		t.Errorf("child-search runtime = %q, want claude (registered)", search.Runtime)
	}
	if search.Repo != "jerryfane/noted" {
		t.Errorf("child-search repo = %q, want jerryfane/noted", search.Repo)
	}
	if search.Kind != "implement" {
		t.Errorf("child-search kind = %q, want implement (Type fallback)", search.Kind)
	}
	if search.State != "succeeded" {
		t.Errorf("child-search state = %q, want succeeded", search.State)
	}
	if search.Depth != 1 {
		t.Errorf("child-search depth = %d, want 1", search.Depth)
	}
	if search.Run != "coord" {
		t.Errorf("child-search run = %q, want coord", search.Run)
	}
	if search.PR != 12 {
		t.Errorf("child-search pr = %d, want 12", search.PR)
	}
	if search.TokensIn != 2000 || search.TokensOut != 500 {
		t.Errorf("child-search tokens = (%d,%d), want (2000,500)", search.TokensIn, search.TokensOut)
	}
	wantStart := parseJobTimeMillis("2026-05-23 10:05:00")
	wantUpdated := parseJobTimeMillis("2026-05-23 10:30:00")
	if search.Started != wantStart || search.Updated != wantUpdated {
		t.Errorf("child-search times = (%d,%d), want (%d,%d)", search.Started, search.Updated, wantStart, wantUpdated)
	}
	if search.Duration != wantUpdated-wantStart {
		t.Errorf("child-search duration = %d, want %d", search.Duration, wantUpdated-wantStart)
	}

	// The root coordinator: depth 0, kind from its Type column, self-rooted.
	coord, _ := jobSummaryByID(jobs, "coord")
	if coord.Depth != 0 {
		t.Errorf("coord depth = %d, want 0", coord.Depth)
	}
	if coord.Kind != "orchestrate" {
		t.Errorf("coord kind = %q, want orchestrate", coord.Kind)
	}
	if coord.Run != "coord" {
		t.Errorf("coord run = %q, want coord (self-root)", coord.Run)
	}
}

// TestWebDataSourceJobsEphemeralRuntime asserts a delegated ephemeral worker
// (no registered agent row) resolves its runtime off the payload's ephemeral
// spec rather than the registry.
func TestWebDataSourceJobsEphemeralRuntime(t *testing.T) {
	home := dashboardTestHome(t)
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ephPayload := workflow.JobPayload{
		Repo:      "jerryfane/noted",
		Ephemeral: &workflow.EphemeralSpec{Runtime: "kimi", Model: "k2"},
	}
	if err := store.CreateJob(context.Background(), db.Job{
		ID: "eph-job", Agent: "task-9-ephemeral-abc123", Type: "implement",
		State: "running", Payload: mustJSON(t, ephPayload),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	store.Close()

	ds := &webDataSource{home: home}
	jobs, err := ds.Jobs(context.Background())
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	eph, ok := jobSummaryByID(jobs, "eph-job")
	if !ok {
		t.Fatalf("eph-job missing")
	}
	if eph.Runtime != "kimi" {
		t.Fatalf("ephemeral runtime = %q, want kimi (from payload spec)", eph.Runtime)
	}
}

// TestJobTitle covers the JobSummary title rule: the first non-empty prompt line,
// else the job id — and that (unlike nodeTitle) it does NOT use the task title.
func TestJobTitle(t *testing.T) {
	got := jobTitle(workflow.JobPayload{Instructions: "\n  Do the thing\nsecond line"}, db.Job{ID: "x"})
	if got != "Do the thing" {
		t.Errorf("title = %q, want %q", got, "Do the thing")
	}
	if got := jobTitle(workflow.JobPayload{TaskTitle: "Fancy Title"}, db.Job{ID: "job-1"}); got != "job-1" {
		t.Errorf("title = %q, want job-1 (task title is not used; id fallback)", got)
	}
	if got := jobTitle(workflow.JobPayload{}, db.Job{ID: "job-2"}); got != "job-2" {
		t.Errorf("title = %q, want job-2 (id fallback)", got)
	}
}

// TestSplitRepoScope covers the comma-separated repo_scope -> []string split.
func TestSplitRepoScope(t *testing.T) {
	if got := splitRepoScope(""); got != nil {
		t.Errorf("empty scope = %v, want nil", got)
	}
	if got := splitRepoScope("  , ,  "); got != nil {
		t.Errorf("blank scope = %v, want nil", got)
	}
	got := splitRepoScope("jerryfane/noted , jerryfane/other")
	if len(got) != 2 || got[0] != "jerryfane/noted" || got[1] != "jerryfane/other" {
		t.Errorf("scope = %v, want [jerryfane/noted jerryfane/other]", got)
	}
}

// TestWebDataSourceAgents asserts Agents() emits one row per registered agent
// (with repo-scope split and per-agent job aggregation) plus one synthetic
// ephemeral rollup last, and that registered rows sort most-recently-active
// first (never-active last).
func TestWebDataSourceAgents(t *testing.T) {
	home := dashboardTestHome(t)
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "builder", Role: "coder", Runtime: "claude",
		RepoScope: "jerryfane/noted, jerryfane/other", Model: "sonnet",
		Capabilities: []string{"implement", "review"}, AutonomyPolicy: "auto", HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent builder: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: "project-lead", Runtime: "codex"}); err != nil {
		t.Fatalf("UpsertAgent project-lead: %v", err)
	}
	// A registered agent that never runs a job (LastActive stays 0 -> sorts last).
	if err := store.UpsertAgent(ctx, db.Agent{Name: "idle-agent", Runtime: "codex"}); err != nil {
		t.Fatalf("UpsertAgent idle-agent: %v", err)
	}

	mustCreateJob(t, store, db.Job{ID: "b1", Agent: "builder", Type: "implement", State: "succeeded", Payload: mustJSON(t, workflow.JobPayload{})}, "", "")
	mustCreateJob(t, store, db.Job{ID: "b2", Agent: "builder", Type: "implement", State: "running", Payload: mustJSON(t, workflow.JobPayload{})}, "", "")
	mustCreateJob(t, store, db.Job{ID: "p1", Agent: "project-lead", Type: "orchestrate", State: "succeeded", Payload: mustJSON(t, workflow.JobPayload{})}, "", "")
	// Two ephemeral-worker jobs (names carry the "-ephemeral-" infix) fold into one rollup.
	mustCreateJob(t, store, db.Job{ID: "e1", Agent: "task-1-ephemeral-aaa111", Type: "implement", State: "succeeded", Payload: mustJSON(t, workflow.JobPayload{})}, "", "")
	mustCreateJob(t, store, db.Job{ID: "e2", Agent: "task-2-ephemeral-bbb222", Type: "review", State: "failed", Payload: mustJSON(t, workflow.JobPayload{})}, "", "")
	store.Close()

	// LastActive ordering: builder(10:30) > project-lead(10:20) > idle-agent(0).
	// The ephemeral rollup has the newest activity (10:40) yet must still sort LAST.
	setJobTimes(t, home, "b1", "2026-05-23 10:00:00", "2026-05-23 10:15:00")
	setJobTimes(t, home, "b2", "2026-05-23 10:00:00", "2026-05-23 10:30:00")
	setJobTimes(t, home, "p1", "2026-05-23 10:00:00", "2026-05-23 10:20:00")
	setJobTimes(t, home, "e1", "2026-05-23 10:00:00", "2026-05-23 10:35:00")
	setJobTimes(t, home, "e2", "2026-05-23 10:00:00", "2026-05-23 10:40:00")

	ds := &webDataSource{home: home}
	agents, err := ds.Agents(ctx)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	// 3 registered + 1 ephemeral rollup.
	if len(agents) != 4 {
		t.Fatalf("agent count = %d, want 4: %+v", len(agents), agents)
	}
	wantOrder := []string{"builder", "project-lead", "idle-agent", "ephemeral workers"}
	for i, name := range wantOrder {
		if agents[i].Name != name {
			t.Fatalf("agents[%d].Name = %q, want %q (order %v)", i, agents[i].Name, name,
				[]string{agents[0].Name, agents[1].Name, agents[2].Name, agents[3].Name})
		}
	}

	builder, _ := agentSummaryByName(agents, "builder")
	if builder.Runtime != "claude" || builder.Role != "coder" || builder.Model != "sonnet" {
		t.Errorf("builder identity = %+v", builder)
	}
	if len(builder.RepoScope) != 2 || builder.RepoScope[0] != "jerryfane/noted" || builder.RepoScope[1] != "jerryfane/other" {
		t.Errorf("builder repoScope = %v, want split [jerryfane/noted jerryfane/other]", builder.RepoScope)
	}
	if len(builder.Capabilities) != 2 {
		t.Errorf("builder capabilities = %v, want 2", builder.Capabilities)
	}
	if builder.JobCount != 2 || builder.RunningCount != 1 || builder.SucceededCount != 1 || builder.FailedCount != 0 {
		t.Errorf("builder counts = job%d run%d ok%d fail%d, want 2/1/1/0",
			builder.JobCount, builder.RunningCount, builder.SucceededCount, builder.FailedCount)
	}
	if builder.LastActive != parseJobTimeMillis("2026-05-23 10:30:00") {
		t.Errorf("builder lastActive = %d, want %d", builder.LastActive, parseJobTimeMillis("2026-05-23 10:30:00"))
	}
	if builder.Ephemeral {
		t.Errorf("builder must not be flagged ephemeral")
	}

	idle, _ := agentSummaryByName(agents, "idle-agent")
	if idle.JobCount != 0 || idle.LastActive != 0 {
		t.Errorf("idle-agent = job%d lastActive%d, want 0/0", idle.JobCount, idle.LastActive)
	}

	// The synthetic ephemeral rollup: both ephemeral jobs, blank runtime, last row.
	rollup := agents[len(agents)-1]
	if !rollup.Ephemeral || rollup.Name != "ephemeral workers" {
		t.Fatalf("last row = %+v, want ephemeral rollup", rollup)
	}
	if rollup.Runtime != "" {
		t.Errorf("ephemeral rollup runtime = %q, want empty", rollup.Runtime)
	}
	if rollup.JobCount != 2 || rollup.SucceededCount != 1 || rollup.FailedCount != 1 || rollup.RunningCount != 0 {
		t.Errorf("ephemeral counts = job%d run%d ok%d fail%d, want 2/0/1/1",
			rollup.JobCount, rollup.RunningCount, rollup.SucceededCount, rollup.FailedCount)
	}
	if rollup.LastActive != parseJobTimeMillis("2026-05-23 10:40:00") {
		t.Errorf("ephemeral lastActive = %d, want %d", rollup.LastActive, parseJobTimeMillis("2026-05-23 10:40:00"))
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

// TestBuildChartsBucketingAndZeroFill asserts jobs bucket by their CreatedAt UTC
// day (respecting the midnight edge), that an activity gap is zero-filled, and
// that totals/agents/repos aggregate over the (days==0) full-history window.
func TestBuildChartsBucketingAndZeroFill(t *testing.T) {
	noted := mustJSON(t, workflow.JobPayload{Repo: "jerryfane/noted"})
	other := mustJSON(t, workflow.JobPayload{Repo: "jerryfane/other"})
	jobs := []db.Job{
		{ID: "j1", Agent: "builder", State: "succeeded", CreatedAt: "2026-05-23 08:00:00", Payload: noted, InputTokens: 100, OutputTokens: 20},
		// 23:59:59 UTC still buckets to the 23rd, not the 24th.
		{ID: "j2", Agent: "builder", State: "failed", CreatedAt: "2026-05-23 23:59:59", Payload: noted, InputTokens: 5, OutputTokens: 7},
		// 00:00:00 UTC opens the 25th; the 24th has no jobs and must zero-fill.
		{ID: "j3", Agent: "project-lead", State: "running", CreatedAt: "2026-05-25 00:00:00", Payload: other, InputTokens: 1, OutputTokens: 2},
	}
	runtimes := map[string]string{"builder": "claude", "project-lead": "codex"}

	// days=0 spans earliest job day .. today (the right edge extends past the
	// last job's day so the all-history view shares its edge with the windowed
	// ones even on idle days). now is pinned two days after the last job to
	// assert that extension deterministically.
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	charts := buildCharts(jobs, 0, now, runtimes)

	if len(charts.Days) != 5 {
		t.Fatalf("days = %d, want 5 (05-23..05-27 continuous, extended to today): %+v", len(charts.Days), charts.Days)
	}
	if charts.Days[0].Date != "2026-05-23" || charts.Days[1].Date != "2026-05-24" || charts.Days[2].Date != "2026-05-25" {
		t.Fatalf("day dates = %q/%q/%q, want 05-23/05-24/05-25", charts.Days[0].Date, charts.Days[1].Date, charts.Days[2].Date)
	}
	if charts.Days[3] != (dashboard.ChartDay{Date: "2026-05-26"}) || charts.Days[4] != (dashboard.ChartDay{Date: "2026-05-27"}) {
		t.Fatalf("trailing idle days = %+v/%+v, want zero-filled 05-26/05-27", charts.Days[3], charts.Days[4])
	}
	d0 := charts.Days[0]
	if d0.Succeeded != 1 || d0.Failed != 1 || d0.TokensIn != 105 || d0.TokensOut != 27 {
		t.Fatalf("05-23 bucket = %+v, want succeeded1 failed1 in105 out27", d0)
	}
	d1 := charts.Days[1]
	if d1 != (dashboard.ChartDay{Date: "2026-05-24"}) {
		t.Fatalf("05-24 bucket = %+v, want zero-filled", d1)
	}
	d2 := charts.Days[2]
	if d2.Running != 1 || d2.TokensIn != 1 || d2.TokensOut != 2 {
		t.Fatalf("05-25 bucket = %+v, want running1 in1 out2", d2)
	}

	tot := charts.Totals
	if tot.Jobs != 3 || tot.Succeeded != 1 || tot.Failed != 1 || tot.TokensIn != 106 || tot.TokensOut != 29 || tot.ActiveAgents != 2 {
		t.Fatalf("totals = %+v, want jobs3 ok1 fail1 in106 out29 active2", tot)
	}

	if len(charts.Agents) != 2 || charts.Agents[0].Name != "builder" {
		t.Fatalf("agents = %+v, want builder leading (2 jobs)", charts.Agents)
	}
	if charts.Agents[0].Runtime != "claude" || charts.Agents[0].Jobs != 2 || charts.Agents[0].TokensOut != 27 {
		t.Fatalf("builder agent = %+v, want claude jobs2 out27", charts.Agents[0])
	}
	if len(charts.Repos) != 2 || charts.Repos[0].Repo != "jerryfane/noted" || charts.Repos[0].Jobs != 2 {
		t.Fatalf("repos = %+v, want jerryfane/noted leading (2 jobs)", charts.Repos)
	}
}

// TestBuildChartsDaysWindow asserts the days param sizes the window to the last N
// days ending today (UTC), zero-filled to exactly N entries, excluding jobs older
// than the window while counting the in-window ones.
func TestBuildChartsDaysWindow(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	noted := mustJSON(t, workflow.JobPayload{Repo: "jerryfane/noted"})
	jobs := []db.Job{
		{ID: "today", Agent: "builder", State: "succeeded", CreatedAt: "2026-05-30 09:00:00", Payload: noted},
		{ID: "recent", Agent: "builder", State: "succeeded", CreatedAt: "2026-05-27 09:00:00", Payload: noted},
		{ID: "ancient", Agent: "builder", State: "succeeded", CreatedAt: "2026-04-20 09:00:00", Payload: noted},
	}

	seven := buildCharts(jobs, 7, now, nil)
	if len(seven.Days) != 7 {
		t.Fatalf("days=7 series length = %d, want 7", len(seven.Days))
	}
	if seven.Days[0].Date != "2026-05-24" || seven.Days[6].Date != "2026-05-30" {
		t.Fatalf("days=7 window = %s..%s, want 2026-05-24..2026-05-30", seven.Days[0].Date, seven.Days[6].Date)
	}
	if seven.Totals.Jobs != 2 {
		t.Fatalf("days=7 totals.Jobs = %d, want 2 (ancient job is out of window)", seven.Totals.Jobs)
	}

	thirty := buildCharts(jobs, 30, now, nil)
	if len(thirty.Days) != 30 {
		t.Fatalf("days=30 series length = %d, want 30", len(thirty.Days))
	}
	if thirty.Days[29].Date != "2026-05-30" {
		t.Fatalf("days=30 last day = %s, want 2026-05-30 (today)", thirty.Days[29].Date)
	}
	if thirty.Totals.Jobs != 2 {
		t.Fatalf("days=30 totals.Jobs = %d, want 2 (ancient job predates the window)", thirty.Totals.Jobs)
	}

	// An empty store still returns a full zero-filled window for a positive days.
	empty := buildCharts(nil, 7, now, nil)
	if len(empty.Days) != 7 || empty.Totals.Jobs != 0 {
		t.Fatalf("empty days=7 = %d days / %d jobs, want 7/0", len(empty.Days), empty.Totals.Jobs)
	}
}

// TestBuildChartsTopN asserts the agent/repo breakdowns cap at chartTopN, order by
// jobs desc, and tie-break on name/repo ascending.
func TestBuildChartsTopN(t *testing.T) {
	var jobs []db.Job
	// 15 agents/repos with one job each (all tie at 1 job) plus one clear leader
	// with two jobs, so the cap keeps the leader + the 11 name-smallest of the ties.
	for i := 0; i < 15; i++ {
		agent := fmt.Sprintf("agent-%02d", i)
		repo := fmt.Sprintf("acme/repo-%02d", i)
		jobs = append(jobs, db.Job{
			ID: fmt.Sprintf("j-%02d", i), Agent: agent, State: "succeeded",
			CreatedAt: "2026-05-23 08:00:00", Payload: mustJSON(t, workflow.JobPayload{Repo: repo}),
		})
	}
	jobs = append(jobs,
		db.Job{ID: "top-a", Agent: "zzz-top", State: "succeeded", CreatedAt: "2026-05-23 08:00:00", Payload: mustJSON(t, workflow.JobPayload{Repo: "acme/repo-top"})},
		db.Job{ID: "top-b", Agent: "zzz-top", State: "succeeded", CreatedAt: "2026-05-23 08:00:00", Payload: mustJSON(t, workflow.JobPayload{Repo: "acme/repo-top"})},
	)

	charts := buildCharts(jobs, 0, time.Now().UTC(), nil)

	if len(charts.Agents) != chartTopN {
		t.Fatalf("agents = %d, want %d (capped)", len(charts.Agents), chartTopN)
	}
	if charts.Agents[0].Name != "zzz-top" || charts.Agents[0].Jobs != 2 {
		t.Fatalf("top agent = %+v, want zzz-top with 2 jobs", charts.Agents[0])
	}
	// The remaining 11 slots are the name-smallest of the single-job ties.
	if charts.Agents[1].Name != "agent-00" || charts.Agents[chartTopN-1].Name != "agent-10" {
		t.Fatalf("tie ordering = %q..%q, want agent-00..agent-10", charts.Agents[1].Name, charts.Agents[chartTopN-1].Name)
	}
	if len(charts.Repos) != chartTopN {
		t.Fatalf("repos = %d, want %d (capped)", len(charts.Repos), chartTopN)
	}
	if charts.Repos[0].Repo != "acme/repo-top" || charts.Repos[0].Jobs != 2 {
		t.Fatalf("top repo = %+v, want acme/repo-top with 2 jobs", charts.Repos[0])
	}
	if charts.Repos[chartTopN-1].Repo != "acme/repo-10" {
		t.Fatalf("last repo = %q, want acme/repo-10 (name tie-break)", charts.Repos[chartTopN-1].Repo)
	}
}

// TestHealthStuckSince covers the wedged-job predicate directly: blocked is always
// stuck, queued is stuck only once its "since" sits at/behind the 10-min cutoff,
// other states never are, and since falls back from UpdatedAt to CreatedAt.
func TestHealthStuckSince(t *testing.T) {
	cutoff := parseJobTimeMillis("2026-05-23 10:00:00")
	cases := []struct {
		name      string
		job       db.Job
		wantStuck bool
		wantSince int64
	}{
		{"blocked always stuck", db.Job{State: "blocked", UpdatedAt: "2026-05-23 11:00:00"}, true, parseJobTimeMillis("2026-05-23 11:00:00")},
		{"queued past cutoff", db.Job{State: "queued", UpdatedAt: "2026-05-23 09:59:00"}, true, parseJobTimeMillis("2026-05-23 09:59:00")},
		{"queued at cutoff", db.Job{State: "queued", UpdatedAt: "2026-05-23 10:00:00"}, true, cutoff},
		{"queued within threshold", db.Job{State: "queued", UpdatedAt: "2026-05-23 10:05:00"}, false, parseJobTimeMillis("2026-05-23 10:05:00")},
		{"running never stuck", db.Job{State: "running", UpdatedAt: "2026-05-23 08:00:00"}, false, parseJobTimeMillis("2026-05-23 08:00:00")},
		{"queued no timestamps", db.Job{State: "queued"}, false, 0},
		{"blocked created-at fallback", db.Job{State: "blocked", CreatedAt: "2026-05-23 08:00:00"}, true, parseJobTimeMillis("2026-05-23 08:00:00")},
	}
	for _, c := range cases {
		since, stuck := healthStuckSince(c.job, cutoff)
		if stuck != c.wantStuck || since != c.wantSince {
			t.Errorf("%s: healthStuckSince = (since=%d, stuck=%v), want (since=%d, stuck=%v)", c.name, since, stuck, c.wantSince, c.wantStuck)
		}
	}
}

// seedHealthHome seeds a store with the shapes the Health page reports: a blocked
// job carrying a reason event, a stale (>10 min) queued job, a fresh queued job, two
// failures at distinct times, a running job, plus one branch lock and one resource
// lock.
func seedHealthHome(t *testing.T, home string) {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	noted := mustJSON(t, workflow.JobPayload{Repo: "jerryfane/noted"})

	mustCreateJob(t, store, db.Job{ID: "blk", Agent: "builder", Type: "implement", State: "blocked", Payload: noted}, "advance_awaiting_human", "needs your approval")
	mustCreateJob(t, store, db.Job{ID: "stale-q", Agent: "builder", Type: "implement", State: "queued", Payload: noted}, "", "")
	mustCreateJob(t, store, db.Job{ID: "fresh-q", Agent: "builder", Type: "implement", State: "queued", Payload: noted}, "", "")
	mustCreateJob(t, store, db.Job{ID: "run1", Agent: "builder", Type: "implement", State: "running", Payload: noted}, "", "")
	mustCreateJob(t, store, db.Job{ID: "f1", Agent: "builder", Type: "implement", State: "failed", Payload: noted}, "", "")
	mustCreateJob(t, store, db.Job{ID: "f2", Agent: "builder", Type: "implement", State: "failed", Payload: noted}, "", "")

	if _, err := store.CreateLock(ctx, db.BranchLock{RepoFullName: "jerryfane/noted", Branch: "feature/x", Owner: "blk"}); err != nil {
		t.Fatalf("CreateLock: %v", err)
	}
	expires := time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339Nano)
	if _, err := store.AcquireResourceLock(ctx, db.ResourceLock{ResourceKey: "runtime:codex:sess", OwnerJobID: "blk", OwnerToken: "tok", ExpiresAt: expires}, time.Now().UTC()); err != nil {
		t.Fatalf("AcquireResourceLock: %v", err)
	}
	store.Close()

	// stale-q's UpdatedAt is far in the past -> stuck; the failures get distinct
	// UpdatedAt so newest-first ordering is exact. fresh-q keeps its CURRENT_TIMESTAMP
	// (~now) so it stays under the threshold.
	setJobTimes(t, home, "stale-q", "2020-01-01 00:00:00", "2020-01-01 00:00:00")
	setJobTimes(t, home, "f1", "2026-05-23 10:00:00", "2026-05-23 10:10:00")
	setJobTimes(t, home, "f2", "2026-05-23 10:00:00", "2026-05-23 10:20:00")
}

func stuckByID(stuck []dashboard.HealthStuckJob, id string) (dashboard.HealthStuckJob, bool) {
	for _, s := range stuck {
		if s.ID == id {
			return s, true
		}
	}
	return dashboard.HealthStuckJob{}, false
}

// TestWebDataSourceHealth asserts Health() rolls up state totals, surfaces blocked
// + stale-queued jobs (with the blocked job's reason) while hiding a fresh queued
// job, orders recent failures newest-first, and maps the branch + resource locks.
// The daemon is unregistered here so it must read as a zero-value (not running).
func TestWebDataSourceHealth(t *testing.T) {
	home := dashboardTestHome(t)
	seedHealthHome(t, home)

	ds := &webDataSource{home: home}
	h, err := ds.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	// Daemon zero-value: no daemon.pid/json in this fresh home.
	if h.Daemon.Running || h.Daemon.PID != 0 || h.Daemon.StartedAt != 0 {
		t.Fatalf("daemon = %+v, want zero-value (not running)", h.Daemon)
	}

	tot := h.Totals
	if tot.Queued != 2 || tot.Running != 1 || tot.Blocked != 1 || tot.Failed != 2 || tot.Succeeded != 0 || tot.Cancelled != 0 {
		t.Fatalf("totals = %+v, want queued2 running1 blocked1 failed2", tot)
	}

	// Stuck: blocked (blk) + stale queued (stale-q); fresh queued excluded. Oldest
	// first => stale-q (2020) before blk (~now).
	if len(h.Stuck) != 2 {
		t.Fatalf("stuck = %d, want 2: %+v", len(h.Stuck), h.Stuck)
	}
	if h.Stuck[0].ID != "stale-q" {
		t.Fatalf("stuck[0] = %q, want stale-q (oldest first)", h.Stuck[0].ID)
	}
	if _, ok := stuckByID(h.Stuck, "fresh-q"); ok {
		t.Fatalf("fresh queued job must not be reported stuck")
	}
	blk, ok := stuckByID(h.Stuck, "blk")
	if !ok {
		t.Fatalf("blocked job blk missing from stuck")
	}
	if blk.State != "blocked" {
		t.Fatalf("blk state = %q, want blocked", blk.State)
	}
	if blk.Reason == "" || !strings.Contains(blk.Reason, "awaiting human") {
		t.Fatalf("blk reason = %q, want it to carry the awaiting-human signal", blk.Reason)
	}

	// Recent failures newest-first: f2 (10:20) before f1 (10:10).
	if len(h.RecentFailures) != 2 || h.RecentFailures[0].ID != "f2" || h.RecentFailures[1].ID != "f1" {
		t.Fatalf("recent failures = %+v, want [f2 f1] newest-first", h.RecentFailures)
	}

	// Branch lock mapping.
	if len(h.Locks) != 1 {
		t.Fatalf("locks = %d, want 1: %+v", len(h.Locks), h.Locks)
	}
	lock := h.Locks[0]
	if lock.Repo != "jerryfane/noted" || lock.Branch != "feature/x" || lock.Owner != "blk" {
		t.Fatalf("lock = %+v, want jerryfane/noted feature/x blk", lock)
	}
	if lock.AcquiredAt <= 0 {
		t.Fatalf("lock AcquiredAt = %d, want > 0 (mapped from created_at)", lock.AcquiredAt)
	}

	// Resource lock mapping.
	if len(h.ResourceLocks) != 1 {
		t.Fatalf("resourceLocks = %d, want 1: %+v", len(h.ResourceLocks), h.ResourceLocks)
	}
	rl := h.ResourceLocks[0]
	if rl.Key != "runtime:codex:sess" || rl.Owner != "blk" {
		t.Fatalf("resource lock = %+v, want runtime:codex:sess owner blk", rl)
	}
	if rl.AcquiredAt <= 0 || rl.ExpiresAt <= rl.AcquiredAt {
		t.Fatalf("resource lock times = acquired %d expires %d, want acquired>0 and expires>acquired", rl.AcquiredAt, rl.ExpiresAt)
	}
}

// TestWebDataSourceHealthDaemonRunning uses the same self-registration fixture the
// daemon tests use (registerDaemonRunState with this process's argv) to prove
// Health reads the live daemon's pid + start time off d.home.
func TestWebDataSourceHealthDaemonRunning(t *testing.T) {
	home := dashboardTestHome(t)
	seedHealthHome(t, home)

	state := daemonProcessState(config.PathsForHome(home))
	wd, _ := os.Getwd()
	if ok, err := registerDaemonRunState(state, os.Args, wd); err != nil || !ok {
		t.Fatalf("registerDaemonRunState ok=%v err=%v, want true nil", ok, err)
	}
	defer deregisterDaemonRunState(state)

	ds := &webDataSource{home: home}
	h, err := ds.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !h.Daemon.Running || h.Daemon.PID != os.Getpid() {
		t.Fatalf("daemon = %+v, want running pid=%d", h.Daemon, os.Getpid())
	}
	if h.Daemon.StartedAt <= 0 {
		t.Fatalf("daemon StartedAt = %d, want > 0 (from daemon.json meta)", h.Daemon.StartedAt)
	}
}

// seedTemplatedAgent registers an agent bound to a template that has a current
// v1 plus a newer pending v2 (so version ordering and the Current marker are both
// exercised), and gives the agent two jobs. It returns the current v1 version id
// and the pending v2 version id.
func seedTemplatedAgent(t *testing.T, home string) (v1ID, v2ID string) {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	base := db.AgentTemplate{
		ID: "planner", Name: "Planner Template", Description: "plans the work",
		SourceRepo: "jerryfane/noted", SourceRef: "main", SourcePath: "agents/planner.md",
		ResolvedCommit: "aaaaaaaaaaaa", Content: "v1 content",
	}
	if err := store.UpsertAgentTemplate(ctx, base); err != nil {
		t.Fatalf("UpsertAgentTemplate: %v", err)
	}
	// current_version_id (what the template resolves to) is v1 at this point.
	tmpl, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}
	v1ID = tmpl.VersionID

	v2 := base
	v2.Content = "v2 content"
	v2.ResolvedCommit = "bbbbbbbbbbbb"
	v2.SourceRef = "candidate"
	pending, err := store.AddPendingAgentTemplateVersion(ctx, v2)
	if err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion: %v", err)
	}
	v2ID = pending.ID

	if err := store.UpsertAgent(ctx, db.Agent{Name: "planner-agent", Runtime: "codex", TemplateID: "planner"}); err != nil {
		t.Fatalf("UpsertAgent planner-agent: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{Name: "plain-agent", Runtime: "claude"}); err != nil {
		t.Fatalf("UpsertAgent plain-agent: %v", err)
	}
	// A registered agent pointing at a template that does not exist -> fail-open.
	if err := store.UpsertAgent(ctx, db.Agent{Name: "ghost-agent", Runtime: "codex", TemplateID: "no-such-template"}); err != nil {
		t.Fatalf("UpsertAgent ghost-agent: %v", err)
	}

	mustCreateJob(t, store, db.Job{ID: "pa1", Agent: "planner-agent", Type: "orchestrate", State: "succeeded", Payload: mustJSON(t, workflow.JobPayload{})}, "", "")
	mustCreateJob(t, store, db.Job{ID: "pa2", Agent: "planner-agent", Type: "orchestrate", State: "running", Payload: mustJSON(t, workflow.JobPayload{})}, "", "")
	return v1ID, v2ID
}

// TestWebDataSourceAgentDetail asserts Agent() builds the same summary as Agents()
// for one row, maps the template, and returns the version history newest-first
// with Current marking the version the template currently resolves to
// (current_version_id = v1), not the newer pending candidate (v2).
func TestWebDataSourceAgentDetail(t *testing.T) {
	home := dashboardTestHome(t)
	v1ID, v2ID := seedTemplatedAgent(t, home)

	ds := &webDataSource{home: home}
	detail, err := ds.Agent(context.Background(), "planner-agent")
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}

	// Summary: identity + job tallies (same as Agents()).
	if detail.Name != "planner-agent" || detail.Runtime != "codex" {
		t.Fatalf("summary identity = %+v", detail.AgentSummary)
	}
	if detail.JobCount != 2 || detail.RunningCount != 1 || detail.SucceededCount != 1 {
		t.Fatalf("summary counts = job%d run%d ok%d, want 2/1/1", detail.JobCount, detail.RunningCount, detail.SucceededCount)
	}

	// Template mapping.
	if detail.Template == nil {
		t.Fatalf("template is nil, want the planner template")
	}
	if detail.Template.ID != "planner" || detail.Template.Name != "Planner Template" {
		t.Fatalf("template identity = %+v", detail.Template)
	}
	if detail.Template.SourceRepo != "jerryfane/noted" || detail.Template.SourceRef != "main" ||
		detail.Template.SourcePath != "agents/planner.md" || detail.Template.ResolvedCommit != "aaaaaaaaaaaa" {
		t.Fatalf("template source fields = %+v", detail.Template)
	}
	// Content is the full body of the version the template currently resolves to
	// (current_version_id = v1), mapped verbatim from GetAgentTemplate().Content.
	if detail.Template.Content != "v1 content" {
		t.Fatalf("template content = %q, want %q (the current v1 body)", detail.Template.Content, "v1 content")
	}

	// Versions newest-first: v2 (pending) before v1 (current). Current marks v1.
	if len(detail.Versions) != 2 {
		t.Fatalf("versions = %d, want 2: %+v", len(detail.Versions), detail.Versions)
	}
	newest := detail.Versions[0]
	if newest.ID != v2ID || newest.Number != 2 {
		t.Fatalf("versions[0] = %+v, want v2 (number 2, id %s)", newest, v2ID)
	}
	if newest.State != "pending" {
		t.Fatalf("versions[0].State = %q, want pending", newest.State)
	}
	if newest.Current {
		t.Fatalf("newest pending v2 must not be marked Current")
	}
	// Each version carries its own body, mapped verbatim from the store row's Content.
	if newest.Content != "v2 content" {
		t.Fatalf("versions[0].Content = %q, want %q (the v2 body)", newest.Content, "v2 content")
	}
	oldest := detail.Versions[1]
	if oldest.ID != v1ID || oldest.Number != 1 {
		t.Fatalf("versions[1] = %+v, want v1 (number 1, id %s)", oldest, v1ID)
	}
	if !oldest.Current {
		t.Fatalf("current v1 must be marked Current (template resolves to current_version_id)")
	}
	if oldest.State != "current" {
		t.Fatalf("versions[1].State = %q, want current", oldest.State)
	}
	if oldest.Content != "v1 content" {
		t.Fatalf("versions[1].Content = %q, want %q (the v1 body)", oldest.Content, "v1 content")
	}
	if oldest.CreatedAt <= 0 {
		t.Fatalf("versions[1].CreatedAt = %d, want > 0 (parsed epoch ms)", oldest.CreatedAt)
	}
}

// TestWebDataSourceAgentNoTemplate asserts an agent with no template (or a
// dangling template id) returns a nil Template and a non-nil empty Versions slice
// rather than erroring the endpoint.
func TestWebDataSourceAgentNoTemplate(t *testing.T) {
	home := dashboardTestHome(t)
	seedTemplatedAgent(t, home)
	ds := &webDataSource{home: home}
	ctx := context.Background()

	plain, err := ds.Agent(ctx, "plain-agent")
	if err != nil {
		t.Fatalf("Agent(plain-agent): %v", err)
	}
	if plain.Template != nil {
		t.Fatalf("plain-agent template = %+v, want nil", plain.Template)
	}
	if plain.Versions == nil || len(plain.Versions) != 0 {
		t.Fatalf("plain-agent versions = %+v, want non-nil empty", plain.Versions)
	}

	// A dangling template id must fail open (template absent), not 500.
	ghost, err := ds.Agent(ctx, "ghost-agent")
	if err != nil {
		t.Fatalf("Agent(ghost-agent) errored on a missing template, want fail-open: %v", err)
	}
	if ghost.Template != nil || len(ghost.Versions) != 0 {
		t.Fatalf("ghost-agent detail = %+v, want template nil + empty versions", ghost)
	}
}

// TestWebDataSourceAgentUnknown asserts an unknown agent name maps to the
// not-found sentinel (so the API returns 404, mirroring Job()).
func TestWebDataSourceAgentUnknown(t *testing.T) {
	home := dashboardTestHome(t)
	seedTemplatedAgent(t, home)
	ds := &webDataSource{home: home}
	if _, err := ds.Agent(context.Background(), "no-such-agent"); !errors.Is(err, dashboard.ErrAgentNotFound) {
		t.Fatalf("Agent(unknown) err = %v, want ErrAgentNotFound", err)
	}
}

// writeFakeVersionBin writes an executable shell script that appends one byte to
// counterFile on every run (so exec count is observable) and prints stdoutBody.
func writeFakeVersionBin(t *testing.T, path, counterFile, stdoutBody string) {
	t.Helper()
	script := "#!/bin/sh\nprintf 'x' >> " + counterFile + "\nprintf '%s\\n' '" + stdoutBody + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
}

func execCount(t *testing.T, counterFile string) int {
	t.Helper()
	data, err := os.ReadFile(counterFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read counter: %v", err)
	}
	return len(data)
}

// TestWebDataSourceDaemonVersionCache asserts resolveOnDiskVersion execs the
// binary's "version --json" once, parses the JSON version, serves subsequent
// calls from the mtime-keyed cache WITHOUT re-execing, and re-execs after the
// binary's mtime changes. A missing/non-executable path yields "" (fail-open).
func TestWebDataSourceDaemonVersionCache(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gitmoot-fake")
	counter := filepath.Join(dir, "calls")
	writeFakeVersionBin(t, bin, counter, `{"version":"v1.2.3"}`)

	ds := &webDataSource{}
	ctx := context.Background()

	if v := ds.resolveOnDiskVersion(ctx, bin); v != "v1.2.3" {
		t.Fatalf("version = %q, want v1.2.3", v)
	}
	if n := execCount(t, counter); n != 1 {
		t.Fatalf("exec count = %d after first resolve, want 1", n)
	}
	// Cache hit: same binary/mtime -> no re-exec.
	if v := ds.resolveOnDiskVersion(ctx, bin); v != "v1.2.3" {
		t.Fatalf("cached version = %q, want v1.2.3", v)
	}
	if n := execCount(t, counter); n != 1 {
		t.Fatalf("exec count = %d after cache hit, want 1 (must not re-exec)", n)
	}

	// New content + new mtime -> cache miss -> re-exec, new version.
	writeFakeVersionBin(t, bin, counter, `{"version":"v4.5.6"}`)
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(bin, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if v := ds.resolveOnDiskVersion(ctx, bin); v != "v4.5.6" {
		t.Fatalf("version after mtime change = %q, want v4.5.6", v)
	}
	if n := execCount(t, counter); n != 2 {
		t.Fatalf("exec count = %d after mtime change, want 2 (must re-exec)", n)
	}

	// Fail-open on a path that does not exist.
	if v := ds.resolveOnDiskVersion(ctx, filepath.Join(dir, "nope")); v != "" {
		t.Fatalf("missing binary version = %q, want empty", v)
	}
}

// TestWebDataSourceDaemonVersionTextFallback asserts resolveOnDiskVersion falls
// back to the plain-text "gitmoot <ver>" form (trimming the prefix) when the JSON
// form does not parse.
func TestWebDataSourceDaemonVersionTextFallback(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gitmoot-text")
	counter := filepath.Join(dir, "calls")
	// Prints non-JSON for every invocation, so "version --json" fails to parse and
	// the plain-text fallback is used.
	writeFakeVersionBin(t, bin, counter, `gitmoot v7.8.9`)

	ds := &webDataSource{}
	if v := ds.resolveOnDiskVersion(context.Background(), bin); v != "v7.8.9" {
		t.Fatalf("text fallback version = %q, want v7.8.9", v)
	}
}

// TestWebDataSourceUpdateCheckCache stubs the GitHub release check and asserts the
// update result is cached (a second call within TTL does not re-invoke the
// checker), and that a stale cache whose refresh FAILS still serves the last good
// result (fail-open).
func TestWebDataSourceUpdateCheckCache(t *testing.T) {
	orig := updateCheckFn
	t.Cleanup(func() { updateCheckFn = orig })

	calls := 0
	updateCheckFn = func(ctx context.Context, current buildinfo.Info, executable string) (update.CheckResult, error) {
		calls++
		return update.CheckResult{
			CurrentVersion: "v1.0.0", LatestVersion: "v2.0.0",
			ReleaseURL: "https://github.com/jerryfane/gitmoot/releases/tag/v2.0.0", UpToDate: false,
		}, nil
	}

	ds := &webDataSource{}
	ctx := context.Background()

	u := ds.checkUpdate(ctx, "")
	if u == nil {
		t.Fatalf("update = nil, want a HealthUpdate")
	}
	if !u.UpdateAvailable || u.Latest != "v2.0.0" || u.Current != "v1.0.0" {
		t.Fatalf("update = %+v, want UpdateAvailable/latest v2.0.0/current v1.0.0", u)
	}
	if u.CheckedAt <= 0 {
		t.Fatalf("update CheckedAt = %d, want > 0", u.CheckedAt)
	}
	if calls != 1 {
		t.Fatalf("checker calls = %d after first checkUpdate, want 1", calls)
	}
	// Cache hit within TTL: no re-invoke.
	if u2 := ds.checkUpdate(ctx, ""); u2 == nil || u2.Latest != "v2.0.0" {
		t.Fatalf("cached update = %+v, want the same result", u2)
	}
	if calls != 1 {
		t.Fatalf("checker calls = %d after cache hit, want 1 (must not re-check)", calls)
	}
	// Returned pointer must be a copy, not the cached value (caller may mutate).
	u.Current = "mutated"
	if u3 := ds.checkUpdate(ctx, ""); u3.Current != "v1.0.0" {
		t.Fatalf("mutating a returned update leaked into the cache: %q", u3.Current)
	}

	// Force the cache stale, then fail the refresh: the last good result is served.
	ds.updateFetchedAt = time.Now().Add(-2 * time.Hour)
	updateCheckFn = func(ctx context.Context, current buildinfo.Info, executable string) (update.CheckResult, error) {
		calls++
		return update.CheckResult{}, errors.New("github unreachable")
	}
	u4 := ds.checkUpdate(ctx, "")
	if calls != 2 {
		t.Fatalf("checker calls = %d, want 2 (stale cache should attempt a refresh)", calls)
	}
	if u4 == nil || u4.Latest != "v2.0.0" {
		t.Fatalf("failed refresh update = %+v, want the last good result served", u4)
	}
}

// TestWebDataSourceUpdateCheckColdFailure asserts a failing check with no prior
// success is fail-open (nil Update, never an error).
func TestWebDataSourceUpdateCheckColdFailure(t *testing.T) {
	orig := updateCheckFn
	t.Cleanup(func() { updateCheckFn = orig })
	updateCheckFn = func(ctx context.Context, current buildinfo.Info, executable string) (update.CheckResult, error) {
		return update.CheckResult{}, errors.New("offline")
	}
	ds := &webDataSource{}
	if u := ds.checkUpdate(context.Background(), ""); u != nil {
		t.Fatalf("cold failure update = %+v, want nil (fail-open)", u)
	}
}

// TestWebDataSourceUpdateNoRelease asserts a "no release" answer is treated as no
// data (nil Update), so the field is omitted rather than reporting a bogus update.
func TestWebDataSourceUpdateNoRelease(t *testing.T) {
	orig := updateCheckFn
	t.Cleanup(func() { updateCheckFn = orig })
	updateCheckFn = func(ctx context.Context, current buildinfo.Info, executable string) (update.CheckResult, error) {
		return update.CheckResult{CurrentVersion: "v1.0.0", LatestVersion: "none", NoRelease: true}, nil
	}
	ds := &webDataSource{}
	if u := ds.checkUpdate(context.Background(), ""); u != nil {
		t.Fatalf("no-release update = %+v, want nil", u)
	}
}

// TestWebDataSourceAgentConfigAndMemory asserts the config-visibility enrichment:
// Agents() sets MemoryEnabled from each agent's [agents.<name>] section, and
// Agent() builds AgentDetail.Config (nil when the agent has no section) plus the
// two owned-memory counts (across all owner versions, scoped to the agent).
func TestWebDataSourceAgentConfigAndMemory(t *testing.T) {
	home := dashboardTestHome(t)
	paths := config.PathsForHome(home)
	ctx := context.Background()

	// Config sections: mem-agent enrolled (memory on, custom pool knobs); plain-agent
	// has a section with memory OFF (parse-time defaults fill the rest); nosection-agent
	// gets NO [agents.<name>] section at all.
	if err := config.SaveAgentType(paths, config.AgentType{
		Name: "mem-agent", Runtime: "codex", Memory: true,
		MaxBackground: 3, IdleTimeout: "15m", JobTimeout: "30m",
		Model: "gpt-5.5", Template: "coordinator", Capabilities: []string{"orchestrate", "review"},
	}); err != nil {
		t.Fatalf("SaveAgentType mem-agent: %v", err)
	}
	if err := config.SaveAgentType(paths, config.AgentType{Name: "plain-agent", Runtime: "claude", Memory: false}); err != nil {
		t.Fatalf("SaveAgentType plain-agent: %v", err)
	}

	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Store registration is independent of the config section: all three agents are
	// registered, but only two carry a section and only one has memory rows.
	for _, a := range []db.Agent{
		{Name: "mem-agent", Runtime: "codex"},
		{Name: "plain-agent", Runtime: "claude"},
		{Name: "nosection-agent", Runtime: "codex"},
	} {
		if err := store.UpsertAgent(ctx, a); err != nil {
			t.Fatalf("UpsertAgent %s: %v", a.Name, err)
		}
	}

	// Seed memory rows OWNED by mem-agent: two confirmed facts (distinct keys, one
	// under a non-empty owner version so the "all owner versions" count is exercised)
	// and three observations (append-only; a repeated key still counts). Rows owned
	// by a DIFFERENT agent must be excluded from mem-agent's counts.
	seedConfirmed := func(ref, version, key string) {
		if _, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
			Owner: db.MemoryOwner{Kind: "agent", Ref: ref, Version: version},
			Repo:  "jerryfane/noted", Scope: "repo", Key: key, Content: "fact " + key,
		}); err != nil {
			t.Fatalf("UpsertConfirmedMemory: %v", err)
		}
	}
	seedObs := func(ref, key string) {
		if _, err := store.InsertMemoryObservation(ctx, db.MemoryObservation{
			Owner: db.MemoryOwner{Kind: "agent", Ref: ref},
			Repo:  "jerryfane/noted", Scope: "repo", Key: key, Content: "obs " + key,
		}); err != nil {
			t.Fatalf("InsertMemoryObservation: %v", err)
		}
	}
	seedConfirmed("mem-agent", "", "fix-rounds")
	seedConfirmed("mem-agent", "v2", "outcome") // different owner version, still counted
	seedObs("mem-agent", "a")
	seedObs("mem-agent", "a") // append-only: the repeated key is a distinct row
	seedObs("mem-agent", "b")
	seedConfirmed("other-agent", "", "x") // foreign owner, must be excluded
	seedObs("other-agent", "y")
	store.Close()

	ds := &webDataSource{home: home}

	// Agents(): MemoryEnabled tracks the config section — enrolled vs off vs none.
	agents, err := ds.Agents(ctx)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	byName := map[string]dashboard.AgentSummary{}
	for _, a := range agents {
		byName[a.Name] = a
	}
	if !byName["mem-agent"].MemoryEnabled {
		t.Fatalf("mem-agent MemoryEnabled = false, want true")
	}
	if byName["plain-agent"].MemoryEnabled {
		t.Fatalf("plain-agent MemoryEnabled = true, want false (config memory off)")
	}
	if byName["nosection-agent"].MemoryEnabled {
		t.Fatalf("nosection-agent MemoryEnabled = true, want false (no config section)")
	}

	// Agent(mem-agent): full config section + owned memory counts.
	mem, err := ds.Agent(ctx, "mem-agent")
	if err != nil {
		t.Fatalf("Agent(mem-agent): %v", err)
	}
	if !mem.MemoryEnabled {
		t.Fatalf("mem-agent detail MemoryEnabled = false, want true")
	}
	if mem.Config == nil {
		t.Fatalf("mem-agent Config = nil, want a section")
	}
	if !mem.Config.Memory || mem.Config.MaxBackground != 3 || mem.Config.IdleTimeout != "15m" ||
		mem.Config.JobTimeout != "30m" || mem.Config.Model != "gpt-5.5" || mem.Config.Template != "coordinator" {
		t.Fatalf("mem-agent Config = %+v, want the seeded section values", mem.Config)
	}
	if len(mem.Config.Capabilities) != 2 {
		t.Fatalf("mem-agent Config.Capabilities = %v, want 2", mem.Config.Capabilities)
	}
	if mem.MemoryFacts != 2 {
		t.Fatalf("mem-agent MemoryFacts = %d, want 2 (both owner versions)", mem.MemoryFacts)
	}
	if mem.MemoryObservations != 3 {
		t.Fatalf("mem-agent MemoryObservations = %d, want 3", mem.MemoryObservations)
	}

	// Agent(plain-agent): a section exists (memory off; parse-time defaults folded in),
	// no memory rows.
	plain, err := ds.Agent(ctx, "plain-agent")
	if err != nil {
		t.Fatalf("Agent(plain-agent): %v", err)
	}
	if plain.MemoryEnabled {
		t.Fatalf("plain-agent detail MemoryEnabled = true, want false")
	}
	if plain.Config == nil || plain.Config.Memory {
		t.Fatalf("plain-agent Config = %+v, want a non-nil section with memory off", plain.Config)
	}
	if plain.Config.MaxBackground != 1 || plain.Config.IdleTimeout != "20m" || plain.Config.JobTimeout != "10m" {
		t.Fatalf("plain-agent Config defaults = %+v, want max_background 1 / idle 20m / job 10m", plain.Config)
	}
	if plain.MemoryFacts != 0 || plain.MemoryObservations != 0 {
		t.Fatalf("plain-agent memory counts = %d/%d, want 0/0", plain.MemoryFacts, plain.MemoryObservations)
	}

	// Agent(nosection-agent): no config section => nil Config, no chip, zero counts.
	none, err := ds.Agent(ctx, "nosection-agent")
	if err != nil {
		t.Fatalf("Agent(nosection-agent): %v", err)
	}
	if none.Config != nil {
		t.Fatalf("nosection-agent Config = %+v, want nil (no section)", none.Config)
	}
	if none.MemoryEnabled {
		t.Fatalf("nosection-agent MemoryEnabled = true, want false")
	}
	if none.MemoryFacts != 0 || none.MemoryObservations != 0 {
		t.Fatalf("nosection-agent memory counts = %d/%d, want 0/0", none.MemoryFacts, none.MemoryObservations)
	}
}
