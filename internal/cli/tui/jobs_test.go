package tui

import (
	"errors"
	"strconv"
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
			{ID: "j-cancelled", Agent: "planner", Type: "ask", State: "cancelled", LatestEvent: "user cancelled"},
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

func TestFormatJobTime(t *testing.T) {
	cases := map[string]string{
		"2026-06-11 14:30:05":  "06-11 14:30",
		"2026-01-02T15:04:05Z": "01-02 15:04",
		"":                     "-",
		"not a date":           "not a date",
	}
	for in, want := range cases {
		if got := formatJobTime(in); got != want {
			t.Fatalf("formatJobTime(%q) = %q, want %q", in, got, want)
		}
	}
	// An unparseable but long value is trimmed, not echoed whole.
	if got := formatJobTime("2026-06-11-garbage-tail-xxxxxxxx"); len(got) != 16 {
		t.Fatalf("long unparseable value should trim to 16 chars, got %q", got)
	}
}

func TestJobsRowShowsTime(t *testing.T) {
	snap := jobsSnapshot()
	snap.JobRows[0].UpdatedAt = "2026-06-11 14:30:05"
	m := jobsModel(t, Deps{}, snap)
	if !strings.Contains(m.View(), "06-11 14:30") {
		t.Fatalf("jobs row should show the formatted time:\n%s", m.View())
	}
}

func TestJobsListWindowsLongList(t *testing.T) {
	snap := Snapshot{Daemon: Daemon{Running: true}}
	for i := 0; i < 100; i++ {
		snap.JobRows = append(snap.JobRows, JobRow{
			ID: "job-" + strconv.Itoa(i), Agent: "planner", Type: "ask", State: "succeeded",
		})
	}
	m := jobsModel(t, Deps{}, snap)
	cap := jobsWindowCap(m.height)
	if cap >= 100 {
		t.Fatalf("test needs a window smaller than the list; cap=%d", cap)
	}
	// Cursor at top: a "more" marker, no "earlier", and the last row is hidden.
	view := m.View()
	if !strings.Contains(view, "more") || strings.Contains(view, "earlier") {
		t.Fatalf("top of a long list should show only a 'more' marker:\n%s", view)
	}
	if strings.Contains(view, "job-99 ") {
		t.Fatalf("the far end should not render while the cursor is at the top")
	}
	// Drive the cursor to the last job; it must stay visible (window follows).
	for i := 0; i < 99; i++ {
		next, _ := m.Update(key("j"))
		m = next.(Model)
	}
	view = m.View()
	if !strings.Contains(view, "job-99") {
		t.Fatalf("the selected last job must be visible after scrolling:\n%s", view)
	}
	if !strings.Contains(view, "earlier") {
		t.Fatalf("the bottom of a long list should show an 'earlier' marker:\n%s", view)
	}
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
	m = drainBatch(t, m, cmd) // events + detail load
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

func TestJobsBugReportPreviewCreateFlow(t *testing.T) {
	var previewed, created string
	deps := Deps{
		BugReportPreview: func(id string) (BugReportPreview, error) {
			previewed = id
			return BugReportPreview{
				Title:       "Gitmoot failed job ask for planner",
				Body:        "<!-- gitmoot:dashboard-report fingerprint:abc123 -->\n\nredacted body",
				Labels:      []string{"gitmoot-dashboard-report", "bug"},
				Fingerprint: "abc123",
			}, nil
		},
		CreateBugReport: func(id string, preview BugReportPreview) (BugReportCreateResult, error) {
			created = id
			if preview.Fingerprint != "abc123" || !strings.Contains(preview.Body, "redacted body") {
				t.Fatalf("CreateBugReport received preview = %+v", preview)
			}
			return BugReportCreateResult{URL: "https://github.com/jerryfane/gitmoot/issues/777"}, nil
		},
	}
	m := jobsModel(t, deps, jobsSnapshot())
	if !strings.Contains(m.View(), "B report bug") {
		t.Fatalf("failed selected job should advertise report action:\n%s", m.View())
	}
	next, cmd := m.Update(key("B"))
	m = next.(Model)
	if m.mode != modeBugReportPreview || cmd == nil {
		t.Fatalf("B should open bug report preview, mode=%v", m.mode)
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if previewed != "j-failed" {
		t.Fatalf("BugReportPreview called with %q", previewed)
	}
	view := m.View()
	for _, want := range []string{"bug report preview", "Gitmoot failed job", "gitmoot-dashboard-report, bug", "abc123", "redacted body"} {
		if !strings.Contains(view, want) {
			t.Fatalf("preview missing %q:\n%s", want, view)
		}
	}

	next, cmd = m.Update(key("g"))
	m = next.(Model)
	if cmd == nil || !m.actionBusy {
		t.Fatalf("g should start issue creation, busy=%v cmd=%v", m.actionBusy, cmd)
	}
	if got := m.footerHelp(); strings.Contains(got, "esc back") || !strings.Contains(got, "creating issue") {
		t.Fatalf("busy footer help = %q, want creating state without esc back", got)
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if created != "j-failed" {
		t.Fatalf("CreateBugReport called with %q", created)
	}
	if m.mode != modeBugReportPreview || !strings.Contains(m.View(), "https://github.com/jerryfane/gitmoot/issues/777") {
		t.Fatalf("success should keep preview open with URL:\n%s", m.View())
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.mode != modeNormal {
		t.Fatalf("esc should return to dashboard, mode=%v", m.mode)
	}
}

func TestJobsBugReportCreateErrorKeepsPreview(t *testing.T) {
	deps := Deps{
		BugReportPreview: func(id string) (BugReportPreview, error) {
			return BugReportPreview{Title: "draft", Body: "body"}, nil
		},
		CreateBugReport: func(id string, preview BugReportPreview) (BugReportCreateResult, error) {
			return BugReportCreateResult{}, errors.New("github unavailable")
		},
	}
	m := jobsModel(t, deps, jobsSnapshot())
	next, cmd := m.Update(key("B"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	next, cmd = m.Update(key("g"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeBugReportPreview {
		t.Fatalf("creation error should keep preview open, mode=%v", m.mode)
	}
	if !strings.Contains(m.View(), "github unavailable") {
		t.Fatalf("creation error should render inline:\n%s", m.View())
	}
}

func TestJobsBugReportExistingIssueLabel(t *testing.T) {
	deps := Deps{
		BugReportPreview: func(id string) (BugReportPreview, error) {
			return BugReportPreview{Title: "draft", Body: "body", Fingerprint: "abc123"}, nil
		},
		CreateBugReport: func(id string, preview BugReportPreview) (BugReportCreateResult, error) {
			return BugReportCreateResult{URL: "https://github.com/jerryfane/gitmoot/issues/777", Existing: true}, nil
		},
	}
	m := jobsModel(t, deps, jobsSnapshot())
	next, cmd := m.Update(key("B"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	next, cmd = m.Update(key("g"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	view := m.View()
	if !strings.Contains(view, "existing: https://github.com/jerryfane/gitmoot/issues/777") {
		t.Fatalf("existing issue should be labeled distinctly:\n%s", view)
	}
	if strings.Contains(view, "created: https://github.com/jerryfane/gitmoot/issues/777") {
		t.Fatalf("existing issue must not be labeled created:\n%s", view)
	}
}

func TestJobsBugReportCreateResultNotDroppedAfterEsc(t *testing.T) {
	deps := Deps{
		BugReportPreview: func(id string) (BugReportPreview, error) {
			return BugReportPreview{Title: "draft", Body: "body", Fingerprint: "abc123"}, nil
		},
		CreateBugReport: func(id string, preview BugReportPreview) (BugReportCreateResult, error) {
			return BugReportCreateResult{URL: "https://github.com/jerryfane/gitmoot/issues/778"}, nil
		},
	}
	m := jobsModel(t, deps, jobsSnapshot())
	next, cmd := m.Update(key("B"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	next, cmd = m.Update(key("g"))
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if m.mode != modeBugReportPreview {
		t.Fatalf("esc while create is in flight must keep preview open, mode=%v", m.mode)
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if !strings.Contains(m.View(), "created: https://github.com/jerryfane/gitmoot/issues/778") {
		t.Fatalf("create result should still render after esc:\n%s", m.View())
	}
}

func TestJobsBugReportBuildErrorKeepsPreview(t *testing.T) {
	deps := Deps{BugReportPreview: func(id string) (BugReportPreview, error) {
		return BugReportPreview{}, errors.New("payload malformed")
	}}
	m := jobsModel(t, deps, jobsSnapshot())
	next, cmd := m.Update(key("B"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.mode != modeBugReportPreview || !strings.Contains(m.View(), "payload malformed") {
		t.Fatalf("build error should render in preview mode:\n%s", m.View())
	}
	next, cmd = m.Update(key("g"))
	if cmd != nil {
		t.Fatal("g must not create when preview build failed")
	}
}

func TestJobsBugReportPreviewWithoutCreateDepDoesNotAdvertiseCreate(t *testing.T) {
	deps := Deps{BugReportPreview: func(id string) (BugReportPreview, error) {
		return BugReportPreview{Title: "draft", Body: "body"}, nil
	}}
	m := jobsModel(t, deps, jobsSnapshot())
	next, cmd := m.Update(key("B"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if strings.Contains(m.footerHelp(), "g create issue") {
		t.Fatalf("missing create dep must not advertise g create issue: %q", m.footerHelp())
	}
	next, cmd = m.Update(key("g"))
	m = next.(Model)
	if cmd != nil || m.actionBusy || m.actionErr != "" {
		t.Fatalf("g without create dep should be inert, busy=%v err=%q cmd=%v", m.actionBusy, m.actionErr, cmd)
	}
}

func TestJobsBugReportIgnoredForNonReportableJob(t *testing.T) {
	deps := Deps{
		BugReportPreview: func(id string) (BugReportPreview, error) {
			t.Fatalf("must not build report for non-reportable job %s", id)
			return BugReportPreview{}, nil
		},
	}
	m := jobsModel(t, deps, jobsSnapshot())
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	if strings.Contains(m.View(), "B report bug") {
		t.Fatalf("running selected job must not advertise report action:\n%s", m.View())
	}
	next, cmd := m.Update(key("B"))
	m = next.(Model)
	if cmd != nil || m.mode != modeNormal {
		t.Fatalf("B on running job should be a no-op, mode=%v cmd=%v", m.mode, cmd)
	}
}

func TestAttentionCancelledJobReportable(t *testing.T) {
	var previewed string
	snap := Snapshot{
		Daemon:  Daemon{Running: true},
		JobRows: []JobRow{{ID: "j-cancelled", Agent: "planner", Type: "ask", State: "cancelled", LatestEvent: "user cancelled"}},
	}
	deps := Deps{
		Load: func() (Snapshot, error) { return snap, nil },
		BugReportPreview: func(id string) (BugReportPreview, error) {
			previewed = id
			return BugReportPreview{Title: "cancelled draft", Body: "body"}, nil
		},
	}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	if !strings.Contains(m.View(), "job j-cancelled") || !strings.Contains(m.View(), "B report bug") {
		t.Fatalf("cancelled attention job should be visible and reportable:\n%s", m.View())
	}
	next, cmd := m.Update(key("B"))
	m = next.(Model)
	next, _ = m.Update(cmd())
	m = next.(Model)
	if previewed != "j-cancelled" || m.mode != modeBugReportPreview {
		t.Fatalf("cancelled job previewed=%q mode=%v", previewed, m.mode)
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
	m = drainBatch(t, m, cmd)
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

func drainBatch(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			if sub != nil {
				next, _ := m.Update(sub())
				m = next.(Model)
			}
		}
		return m
	}
	next, _ := m.Update(msg)
	return next.(Model)
}

func TestJobDetailShowsRequestAndResult(t *testing.T) {
	deps := Deps{
		JobEvents: func(id string) ([]JobEventView, error) {
			return []JobEventView{{Kind: "failed", Message: "boom"}}, nil
		},
		JobDetail: func(id string) (JobDetail, error) {
			return JobDetail{Repo: "o/r", PullRequest: 7, Request: "Plan the auth refactor.",
				ResultDecision: "failed", ResultSummary: "the checkout is read-only"}, nil
		},
	}
	m := jobsModel(t, deps, jobsSnapshot())
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	m = drainBatch(t, m, cmd)
	view := m.View()
	for _, want := range []string{"repo", "o/r", "#7", "request", "Plan the auth refactor", "result", "decision", "failed", "read-only", "events"} {
		if !strings.Contains(view, want) {
			t.Fatalf("job detail missing %q:\n%s", want, view)
		}
	}
}

func TestJobDetailOmitsBlocksWhenAbsent(t *testing.T) {
	deps := Deps{
		JobEvents: func(id string) ([]JobEventView, error) { return nil, nil },
		JobDetail: func(id string) (JobDetail, error) { return JobDetail{}, nil },
	}
	m := jobsModel(t, deps, jobsSnapshot())
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	m = drainBatch(t, m, cmd)
	view := m.View()
	if strings.Contains(view, "request") || strings.Contains(view, "result") {
		t.Fatalf("a job without payload content should omit request/result blocks:\n%s", view)
	}
}

func TestJobDetailClampLines(t *testing.T) {
	got := clampLines("a\nb\nc\nd\ne", 3)
	if !strings.Contains(got, "a\nb\nc") || !strings.Contains(got, "2 more lines") {
		t.Fatalf("clampLines = %q", got)
	}
	// Within the cap, newlines are preserved verbatim (not collapsed).
	if clampLines("one\ntwo", 5) != "one\ntwo" {
		t.Fatalf("clampLines should preserve newlines under the cap")
	}
}
