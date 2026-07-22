package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gitmoot/gitmoot/internal/db"
)

func TestDismissConfirmYes(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	return tabToPage(t, m, pageTrains)
}

func TestTrainDetailShowsSessionLocksOnly(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	cases := []struct {
		id       string
		wantBase string
		wantHas  bool
	}{
		{"officeqa-treasury-skillopt-v1", "officeqa-treasury-skillopt", true},
		{"officeqa-treasury-skillopt-v8", "officeqa-treasury-skillopt", true},
		{"run-v12", "run", true},
		{"run-12", "run-12", false},                         // bare numeric is NOT a version lineage
		{"train-x-20260611-1", "train-x-20260611-1", false}, // timestamp tail, not a version
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
	t.Parallel()
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
	m = tabToPage(t, m, pageTrains)
	view := m.View()
	for _, want := range []string{"Active", "Blocked", "Done"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected section header %q:\n%s", want, view)
		}
	}
	// Repo is the only group: the repo header carries the count, and there is NO
	// separate lineage sub-group line (e.g. "skillopt  ×2") inside it.
	if !strings.Contains(view, "o/r") {
		t.Fatalf("expected the repo group header:\n%s", view)
	}
	if strings.Contains(view, "skillopt  ×") {
		t.Fatalf("lineage sub-group line should be gone (only the repo group remains):\n%s", view)
	}
	// All individual session ids still render directly under the repo.
	for _, id := range []string{"skillopt-v1", "skillopt-v2", "lonely", "stalled", "gone"} {
		if !strings.Contains(view, id) {
			t.Fatalf("expected session %q in view:\n%s", id, view)
		}
	}
	// Lineage children are contiguous under their parent: v2 follows v1, and both
	// come before the unrelated flat 'lonely' row (snapshot order is v1, lonely,
	// v2 — grouping must keep the lineage together regardless).
	v1i, v2i, lonelyi := strings.Index(view, "skillopt-v1"), strings.Index(view, "skillopt-v2"), strings.Index(view, "lonely")
	if !(v1i < v2i && v2i < lonelyi) {
		t.Fatalf("lineage children must be contiguous (v1 < v2 < lonely): v1=%d v2=%d lonely=%d\n%s", v1i, v2i, lonelyi, view)
	}
	// Sections render in order: Active before Blocked before Done.
	ai, bi, di := strings.Index(view, "Active"), strings.Index(view, "Blocked"), strings.Index(view, "Done")
	if !(ai < bi && bi < di) {
		t.Fatalf("sections out of order: Active=%d Blocked=%d Done=%d\n%s", ai, bi, di, view)
	}
}

// TestTrainCursorFollowsDisplayOrder proves the cursor steps through the visible
// (grouped) order, not raw snapshot order: an Active session at a late snap index
// is selected first because the Active section renders before Done, so ↑/↓ always
// move the highlight to the visually adjacent row.
func TestTrainCursorFollowsDisplayOrder(t *testing.T) {
	t.Parallel()
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		Trains: []TrainSession{
			{ID: "done-early", Phase: "run_abandoned", Repo: "o/r"}, // idx 0 → Done section (renders last)
			{ID: "live-late", Phase: "items_ready", Repo: "o/r"},    // idx 1 → Active section (renders first)
		},
	}
	deps := Deps{Load: func() (Snapshot, error) { return snap, nil }}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	m = tabToPage(t, m, pageTrains)
	// Cursor 0 selects the first VISIBLE row = live-late (Active renders before Done).
	if got, _ := m.trainUnderCursor(); got.ID != "live-late" {
		t.Fatalf("cursor 0 should select the first visible row (live-late), got %q", got.ID)
	}
	// Down moves to the next visible row = done-early (Done section).
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	if m.trainCursor != 1 {
		t.Fatalf("down should move cursor to 1, got %d", m.trainCursor)
	}
	if got, _ := m.trainUnderCursor(); got.ID != "done-early" {
		t.Fatalf("cursor 1 should select the next visible row (done-early), got %q", got.ID)
	}
}

// TestTrainsGroupByRepoWithinSection verifies that within a status section,
// sessions are sub-grouped by repo (first-appearance order) with lineage
// collapse inside each repo, and the cursor follows that display order.
func TestTrainsGroupByRepoWithinSection(t *testing.T) {
	t.Parallel()
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		Trains: []TrainSession{
			{ID: "a-v1", Phase: "items_ready", Repo: "o/alpha"},      // Active, alpha, lineage
			{ID: "b1", Phase: "generating_options", Repo: "o/beta"},  // Active, beta, lone
			{ID: "a-v2", Phase: "review_published", Repo: "o/alpha"}, // Active, alpha, lineage
		},
	}
	deps := Deps{Load: func() (Snapshot, error) { return snap, nil }}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	m = tabToPage(t, m, pageTrains)
	view := m.View()
	for _, want := range []string{"Active", "o/alpha", "o/beta", "a-v1", "a-v2", "b1"} {
		if !strings.Contains(view, want) {
			t.Fatalf("trains view missing %q:\n%s", want, view)
		}
	}
	// Repos in first-appearance order: alpha before beta.
	if strings.Index(view, "o/alpha") > strings.Index(view, "o/beta") {
		t.Fatalf("repos out of order (alpha before beta):\n%s", view)
	}
	// alpha's lineage collapses; its members precede beta's session.
	v1, v2, b := strings.Index(view, "a-v1"), strings.Index(view, "a-v2"), strings.Index(view, "b1")
	if !(v1 < v2 && v2 < b) {
		t.Fatalf("repo/lineage order wrong (want a-v1<a-v2<b1): %d %d %d\n%s", v1, v2, b, view)
	}
	// Cursor 0 selects the first display row (a-v1 under alpha).
	if got, _ := m.trainUnderCursor(); got.ID != "a-v1" {
		t.Fatalf("cursor 0 should select a-v1, got %q", got.ID)
	}
}

// TestTrainsCollapseRepoFoldsSessions verifies space folds the cursor's repo
// group (hiding its sessions and showing a [+] header) and that the collapsed
// header is selectable so space re-expands it.
func TestTrainsCollapseRepoFoldsSessions(t *testing.T) {
	t.Parallel()
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		Trains: []TrainSession{
			{ID: "a1", Phase: "items_ready", Repo: "o/alpha"},
			{ID: "b1", Phase: "items_ready", Repo: "o/beta"},
		},
	}
	deps := Deps{Load: func() (Snapshot, error) { return snap, nil }}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	m = tabToPage(t, m, pageTrains)
	// Cursor 0 = a1 (o/alpha). Space folds o/alpha.
	next, _ = m.Update(key(" "))
	m = next.(Model)
	view := m.View()
	if strings.Contains(view, "a1") {
		t.Fatalf("a1 should be hidden after folding o/alpha:\n%s", view)
	}
	if !strings.Contains(view, "[+]") {
		t.Fatalf("folded repo should show a [+] marker:\n%s", view)
	}
	if !strings.Contains(view, "b1") {
		t.Fatalf("o/beta should still be visible:\n%s", view)
	}
	// Cursor now sits on the collapsed o/alpha header; space re-expands it.
	next, _ = m.Update(key(" "))
	m = next.(Model)
	if !strings.Contains(m.View(), "a1") {
		t.Fatalf("expanding o/alpha should restore a1:\n%s", m.View())
	}
}

// TestTrainsCollapsedByDefault verifies the live default (CollapseGroupsByDefault)
// folds repo groups on first show, and space on a [+] header expands one.
func TestTrainsCollapsedByDefault(t *testing.T) {
	t.Parallel()
	snap := Snapshot{
		Daemon: Daemon{Running: true},
		Trains: []TrainSession{{ID: "s1", Phase: "items_ready", Repo: "o/alpha"}},
	}
	deps := Deps{Load: func() (Snapshot, error) { return snap, nil }, CollapseGroupsByDefault: true}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	m = tabToPage(t, m, pageTrains)
	view := m.View()
	if strings.Contains(view, "s1") {
		t.Fatalf("sessions should be folded by default:\n%s", view)
	}
	if !strings.Contains(view, "[+]") {
		t.Fatalf("collapsed repo should show a [+] marker:\n%s", view)
	}
	// Space on the collapsed repo header expands it.
	next, _ = m.Update(key(" "))
	m = next.(Model)
	if !strings.Contains(m.View(), "s1") {
		t.Fatalf("space should expand the repo and reveal s1:\n%s", m.View())
	}
}
