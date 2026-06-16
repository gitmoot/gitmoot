package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
