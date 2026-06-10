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
