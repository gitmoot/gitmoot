package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func agentsSnapshot() Snapshot {
	return Snapshot{
		Daemon: Daemon{Running: true},
		Agents: []Agent{
			{Name: "planner", Runtime: "claude", Role: "plan", Health: "ok", TemplateID: "planner-tpl"},
			{Name: "reviewer", Runtime: "codex", Health: "ok", TemplateID: "reviewer-tpl"},
		},
		JobRows: []JobRow{
			{ID: "job-1", Agent: "planner", Type: "ask", State: "succeeded"},
			{ID: "job-2", Agent: "reviewer", Type: "review", State: "failed"},
		},
	}
}

func agentsModel(t *testing.T, deps Deps, snap Snapshot) Model {
	t.Helper()
	if deps.Load == nil {
		deps.Load = func() (Snapshot, error) { return snap, nil }
	}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	for i := 0; i < 2; i++ { // Attention → Trains → Agents
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(Model)
	}
	if pages[m.selected].page != pageAgents {
		t.Fatalf("expected Agents page, got %v", pages[m.selected].page)
	}
	return m
}

func TestAgentDetailRendersVersionsAndJobs(t *testing.T) {
	var asked string
	deps := Deps{TemplateVersions: func(templateID string) ([]TemplateVersion, error) {
		asked = templateID
		return []TemplateVersion{
			{ID: "v2-id", Number: 2, State: "current", Name: "improved"},
			{ID: "v1-id", Number: 1, State: "superseded", Name: "initial"},
		}, nil
	}}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.mode != modeAgentDetail || cmd == nil {
		t.Fatalf("enter should open the agent detail, mode=%v", m.mode)
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if asked != "planner-tpl" {
		t.Fatalf("TemplateVersions asked for %q", asked)
	}
	view := m.View()
	for _, want := range []string{"agent planner", "planner-tpl", "v2", "improved", "v1", "initial", "job-1"} {
		if !strings.Contains(view, want) {
			t.Fatalf("detail missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "job-2") {
		t.Fatalf("detail must only list the agent's own jobs:\n%s", view)
	}
}

// openAgentVersionDetail opens the agent detail and loads its versions, leaving
// the model on modeAgentDetail with the cursor on the first (current) version.
func openAgentVersionDetail(t *testing.T, deps Deps) Model {
	t.Helper()
	if deps.TemplateVersions == nil {
		deps.TemplateVersions = func(string) ([]TemplateVersion, error) {
			return []TemplateVersion{
				{ID: "v2-id", Number: 2, State: "current", Name: "improved"},
				{ID: "v1-id", Number: 1, State: "superseded", Name: "initial"},
			}, nil
		}
	}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd != nil {
		next, _ = m.Update(cmd())
		m = next.(Model)
	}
	if m.mode != modeAgentDetail {
		t.Fatalf("expected modeAgentDetail, got %v", m.mode)
	}
	return m
}

func TestAgentVersionPreviewLoadsAndRenders(t *testing.T) {
	var asked string
	deps := Deps{TemplateVersionContent: func(versionID string) (string, error) {
		asked = versionID
		return "You are the planner.\nThink step by step.", nil
	}}
	m := openAgentVersionDetail(t, deps)
	// enter opens the version under the cursor (the current one, v2).
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.mode != modeAgentVersionView || cmd == nil {
		t.Fatalf("enter should open the version pager, mode=%v", m.mode)
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if asked != "v2-id" {
		t.Fatalf("TemplateVersionContent asked for %q, want v2-id", asked)
	}
	view := m.View()
	for _, want := range []string{"version v2", "planner-tpl", "You are the planner."} {
		if !strings.Contains(view, want) {
			t.Fatalf("version view missing %q:\n%s", want, view)
		}
	}
	// esc returns to the detail.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.mode != modeAgentDetail {
		t.Fatalf("esc should return to the detail, got %v", m.mode)
	}
}

func TestAgentVersionPreviewEmptyContent(t *testing.T) {
	deps := Deps{TemplateVersionContent: func(string) (string, error) { return "   \n", nil }}
	m := openAgentVersionDetail(t, deps)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if !strings.Contains(m.View(), "no content") {
		t.Fatalf("blank version should show a no-content state:\n%s", m.View())
	}
}

func TestAgentVersionPreviewCachesPerVersion(t *testing.T) {
	calls := map[string]int{}
	deps := Deps{TemplateVersionContent: func(versionID string) (string, error) {
		calls[versionID]++
		return "content of " + versionID, nil
	}}
	m := openAgentVersionDetail(t, deps)
	// Open v2 (cursor 0), load, back out.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	// Re-open the same version: cache hit, no command, no second fetch.
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd != nil {
		t.Fatal("re-opening the same version should hit the cache (no command)")
	}
	// Move the cursor to v1 and open: a different id must fetch.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	next, _ = m.Update(key("j"))
	m = next.(Model)
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("a different version should fetch its content")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if calls["v2-id"] != 1 || calls["v1-id"] != 1 {
		t.Fatalf("expected one fetch per version, got %v", calls)
	}
}

func TestAgentCreateFlowRunsCreateAgent(t *testing.T) {
	var created []string
	form := NewAgentCreateForm(newFakeStore(), []Choice{{Value: "planner-tpl", Label: "planner"}})
	deps := Deps{
		OpenAgentCreate: func() (tea.Model, error) { return form, nil },
		CreateAgent: func(name, runtime, template string) error {
			created = []string{name, runtime, template}
			return nil
		},
	}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(key("n"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("n should push the create form")
	}
	if _, ok := cmd().(PushModelMsg); !ok {
		t.Fatal("n should produce a PushModelMsg")
	}
	// The popped form delivers its answers; the dashboard runs the create.
	next, cmd = m.Update(agentFormResultMsg{result: Result{Values: map[string]string{
		"name": "scout", "runtime": "claude", "template": "planner-tpl",
	}}})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("a completed form should trigger the create")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if len(created) != 3 || created[0] != "scout" || created[1] != "claude" || created[2] != "planner-tpl" {
		t.Fatalf("CreateAgent called with %v", created)
	}
}

func TestAgentCreateAbortedFormDoesNothing(t *testing.T) {
	deps := Deps{CreateAgent: func(name, runtime, template string) error {
		t.Fatal("aborted form must not create")
		return nil
	}}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(agentFormResultMsg{result: Result{Aborted: true}})
	m = next.(Model)
	if cmd != nil {
		// Drain: any returned cmd must not be a create.
		if msg := cmd(); msg != nil {
			if _, ok := msg.(agentActionMsg); ok {
				t.Fatal("aborted form must not fire agentActionMsg")
			}
		}
	}
	_ = m
}

func TestAgentCreateErrorRendersInline(t *testing.T) {
	deps := Deps{CreateAgent: func(name, runtime, template string) error {
		return errors.New("agent name already registered")
	}}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(agentFormResultMsg{result: Result{Values: map[string]string{"name": "planner"}}})
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if !strings.Contains(m.View(), "agent name already registered") {
		t.Fatalf("create error should render on the Agents page:\n%s", m.View())
	}
}

func TestAgentDeleteGuardKeepsConfirmOpen(t *testing.T) {
	deps := Deps{DeleteAgent: func(name string) error {
		return errors.New("agent planner has queued or running jobs")
	}}
	m := agentsModel(t, deps, agentsSnapshot())
	next, _ := m.Update(key("D"))
	m = next.(Model)
	if m.mode != modeConfirmAgentDelete {
		t.Fatalf("D should open the delete confirm, mode=%v", m.mode)
	}
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeConfirmAgentDelete {
		t.Fatalf("refusal should keep the confirm open, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "queued or running jobs") {
		t.Fatalf("refusal should render:\n%s", m.View())
	}
}

func TestAgentDeleteSuccess(t *testing.T) {
	var deleted string
	deps := Deps{DeleteAgent: func(name string) error { deleted = name; return nil }}
	m := agentsModel(t, deps, agentsSnapshot())
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown}) // → reviewer
	m = next.(Model)
	next, _ = m.Update(key("D"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if deleted != "reviewer" {
		t.Fatalf("DeleteAgent called with %q", deleted)
	}
	if m.mode != modeNormal {
		t.Fatalf("success should close the confirm, mode=%v", m.mode)
	}
}

func TestAgentRevertPicksSupersededVersion(t *testing.T) {
	var gotTemplate, gotVersion string
	deps := Deps{
		TemplateVersions: func(templateID string) ([]TemplateVersion, error) {
			return []TemplateVersion{
				{ID: "v3-id", Number: 3, State: "current"},
				{ID: "v2-id", Number: 2, State: "superseded"},
				{ID: "v1-id", Number: 1, State: "superseded"},
			}, nil
		},
		RevertTemplate: func(templateID, versionID string) error {
			gotTemplate, gotVersion = templateID, versionID
			return nil
		},
	}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	next, _ = m.Update(cmd()) // versions load
	m = next.(Model)
	next, _ = m.Update(key("v"))
	m = next.(Model)
	if m.mode != modeAgentRevertPick {
		t.Fatalf("v should open the revert pick, mode=%v", m.mode)
	}
	// Pick the second superseded version (v1).
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.mode != modeConfirmAgentRevert {
		t.Fatalf("enter should confirm the pick, mode=%v", m.mode)
	}
	next, cmd = m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if gotTemplate != "planner-tpl" || gotVersion != "v1-id" {
		t.Fatalf("RevertTemplate called with (%q, %q)", gotTemplate, gotVersion)
	}
	if m.mode != modeAgentDetail {
		t.Fatalf("revert success should return to the detail, mode=%v", m.mode)
	}
}

func TestAgentRevertUnavailableWithoutSuperseded(t *testing.T) {
	deps := Deps{TemplateVersions: func(templateID string) ([]TemplateVersion, error) {
		return []TemplateVersion{{ID: "v1-id", Number: 1, State: "current"}}, nil
	}}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	next, _ = m.Update(key("v"))
	m = next.(Model)
	if m.mode != modeAgentDetail {
		t.Fatalf("v with no superseded versions must be a no-op, mode=%v", m.mode)
	}
}

func TestAgentOptimizeFlowOpensPhaseView(t *testing.T) {
	var startedTemplate string
	var startedValues map[string]string
	pushedTrain := ""
	form := NewAgentOptimizeForm(newFakeStore(), "planner-tpl", []Field{{Name: "name", Kind: FieldText}}, nil, nil)
	deps := Deps{
		OpenAgentOptimize: func(agent Agent) (tea.Model, error) { return form, nil },
		StartOptimize: func(templateID string, values map[string]string) (string, error) {
			startedTemplate = templateID
			startedValues = values
			return "train-planner-1", nil
		},
		OpenTrain: func(sessionID string) tea.Model {
			pushedTrain = sessionID
			return New(Deps{})
		},
	}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(key("o"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("o should build the optimize form")
	}
	if _, ok := cmd().(PushModelMsg); !ok {
		t.Fatalf("o should push the form, got %T", cmd())
	}
	// The popped form delivers its answers; the dashboard starts the session.
	next, cmd = m.Update(agentOptimizeFormResultMsg{templateID: "planner-tpl", result: Result{Values: map[string]string{
		"name": "opt-planner", "backend": "claude", "model": "claude-opus-4-8",
	}}})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("a completed optimize form should start the session")
	}
	if !strings.Contains(m.View(), "starting optimization…") {
		t.Fatalf("the in-flight state should render:\n%s", m.View())
	}
	started := cmd()
	if startedTemplate != "planner-tpl" || startedValues["backend"] != "claude" {
		t.Fatalf("StartOptimize called with (%q, %v)", startedTemplate, startedValues)
	}
	// Success pushes the new session's phase view.
	next, cmd = m.Update(started)
	m = next.(Model)
	if cmd == nil {
		t.Fatal("a started session should open its phase view")
	}
	var sawPush bool
	var walk func(tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				walk(sub)
			}
			return
		}
		if _, ok := msg.(PushModelMsg); ok {
			sawPush = true
		}
	}
	walk(cmd)
	if !sawPush || pushedTrain != "train-planner-1" {
		t.Fatalf("expected the phase view push for train-planner-1; push=%v opened=%q", sawPush, pushedTrain)
	}
	if m.optimizeBusy {
		t.Fatal("the in-flight flag should clear once the session started")
	}
}

func TestAgentOptimizeErrorRendersInline(t *testing.T) {
	deps := Deps{
		StartOptimize: func(templateID string, values map[string]string) (string, error) {
			return "", errors.New("train start failed: no items")
		},
	}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(agentOptimizeFormResultMsg{templateID: "planner-tpl", result: Result{Values: map[string]string{"name": "x"}}})
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if !strings.Contains(m.View(), "train start failed: no items") {
		t.Fatalf("the start error should render on the Agents page:\n%s", m.View())
	}
}

func TestAgentOptimizeAbortedFormDoesNothing(t *testing.T) {
	deps := Deps{
		StartOptimize: func(templateID string, values map[string]string) (string, error) {
			t.Fatal("aborted form must not start a session")
			return "", nil
		},
	}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(agentOptimizeFormResultMsg{templateID: "planner-tpl", result: Result{Aborted: true}})
	_ = next
	if cmd != nil {
		t.Fatal("an aborted optimize form must produce no commands")
	}
}

func TestAgentCreateFormCompletionPopsWithResult(t *testing.T) {
	form := NewAgentCreateForm(newFakeStore(), []Choice{{Value: "planner-tpl", Label: "planner"}})
	var model tea.Model = form
	step := func(msg tea.Msg) tea.Cmd {
		next, cmd := model.Update(msg)
		model = next
		return cmd
	}
	step(initMsg{})
	// name
	for _, r := range "scout" {
		step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	step(tea.KeyMsg{Type: tea.KeyEnter})
	// runtime: codex default → enter
	step(tea.KeyMsg{Type: tea.KeyEnter})
	// template: first choice → enter
	step(tea.KeyMsg{Type: tea.KeyEnter})
	// confirm
	cmd := step(key("y"))
	if cmd == nil {
		t.Fatal("confirm should produce the Done command")
	}
	pop, ok := cmd().(PopModelMsg)
	if !ok {
		t.Fatalf("Done must pop the form, got %T", cmd())
	}
	result, ok := pop.Deliver.(agentFormResultMsg)
	if !ok {
		t.Fatalf("the pop must deliver the form result, got %T", pop.Deliver)
	}
	if result.result.Aborted {
		t.Fatal("completed form must not be aborted")
	}
	values := result.result.Values
	if values["name"] != "scout" || values["runtime"] != "codex" || values["template"] != "planner-tpl" {
		t.Fatalf("form values = %v", values)
	}
}

func TestAgentCreateFormExternalFinishPopsNotQuits(t *testing.T) {
	form := NewAgentCreateForm(newFakeStore(), []Choice{{Value: "planner-tpl", Label: "planner"}})
	var model tea.Model = form
	step := func(msg tea.Msg) tea.Cmd {
		next, cmd := model.Update(msg)
		model = next
		return cmd
	}
	step(initMsg{})
	for _, r := range "scout" {
		step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	step(tea.KeyMsg{Type: tea.KeyEnter}) // name
	step(tea.KeyMsg{Type: tea.KeyEnter}) // runtime
	// As if an agent answered a field externally: the final commit then
	// auto-accepts instead of showing the confirm — and must pop, not quit
	// the whole dashboard.
	ti := model.(TrainInitModel)
	ti.external = true
	model = ti
	cmd := step(tea.KeyMsg{Type: tea.KeyEnter}) // template → final commit
	if cmd == nil {
		t.Fatal("the final commit should produce commands")
	}
	var sawPop, sawQuit bool
	var walk func(tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		msg := c()
		switch msg := msg.(type) {
		case tea.BatchMsg:
			for _, sub := range msg {
				walk(sub)
			}
		case PopModelMsg:
			sawPop = true
			if result, ok := msg.Deliver.(agentFormResultMsg); !ok || result.result.Aborted {
				t.Fatalf("external finish must deliver a non-aborted result, got %+v", msg.Deliver)
			}
		case tea.QuitMsg:
			sawQuit = true
		}
	}
	walk(cmd)
	if !sawPop || sawQuit {
		t.Fatalf("external finish must pop (got pop=%v) and never quit (got quit=%v)", sawPop, sawQuit)
	}
}

func TestAgentCreateFormAbortPopsAborted(t *testing.T) {
	form := NewAgentCreateForm(newFakeStore(), []Choice{{Value: "planner-tpl", Label: "planner"}})
	next, _ := form.Update(initMsg{})
	next, cmd := next.Update(tea.KeyMsg{Type: tea.KeyEsc})
	_ = next
	if cmd == nil {
		t.Fatal("esc should produce the abort command")
	}
	// abort batches the Done cmd with prompt cleanup; find the pop.
	var pop *PopModelMsg
	var walk func(tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				walk(sub)
			}
			return
		}
		if p, ok := msg.(PopModelMsg); ok {
			pop = &p
		}
	}
	walk(cmd)
	if pop == nil {
		t.Fatal("abort must pop the form")
	}
	result, ok := pop.Deliver.(agentFormResultMsg)
	if !ok || !result.result.Aborted {
		t.Fatalf("abort must deliver an aborted result, got %+v", pop.Deliver)
	}
}
