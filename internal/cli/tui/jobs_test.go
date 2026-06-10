package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func jobsSnapshot() Snapshot {
	return Snapshot{
		Daemon: Daemon{Running: true},
		JobRows: []JobRow{
			{ID: "j-failed", Agent: "planner", Type: "ask", State: "failed", LatestEvent: "boom"},
			{ID: "j-running", Agent: "planner", Type: "implement", State: "running"},
			{ID: "j-done", Agent: "planner", Type: "review", State: "succeeded"},
		},
	}
}

func jobsModel(t *testing.T, deps Deps, snap Snapshot) Model {
	t.Helper()
	if deps.Load == nil {
		deps.Load = func() (Snapshot, error) { return snap, nil }
	}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	// Tab to the Jobs page (Attention → Trains → Agents → Sessions → Jobs).
	for i := 0; i < 4; i++ {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(Model)
	}
	if pages[m.selected].page != pageJobs {
		t.Fatalf("expected Jobs page, got %v", pages[m.selected].page)
	}
	return m
}

func TestJobsPageDetailLoadsEvents(t *testing.T) {
	var asked string
	deps := Deps{JobEvents: func(id string) ([]JobEventView, error) {
		asked = id
		return []JobEventView{{Kind: "failed", Message: "boom"}}, nil
	}}
	m := jobsModel(t, deps, jobsSnapshot())
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if m.mode != modeJobDetail || cmd == nil {
		t.Fatalf("enter should open the job detail, mode=%v", m.mode)
	}
	next, _ = m.Update(cmd()) // jobEventsMsg
	m = next.(Model)
	if asked != "j-failed" {
		t.Fatalf("JobEvents asked for %q", asked)
	}
	if !strings.Contains(m.View(), "boom") {
		t.Fatalf("event message missing:\n%s", m.View())
	}
}

func TestJobsRetryConfirmFlow(t *testing.T) {
	var retried string
	deps := Deps{RetryJob: func(id string) error { retried = id; return nil }}
	m := jobsModel(t, deps, jobsSnapshot())
	// Cursor on j-failed (first row); R opens the confirm.
	next, _ := m.Update(key("R"))
	m = next.(Model)
	if m.mode != modeConfirmJobRetry {
		t.Fatalf("R should confirm retry, mode=%v", m.mode)
	}
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("y should run the retry")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if retried != "j-failed" {
		t.Fatalf("RetryJob called with %q", retried)
	}
	if m.mode != modeNormal {
		t.Fatalf("success should close the confirm, mode=%v", m.mode)
	}
}

func TestJobsCancelRunningShowsCancelling(t *testing.T) {
	var cancelled string
	snap := jobsSnapshot()
	deps := Deps{
		Load:      func() (Snapshot, error) { return snap, nil },
		CancelJob: func(id string) error { cancelled = id; return nil },
	}
	m := jobsModel(t, deps, snap)
	// Move to the running job (row 2).
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	next, _ = m.Update(key("c"))
	m = next.(Model)
	if m.mode != modeConfirmJobCancel {
		t.Fatalf("c should confirm cancel, mode=%v", m.mode)
	}
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if cancelled != "j-running" {
		t.Fatalf("CancelJob called with %q", cancelled)
	}
	if !strings.Contains(m.View(), "cancelling…") {
		t.Fatalf("running row should show cancelling…:\n%s", m.View())
	}
	// Once a refresh shows the job settled, the transitional label clears.
	settled := jobsSnapshot()
	settled.JobRows[1].State = "cancelled"
	next, _ = m.Update(snapshotMsg{snap: settled, at: time.Unix(2, 0)})
	m = next.(Model)
	if strings.Contains(m.View(), "cancelling…") {
		t.Fatalf("settled job should drop the cancelling label:\n%s", m.View())
	}
}

func TestJobsRetryOnNonRetryableIgnored(t *testing.T) {
	deps := Deps{RetryJob: func(id string) error { t.Fatal("must not retry"); return nil }}
	m := jobsModel(t, deps, jobsSnapshot())
	// Move to the succeeded job (row 3).
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	next, _ = m.Update(key("R"))
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("R on a succeeded job must be a no-op, mode=%v", m.mode)
	}
}

func TestAttentionJobRowsActionable(t *testing.T) {
	var retried string
	snap := jobsSnapshot()
	deps := Deps{
		Load:     func() (Snapshot, error) { return snap, nil },
		RetryJob: func(id string) error { retried = id; return nil },
	}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	// Attention page: no prompts, so the first item is the failed job.
	view := m.View()
	if !strings.Contains(view, "job j-failed") || !strings.Contains(view, "boom") {
		t.Fatalf("attention should list the failed job with its reason:\n%s", view)
	}
	next, _ = m.Update(key("R"))
	m = next.(Model)
	if m.mode != modeConfirmJobRetry {
		t.Fatalf("R on an attention job row should confirm retry, mode=%v", m.mode)
	}
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	_, _ = m.Update(cmd())
	if retried != "j-failed" {
		t.Fatalf("RetryJob called with %q", retried)
	}
}

func TestAttentionDaemonStart(t *testing.T) {
	started := false
	snap := jobsSnapshot()
	snap.Daemon.Running = false
	deps := Deps{
		Load:        func() (Snapshot, error) { return snap, nil },
		StartDaemon: func() error { started = true; return nil },
	}
	m := sizedModel(deps)
	// Before the first snapshot the daemon state is unknown; s must be inert.
	if _, cmd := m.Update(key("s")); cmd != nil {
		t.Fatal("s before the first snapshot must not start the daemon")
	}
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	if !strings.Contains(m.View(), "daemon stopped") {
		t.Fatalf("expected the daemon-stopped banner:\n%s", m.View())
	}
	next, cmd := m.Update(key("s"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("s should start the daemon")
	}
	if !strings.Contains(m.View(), "starting…") {
		t.Fatalf("banner should show the start is in flight:\n%s", m.View())
	}
	// A second press while in flight must not fire again.
	next, second := m.Update(key("s"))
	m = next.(Model)
	if second != nil {
		t.Fatal("s while a start is in flight must be a no-op")
	}
	msg := cmd()
	if !started {
		t.Fatal("StartDaemon should have been called")
	}
	next, _ = m.Update(msg)
	m = next.(Model)
	if m.daemonBusy {
		t.Fatal("daemonStartMsg should clear the in-flight flag")
	}
}

func TestDaemonStartResultDoesNotCloseJobConfirm(t *testing.T) {
	snap := jobsSnapshot()
	snap.Daemon.Running = false
	deps := Deps{
		Load:        func() (Snapshot, error) { return snap, nil },
		StartDaemon: func() error { return errors.New("spawn failed") },
		RetryJob:    func(id string) error { return nil },
	}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	next, daemonCmd := m.Update(key("s"))
	m = next.(Model)
	// Open a retry confirm while the daemon start is still in flight.
	next, _ = m.Update(key("R"))
	m = next.(Model)
	if m.mode != modeConfirmJobRetry {
		t.Fatalf("expected the retry confirm, mode=%v", m.mode)
	}
	next, _ = m.Update(daemonCmd())
	m = next.(Model)
	if m.mode != modeConfirmJobRetry {
		t.Fatalf("the daemon result must not close the job confirm, mode=%v", m.mode)
	}
	if strings.Contains(m.View(), "spawn failed") {
		t.Fatalf("the daemon error must not render inside the job confirm:\n%s", m.View())
	}
}

func TestJobDetailZeroEventsShowsNoEvents(t *testing.T) {
	deps := Deps{JobEvents: func(id string) ([]JobEventView, error) { return nil, nil }}
	m := jobsModel(t, deps, jobsSnapshot())
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if !strings.Contains(m.View(), "loading…") {
		t.Fatalf("detail should show loading before the events arrive:\n%s", m.View())
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if !strings.Contains(m.View(), "no events") {
		t.Fatalf("an empty (loaded) history should say so, not keep loading:\n%s", m.View())
	}
}

func TestJobConfirmStaysOpenWhileActionInFlight(t *testing.T) {
	deps := Deps{RetryJob: func(id string) error { return nil }}
	m := jobsModel(t, deps, jobsSnapshot())
	next, _ := m.Update(key("R"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	// esc while the retry is in flight must not close the confirm, or its
	// eventual error would be dropped silently.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.mode != modeConfirmJobRetry {
		t.Fatalf("confirm must stay open while busy, mode=%v", m.mode)
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("success should close the confirm, mode=%v", m.mode)
	}
}

func TestJobActionErrorKeepsConfirm(t *testing.T) {
	deps := Deps{RetryJob: func(id string) error { return errors.New("db locked") }}
	m := jobsModel(t, deps, jobsSnapshot())
	next, _ := m.Update(key("R"))
	m = next.(Model)
	next, cmd := m.Update(key("y"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeConfirmJobRetry {
		t.Fatalf("error should keep the confirm open, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "db locked") {
		t.Fatalf("error should render:\n%s", m.View())
	}
}
