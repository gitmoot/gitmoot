package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestDismissConfirmYes(t *testing.T) {
	var dismissed string
	deps := Deps{
		Load:    func() (Snapshot, error) { return promptSnap(choicePrompt()), nil },
		Dismiss: func(id string) error { dismissed = id; return nil },
	}
	m := attentionModel(t, deps, promptSnap(choicePrompt()))

	next, _ := m.Update(key("d"))
	m = next.(Model)
	if m.mode != modeConfirmDismiss {
		t.Fatalf("d should open the dismiss confirm, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "Delete this prompt?") {
		t.Fatalf("confirm prompt missing:\n%s", m.View())
	}
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("y should submit a dismiss command")
	}
	_ = cmd()
	if dismissed != "choice.p" {
		t.Fatalf("Dismiss called with %q, want choice.p", dismissed)
	}
}

func TestDismissConfirmCancel(t *testing.T) {
	called := false
	deps := Deps{
		Load:    func() (Snapshot, error) { return promptSnap(choicePrompt()), nil },
		Dismiss: func(id string) error { called = true; return nil },
	}
	m := attentionModel(t, deps, promptSnap(choicePrompt()))

	next, _ := m.Update(key("d"))
	m = next.(Model)
	// 'n' cancels.
	next, _ = m.Update(key("n"))
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("n should cancel the dismiss, mode=%v", m.mode)
	}
	if called {
		t.Fatal("cancel must not call Dismiss")
	}
}

func TestDismissNotFoundTreatedAsSuccess(t *testing.T) {
	deps := Deps{
		Load: func() (Snapshot, error) { return promptSnap(choicePrompt()), nil },
		Dismiss: func(id string) error {
			return fmt.Errorf("interactive prompt %q: %w", id, db.ErrInteractivePromptNotFound)
		},
	}
	m := attentionModel(t, deps, promptSnap(choicePrompt()))

	next, _ := m.Update(key("d"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("a not-found prompt should still close the overlay, mode=%v", m.mode)
	}
	if m.actionErr != "" {
		t.Fatalf("not-found should not surface an error, got %q", m.actionErr)
	}
}

func TestDismissRealErrorKeepsOverlay(t *testing.T) {
	deps := Deps{
		Load:    func() (Snapshot, error) { return promptSnap(choicePrompt()), nil },
		Dismiss: func(id string) error { return errors.New("database is locked") },
	}
	m := attentionModel(t, deps, promptSnap(choicePrompt()))

	next, _ := m.Update(key("d"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeConfirmDismiss {
		t.Fatalf("a real error should keep the confirm open, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "database is locked") {
		t.Fatalf("expected the error in view:\n%s", m.View())
	}
}

func trainSnapshot() Snapshot {
	return Snapshot{
		Trains: []TrainSession{
			{ID: "train-aaa", Phase: "generating_options", Candidate: "c@v1", Repo: "owner/a"},
			{ID: "train-bbb", Phase: "run_abandoned", Repo: "owner/b"},
		},
		ResourceLocks: []ResourceLock{
			{Key: "generation:train-aaa", Owner: "pid:1", Stale: false},
			{Key: "optimizer:train-bbb", Owner: "pid:2", Stale: true},
		},
	}
}

func trainsModel(t *testing.T) Model {
	t.Helper()
	deps := Deps{Load: func() (Snapshot, error) { return trainSnapshot(), nil }}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: trainSnapshot(), at: time.Unix(1, 0)})
	m = next.(Model)
	// Tab from Attention to Trains.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	return next.(Model)
}

func TestTrainDetailShowsSessionLocksOnly(t *testing.T) {
	m := trainsModel(t)
	if pages[m.selected].page != pageTrains {
		t.Fatalf("expected to be on the Trains page")
	}
	// Open detail for the first train.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.mode != modeTrainDetail {
		t.Fatalf("enter should open train detail, mode=%v", m.mode)
	}
	view := m.View()
	if !strings.Contains(view, "generating_options") || !strings.Contains(view, "generation:train-aaa") {
		t.Fatalf("detail should show the session phase and its lock:\n%s", view)
	}
	if strings.Contains(view, "optimizer:train-bbb") {
		t.Fatalf("detail must not show another session's lock:\n%s", view)
	}
	// Esc returns to the list.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("esc should leave detail, mode=%v", m.mode)
	}
}

func TestTrainLocksMatchWholeSegmentNotPrefix(t *testing.T) {
	locks := []ResourceLock{
		{Key: "generation:train-a"},
		{Key: "generation:train-aaa"},
		{Key: "skillopt-train:train-a:iter-1"},
	}
	got := trainLocks(locks, "train-a")
	if len(got) != 2 {
		t.Fatalf("expected 2 locks for train-a (exact segment), got %d: %+v", len(got), got)
	}
	for _, l := range got {
		if l.Key == "generation:train-aaa" {
			t.Fatalf("train-a must not match train-aaa's lock: %+v", got)
		}
	}
}

func TestTrainCursorClampedOnRefresh(t *testing.T) {
	m := trainsModel(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	if m.trainCursor != 1 {
		t.Fatalf("train cursor should be 1, got %d", m.trainCursor)
	}
	next, _ = m.Update(snapshotMsg{snap: Snapshot{}, at: time.Unix(2, 0)})
	m = next.(Model)
	if m.trainCursor != 0 {
		t.Fatalf("train cursor should clamp to 0, got %d", m.trainCursor)
	}
}

func TestTrainStatusCategory(t *testing.T) {
	cases := map[string]int{
		"request_confirmed":                trainCatActive,
		"items_ready":                      trainCatActive,
		"optimizer_completed":              trainCatActive,
		"generating_options":               trainCatActive,
		"optimizer_running":                trainCatActive,
		"preflight_running":                trainCatActive,
		"recovery_available":               trainCatActive,
		"blocked_stale_lock":               trainCatBlocked,
		"failed_unrecoverable":             trainCatBlocked,
		"blocked_config":                   trainCatBlocked,
		"optimizer_heartbeat_stale":        trainCatBlocked,
		"candidate_promoted":               trainCatDone,
		"candidate_rejected":               trainCatDone,
		"run_abandoned":                    trainCatDone,
		"optimizer_completed_no_candidate": trainCatDone,
	}
	for phase, want := range cases {
		if got := trainStatusCategory(phase); got != want {
			t.Errorf("trainStatusCategory(%q) = %d, want %d", phase, got, want)
		}
	}
}

func TestTrainLineageBase(t *testing.T) {
	cases := []struct {
		id       string
		wantBase string
		wantHas  bool
	}{
		{"officeqa-treasury-skillopt-v1", "officeqa-treasury-skillopt", true},
		{"officeqa-treasury-skillopt-v8", "officeqa-treasury-skillopt", true},
		{"run-12", "run", true},
		{"plain-name", "plain-name", false},
		{"train-aaa", "train-aaa", false},
	}
	for _, c := range cases {
		base, has := trainLineageBase(c.id)
		if base != c.wantBase || has != c.wantHas {
			t.Errorf("trainLineageBase(%q) = (%q,%v), want (%q,%v)", c.id, base, has, c.wantBase, c.wantHas)
		}
	}
}

// TestTrainsListGroupsAndCollapses renders a mixed list and asserts the section
// headers, the collapsed lineage parent line, and that flat sessions stay flat.
func TestTrainsListGroupsAndCollapses(t *testing.T) {
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		Trains: []TrainSession{
			{ID: "skillopt-v1", Phase: "items_ready", Repo: "o/r"},      // 0 Active, lineage
			{ID: "lonely", Phase: "generating_options", Repo: "o/r"},    // 1 Active, flat
			{ID: "skillopt-v2", Phase: "review_published", Repo: "o/r"}, // 2 Active, lineage
			{ID: "stalled", Phase: "blocked_stale_lock", Repo: "o/r"},   // 3 Blocked
			{ID: "gone", Phase: "run_abandoned", Repo: "o/r"},           // 4 Done
		},
	}
	deps := Deps{Load: func() (Snapshot, error) { return snap, nil }}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // Attention → Trains
	m = next.(Model)
	if pages[m.selected].page != pageTrains {
		t.Fatalf("expected Trains page")
	}
	view := m.View()
	for _, want := range []string{"Active", "Blocked", "Done"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected section header %q:\n%s", want, view)
		}
	}
	// Two skillopt-v* sessions collapse under one lineage parent line.
	if !strings.Contains(view, "skillopt  ") || !strings.Contains(view, "×2") {
		t.Fatalf("expected collapsed lineage parent 'skillopt ×2':\n%s", view)
	}
	// All individual session ids still render (children + flat rows).
	for _, id := range []string{"skillopt-v1", "skillopt-v2", "lonely", "stalled", "gone"} {
		if !strings.Contains(view, id) {
			t.Fatalf("expected session %q in view:\n%s", id, view)
		}
	}
	// Sections render in order: Active before Blocked before Done.
	ai, bi, di := strings.Index(view, "Active"), strings.Index(view, "Blocked"), strings.Index(view, "Done")
	if !(ai < bi && bi < di) {
		t.Fatalf("sections out of order: Active=%d Blocked=%d Done=%d\n%s", ai, bi, di, view)
	}
}

// TestTrainCursorSelectsSessionDespiteGrouping proves the cursor still indexes
// into snap.Trains regardless of the grouped/collapsed visual layout: a session
// that sorts late visually (Done) but sits at an early snap index is reachable
// by its index, and the action overlay opens for the right session.
func TestTrainCursorSelectsSessionDespiteGrouping(t *testing.T) {
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		Trains: []TrainSession{
			{ID: "done-early", Phase: "run_abandoned", Repo: "o/r"}, // idx 0 → Done section
			{ID: "live-late", Phase: "items_ready", Repo: "o/r"},    // idx 1 → Active section
		},
	}
	deps := Deps{Load: func() (Snapshot, error) { return snap, nil }}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(Model)
	// Cursor 0 selects snap.Trains[0] = done-early, even though it renders last.
	if got, _ := m.trainUnderCursor(); got.ID != "done-early" {
		t.Fatalf("cursor 0 should select snap index 0 (done-early), got %q", got.ID)
	}
	// Down moves to snap index 1 = live-late.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	if m.trainCursor != 1 {
		t.Fatalf("down should move cursor to 1, got %d", m.trainCursor)
	}
	if got, _ := m.trainUnderCursor(); got.ID != "live-late" {
		t.Fatalf("cursor 1 should select snap index 1 (live-late), got %q", got.ID)
	}
}
