package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func trainsSnapshot() Snapshot {
	return Snapshot{
		Daemon: Daemon{Running: true},
		Trains: []TrainSession{
			{ID: "train-live", Phase: "items_ready", Repo: "o/r"},
			{ID: "train-done", Phase: "run_abandoned", Repo: "o/r"},
		},
	}
}

func trainsActionModel(t *testing.T, deps Deps, snap Snapshot) Model {
	t.Helper()
	if deps.Load == nil {
		deps.Load = func() (Snapshot, error) { return snap, nil }
	}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // Attention → Trains
	m = next.(Model)
	if pages[m.selected].page != pageTrains {
		t.Fatalf("expected Trains page, got %v", pages[m.selected].page)
	}
	return m
}

func typeText(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(Model)
	}
	return m
}

func TestTrainStopReasonFlow(t *testing.T) {
	var gotID, gotReason string
	deps := Deps{StopTrain: func(id, reason string) error {
		gotID, gotReason = id, reason
		return nil
	}}
	m := trainsActionModel(t, deps, trainsSnapshot())
	// Cursor on train-live (first row); s opens the reason input.
	next, _ := m.Update(key("s"))
	m = next.(Model)
	if m.mode != modeTrainStopReason {
		t.Fatalf("s on a live session should ask for a reason, mode=%v", m.mode)
	}
	// Empty reason is refused inline.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.mode != modeTrainStopReason || !strings.Contains(m.View(), "a reason is required") {
		t.Fatalf("empty reason must be refused:\n%s", m.View())
	}
	m = typeText(t, m, "wrong template")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("enter with a reason should run the stop")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if gotID != "train-live" || gotReason != "wrong template" {
		t.Fatalf("StopTrain called with (%q, %q)", gotID, gotReason)
	}
	if m.mode != modeNormal {
		t.Fatalf("success should close the overlay, mode=%v", m.mode)
	}
}

func TestTrainStopOnTerminalIgnored(t *testing.T) {
	deps := Deps{StopTrain: func(id, reason string) error { t.Fatal("must not stop"); return nil }}
	m := trainsActionModel(t, deps, trainsSnapshot())
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown}) // → train-done
	m = next.(Model)
	next, _ = m.Update(key("s"))
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("s on a terminal session must be a no-op, mode=%v", m.mode)
	}
}

func TestTrainDeleteOnLiveIgnored(t *testing.T) {
	deps := Deps{DeleteTrain: func(id string) ([]string, error) { t.Fatal("must not delete"); return nil, nil }}
	m := trainsActionModel(t, deps, trainsSnapshot())
	next, _ := m.Update(key("d"))
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("d on a live session must be a no-op, mode=%v", m.mode)
	}
}

func TestTrainDeleteWithoutReposClosesAfterConfirm(t *testing.T) {
	var deleted string
	deps := Deps{DeleteTrain: func(id string) ([]string, error) { deleted = id; return nil, nil }}
	m := trainsActionModel(t, deps, trainsSnapshot())
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown}) // → train-done
	m = next.(Model)
	next, _ = m.Update(key("d"))
	m = next.(Model)
	if m.mode != modeConfirmTrainDelete {
		t.Fatalf("d on a terminal session should confirm, mode=%v", m.mode)
	}
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if deleted != "train-done" {
		t.Fatalf("DeleteTrain called with %q", deleted)
	}
	if m.mode != modeNormal {
		t.Fatalf("no recorded repos → straight back to the list, mode=%v", m.mode)
	}
}

func TestTrainDeleteRefusalStaysInConfirm(t *testing.T) {
	deps := Deps{DeleteTrain: func(id string) ([]string, error) {
		return nil, errors.New("session train-done still holds an active resource lock")
	}}
	m := trainsActionModel(t, deps, trainsSnapshot())
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	next, _ = m.Update(key("d"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeConfirmTrainDelete {
		t.Fatalf("refusal should keep the confirm open, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "active resource lock") {
		t.Fatalf("refusal should render:\n%s", m.View())
	}
}

func TestTrainDeleteOffersRepoCleanupOnlyWhenRecorded(t *testing.T) {
	var deletedRepos []string
	deps := Deps{
		DeleteTrain:     func(id string) ([]string, error) { return []string{"o/eval-repo-1", "o/eval-repo-2"}, nil },
		DeleteTrainRepo: func(repo string) error { deletedRepos = append(deletedRepos, repo); return nil },
	}
	m := trainsActionModel(t, deps, trainsSnapshot())
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	next, _ = m.Update(key("d"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeConfirmTrainRepoCleanup {
		t.Fatalf("recorded repos should trigger the second confirm, mode=%v", m.mode)
	}
	view := m.View()
	if !strings.Contains(view, "o/eval-repo-1") || !strings.Contains(view, "o/eval-repo-2") {
		t.Fatalf("the repos on offer must be listed:\n%s", view)
	}
	next, cmd = m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if len(deletedRepos) != 2 || deletedRepos[0] != "o/eval-repo-1" || deletedRepos[1] != "o/eval-repo-2" {
		t.Fatalf("DeleteTrainRepo calls = %v", deletedRepos)
	}
	if m.mode != modeNormal {
		t.Fatalf("cleanup success should close the overlay, mode=%v", m.mode)
	}
}

func TestTrainRepoCleanupDeclinedKeepsRepos(t *testing.T) {
	deps := Deps{
		DeleteTrain:     func(id string) ([]string, error) { return []string{"o/eval-repo-1"}, nil },
		DeleteTrainRepo: func(repo string) error { t.Fatal("declining must not delete repos"); return nil },
	}
	m := trainsActionModel(t, deps, trainsSnapshot())
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	next, _ = m.Update(key("d"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	next, _ = m.Update(key("n"))
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("declining should close the overlay, mode=%v", m.mode)
	}
}

func TestTrainRepoCleanupRetryOnlyReplaysFailedRepos(t *testing.T) {
	var calls []string
	deps := Deps{
		DeleteTrain: func(id string) ([]string, error) { return []string{"o/repo-ok", "o/repo-bad"}, nil },
		DeleteTrainRepo: func(repo string) error {
			calls = append(calls, repo)
			if repo == "o/repo-bad" {
				return errors.New("transient network error")
			}
			return nil
		},
	}
	m := trainsActionModel(t, deps, trainsSnapshot())
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	next, _ = m.Update(key("d"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	next, cmd = m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeConfirmTrainRepoCleanup {
		t.Fatalf("partial failure should keep the confirm open, mode=%v", m.mode)
	}
	// Retry: only the failed repo may be replayed; the deleted one is gone.
	next, cmd = m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	want := []string{"o/repo-ok", "o/repo-bad", "o/repo-bad"}
	if len(calls) != len(want) {
		t.Fatalf("DeleteTrainRepo calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("DeleteTrainRepo calls = %v, want %v", calls, want)
		}
	}
}

func TestTrainRepoCleanupScopeErrorRendered(t *testing.T) {
	scopeErr := "deleting o/eval-repo-1 requires the delete_repo token scope; run `gh auth refresh -h github.com -s delete_repo` and retry"
	deps := Deps{
		DeleteTrain:     func(id string) ([]string, error) { return []string{"o/eval-repo-1"}, nil },
		DeleteTrainRepo: func(repo string) error { return errors.New(scopeErr) },
	}
	m := trainsActionModel(t, deps, trainsSnapshot())
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	next, _ = m.Update(key("d"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	next, cmd = m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeConfirmTrainRepoCleanup {
		t.Fatalf("repo errors should keep the confirm open, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "delete_repo token scope") {
		t.Fatalf("the scope remedy must render verbatim:\n%s", m.View())
	}
}
