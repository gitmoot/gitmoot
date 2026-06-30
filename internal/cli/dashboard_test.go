package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/cli/style"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

func dashboardTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	return home
}

func seedDashboardPrompt(t *testing.T, home, id, question string, choices []string) {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertInteractivePrompt(context.Background(), db.InteractivePrompt{
		ID:            id,
		Question:      question,
		Choices:       choices,
		Required:      true,
		AnswerFormat:  "text",
		SourceCommand: "test",
	}); err != nil {
		t.Fatalf("UpsertInteractivePrompt returned error: %v", err)
	}
}

func TestDashboardSnapshotRendersSections(t *testing.T) {
	home := dashboardTestHome(t)
	seedDashboardPrompt(t, home, "dash.prompt.one", "Pick a value", nil)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("dashboard exit code = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"daemon: stopped",
		"repos: 0",
		"agents: 0",
		"runtime_sessions: 0",
		"jobs: 0",
		"branch_locks: 0",
		"train_sessions: 0",
		"pending_prompts: 1",
		"dash.prompt.one\tPick a value",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dashboard output missing %q:\n%s", want, out)
		}
	}
}

func TestDashboardJSONPromptsMatchInteractiveList(t *testing.T) {
	home := dashboardTestHome(t)
	seedDashboardPrompt(t, home, "dash.prompt.alpha", "Alpha?", nil)
	seedDashboardPrompt(t, home, "dash.prompt.beta", "Beta?", []string{"x", "y"})

	var dashOut, dashErr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home, "--json"}, &dashOut, &dashErr); code != 0 {
		t.Fatalf("dashboard --json exit code = %d, stderr=%s", code, dashErr.String())
	}
	var snapshot dashboardSnapshot
	if err := json.Unmarshal(dashOut.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode dashboard snapshot: %v\n%s", err, dashOut.String())
	}
	dashIDs := map[string]bool{}
	for _, prompt := range snapshot.PendingPrompts {
		dashIDs[prompt.ID] = true
	}

	var listOut, listErr bytes.Buffer
	if code := Run([]string{"interactive", "list", "--home", home, "--state", "pending", "--json"}, &listOut, &listErr); code != 0 {
		t.Fatalf("interactive list exit code = %d, stderr=%s", code, listErr.String())
	}
	var listPrompts []db.InteractivePrompt
	if err := json.Unmarshal(listOut.Bytes(), &listPrompts); err != nil {
		t.Fatalf("decode interactive list: %v\n%s", err, listOut.String())
	}
	if len(listPrompts) != len(snapshot.PendingPrompts) {
		t.Fatalf("dashboard pending prompts (%d) != interactive list (%d)", len(snapshot.PendingPrompts), len(listPrompts))
	}
	for _, prompt := range listPrompts {
		if !dashIDs[prompt.ID] {
			t.Fatalf("interactive list prompt %q missing from dashboard: %+v", prompt.ID, snapshot.PendingPrompts)
		}
	}
}

func TestDashboardAnswerResolvesPromptThroughSharedAPI(t *testing.T) {
	home := dashboardTestHome(t)
	seedDashboardPrompt(t, home, "dash.prompt.answerable", "Choose", []string{"keep", "drop"})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"dashboard", "--home", home, "--answer", "dash.prompt.answerable", "--value", "keep", "--source", "test"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dashboard --answer exit code = %d, stderr=%s", code, stderr.String())
	}
	// The snapshot after answering shows no pending prompts.
	if !strings.Contains(stdout.String(), "pending_prompts: 0") {
		t.Fatalf("answered prompt should not remain pending:\n%s", stdout.String())
	}
	// The prompt is resolved through the same store API interactive answer uses.
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	prompt, err := store.GetInteractivePrompt(context.Background(), "dash.prompt.answerable")
	if err != nil {
		t.Fatalf("GetInteractivePrompt returned error: %v", err)
	}
	if prompt.State != db.InteractivePromptStateResolved || prompt.AnswerValue != "keep" || prompt.AnswerSource != "test" {
		t.Fatalf("prompt not resolved via shared API: %+v", prompt)
	}
}

func TestDashboardAnswerRejectsInvalidChoiceAndKeepsPrompt(t *testing.T) {
	home := dashboardTestHome(t)
	seedDashboardPrompt(t, home, "dash.prompt.choice", "Choose", []string{"keep", "drop"})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"dashboard", "--home", home, "--answer", "dash.prompt.choice", "--value", "bogus"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("invalid dashboard answer exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	prompt, err := store.GetInteractivePrompt(context.Background(), "dash.prompt.choice")
	if err != nil {
		t.Fatalf("GetInteractivePrompt returned error: %v", err)
	}
	if prompt.State != db.InteractivePromptStatePending {
		t.Fatalf("invalid answer should leave prompt pending: %+v", prompt)
	}
}

func TestDashboardWithoutAnswerDoesNotMutate(t *testing.T) {
	home := dashboardTestHome(t)
	seedDashboardPrompt(t, home, "dash.prompt.untouched", "Choose", nil)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("dashboard exit code = %d, stderr=%s", code, stderr.String())
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	prompt, err := store.GetInteractivePrompt(context.Background(), "dash.prompt.untouched")
	if err != nil {
		t.Fatalf("GetInteractivePrompt returned error: %v", err)
	}
	if prompt.State != db.InteractivePromptStatePending {
		t.Fatalf("dashboard without --answer must not resolve prompts: %+v", prompt)
	}
}

func TestDashboardDismissDeletesPrompt(t *testing.T) {
	home := dashboardTestHome(t)
	seedDashboardPrompt(t, home, "dash.prompt.dismiss", "Choose", nil)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"dashboard", "--home", home, "--dismiss", "dash.prompt.dismiss"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dashboard --dismiss exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pending_prompts: 0") {
		t.Fatalf("dismissed prompt should not remain pending:\n%s", stdout.String())
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	prompts, err := store.ListInteractivePrompts(context.Background(), "")
	if err != nil {
		t.Fatalf("ListInteractivePrompts returned error: %v", err)
	}
	if len(prompts) != 0 {
		t.Fatalf("dismiss should delete the prompt entirely: %+v", prompts)
	}
}

func TestDashboardDismissMissingPromptFails(t *testing.T) {
	home := dashboardTestHome(t)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"dashboard", "--home", home, "--dismiss", "ghost"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("dashboard --dismiss missing code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestDashboardAttentionBlock(t *testing.T) {
	home := dashboardTestHome(t)
	seedDashboardPrompt(t, home, "attn.prompt", "Pick", nil)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("dashboard exit = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"needs attention:",
		"prompt attn.prompt",
		"gitmoot interactive answer --home " + home + " attn.prompt <value>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("attention block missing %q:\n%s", want, out)
		}
	}
}

func TestDashboardStyledRendering(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "")
	home := dashboardTestHome(t)
	seedDashboardPrompt(t, home, "styled.prompt", "Pick", nil)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("dashboard exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "\x1b[") {
		t.Fatalf("expected ANSI styling with CLICOLOR_FORCE:\n%q", stdout.String())
	}
}

func TestDashboardAnswerCommand(t *testing.T) {
	if got := dashboardAnswerCommand("/h", "p1"); got != "gitmoot interactive answer --home /h p1 <value>" {
		t.Fatalf("with home = %q", got)
	}
	if got := dashboardAnswerCommand("", "p1"); got != "gitmoot interactive answer p1 <value>" {
		t.Fatalf("no home = %q", got)
	}
}

func TestDashboardTruncate(t *testing.T) {
	items := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if shown, hidden := dashboardTruncate(style.Enabled(), false, items); len(shown) != dashboardListCap || hidden != 2 {
		t.Fatalf("styled truncate = %d shown, %d hidden", len(shown), hidden)
	}
	if shown, hidden := dashboardTruncate(style.Enabled(), true, items); len(shown) != 10 || hidden != 0 {
		t.Fatalf("--all should keep all: %d, %d", len(shown), hidden)
	}
	if shown, hidden := dashboardTruncate(style.Disabled(), false, items); len(shown) != 10 || hidden != 0 {
		t.Fatalf("plain mode keeps all: %d, %d", len(shown), hidden)
	}
}

func TestGroupedRuntimeSessions(t *testing.T) {
	sessions := []dashboardSession{
		{Name: "skillopt-generator-bg-aaa", Runtime: "codex", State: "idle"},
		{Name: "skillopt-generator-bg-bbb", Runtime: "codex", State: "idle"},
		{Name: "skillopt-generator-bg-ccc", Runtime: "codex", State: "running"},
		{Name: "planner", Runtime: "codex", Repo: "owner/repo", State: "idle"},
	}
	lines := groupedRuntimeSessions(sessions)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "skillopt-generator [codex] ×2 idle") {
		t.Fatalf("missing grouped idle ×2:\n%s", joined)
	}
	if !strings.Contains(joined, "skillopt-generator [codex] ×1 running") {
		t.Fatalf("missing grouped running ×1:\n%s", joined)
	}
	if !strings.Contains(joined, "planner [codex] owner/repo idle") {
		t.Fatalf("ungrouped single missing:\n%s", joined)
	}
}

func TestDashboardLockStale(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	past := "2026-06-10T11:00:00.000000000Z"
	future := "2026-06-10T13:00:00.000000000Z"
	if !dashboardLockStale(past, now) {
		t.Fatalf("past expiry should be stale")
	}
	if dashboardLockStale(future, now) {
		t.Fatalf("future expiry should not be stale")
	}
	if dashboardLockStale("not-a-time", now) {
		t.Fatalf("unparseable expiry should not be stale")
	}
}

func TestDashboardWatchRejectsInvalidCombos(t *testing.T) {
	home := dashboardTestHome(t)
	cases := [][]string{
		{"dashboard", "--home", home, "--watch", "--json"},
		{"dashboard", "--home", home, "--watch", "--answer", "p1", "--value", "x"},
		{"dashboard", "--home", home, "--watch", "--dismiss", "p1"},
		{"dashboard", "--home", home, "--watch"}, // stdout is a bytes.Buffer, not a terminal
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("Run(%v) = %d, want 2; stderr=%s", args, code, stderr.String())
		}
	}
}

func TestDashboardWatchFrame(t *testing.T) {
	body := []byte("home: /h\n")
	first := dashboardWatchFrame(body, true)
	if !bytes.HasPrefix(first, []byte("\x1b[2J\x1b[H\x1b[0J")) || !bytes.Contains(first, body) {
		t.Fatalf("first frame = %q", first)
	}
	next := dashboardWatchFrame(body, false)
	if bytes.Contains(next, []byte("\x1b[2J")) {
		t.Fatalf("non-first frame should not clear the whole screen: %q", next)
	}
	if !bytes.HasPrefix(next, []byte("\x1b[H\x1b[0J")) || !bytes.Contains(next, body) {
		t.Fatalf("next frame = %q", next)
	}
}

func TestTailDaemonLogErrors(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "daemon.log")
	lines := []string{
		"info: started",
		"ERROR: first failure",
		"info: working",
		"job failed: db locked",
		"info: idle",
		"panic: boom",
	}
	if err := os.WriteFile(logFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	got := tailDaemonLogErrors(logFile, 2)
	if len(got) != 2 || got[0] != "job failed: db locked" || got[1] != "panic: boom" {
		t.Fatalf("tail = %v, want the last 2 error-ish lines", got)
	}

	// A large log is read from the END only (bounded), and a partial leading
	// line from the seek is dropped without crashing.
	var big strings.Builder
	big.WriteString(strings.Repeat("info: filler line padding the head\n", 5000)) // > 64KB
	big.WriteString("ERROR: tail failure near the end\n")
	if err := os.WriteFile(logFile, []byte(big.String()), 0o600); err != nil {
		t.Fatalf("write big log: %v", err)
	}
	if got := tailDaemonLogErrors(logFile, 5); len(got) != 1 || got[0] != "ERROR: tail failure near the end" {
		t.Fatalf("bounded tail = %v, want only the trailing error", got)
	}
	// Missing file → nil, no error.
	if got := tailDaemonLogErrors(filepath.Join(dir, "absent.log"), 5); got != nil {
		t.Fatalf("missing log should yield nil, got %v", got)
	}
	if got := tailDaemonLogErrors("", 5); got != nil {
		t.Fatalf("empty path should yield nil, got %v", got)
	}
}

func TestBuildDashboardActiveJobsKeepsInFlightOnly(t *testing.T) {
	jobs := []db.Job{
		{ID: "j-run", Agent: "planner", Type: "ask", State: "running", Payload: `{"repo":"o/r"}`},
		{ID: "j-queued", Agent: "impl", Type: "implement", State: "queued", Payload: `{"repo":"o/x"}`},
		{ID: "j-succeeded", Agent: "planner", Type: "ask", State: "succeeded", Payload: `{"repo":"o/r"}`},
		{ID: "j-failed", Agent: "impl", Type: "implement", State: "failed", Payload: `{"repo":"o/r"}`},
		{ID: "j-blocked", Agent: "impl", Type: "review", State: "blocked", Payload: `{"repo":"o/r"}`},
		{ID: "j-cancelled", Agent: "impl", Type: "review", State: "cancelled", Payload: `{"repo":"o/r"}`},
		{ID: "j-badpayload", Agent: "x", Type: "ask", State: "running", Payload: "not json"},
	}
	active := buildDashboardActiveJobs(jobs)
	if active == nil {
		t.Fatal("active jobs must be non-nil for stable JSON")
	}
	gotIDs := map[string]dashboardActiveJob{}
	for _, j := range active {
		gotIDs[j.ID] = j
	}
	if len(active) != 3 {
		t.Fatalf("expected 3 in-flight jobs, got %d: %+v", len(active), active)
	}
	for _, terminal := range []string{"j-succeeded", "j-failed", "j-blocked", "j-cancelled"} {
		if _, ok := gotIDs[terminal]; ok {
			t.Fatalf("terminal job %s must be excluded from active jobs", terminal)
		}
	}
	run, ok := gotIDs["j-run"]
	if !ok {
		t.Fatal("running job j-run missing from active jobs")
	}
	if run.Agent != "planner" || run.Type != "ask" || run.State != "running" || run.Repo != "o/r" {
		t.Fatalf("running job fields wrong: %+v", run)
	}
	if q := gotIDs["j-queued"]; q.State != "queued" || q.Repo != "o/x" {
		t.Fatalf("queued job fields wrong: %+v", q)
	}
	// An unparseable payload still surfaces the job, just with an empty repo.
	if bad, ok := gotIDs["j-badpayload"]; !ok || bad.Repo != "" {
		t.Fatalf("job with bad payload should surface with empty repo: %+v", bad)
	}
}

func TestBuildDashboardActiveJobsEmpty(t *testing.T) {
	active := buildDashboardActiveJobs(nil)
	if active == nil {
		t.Fatal("active jobs must be non-nil even with no jobs")
	}
	if len(active) != 0 {
		t.Fatalf("expected no active jobs, got %d", len(active))
	}
}

func TestDashboardSessionStale(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	past := "2026-06-27T11:00:00.000000000Z"
	future := "2026-06-27T13:00:00.000000000Z"
	cases := []struct {
		name    string
		state   string
		expires string
		want    bool
	}{
		{name: "running and lease elapsed is a phantom", state: "running", expires: past, want: true},
		{name: "running within lease is live", state: "running", expires: future, want: false},
		{name: "idle past lease is normal GC, not phantom", state: "idle", expires: past, want: false},
		{name: "running with unparseable lease is not flagged", state: "running", expires: "nope", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dashboardSessionStale(tc.state, tc.expires, now); got != tc.want {
				t.Fatalf("dashboardSessionStale(%q,%q) = %v, want %v", tc.state, tc.expires, got, tc.want)
			}
		})
	}
}

// TestDashboardFlagsPhantomRunningSession is the #505 gap-2 regression at the
// snapshot/render boundary: an agent_instance left at state=running with an
// elapsed lease must surface as "(stale)" (and on the needs-attention list), not
// as a plainly-live "running" session.
func TestDashboardFlagsPhantomRunningSession(t *testing.T) {
	home := dashboardTestHome(t)
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	// expires_at is far in the past, so the running lease has elapsed → phantom.
	past := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000000000Z")
	nowStr := time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	if err := store.UpsertAgentInstance(context.Background(), db.AgentInstance{
		Name:           "researcher-bg-dead",
		Type:           "researcher",
		Runtime:        "claude",
		RuntimeRef:     "ref-dead",
		RepoFullName:   "owner/repo",
		Role:           "researcher",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: "read-only",
		State:          "running",
		CreatedAt:      nowStr,
		LastUsedAt:     nowStr,
		ExpiresAt:      past,
	}); err != nil {
		t.Fatalf("UpsertAgentInstance returned error: %v", err)
	}
	store.Close()

	// Snapshot carries the Stale flag.
	paths, err := initializedPaths(home)
	if err != nil {
		t.Fatalf("initializedPaths returned error: %v", err)
	}
	snap, err := buildDashboardSnapshot(home, paths)
	if err != nil {
		t.Fatalf("buildDashboardSnapshot returned error: %v", err)
	}
	if len(snap.RuntimeSessions) != 1 || !snap.RuntimeSessions[0].Stale {
		t.Fatalf("expected one stale runtime session, got %+v", snap.RuntimeSessions)
	}

	// Plain render shows "(stale)" and flags it for attention.
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("dashboard exit code = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"(stale)", "stale session", "researcher-bg-dead"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dashboard output missing %q:\n%s", want, out)
		}
	}
}

func TestDashboardRendersActiveJobsSection(t *testing.T) {
	home := dashboardTestHome(t)
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.CreateJob(context.Background(), db.Job{ID: "live-1", Agent: "planner", Type: "ask", State: "running", Payload: `{"repo":"o/r"}`}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	store.Close()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("dashboard exit code = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"active_jobs: 1", "live-1", "planner"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dashboard output missing %q:\n%s", want, out)
		}
	}
}

func TestBuildDashboardDaemonDetailNoMeta(t *testing.T) {
	dir := t.TempDir()
	state := daemonState{
		MetaFile: filepath.Join(dir, "daemon.json"),
		LogFile:  filepath.Join(dir, "daemon.log"),
	}
	// No meta file, no log → all zero, no panic.
	detail := buildDashboardDaemonDetail(state)
	if detail.Flags != nil || detail.WorkDir != "" || detail.StartedAt != "" || detail.LogErrors != nil {
		t.Fatalf("detail without files should be zero: %+v", detail)
	}
}
