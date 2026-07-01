package cli

import (
	"context"
	"encoding/json"
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
