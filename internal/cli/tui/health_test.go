package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func healthSnapshot() Snapshot {
	return Snapshot{
		Daemon: Daemon{
			Running:   true,
			PID:       4242,
			LogFile:   "/home/.gitmoot/logs/daemon.log",
			Flags:     []string{"--workers", "4", "--watch-skillopt-reviews"},
			WorkDir:   "/home/work",
			StartedAt: "2026-06-11T09:00:00Z",
			LogErrors: []string{"2026-06-11 boom: job failed: db locked"},
		},
	}
}

// healthModel returns a sized model on the Health page (the last page).
func healthModel(t *testing.T, deps Deps, snap Snapshot) Model {
	t.Helper()
	if deps.Load == nil {
		deps.Load = func() (Snapshot, error) { return snap, nil }
	}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	for pages[m.selected].page != pageHealth {
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(Model)
		// Landing on Health dispatches the lazy load; run it.
		if cmd != nil {
			if msg := cmd(); msg != nil {
				next, _ = m.Update(msg)
				m = next.(Model)
			}
		}
	}
	return m
}

func TestHealthPageRendersDaemonAndChecks(t *testing.T) {
	deps := Deps{
		HealthChecks: func() ([]HealthCheck, error) {
			return []HealthCheck{
				{Name: "gh", Status: "ok", Detail: "gh version 2.45", Required: true},
				{Name: "claude auth", Status: "warn", Detail: "token unset", Required: false},
			}, nil
		},
	}
	m := healthModel(t, deps, healthSnapshot())
	view := m.View()
	for _, want := range []string{
		"daemon", "running", "pid 4242",
		"--workers 4 --watch-skillopt-reviews", "/home/work",
		"recent log errors", "job failed: db locked",
		"environment", "gh", "claude auth",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("health view missing %q:\n%s", want, view)
		}
	}
}

func TestHealthChecksLoadOnceAndRefetchOnR(t *testing.T) {
	calls := 0
	deps := Deps{
		HealthChecks: func() ([]HealthCheck, error) {
			calls++
			return []HealthCheck{{Name: "gh", Status: "ok", Required: true}}, nil
		},
	}
	m := healthModel(t, deps, healthSnapshot())
	if calls != 1 {
		t.Fatalf("expected one lazy load on first open, got %d", calls)
	}
	// Tabbing away and back must NOT reload (cache).
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(Model)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = next.(Model)
	if cmd != nil {
		cmd()
	}
	if calls != 1 {
		t.Fatalf("revisiting Health must reuse the cache, got %d calls", calls)
	}
	// r forces a re-run.
	next, cmd = m.Update(key("r"))
	m = next.(Model)
	if cmd != nil {
		drainHealthCmd(cmd)
	}
	if calls != 2 {
		t.Fatalf("r should re-run the checks, got %d calls", calls)
	}
}

func TestHealthChecksErrorRenders(t *testing.T) {
	deps := Deps{HealthChecks: func() ([]HealthCheck, error) { return nil, errors.New("doctor exploded") }}
	m := healthModel(t, deps, healthSnapshot())
	if !strings.Contains(m.View(), "doctor exploded") {
		t.Fatalf("health error should render:\n%s", m.View())
	}
}

func TestAttentionCrossLinkOnRequiredFailure(t *testing.T) {
	snap := healthSnapshot()
	deps := Deps{
		Load: func() (Snapshot, error) { return snap, nil },
		HealthChecks: func() ([]HealthCheck, error) {
			return []HealthCheck{{Name: "git", Status: "fail", Required: true}}, nil
		},
	}
	m := sizedModel(deps)
	next, _ := m.Update(snapshotMsg{snap: snap, at: time.Unix(1, 0)})
	m = next.(Model)
	// Before health loads, no cross-link.
	if strings.Contains(m.View(), "see the Health page") {
		t.Fatal("cross-link must not appear before health is loaded")
	}
	// Visit Health to load the checks, then return to Attention.
	m = healthModel(t, deps, snap)
	for pages[m.selected].page != pageAttention {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = next.(Model)
	}
	if !strings.Contains(m.View(), "environment problem — see the Health page") {
		t.Fatalf("a failed required check should cross-link on Attention:\n%s", m.View())
	}
}

func TestHealthStartDaemonKey(t *testing.T) {
	started := false
	snap := healthSnapshot()
	snap.Daemon.Running = false
	deps := Deps{
		Load:         func() (Snapshot, error) { return snap, nil },
		HealthChecks: func() ([]HealthCheck, error) { return nil, nil },
		StartDaemon:  func() error { started = true; return nil },
	}
	m := healthModel(t, deps, snap)
	next, cmd := m.Update(key("s"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("s should start the daemon from the Health page")
	}
	cmd()
	if !started {
		t.Fatal("StartDaemon should have been called")
	}
}

// drainHealthCmd runs a (possibly batched) cmd and feeds any healthChecksMsg
// nowhere — the test only cares the dep was invoked.
func drainHealthCmd(cmd tea.Cmd) {
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			if sub != nil {
				sub()
			}
		}
	}
}

func TestHealthStartDaemonShowsInFlightAndError(t *testing.T) {
	snap := healthSnapshot()
	snap.Daemon.Running = false
	deps := Deps{
		Load:         func() (Snapshot, error) { return snap, nil },
		HealthChecks: func() ([]HealthCheck, error) { return nil, nil },
		StartDaemon:  func() error { return errors.New("spawn failed") },
	}
	m := healthModel(t, deps, snap)
	next, cmd := m.Update(key("s"))
	m = next.(Model)
	if !strings.Contains(m.View(), "starting…") {
		t.Fatalf("Health should show the start in flight:\n%s", m.View())
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if !strings.Contains(m.View(), "spawn failed") {
		t.Fatalf("a daemon start failure should render on Health:\n%s", m.View())
	}
}

func TestRenderHealthChecksGroupsByScope(t *testing.T) {
	out := renderHealthChecks([]HealthCheck{
		{Name: "git", Status: "ok", Required: true},
		{Name: "gh auth", Status: "ok", Required: true},
		{Name: "repo remote", Status: "ok", Required: true, Scope: "owner/a"},
		{Name: "base branch", Status: "ok", Required: true, Scope: "owner/a"},
		{Name: "checkout", Status: "warn", Required: false, Scope: "owner/b"},
	})
	for _, want := range []string{"git", "gh auth", "owner/a", "repo remote", "base branch", "owner/b", "checkout"} {
		if !strings.Contains(out, want) {
			t.Fatalf("renderHealthChecks output missing %q:\n%s", want, out)
		}
	}
	// The global block (git) must appear before the first repo scope header.
	if strings.Index(out, "git") > strings.Index(out, "owner/a") {
		t.Fatalf("global checks should render before repo scopes:\n%s", out)
	}
	// owner/a's checks render under its header, before owner/b's header.
	if strings.Index(out, "owner/a") > strings.Index(out, "owner/b") {
		t.Fatalf("repo scopes should render in first-seen order:\n%s", out)
	}
}
