package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jerryfane/gitmoot/internal/db"
)

func promptSnap(prompts ...db.InteractivePrompt) Snapshot {
	return Snapshot{Prompts: prompts}
}

func choicePrompt() db.InteractivePrompt {
	return db.InteractivePrompt{ID: "choice.p", Question: "Pick one", Choices: []string{"keep", "drop"}, Default: "keep", State: db.InteractivePromptStatePending}
}

func textPrompt() db.InteractivePrompt {
	return db.InteractivePrompt{ID: "text.p", Question: "Training name?", State: db.InteractivePromptStatePending}
}

func attentionModel(t *testing.T, deps Deps, snap Snapshot) Model {
	t.Helper()
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	if pages[m.selected].page != pageAttention {
		t.Fatalf("expected Attention to be the default page")
	}
	return m
}

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestAnswerChoiceHappyPath(t *testing.T) {
	var gotID, gotVal string
	deps := Deps{
		Load:   func() (Snapshot, error) { return promptSnap(choicePrompt()), nil },
		Answer: func(id, value string) error { gotID, gotVal = id, value; return nil },
	}
	m := attentionModel(t, deps, promptSnap(choicePrompt()))

	next, _ := m.Update(key("a"))
	m = next.(Model)
	if m.mode != modeAnswerChoice {
		t.Fatalf("expected choice overlay, mode=%v", m.mode)
	}
	if m.choiceIdx != 0 {
		t.Fatalf("default 'keep' should be preselected, idx=%d", m.choiceIdx)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if !m.actionBusy || cmd == nil {
		t.Fatalf("expected an in-flight answer command")
	}
	msg := cmd()
	if gotID != "choice.p" || gotVal != "drop" {
		t.Fatalf("Answer called with (%q,%q), want (choice.p,drop)", gotID, gotVal)
	}
	next, _ = m.Update(msg)
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("successful answer should return to normal mode, got %v", m.mode)
	}
}

func TestAnswerTextHappyPath(t *testing.T) {
	var gotVal string
	deps := Deps{
		Load:   func() (Snapshot, error) { return promptSnap(textPrompt()), nil },
		Answer: func(id, value string) error { gotVal = value; return nil },
	}
	m := attentionModel(t, deps, promptSnap(textPrompt()))

	next, _ := m.Update(key("a"))
	m = next.(Model)
	if m.mode != modeAnswerText {
		t.Fatalf("expected text overlay, mode=%v", m.mode)
	}
	next, _ = m.Update(key("smithyx"))
	m = next.(Model)
	// Typed text must land in the input, not trigger globals.
	if !strings.Contains(m.input.Value(), "smithyx") {
		t.Fatalf("typed text missing from input: %q", m.input.Value())
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("expected answer command")
	}
	_ = cmd()
	if gotVal != "smithyx" {
		t.Fatalf("Answer value = %q, want smithyx", gotVal)
	}
}

func TestAnswerInvalidKeepsOverlay(t *testing.T) {
	deps := Deps{
		Load:   func() (Snapshot, error) { return promptSnap(choicePrompt()), nil },
		Answer: func(id, value string) error { return errors.New("value \"drop\" is not one of the choices") },
	}
	m := attentionModel(t, deps, promptSnap(choicePrompt()))

	next, _ := m.Update(key("a"))
	m = next.(Model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeAnswerChoice {
		t.Fatalf("invalid answer should keep the overlay open, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "not one of") {
		t.Fatalf("expected inline error in view:\n%s", m.View())
	}
	if len(m.snap.Prompts) != 1 {
		t.Fatalf("prompt should remain pending: %+v", m.snap.Prompts)
	}
}

func TestAnswerCancelDoesNotMutate(t *testing.T) {
	called := false
	deps := Deps{
		Load:   func() (Snapshot, error) { return promptSnap(choicePrompt()), nil },
		Answer: func(id, value string) error { called = true; return nil },
	}
	m := attentionModel(t, deps, promptSnap(choicePrompt()))

	next, _ := m.Update(key("a"))
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("esc should close the overlay, mode=%v", m.mode)
	}
	if called {
		t.Fatal("cancel must not call Answer")
	}
}

func TestAnswerDoubleSubmitSuppressed(t *testing.T) {
	count := 0
	deps := Deps{
		Load:   func() (Snapshot, error) { return promptSnap(choicePrompt()), nil },
		Answer: func(id, value string) error { count++; return nil },
	}
	m := attentionModel(t, deps, promptSnap(choicePrompt()))

	next, _ := m.Update(key("a"))
	m = next.(Model)
	next, cmd1 := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	next, cmd2 := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd1 == nil {
		t.Fatal("first enter should submit")
	}
	if cmd2 != nil {
		t.Fatal("second enter while busy should be suppressed")
	}
	cmd1()
	if count != 1 {
		t.Fatalf("Answer should be called once, got %d", count)
	}
}

// A coordinator whose delegation fan-out could not be routed (#451) ends
// succeeded — not blocked/failed — so it is not jobReportable by state. The
// PreflightFailed flag must still surface it on the Attention page (with its
// reason), or the zero-child fan-out this issue set out to fix stays invisible.
func TestAttentionSurfacesPreflightFailedCoordinator(t *testing.T) {
	deps := Deps{Load: func() (Snapshot, error) { return Snapshot{}, nil }}
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		JobRows: []JobRow{
			{
				ID:              "coord-1",
				Type:            "ask",
				State:           "succeeded",
				PreflightFailed: true,
				LatestEvent:     "PREFLIGHT_FAILED: \"claude\" is a runtime, not a registered agent",
				Repo:            "owner/repo",
			},
			{ID: "plain", Type: "merge", State: "succeeded"}, // ordinary success, excluded
		},
	}
	m := attentionModel(t, deps, snap)
	view := m.View()

	if !strings.Contains(view, "Blocked, failed & preflight-failed jobs (1)") {
		t.Fatalf("expected widened section title incl. preflight-failed (1):\n%s", view)
	}
	if !strings.Contains(view, "coord-1") {
		t.Fatalf("preflight-failed coordinator should be listed:\n%s", view)
	}
	// The reason is rendered as the one-line 'why' (truncated for the row width),
	// so assert on the leading, non-truncated portion.
	if !strings.Contains(view, "PREFLIGHT_FAILED:") || !strings.Contains(view, "is a runtime") {
		t.Fatalf("preflight reason should be the one-line 'why':\n%s", view)
	}
	if strings.Contains(view, "plain") {
		t.Fatalf("ordinary succeeded job should not be listed:\n%s", view)
	}
}

func TestAttentionGroupsUnderSectionHeaders(t *testing.T) {
	deps := Deps{Load: func() (Snapshot, error) { return Snapshot{}, nil }}
	snap := Snapshot{
		Daemon:  Daemon{Running: true},
		Prompts: []db.InteractivePrompt{choicePrompt(), textPrompt()},
		JobRows: []JobRow{
			{ID: "j1", Type: "review", State: "failed", LatestEvent: "boom went wrong"},
			{ID: "j2", Type: "merge", State: "succeeded"}, // not reportable, must be skipped
			{ID: "j3", Type: "build", State: "blocked", LatestEvent: "stuck on dep"},
		},
		ResourceLocks: []ResourceLock{
			{Key: "skillopt-train:abc", Stale: true},
			{Key: "live-lock", Stale: false},
		},
		BranchLocks: []BranchLock{{Repo: "gitmoot", Branch: "main", Owner: "alice"}},
	}
	m := attentionModel(t, deps, snap)
	view := m.View()

	// Section headers carry the count of items in that section.
	if !strings.Contains(view, "Prompts (2)") {
		t.Fatalf("expected Prompts (2) header:\n%s", view)
	}
	if !strings.Contains(view, "Blocked & failed jobs (2)") {
		t.Fatalf("expected Blocked & failed jobs (2) header (succeeded job excluded):\n%s", view)
	}
	if !strings.Contains(view, "Stale locks (1)") {
		t.Fatalf("expected Stale locks (1) header:\n%s", view)
	}
	if !strings.Contains(view, "Branch locks (1)") {
		t.Fatalf("expected Branch locks (1) header:\n%s", view)
	}

	// The reportable jobs' LatestEvent is shown as the one-line 'why'.
	if !strings.Contains(view, "boom went wrong") {
		t.Fatalf("expected failed job's LatestEvent in view:\n%s", view)
	}

	// The succeeded job must not be listed, and the non-stale lock must not
	// appear under Stale locks.
	if strings.Contains(view, "j2") {
		t.Fatalf("succeeded job j2 should not be listed:\n%s", view)
	}
	if strings.Contains(view, "live-lock") {
		t.Fatalf("non-stale lock should not appear:\n%s", view)
	}

	// Sections render in order: Prompts, then jobs, then stale locks, then
	// branch locks.
	pi := strings.Index(view, "Prompts (2)")
	ji := strings.Index(view, "Blocked & failed jobs (2)")
	si := strings.Index(view, "Stale locks (1)")
	bi := strings.Index(view, "Branch locks (1)")
	if !(pi < ji && ji < si && si < bi) {
		t.Fatalf("sections out of order: prompts=%d jobs=%d stale=%d branch=%d\n%s", pi, ji, si, bi, view)
	}
}

func TestAttentionHeadersDoNotShiftCursor(t *testing.T) {
	deps := Deps{Load: func() (Snapshot, error) { return Snapshot{}, nil }}
	snap := Snapshot{
		Daemon:  Daemon{Running: true},
		Prompts: []db.InteractivePrompt{choicePrompt(), textPrompt()},
		JobRows: []JobRow{{ID: "j1", Type: "review", State: "failed"}},
	}
	m := attentionModel(t, deps, snap)

	// Cursor starts on the first prompt (index 0); 'a' answers it.
	if _, ok := m.attentionPrompt(); !ok {
		t.Fatalf("cursor 0 should select the first prompt")
	}

	// Move down twice: index 1 is the second prompt, index 2 is the failed job.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	if m.promptCursor != 2 {
		t.Fatalf("cursor should be at the job (index 2), got %d", m.promptCursor)
	}
	job, ok := m.jobUnderCursor()
	if !ok || job.ID != "j1" {
		t.Fatalf("section headers must not shift cursor; expected job j1 under cursor, got ok=%v job=%+v", ok, job)
	}
	if _, ok := m.attentionPrompt(); ok {
		t.Fatalf("a job row must not resolve to a prompt")
	}
}

func TestAttentionLocksOnlyNoNeedsAttentionItems(t *testing.T) {
	deps := Deps{Load: func() (Snapshot, error) { return Snapshot{}, nil }}
	snap := Snapshot{
		Daemon:        Daemon{Running: true},
		ResourceLocks: []ResourceLock{{Key: "skillopt-train:abc", Stale: true}},
	}
	m := attentionModel(t, deps, snap)
	view := m.View()
	if !strings.Contains(view, "Stale locks (1)") {
		t.Fatalf("expected Stale locks (1) header:\n%s", view)
	}
	// With no prompts or jobs, the "none" placeholder is shown.
	if !strings.Contains(view, "none") {
		t.Fatalf("expected 'none' placeholder when no prompts/jobs:\n%s", view)
	}
}

// TestAttentionShowsAwaitingHuman pins the #340 Attention surface: a tree paused
// at awaiting_human renders under an "Awaiting human" section with the resume
// hint, even when there are no actionable prompts/jobs.
func TestAttentionShowsAwaitingHuman(t *testing.T) {
	deps := Deps{Load: func() (Snapshot, error) { return Snapshot{}, nil }}
	snap := Snapshot{
		Daemon:        Daemon{Running: true},
		AwaitingHuman: []AwaitingHumanTask{{TaskID: "task-5", Repo: "jerryfane/gitmoot", Title: "Parent"}},
	}
	m := attentionModel(t, deps, snap)
	view := m.View()
	if !strings.Contains(view, "Awaiting human (1)") {
		t.Fatalf("expected Awaiting human (1) header:\n%s", view)
	}
	if !strings.Contains(view, "task-5") {
		t.Fatalf("expected the paused task id:\n%s", view)
	}
	if !strings.Contains(view, "/gitmoot resume") {
		t.Fatalf("expected the resume hint:\n%s", view)
	}
}

// TestAttentionShowsPendingCandidates proves a pending SkillOpt candidate (#471)
// renders on the Attention page next to AwaitingHumanTask, with its version id,
// template id, score, and the decide hint; zero candidates renders unchanged.
func TestAttentionShowsPendingCandidates(t *testing.T) {
	deps := Deps{Load: func() (Snapshot, error) { return Snapshot{}, nil }}
	snap := Snapshot{
		Daemon:            Daemon{Running: true},
		PendingCandidates: []PendingCandidate{{VersionID: "planner:v7", TemplateID: "planner", Score: "0.96"}},
	}
	m := attentionModel(t, deps, snap)
	view := m.View()
	for _, want := range []string{"Pending candidates (1)", "planner:v7", "template planner", "score 0.96", "skillopt candidate promote|reject"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected %q in attention view:\n%s", want, view)
		}
	}

	// Zero pending candidates: no Pending candidates header.
	empty := attentionModel(t, deps, Snapshot{Daemon: Daemon{Running: true}})
	if strings.Contains(empty.View(), "Pending candidates") {
		t.Fatalf("empty candidate list should not render the header:\n%s", empty.View())
	}
}

func TestAttentionCursorClampedOnRefresh(t *testing.T) {
	deps := Deps{Load: func() (Snapshot, error) { return Snapshot{}, nil }}
	m := attentionModel(t, deps, promptSnap(choicePrompt(), textPrompt()))
	// Move cursor to the second prompt.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	if m.promptCursor != 1 {
		t.Fatalf("cursor should be at 1, got %d", m.promptCursor)
	}
	// Refresh with an empty prompt list must not leave a dangling cursor.
	next, _ = m.Update(snapshotMsg{snap: Snapshot{Daemon: Daemon{Running: true}}, at: time.Unix(2, 0)})
	m = next.(Model)
	if m.promptCursor != 0 {
		t.Fatalf("cursor should clamp to 0 when prompts empty, got %d", m.promptCursor)
	}
	if !strings.Contains(m.View(), "Nothing needs attention") {
		t.Fatalf("empty attention page expected:\n%s", m.View())
	}
}

// TestAttentionGroupsJobsByRepo verifies the blocked/failed jobs render under
// repo sub-headers, with same-repo jobs contiguous, and the cursor walking them
// in that display order.
func TestAttentionGroupsJobsByRepo(t *testing.T) {
	deps := Deps{Load: func() (Snapshot, error) { return Snapshot{}, nil }}
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		JobRows: []JobRow{
			{ID: "j1", Type: "ask", State: "failed", Repo: "o/alpha"},
			{ID: "j2", Type: "ask", State: "blocked", Repo: "o/beta"},
			{ID: "j3", Type: "ask", State: "failed", Repo: "o/alpha"},
		},
	}
	m := attentionModel(t, deps, snap)
	view := m.View()
	for _, want := range []string{"Blocked & failed jobs (3)", "o/alpha", "o/beta", "j1", "j2", "j3"} {
		if !strings.Contains(view, want) {
			t.Fatalf("attention view missing %q:\n%s", want, view)
		}
	}
	// alpha is first-seen so its jobs (j1, j3) are contiguous and precede beta's j2.
	i1, i3, i2 := strings.Index(view, "j1"), strings.Index(view, "j3"), strings.Index(view, "j2")
	if !(i1 < i3 && i3 < i2) {
		t.Fatalf("jobs not grouped by repo (want j1<j3<j2): j1=%d j3=%d j2=%d\n%s", i1, i3, i2, view)
	}
	// Cursor walks the repo-grouped order: 0=j1, 1=j3, 2=j2.
	wants := []string{"j1", "j3", "j2"}
	for i, want := range wants {
		got, ok := m.jobUnderCursor()
		if !ok || got.ID != want {
			t.Fatalf("cursor %d should select %q, got %q ok=%v", i, want, got.ID, ok)
		}
		if i < len(wants)-1 {
			next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
			m = next.(Model)
		}
	}
}

// TestAttentionCollapseJobRepo verifies a job repo group folds with space and
// re-expands, while prompts and other repos stay put.
func TestAttentionCollapseJobRepo(t *testing.T) {
	deps := Deps{Load: func() (Snapshot, error) { return Snapshot{}, nil }}
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		JobRows: []JobRow{
			{ID: "j1", Type: "ask", State: "failed", Repo: "o/alpha"},
			{ID: "j2", Type: "ask", State: "blocked", Repo: "o/beta"},
		},
	}
	m := attentionModel(t, deps, snap)
	// Cursor 0 = j1 (o/alpha). Space folds o/alpha.
	next, _ := m.Update(key(" "))
	m = next.(Model)
	view := m.View()
	if strings.Contains(view, "j1") {
		t.Fatalf("j1 should be hidden after folding o/alpha:\n%s", view)
	}
	if !strings.Contains(view, "[+]") {
		t.Fatalf("folded repo should show a [+] marker:\n%s", view)
	}
	if !strings.Contains(view, "j2") {
		t.Fatalf("o/beta job should still be visible:\n%s", view)
	}
	// Space on the collapsed header re-expands it.
	next, _ = m.Update(key(" "))
	m = next.(Model)
	if !strings.Contains(m.View(), "j1") {
		t.Fatalf("expanding o/alpha should restore j1:\n%s", m.View())
	}
}
