package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/cli/tui"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func TestBuildDelegationTree(t *testing.T) {
	parent := workflow.JobPayload{
		Result: &workflow.AgentResult{
			Delegations: []workflow.Delegation{
				{ID: "api", Agent: "coder", Action: "implement api"},
				{ID: "ui", Agent: "coder", Action: "implement ui", Deps: []string{"api"}},
			},
		},
	}
	children := []db.Job{
		{ID: "j-api", Agent: "coder", Type: "implement", State: "succeeded", ParentJobID: "p", DelegationID: "api"},
		{ID: "j-ui", Agent: "coder", Type: "implement", State: "running", ParentJobID: "p", DelegationID: "ui"},
		{ID: "j-cont", Agent: "planner", Type: "ask", State: "queued", ParentJobID: "p"},
	}

	got, contID, contState := buildDelegationTree(parent, children)
	if contID != "j-cont" || contState != "queued" {
		t.Fatalf("continuation = (%q,%q), want (j-cont,queued)", contID, contState)
	}
	if len(got) != 2 {
		t.Fatalf("got %d delegation children, want 2: %+v", len(got), got)
	}
	byDelegation := map[string]struct {
		action    string
		satisfied bool
	}{}
	for _, c := range got {
		byDelegation[c.DelegationID] = struct {
			action    string
			satisfied bool
		}{c.Action, c.DepsSatisfied}
	}
	if api := byDelegation["api"]; api.action != "implement api" {
		t.Fatalf("api action = %q, want implement api", api.action)
	}
	// ui's only dep (api) succeeded, so its deps are satisfied.
	if ui := byDelegation["ui"]; ui.action != "implement ui" || !ui.satisfied {
		t.Fatalf("ui = %+v, want action=implement ui satisfied=true", ui)
	}
}

func TestBuildDelegationTreeActionFallsBackToType(t *testing.T) {
	// No parent result: action falls back to the child job type, deps unsatisfied
	// when a dep's sibling has not succeeded.
	children := []db.Job{
		{ID: "j-api", Agent: "coder", Type: "implement", State: "running", ParentJobID: "p", DelegationID: "api"},
		{ID: "j-ui", Agent: "coder", Type: "review", State: "running", ParentJobID: "p", DelegationID: "ui"},
	}
	parent := workflow.JobPayload{
		Result: &workflow.AgentResult{
			Delegations: []workflow.Delegation{{ID: "ui", Agent: "coder", Action: "", Deps: []string{"api"}}},
		},
	}
	got, contID, _ := buildDelegationTree(parent, children)
	if contID != "" {
		t.Fatalf("no continuation child should leave continuation id empty, got %q", contID)
	}
	var apiAction, uiAction string
	var uiSatisfied bool
	for _, c := range got {
		switch c.DelegationID {
		case "api":
			apiAction = c.Action
		case "ui":
			uiAction = c.Action
			uiSatisfied = c.DepsSatisfied
		}
	}
	if apiAction != "implement" {
		t.Fatalf("api action fallback = %q, want job type implement", apiAction)
	}
	if uiAction != "review" {
		t.Fatalf("ui action fallback = %q, want job type review", uiAction)
	}
	if uiSatisfied {
		t.Fatalf("ui deps should be pending while api is still running")
	}
}

func TestBuildDelegationTreeCollapsesRetriedDelegation(t *testing.T) {
	// A Phase 2 retry creates an additional child row sharing the DelegationID
	// (only the job ID and RetryCount differ). The tree must show ONE node per
	// delegation -- the latest attempt -- not a duplicate row per attempt.
	parent := workflow.JobPayload{
		Result: &workflow.AgentResult{
			Delegations: []workflow.Delegation{
				{ID: "api", Agent: "coder", Action: "implement api"},
				{ID: "ui", Agent: "coder", Action: "implement ui", Deps: []string{"api"}},
			},
		},
	}
	mustPayload := func(p workflow.JobPayload) string {
		t.Helper()
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return string(raw)
	}
	// api's first attempt failed; the retry (RetryCount=1) succeeded.
	apiAttempt0 := workflow.JobPayload{ParentJobID: "p", DelegationID: "api", RetryCount: 0}
	apiAttempt1 := workflow.JobPayload{ParentJobID: "p", DelegationID: "api", RetryCount: 1}
	children := []db.Job{
		{ID: "p/delegation/api", Agent: "coder", Type: "implement", State: "failed", ParentJobID: "p", DelegationID: "api", Payload: mustPayload(apiAttempt0)},
		{ID: "p/delegation/api/retry/1", Agent: "coder", Type: "implement", State: "succeeded", ParentJobID: "p", DelegationID: "api", Payload: mustPayload(apiAttempt1)},
		{ID: "p/delegation/ui", Agent: "coder", Type: "implement", State: "running", ParentJobID: "p", DelegationID: "ui", Payload: mustPayload(workflow.JobPayload{ParentJobID: "p", DelegationID: "ui"})},
	}

	got, _, _ := buildDelegationTree(parent, children)

	byDelegation := map[string]tui.JobChild{}
	for _, c := range got {
		if _, dup := byDelegation[c.DelegationID]; dup {
			t.Fatalf("delegation %q rendered as a duplicate row: %+v", c.DelegationID, got)
		}
		byDelegation[c.DelegationID] = c
	}
	if len(got) != 2 {
		t.Fatalf("got %d delegation rows, want 2 (api collapsed, ui): %+v", len(got), got)
	}
	api := byDelegation["api"]
	// Latest attempt (the retry) wins, so the node shows succeeded, not the
	// stale failed first attempt.
	if api.State != "succeeded" || api.ID != "p/delegation/api/retry/1" {
		t.Fatalf("api node = (id=%q state=%q), want latest retry (p/delegation/api/retry/1, succeeded)", api.ID, api.State)
	}
	// ui depends on api; api's LATEST attempt succeeded, so deps are satisfied
	// (computed from the latest attempt, not the stale failed first attempt).
	ui := byDelegation["ui"]
	if !ui.DepsSatisfied {
		t.Fatalf("ui deps should be satisfied: api's latest attempt succeeded, got %+v", ui)
	}
}

func TestBuildDelegationTreeRetryFailureLeavesDepsPending(t *testing.T) {
	// The inverse: api's first attempt "succeeded" but a later retry attempt is
	// still running. DepsSatisfied must reflect the LATEST attempt (running ->
	// pending), not the stale first attempt, regardless of row ordering.
	parent := workflow.JobPayload{
		Result: &workflow.AgentResult{
			Delegations: []workflow.Delegation{
				{ID: "api", Agent: "coder", Action: "implement api"},
				{ID: "ui", Agent: "coder", Action: "implement ui", Deps: []string{"api"}},
			},
		},
	}
	mustPayload := func(p workflow.JobPayload) string {
		t.Helper()
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return string(raw)
	}
	children := []db.Job{
		{ID: "p/delegation/api", Agent: "coder", Type: "implement", State: "succeeded", ParentJobID: "p", DelegationID: "api", Payload: mustPayload(workflow.JobPayload{ParentJobID: "p", DelegationID: "api", RetryCount: 0})},
		{ID: "p/delegation/api/retry/1", Agent: "coder", Type: "implement", State: "running", ParentJobID: "p", DelegationID: "api", Payload: mustPayload(workflow.JobPayload{ParentJobID: "p", DelegationID: "api", RetryCount: 1})},
		{ID: "p/delegation/ui", Agent: "coder", Type: "implement", State: "queued", ParentJobID: "p", DelegationID: "ui", Payload: mustPayload(workflow.JobPayload{ParentJobID: "p", DelegationID: "ui"})},
	}

	got, _, _ := buildDelegationTree(parent, children)
	var ui tui.JobChild
	apiCount := 0
	for _, c := range got {
		switch c.DelegationID {
		case "api":
			apiCount++
		case "ui":
			ui = c
		}
	}
	if apiCount != 1 {
		t.Fatalf("api should collapse to one node, got %d", apiCount)
	}
	if ui.DepsSatisfied {
		t.Fatalf("ui deps should be PENDING: api's latest attempt is still running, got %+v", ui)
	}
}

func TestShouldLaunchTUI(t *testing.T) {
	cases := []struct {
		name       string
		flags      dashboardFlags
		stdoutTTY  bool
		stdinTTY   bool
		wantLaunch bool
	}{
		{"both ttys no flags", dashboardFlags{}, true, true, true},
		{"stdout not tty", dashboardFlags{}, false, true, false},
		{"stdin not tty", dashboardFlags{}, true, false, false},
		{"plain", dashboardFlags{plain: true}, true, true, false},
		{"json", dashboardFlags{jsonOutput: true}, true, true, false},
		{"all", dashboardFlags{all: true}, true, true, false},
		{"watch", dashboardFlags{watch: true}, true, true, false},
		{"answer", dashboardFlags{answerID: "p1"}, true, true, false},
		{"answer whitespace", dashboardFlags{answerID: "  "}, true, true, true},
		{"dismiss", dashboardFlags{dismissID: "p1"}, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldLaunchTUI(tc.flags, tc.stdoutTTY, tc.stdinTTY); got != tc.wantLaunch {
				t.Fatalf("shouldLaunchTUI(%+v, %v, %v) = %v, want %v", tc.flags, tc.stdoutTTY, tc.stdinTTY, got, tc.wantLaunch)
			}
		})
	}
}

// TestDashboardNonTTYStaysPlain guards the core compatibility promise: with a
// bytes.Buffer (never a terminal), the dashboard prints the one-shot snapshot
// and never launches the TUI, regardless of the new --plain flag default.
func TestDashboardNonTTYStaysPlain(t *testing.T) {
	home := dashboardTestHome(t)
	seedDashboardPrompt(t, home, "compat.prompt", "Choose", nil)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("dashboard exit = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"daemon:", "pending_prompts: 1", "needs attention:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected plain snapshot to contain %q:\n%s", want, out)
		}
	}
}

// TestToTUISnapshotCarriesPromptDetails verifies the snapshot exposes the full
// prompt records the TUI needs, sourced from the same store query.
func TestToTUISnapshotCarriesPromptDetails(t *testing.T) {
	home := dashboardTestHome(t)
	seedDashboardPrompt(t, home, "detail.prompt", "Pick one", []string{"a", "b"})

	paths, err := initializedPaths(home)
	if err != nil {
		t.Fatalf("initializedPaths: %v", err)
	}
	snap, err := buildDashboardSnapshot(home, paths)
	if err != nil {
		t.Fatalf("buildDashboardSnapshot: %v", err)
	}
	if len(snap.promptDetails) != 1 || snap.promptDetails[0].ID != "detail.prompt" {
		t.Fatalf("promptDetails = %+v", snap.promptDetails)
	}
	tuiSnap := toTUISnapshot(snap)
	if len(tuiSnap.Prompts) != 1 || tuiSnap.Prompts[0].ID != "detail.prompt" || len(tuiSnap.Prompts[0].Choices) != 2 {
		t.Fatalf("tui prompts = %+v", tuiSnap.Prompts)
	}
}

// TestDashboardSnapshotSurfacesDelegationPreflightFailure verifies the dashboard
// snapshot flags a coordinator whose delegation fan-out could not be routed
// (#451). Such a coordinator ends succeeded (it took a corrective continuation),
// so neither its state nor its overall-latest event reveals the zero-child
// fan-out — the snapshot must mark it PreflightFailed with the reason as the
// row's "why", and that must flow through to the TUI snapshot. Mirrors the
// `job list` PREFLIGHT_FAILED treatment, reusing the same store helper.
func TestDashboardSnapshotSurfacesDelegationPreflightFailure(t *testing.T) {
	home := dashboardTestHome(t)
	store := openCLIJobStore(t, home)
	seedCLIJob(t, store, db.Job{
		ID:      "coord-1",
		Agent:   "coordinator",
		Type:    "ask",
		State:   string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main"}),
	}, "succeeded")
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID:   "coord-1",
		Kind:    "delegation_preflight_failed",
		Message: `delegation "impl": "claude" is a runtime, not a registered agent`,
	}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	// A later continuation event becomes the overall-latest event, so a
	// latest-event-only surface would miss the preflight reason.
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID:   "coord-1",
		Kind:    "delegation_continuation_enqueued",
		Message: "preflight corrective continuation",
	}); err != nil {
		t.Fatalf("AddJobEvent returned error: %v", err)
	}
	store.Close()

	paths, err := initializedPaths(home)
	if err != nil {
		t.Fatalf("initializedPaths: %v", err)
	}
	snap, err := buildDashboardSnapshot(home, paths)
	if err != nil {
		t.Fatalf("buildDashboardSnapshot: %v", err)
	}

	var row *dashboardJobRow
	for i := range snap.jobRows {
		if snap.jobRows[i].ID == "coord-1" {
			row = &snap.jobRows[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("coord-1 not in jobRows: %+v", snap.jobRows)
	}
	if !row.PreflightFailed {
		t.Fatalf("coord-1 should be flagged PreflightFailed: %+v", row)
	}
	if !strings.Contains(row.LatestEvent, "is a runtime, not a registered agent") {
		t.Fatalf("preflight reason should be the row's why, got %q", row.LatestEvent)
	}
	if row.Repo != "owner/repo" {
		t.Fatalf("repo should be parsed for attention grouping, got %q", row.Repo)
	}

	tuiSnap := toTUISnapshot(snap)
	var found bool
	for _, jr := range tuiSnap.JobRows {
		if jr.ID == "coord-1" {
			found = true
			if !jr.PreflightFailed {
				t.Fatalf("tui JobRow should carry PreflightFailed: %+v", jr)
			}
			if !strings.Contains(jr.LatestEvent, "is a runtime, not a registered agent") {
				t.Fatalf("tui JobRow should carry the reason, got %q", jr.LatestEvent)
			}
		}
	}
	if !found {
		t.Fatalf("coord-1 missing from tui snapshot JobRows")
	}
}

// TestDashboardTUIDepsActions exercises the injected Answer/Dismiss closures end
// to end against a real store, the same APIs the model will call.
func TestDashboardTUIDepsActions(t *testing.T) {
	home := dashboardTestHome(t)
	seedDashboardPrompt(t, home, "act.answer", "Choose", []string{"keep", "drop"})
	seedDashboardPrompt(t, home, "act.dismiss", "Choose", nil)

	deps := dashboardTUIDeps(home, 0)
	if err := deps.Answer("act.answer", "keep"); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if err := deps.Dismiss("act.dismiss"); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}

	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	answered, err := store.GetInteractivePrompt(context.Background(), "act.answer")
	if err != nil {
		t.Fatalf("get answered: %v", err)
	}
	if answered.State != db.InteractivePromptStateResolved || answered.AnswerValue != "keep" || answered.AnswerSource != "dashboard-tui" {
		t.Fatalf("answered prompt = %+v", answered)
	}
	remaining, err := store.ListInteractivePrompts(context.Background(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != "act.answer" {
		t.Fatalf("dismiss should remove act.dismiss only: %+v", remaining)
	}
}

// TestDashboardCreateAgentWithPrompt exercises the custom-prompt create dep: it
// stores the prompt as a new template and registers the agent against it.
func TestDashboardCreateAgentWithPrompt(t *testing.T) {
	home := dashboardTestHome(t)
	deps := dashboardTUIDeps(home, 0)

	const content = "You are scout.\nReturn a gitmoot_result."
	if err := deps.CreateAgentWithPrompt("scout", "claude", content); err != nil {
		t.Fatalf("CreateAgentWithPrompt: %v", err)
	}

	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	agent, err := store.GetAgent(ctx, "scout")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if agent.Runtime != "claude" || agent.TemplateID != "scout" {
		t.Fatalf("agent = %+v, want runtime=claude template=scout", agent)
	}
	tpl, err := store.GetAgentTemplate(ctx, "scout")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if tpl.Content != content {
		t.Fatalf("template content = %q, want the edited prompt", tpl.Content)
	}
}

// TestDashboardTUIDeleteAgentsSkipsActive exercises the bulk-delete dep: it
// deletes the agents it can and skips any with a queued/running job.
func TestDashboardTUIDeleteAgentsSkipsActive(t *testing.T) {
	home := dashboardTestHome(t)
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	for _, n := range []string{"a1", "a2"} {
		if err := store.UpsertAgent(ctx, db.Agent{Name: n, Runtime: "codex"}); err != nil {
			t.Fatalf("upsert %s: %v", n, err)
		}
	}
	// a2 has a running job → must be skipped, not deleted.
	if err := store.CreateJob(ctx, db.Job{ID: "j1", Agent: "a2", Type: "ask", State: "running"}); err != nil {
		t.Fatalf("job: %v", err)
	}
	store.Close()

	deps := dashboardTUIDeps(home, 0)
	deleted, skipped, err := deps.DeleteAgents([]string{"a1", "a2"})
	if err != nil {
		t.Fatalf("DeleteAgents: %v", err)
	}
	if deleted != 1 || len(skipped) != 1 || skipped[0] != "a2" {
		t.Fatalf("deleted=%d skipped=%v, want 1 / [a2]", deleted, skipped)
	}

	store2, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	if _, err := store2.GetAgent(ctx, "a1"); err == nil {
		t.Fatalf("a1 should be deleted")
	}
	if _, err := store2.GetAgent(ctx, "a2"); err != nil {
		t.Fatalf("a2 should remain (had an active job): %v", err)
	}
}

func TestBuildDashboardActivity(t *testing.T) {
	mustPayload := func(p workflow.JobPayload) string {
		t.Helper()
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return string(raw)
	}
	jobs := []db.Job{
		// Active root coordinator (no RootJobID → its own id is the root); its
		// result names the two delegations.
		{ID: "root-1", Agent: "planner", Type: "implement", State: "running", UpdatedAt: "2026-01-02T00:00:00Z",
			Payload: mustPayload(workflow.JobPayload{Repo: "o/r", Result: &workflow.AgentResult{
				Delegations: []workflow.Delegation{{ID: "d1", Action: "build"}, {ID: "d2", Action: "test"}},
			}})},
		{ID: "root-1/delegation/d1", Agent: "impl-a", Type: "implement", State: "running", ParentJobID: "root-1", DelegationID: "d1",
			Payload: mustPayload(workflow.JobPayload{RootJobID: "root-1", ParentJobID: "root-1", DelegationID: "d1"})},
		{ID: "root-1/delegation/d2", Agent: "impl-b", Type: "implement", State: "succeeded", ParentJobID: "root-1", DelegationID: "d2",
			Payload: mustPayload(workflow.JobPayload{RootJobID: "root-1", ParentJobID: "root-1", DelegationID: "d2"})},
		// A fully-settled tree (no active member) must be excluded.
		{ID: "root-2", Agent: "planner", Type: "ask", State: "succeeded", UpdatedAt: "2026-01-01T00:00:00Z",
			Payload: mustPayload(workflow.JobPayload{Repo: "o/x"})},
	}
	roots := buildDashboardActivity(jobs)
	if len(roots) != 1 {
		t.Fatalf("expected 1 active root, got %d: %+v", len(roots), roots)
	}
	r := roots[0]
	if r.JobID != "root-1" || r.Repo != "o/r" || r.State != "running" {
		t.Fatalf("root fields wrong: %+v", r)
	}
	if r.Total != 2 || r.Running != 1 || r.Blocked != 0 || r.Done != 1 {
		t.Fatalf("progress counts wrong: total=%d running=%d blocked=%d done=%d", r.Total, r.Running, r.Blocked, r.Done)
	}
	if len(r.Children) != 2 {
		t.Fatalf("expected 2 delegation children, got %d", len(r.Children))
	}
}

// TestBuildDashboardActivityScopesActiveToDirectTree guards the consistency
// invariant: a root's activeness is decided over the same scope the page renders
// (root + direct children + continuation), so a surfaced tree always has visible
// live work. A root whose only live work is a deeper grandchild (its direct
// children all settled) is NOT surfaced — it would otherwise render a settled
// coordinator with "0 running", which is what the page promises never to show.
func TestBuildDashboardActivityScopesActiveToDirectTree(t *testing.T) {
	mustPayload := func(p workflow.JobPayload) string {
		t.Helper()
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return string(raw)
	}
	jobs := []db.Job{
		// Tree A: a live continuation (a direct child) keeps the root surfaced even
		// though the delegation child itself settled.
		{ID: "a-root", Agent: "planner", Type: "implement", State: "succeeded", UpdatedAt: "2026-01-02T00:00:00Z",
			Payload: mustPayload(workflow.JobPayload{Result: &workflow.AgentResult{
				Delegations: []workflow.Delegation{{ID: "d1", Action: "build"}},
			}})},
		{ID: "a-root/delegation/d1", Agent: "impl", Type: "implement", State: "succeeded", ParentJobID: "a-root", DelegationID: "d1",
			Payload: mustPayload(workflow.JobPayload{RootJobID: "a-root", ParentJobID: "a-root", DelegationID: "d1"})},
		{ID: "a-root/continuation", Agent: "planner", Type: "ask", State: "running", ParentJobID: "a-root",
			Payload: mustPayload(workflow.JobPayload{RootJobID: "a-root", ParentJobID: "a-root"})},

		// Tree B: root + direct child both settled, but a grandchild (child of the
		// sub-coordinator b-sub) is still running. The grandchild is not a direct
		// child of b-root, so b-root must NOT be surfaced as live work.
		{ID: "b-root", Agent: "planner", Type: "implement", State: "succeeded", UpdatedAt: "2026-01-03T00:00:00Z",
			Payload: mustPayload(workflow.JobPayload{Result: &workflow.AgentResult{
				Delegations: []workflow.Delegation{{ID: "s1", Action: "sub"}},
			}})},
		{ID: "b-sub", Agent: "coord", Type: "implement", State: "succeeded", ParentJobID: "b-root", DelegationID: "s1",
			Payload: mustPayload(workflow.JobPayload{RootJobID: "b-root", ParentJobID: "b-root", DelegationID: "s1"})},
		{ID: "b-grandchild", Agent: "impl", Type: "implement", State: "running", ParentJobID: "b-sub", DelegationID: "g1",
			Payload: mustPayload(workflow.JobPayload{RootJobID: "b-root", ParentJobID: "b-sub", DelegationID: "g1"})},
	}
	roots := buildDashboardActivity(jobs)
	if len(roots) != 1 {
		t.Fatalf("expected only the continuation-live root, got %d: %+v", len(roots), roots)
	}
	r := roots[0]
	if r.JobID != "a-root" {
		t.Fatalf("surfaced root = %q, want a-root (b-root has no live direct child)", r.JobID)
	}
	if r.ContinuationID != "a-root/continuation" || r.ContinuationState != "running" {
		t.Fatalf("continuation = (%q,%q), want (a-root/continuation, running)", r.ContinuationID, r.ContinuationState)
	}
}

// TestBuildDashboardActivitySortsByTreeWideActivity guards that roots sort by the
// freshest update anywhere in the tree, not the coordinator's own (frozen)
// timestamp: a coordinator that settled earlier but has a child running right now
// must rank above one that settled later with only stale children.
func TestBuildDashboardActivitySortsByTreeWideActivity(t *testing.T) {
	mustPayload := func(p workflow.JobPayload) string {
		t.Helper()
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return string(raw)
	}
	jobs := []db.Job{
		// coord-A settled at 09:00 but its child is running and was updated at 14:00.
		{ID: "coord-A", Agent: "planner", Type: "implement", State: "succeeded", UpdatedAt: "2026-01-01T09:00:00Z",
			Payload: mustPayload(workflow.JobPayload{Result: &workflow.AgentResult{
				Delegations: []workflow.Delegation{{ID: "a1", Action: "x"}},
			}})},
		{ID: "coord-A/delegation/a1", Agent: "impl", Type: "implement", State: "running", UpdatedAt: "2026-01-01T14:00:00Z", ParentJobID: "coord-A", DelegationID: "a1",
			Payload: mustPayload(workflow.JobPayload{ParentJobID: "coord-A", DelegationID: "a1"})},
		// coord-B settled later (10:00) but its child only sits queued (10:30).
		{ID: "coord-B", Agent: "planner", Type: "implement", State: "succeeded", UpdatedAt: "2026-01-01T10:00:00Z",
			Payload: mustPayload(workflow.JobPayload{Result: &workflow.AgentResult{
				Delegations: []workflow.Delegation{{ID: "b1", Action: "y"}},
			}})},
		{ID: "coord-B/delegation/b1", Agent: "impl", Type: "implement", State: "queued", UpdatedAt: "2026-01-01T10:30:00Z", ParentJobID: "coord-B", DelegationID: "b1",
			Payload: mustPayload(workflow.JobPayload{ParentJobID: "coord-B", DelegationID: "b1"})},
	}
	roots := buildDashboardActivity(jobs)
	if len(roots) != 2 {
		t.Fatalf("want 2 active roots, got %d: %+v", len(roots), roots)
	}
	if roots[0].JobID != "coord-A" {
		t.Fatalf("want coord-A first (freshest activity is its running child), got %q then %q", roots[0].JobID, roots[1].JobID)
	}
}
