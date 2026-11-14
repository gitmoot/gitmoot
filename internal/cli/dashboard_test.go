package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/cli/style"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
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

func TestDashboardSnapshotRendersSections(t *testing.T) {
	home := dashboardTestHome(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("dashboard exit code = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"daemon: stopped",
		"repos: 0",
		"reviewers: 0",
		"active_runs: 0",
		"runs: 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dashboard output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "runtime_sessions") {
		t.Fatalf("dashboard still renders the removed runtime_sessions section:\n%s", out)
	}
}

// TestDashboardJSONUsesNewVocabulary pins the #2603 --json key renames:
// agents -> reviewers, active_jobs -> active_runs, jobs -> runs.
func TestDashboardJSONUsesNewVocabulary(t *testing.T) {
	home := dashboardTestHome(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("dashboard --json exit code = %d, stderr=%s", code, stderr.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("dashboard --json is not an object: %v\n%s", err, stdout.String())
	}
	for _, key := range []string{"reviewers", "active_runs", "runs"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("dashboard --json missing %q:\n%s", key, stdout.String())
		}
	}
	for _, key := range []string{"agents", "active_jobs", "jobs"} {
		if _, ok := got[key]; ok {
			t.Fatalf("dashboard --json still has old key %q:\n%s", key, stdout.String())
		}
	}
}

func TestDashboardStyledRendering(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "")
	home := dashboardTestHome(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("dashboard exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "\x1b[") {
		t.Fatalf("expected ANSI styling with CLICOLOR_FORCE:\n%q", stdout.String())
	}
}

func TestDashboardTruncate(t *testing.T) {
	items := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")
	styled := style.For(io.Discard)
	if shown, hidden := dashboardTruncate(styled, false, items); len(shown) != dashboardListCap || hidden != 2 {
		t.Fatalf("styled truncate = %d shown, %d hidden", len(shown), hidden)
	}
	if shown, hidden := dashboardTruncate(styled, true, items); len(shown) != 10 || hidden != 0 {
		t.Fatalf("--all should keep all: %d, %d", len(shown), hidden)
	}
	t.Setenv("CLICOLOR_FORCE", "")
	if shown, hidden := dashboardTruncate(style.For(io.Discard), false, items); len(shown) != 10 || hidden != 0 {
		t.Fatalf("plain mode keeps all: %d, %d", len(shown), hidden)
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

func TestDashboardRejectsPositionalArguments(t *testing.T) {
	home := dashboardTestHome(t)
	var stdout, stderr bytes.Buffer
	args := []string{"dashboard", "--home", home, "extra"}
	if code := Run(args, &stdout, &stderr); code != 2 {
		t.Fatalf("Run(%v) = %d, want 2; stderr=%s", args, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dashboard does not accept positional arguments") {
		t.Fatalf("Run(%v) stderr = %q, want the positional-argument message", args, stderr.String())
	}
}

// Removed flags must still be REJECTED by the parser rather than silently
// ignored: the TUI flags (--answer/--value/--dismiss) and the terminal
// --watch/--interval redraw loop (#2593).
func TestDashboardRejectsRemovedFlags(t *testing.T) {
	home := dashboardTestHome(t)
	for _, args := range [][]string{
		{"dashboard", "--home", home, "--answer", "p1", "--value", "x"},
		{"dashboard", "--home", home, "--dismiss", "p1"},
		{"dashboard", "--home", home, "--watch"},
		{"dashboard", "--home", home, "--interval", "2s"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("Run(%v) = %d, want 2 for a removed flag; stderr=%s", args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Fatalf("Run(%v) stderr = %q, want the parser to reject the removed flag", args, stderr.String())
		}
	}
}

// The --web branch returns BEFORE the one-shot snapshot. A mutant deleting that
// early return survived the entire package (#1787 review N2), because --web
// blocks until interrupted and nothing could observe the branch. The seam makes
// the invariant observable without starting a server.
func TestDashboardWebReturnsBeforeTheSnapshot(t *testing.T) {
	home := dashboardTestHome(t)
	original := runDashboardWebFn
	t.Cleanup(func() { runDashboardWebFn = original })
	var calledHome, calledAddr string
	calls := 0
	runDashboardWebFn = func(home, addr string, stdout, stderr io.Writer) int {
		calls++
		calledHome, calledAddr = home, addr
		return 7
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"dashboard", "--home", home, "--web", "--addr", "127.0.0.1:0"}, &stdout, &stderr)
	if calls != 1 {
		t.Fatalf("--web took the snapshot path instead of returning early: calls=%d stdout=%q", calls, stdout.String())
	}
	if code != 7 {
		t.Fatalf("Run returned %d, want the web server's own %d: the branch must RETURN, not fall through", code, 7)
	}
	if calledHome != home || calledAddr != "127.0.0.1:0" {
		t.Fatalf("web server received home=%q addr=%q, want %q and 127.0.0.1:0", calledHome, calledAddr, home)
	}
	if stdout.Len() != 0 {
		t.Fatalf("the one-shot snapshot ran anyway and wrote %d bytes", stdout.Len())
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

func TestDashboardRendersActiveJobsSection(t *testing.T) {
	home := dashboardTestHome(t)
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
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
	for _, want := range []string{"active_runs: 1", "live-1", "planner"} {
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
