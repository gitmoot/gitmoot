package tui

import (
	"errors"
	"strconv"
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
	m = tabToPage(t, m, pageAgents)
	return m
}

// TestAgentGroupDeleteFlow exercises X → confirm → y: it bulk-deletes the cursor
// agent's whole template group, surfaces the active-job skip heads-up, calls
// DeleteAgents with the group's names, and reports the deleted/skipped result.
func TestAgentGroupDeleteFlow(t *testing.T) {
	snap := agentsSnapshot()
	snap.Agents = append(snap.Agents, Agent{Name: "planner-2", Runtime: "claude", TemplateID: "planner-tpl"})
	snap.JobRows = append(snap.JobRows, JobRow{ID: "jx", Agent: "planner", Type: "ask", State: "running"})
	var got []string
	deps := Deps{DeleteAgents: func(names []string) (int, []string, error) {
		got = names
		return len(names) - 1, []string{"planner"}, nil // planner skipped (active job)
	}}
	m := agentsModel(t, deps, snap)
	// Cursor 0 = planner (planner-tpl group). X opens the group-delete confirm.
	next, _ := m.Update(key("X"))
	m = next.(Model)
	if m.mode != modeConfirmAgentGroupDelete {
		t.Fatalf("X should open the group-delete confirm, mode=%v", m.mode)
	}
	view := m.View()
	for _, want := range []string{"planner-tpl", "planner-2", "have active jobs"} {
		if !strings.Contains(view, want) {
			t.Fatalf("confirm missing %q:\n%s", want, view)
		}
	}
	// y runs the bulk delete with the whole group's names.
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("y should dispatch the bulk delete")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if len(got) != 2 || got[0] != "planner" || got[1] != "planner-2" {
		t.Fatalf("DeleteAgents called with %v, want [planner planner-2]", got)
	}
	if m.mode != modeNormal {
		t.Fatalf("success should close the confirm, mode=%v", m.mode)
	}
	if !strings.Contains(m.agentNotice, "deleted 1") || !strings.Contains(m.agentNotice, "1 skipped") {
		t.Fatalf("notice should report deleted/skipped: %q", m.agentNotice)
	}
}

// TestAgentGroupDeleteStandaloneFallsBackToSingle guards the safety rule: X on a
// template-less agent opens the single-agent delete (not a bulk delete of the
// whole heterogeneous "Standalone agents" catch-all).
func TestAgentGroupDeleteStandaloneFallsBackToSingle(t *testing.T) {
	snap := agentsSnapshot()
	snap.Agents = append(snap.Agents, Agent{Name: "drifter", Runtime: "codex"}) // TemplateID ""
	called := false
	deps := Deps{DeleteAgents: func(names []string) (int, []string, error) { called = true; return 0, nil, nil }}
	m := agentsModel(t, deps, snap)
	for i := 0; i < 6; i++ {
		if a, ok := m.agentUnderCursor(); ok && a.Name == "drifter" {
			break
		}
		next, _ := m.Update(key("j"))
		m = next.(Model)
	}
	next, _ := m.Update(key("X"))
	m = next.(Model)
	if m.mode != modeConfirmAgentDelete {
		t.Fatalf("X on a standalone agent should open single delete, mode=%v", m.mode)
	}
	if called {
		t.Fatal("standalone X must not bulk-delete the catch-all group")
	}
}

// TestAgentGroupDeletePartialErrorReported guards that a partial-error bulk delete
// still closes the overlay, reports the committed deletes, and surfaces the error
// (so the stale list/retry-wedge can't happen).
func TestAgentGroupDeletePartialErrorReported(t *testing.T) {
	snap := agentsSnapshot()
	snap.Agents = append(snap.Agents, Agent{Name: "planner-2", Runtime: "claude", TemplateID: "planner-tpl"})
	deps := Deps{DeleteAgents: func(names []string) (int, []string, error) {
		return 1, nil, errors.New("db locked") // one committed, then failed
	}}
	m := agentsModel(t, deps, snap)
	next, _ := m.Update(key("X"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("a partial-error delete should still close the overlay, mode=%v", m.mode)
	}
	if !strings.Contains(m.agentNotice, "deleted 1") {
		t.Fatalf("should still report the committed deletes: %q", m.agentNotice)
	}
	if !strings.Contains(m.agentErr, "db locked") {
		t.Fatalf("should surface the error: %q", m.agentErr)
	}
}

// TestAgentGroupDeleteNoDepInert verifies X is inert without a DeleteAgents dep.
func TestAgentGroupDeleteNoDepInert(t *testing.T) {
	m := agentsModel(t, Deps{}, agentsSnapshot())
	next, _ := m.Update(key("X"))
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("X without a DeleteAgents dep should be a no-op, mode=%v", m.mode)
	}
}

// TestAgentsListWindowsLongList guards that a long agent list (e.g. with 'a'
// showing many training agents) windows around the cursor so the selection stays
// visible and the scroll markers appear — rather than overflowing unscrollably.
func TestAgentsListWindowsLongList(t *testing.T) {
	snap := Snapshot{Daemon: Daemon{Running: true}}
	for i := 0; i < 80; i++ {
		snap.Agents = append(snap.Agents, Agent{
			Name: "agent-" + strconv.Itoa(i), Runtime: "codex", Role: "impl", Health: "ok", TemplateID: "tpl",
		})
	}
	m := sizedModel(Deps{Load: func() (Snapshot, error) { return snap, nil }})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	m = next.(Model)
	next, _ = m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	m = tabToPage(t, m, pageAgents)

	cap := agentsWindowCap(m.height)
	if cap >= 80 {
		t.Fatalf("test needs a window smaller than the list; cap=%d", cap)
	}
	// At the top: a "below" marker, no "above", and the far end hidden.
	view := m.View()
	if !strings.Contains(view, "more below") || strings.Contains(view, "more above") {
		t.Fatalf("top of a long list should show only a 'below' marker:\n%s", view)
	}
	if strings.Contains(view, "agent-79 ") {
		t.Fatalf("the far end should not render while the cursor is at the top:\n%s", view)
	}
	// Drive the cursor to the last agent; it must stay visible (window follows).
	for i := 0; i < 79; i++ {
		next, _ := m.Update(key("j"))
		m = next.(Model)
	}
	view = m.View()
	if !strings.Contains(view, "agent-79") {
		t.Fatalf("the selected last agent must be visible after scrolling:\n%s", view)
	}
	if !strings.Contains(view, "more above") {
		t.Fatalf("the bottom of a long list should show an 'above' marker:\n%s", view)
	}
}

// TestAgentsHiddenLineShowsLiveCount verifies the hidden-agents line annotates how
// many live sessions belong to hidden training agents (so the LIVE column isn't
// silently empty when all live work is under hidden agents).
func TestAgentsHiddenLineShowsLiveCount(t *testing.T) {
	snap := agentsSnapshot()
	snap.Agents = append(snap.Agents, Agent{Name: "skillopt-generator", Runtime: "codex"})
	snap.Sessions = []Session{
		{Name: "skillopt-generator-bg-1", Type: "skillopt-generator", Runtime: "codex", State: "running"},
		{Name: "skillopt-generator-bg-2", Type: "skillopt-generator", Runtime: "codex", State: "idle"},
	}
	m := agentsModel(t, Deps{}, snap)
	view := m.View()
	if !strings.Contains(view, "1 training agents hidden") {
		t.Fatalf("expected a hidden count:\n%s", view)
	}
	if !strings.Contains(view, "2 live") || !strings.Contains(view, "1 running") {
		t.Fatalf("hidden line should annotate live/running session counts:\n%s", view)
	}
}

// TestAgentsShowAllToggle verifies the 'a' key un-hides the training agents (and
// hides them again), and that their LIVE counts then appear.
func TestAgentsShowAllToggle(t *testing.T) {
	snap := agentsSnapshot()
	snap.Agents = append(snap.Agents, Agent{Name: "skillopt-generator", Runtime: "codex"})
	snap.Sessions = []Session{
		{Name: "skillopt-generator-bg-1", Type: "skillopt-generator", Runtime: "codex", State: "running"},
	}
	m := agentsModel(t, Deps{}, snap)
	if strings.Contains(m.View(), "skillopt-generator") {
		t.Fatalf("training agent should be hidden by default:\n%s", m.View())
	}
	// 'a' shows all.
	next, _ := m.Update(key("a"))
	m = next.(Model)
	view := m.View()
	if !strings.Contains(view, "skillopt-generator") {
		t.Fatalf("'a' should reveal the hidden training agent:\n%s", view)
	}
	if !strings.Contains(view, "showing all agents") {
		t.Fatalf("show-all mode should be indicated:\n%s", view)
	}
	// 'a' again hides them.
	next, _ = m.Update(key("a"))
	m = next.(Model)
	if strings.Contains(m.View(), "skillopt-generator") {
		t.Fatalf("'a' again should re-hide the training agent:\n%s", m.View())
	}
}

func TestAgentsPageHidesTrainingAgents(t *testing.T) {
	snap := agentsSnapshot()
	// Add internal training plumbing alongside the real agents.
	snap.Agents = append(snap.Agents,
		Agent{Name: "skillopt-target-train-s1-item-001-generator-abc", Runtime: "codex"},
		Agent{Name: "skillopt-target-train-s1-item-002-generator-def", Runtime: "codex"},
		Agent{Name: "skillopt-generator-bg-18b5", Runtime: "codex"},
	)
	deps := Deps{}
	m := agentsModel(t, deps, snap)
	view := m.View()
	// Real agents shown under their template-group headers, training plumbing
	// hidden + a count line.
	for _, want := range []string{"planner", "reviewer", "planner-tpl", "reviewer-tpl", "3 training agents hidden"} {
		if !strings.Contains(view, want) {
			t.Fatalf("agents view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "skillopt-target-") || strings.Contains(view, "skillopt-generator-bg") {
		t.Fatalf("training agents must be hidden:\n%s", view)
	}
	// The hidden training agents have empty TemplateID, but they are filtered
	// before grouping, so they must not surface a "Standalone agents (custom prompt)" header.
	if strings.Contains(view, "Standalone agents (custom prompt)") {
		t.Fatalf("hidden agents must not introduce a Standalone agents (custom prompt) group:\n%s", view)
	}
	// The cursor + enter act on the VISIBLE list (planner is first visible).
	if a, ok := m.agentUnderCursor(); !ok || a.Name != "planner" {
		t.Fatalf("cursor should target the first visible agent, got %q ok=%v", a.Name, ok)
	}
	// Moving down lands on the next visible agent, never the hidden ones.
	next, _ := m.Update(key("j"))
	m = next.(Model)
	if a, _ := m.agentUnderCursor(); a.Name != "reviewer" {
		t.Fatalf("down should move to the next visible agent, got %q", a.Name)
	}
}

func TestAgentsPageGroupsByTemplate(t *testing.T) {
	snap := agentsSnapshot()
	// A second agent on planner-tpl (stays in the planner-tpl group) and one
	// with no template (its own "Standalone agents (custom prompt)" group).
	snap.Agents = append(snap.Agents,
		Agent{Name: "scout", Runtime: "claude", TemplateID: "planner-tpl"},
		Agent{Name: "drifter", Runtime: "codex"},
	)
	m := agentsModel(t, Deps{}, snap)
	view := m.View()
	// Both template headers and the no-template header render.
	for _, want := range []string{"planner-tpl", "reviewer-tpl", "Standalone agents (custom prompt)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("grouped view missing header %q:\n%s", want, view)
		}
	}
	// Headers appear in first-appearance order: planner-tpl, then reviewer-tpl,
	// then the Standalone agents (custom prompt) group last.
	pIdx := strings.Index(view, "planner-tpl")
	rIdx := strings.Index(view, "reviewer-tpl")
	nIdx := strings.Index(view, "Standalone agents (custom prompt)")
	if !(pIdx < rIdx && rIdx < nIdx) {
		t.Fatalf("group headers out of order (planner=%d reviewer=%d none=%d):\n%s", pIdx, rIdx, nIdx, view)
	}
	// The Standalone agents (custom prompt) header precedes its sole member.
	if nIdx > strings.Index(view, "drifter") {
		t.Fatalf("the Standalone agents (custom prompt) header should precede its agent:\n%s", view)
	}
	// Cursor follows the on-screen (grouped) order, so ↑/↓ step through rows in
	// the order they render: the planner-tpl group (planner, scout) first, then
	// reviewer-tpl (reviewer), then the Standalone agents (custom prompt) group (drifter).
	wants := []string{"planner", "scout", "reviewer", "drifter"}
	for i, want := range wants {
		a, ok := m.agentUnderCursor()
		if !ok || a.Name != want {
			t.Fatalf("cursor %d should select %q, got %q ok=%v", i, want, a.Name, ok)
		}
		if i < len(wants)-1 {
			next, _ := m.Update(key("j"))
			m = next.(Model)
		}
	}
}

// TestAgentDetailShowsLiveSessions verifies an agent's detail lists its live
// runtime sessions (matched by owning agent — including temp workers, whose Type
// carries a "temp:" prefix) and that the agent list shows the per-agent live
// count, while another agent's sessions don't leak.
func TestAgentDetailShowsLiveSessions(t *testing.T) {
	snap := agentsSnapshot()
	snap.Sessions = []Session{
		{Name: "claude-bg-9f2", Type: "planner", Runtime: "claude", Repo: "o/r", State: "running", Expires: "2026-06-18T10:09:00Z"},
		{Name: "claude-bg-1a7", Type: "planner", Runtime: "claude", Repo: "o/r", State: "idle", Expires: "2026-06-18T10:04:00Z"},
		// A temp worker for planner: its Type carries the daemon's "temp:" prefix
		// and must still roll up under planner.
		{Name: "planner-temp-job9", Type: "temp:planner", Runtime: "claude", Repo: "o/r", State: "running"},
		{Name: "codex-bg-77", Type: "reviewer", Runtime: "codex", Repo: "o/x", State: "running"},
	}
	m := agentsModel(t, Deps{}, snap)
	// The list shows the per-agent live count column.
	if !strings.Contains(m.View(), "LIVE") {
		t.Fatalf("agent list should have a LIVE column:\n%s", m.View())
	}
	// Open planner's detail (cursor 0).
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd != nil {
		next, _ = m.Update(cmd())
		m = next.(Model)
	}
	view := m.View()
	for _, want := range []string{"live sessions", "claude-bg-9f2", "running", "claude-bg-1a7", "idle", "planner-temp-job9"} {
		if !strings.Contains(view, want) {
			t.Fatalf("planner detail missing %q:\n%s", want, view)
		}
	}
	// reviewer's session must not appear under planner.
	if strings.Contains(view, "codex-bg-77") {
		t.Fatalf("another agent's session leaked into planner's detail:\n%s", view)
	}
}

// TestAgentDetailNoLiveSessions shows the empty state when an agent has none.
func TestAgentDetailNoLiveSessions(t *testing.T) {
	m := agentsModel(t, Deps{}, agentsSnapshot()) // snapshot has no Sessions
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd != nil {
		next, _ = m.Update(cmd())
		m = next.(Model)
	}
	view := m.View()
	idx := strings.Index(view, "live sessions")
	if idx < 0 {
		t.Fatalf("detail should have a live sessions section:\n%s", view)
	}
	if !strings.Contains(view[idx:], "none") {
		t.Fatalf("an agent with no sessions should show none:\n%s", view[idx:])
	}
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

func TestAgentSwitchRuntimeFlow(t *testing.T) {
	var got []string
	deps := Deps{SetAgentRuntime: func(name, runtime string) error {
		got = []string{name, runtime}
		return nil
	}}
	m := agentsModel(t, deps, agentsSnapshot())
	// planner (claude) is under the cursor; e opens the runtime picker.
	next, _ := m.Update(key("e"))
	m = next.(Model)
	if m.mode != modeAgentRuntimePick {
		t.Fatalf("e should open the runtime picker, mode=%v", m.mode)
	}
	// Cursor preselects the current runtime (claude, index 1). Move up to codex.
	if m.runtimePickCursor != 1 {
		t.Fatalf("picker should preselect the current runtime, cursor=%d", m.runtimePickCursor)
	}
	next, _ = m.Update(key("k"))
	m = next.(Model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("enter on a different runtime should apply it")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if len(got) != 2 || got[0] != "planner" || got[1] != "codex" {
		t.Fatalf("SetAgentRuntime called with %v, want [planner codex]", got)
	}
	if m.mode != modeNormal {
		t.Fatalf("a successful switch should close the overlay, mode=%v", m.mode)
	}
}

func TestAgentSwitchRuntimeNoChangeCloses(t *testing.T) {
	deps := Deps{SetAgentRuntime: func(string, string) error {
		t.Fatal("picking the current runtime must not call SetAgentRuntime")
		return nil
	}}
	m := agentsModel(t, deps, agentsSnapshot())
	next, _ := m.Update(key("e"))
	m = next.(Model)
	// Enter on the preselected (current) runtime is a no-op close.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd != nil {
		t.Fatal("re-picking the current runtime should not issue a command")
	}
	if m.mode != modeNormal {
		t.Fatalf("no-op pick should close the overlay, mode=%v", m.mode)
	}
}

func TestAgentSwitchRuntimeErrorStaysOpen(t *testing.T) {
	deps := Deps{SetAgentRuntime: func(string, string) error {
		return errors.New("unknown runtime")
	}}
	m := agentsModel(t, deps, agentsSnapshot())
	next, _ := m.Update(key("e"))
	m = next.(Model)
	next, _ = m.Update(key("k")) // claude → codex
	m = next.(Model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeAgentRuntimePick {
		t.Fatalf("a failed switch should keep the overlay open, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "unknown runtime") {
		t.Fatalf("the error should render in the overlay:\n%s", m.View())
	}
}

func TestAgentCustomPromptRoutesToEditor(t *testing.T) {
	var seededWith string
	var createdPlain bool
	deps := Deps{
		CreateAgent: func(string, string, string) error { createdPlain = true; return nil },
		EditAgentPrompt: func(seed string) tea.Cmd {
			seededWith = seed
			return func() tea.Msg { return AgentPromptEditedMsg{Content: "prompt with gitmoot_result"} }
		},
	}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(agentFormResultMsg{result: Result{Values: map[string]string{
		"name": "scout", "runtime": "claude", "template": agentCustomPromptValue, "seed": "planner-tpl",
	}}})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("custom prompt should dispatch the editor command")
	}
	if createdPlain {
		t.Fatal("custom prompt must not run the plain CreateAgent path")
	}
	// Run the editor command → AgentPromptEditedMsg.
	next, _ = m.Update(cmd())
	m = next.(Model)
	if seededWith != "planner-tpl" {
		t.Fatalf("EditAgentPrompt seed = %q, want planner-tpl", seededWith)
	}
	if m.pendingAgentName != "" || m.pendingAgentRuntime != "" {
		t.Fatalf("pending agent state should be cleared after the edit, got %q/%q", m.pendingAgentName, m.pendingAgentRuntime)
	}
}

func TestAgentCustomPromptCreatesFromContent(t *testing.T) {
	var got []string
	deps := Deps{
		EditAgentPrompt: func(string) tea.Cmd {
			return func() tea.Msg { return AgentPromptEditedMsg{Content: "You are X.\nReturn gitmoot_result."} }
		},
		CreateAgentWithPrompt: func(name, runtime, content string) error {
			got = []string{name, runtime, content}
			return nil
		},
	}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(agentFormResultMsg{result: Result{Values: map[string]string{
		"name": "scout", "runtime": "claude", "template": agentCustomPromptValue, "seed": "",
	}}})
	m = next.(Model)
	next, cmd = m.Update(cmd()) // editor msg → create cmd
	m = next.(Model)
	if cmd == nil {
		t.Fatal("a non-empty edit should dispatch the create")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if len(got) != 3 || got[0] != "scout" || got[1] != "claude" || got[2] != "You are X.\nReturn gitmoot_result." {
		t.Fatalf("CreateAgentWithPrompt called with %v", got)
	}
	if m.agentNotice != "" {
		t.Fatalf("a contract-bearing prompt should not warn: %q", m.agentNotice)
	}
}

func TestAgentCustomPromptWarnsWithoutContract(t *testing.T) {
	created := false
	deps := Deps{
		EditAgentPrompt: func(string) tea.Cmd {
			return func() tea.Msg { return AgentPromptEditedMsg{Content: "just do stuff, no contract"} }
		},
		CreateAgentWithPrompt: func(string, string, string) error { created = true; return nil },
	}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(agentFormResultMsg{result: Result{Values: map[string]string{
		"name": "scout", "runtime": "claude", "template": agentCustomPromptValue,
	}}})
	m = next.(Model)
	next, cmd = m.Update(cmd()) // editor msg
	m = next.(Model)
	if m.agentNotice == "" || !strings.Contains(m.agentNotice, "gitmoot_result") {
		t.Fatalf("missing-contract prompt should set a notice, got %q", m.agentNotice)
	}
	next, _ = m.Update(cmd()) // run create
	m = next.(Model)
	if !created {
		t.Fatal("a missing-contract prompt should still create the agent")
	}
	// The notice must survive the create-success handler's agentErr reset.
	if m.agentNotice == "" {
		t.Fatal("the notice should persist after a successful create")
	}
}

func TestAgentCustomPromptEditorErrorSurfaces(t *testing.T) {
	deps := Deps{
		EditAgentPrompt: func(string) tea.Cmd {
			return func() tea.Msg { return AgentPromptEditedMsg{Err: errors.New("editor blew up")} }
		},
		CreateAgentWithPrompt: func(string, string, string) error {
			t.Fatal("a failed edit must not create")
			return nil
		},
	}
	m := agentsModel(t, deps, agentsSnapshot())
	next, cmd := m.Update(agentFormResultMsg{result: Result{Values: map[string]string{
		"name": "scout", "runtime": "claude", "template": agentCustomPromptValue,
	}}})
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if !strings.Contains(m.agentErr, "editor blew up") {
		t.Fatalf("editor error should surface inline, got %q", m.agentErr)
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
