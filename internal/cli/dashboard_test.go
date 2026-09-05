package cli

import (
	"bytes"
	"context"
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
	"github.com/gitmoot/gitmoot/internal/runtime"
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
		"agents: 0",
		"runtime_sessions: 0",
		"jobs: 0",
		"branch_locks: 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dashboard output missing %q:\n%s", want, out)
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
	// ASSERT THE REASON, NOT ONLY THE CODE. Two of these cases used to pass the
	// removed --answer/--value/--dismiss flags and "passed" on exit 2 from the
	// flag parser, never reaching the combination check they were named for
	// (#1787 review N1). Exit code alone cannot tell those apart, so every case
	// now names the message it expects.
	home := dashboardTestHome(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "watch with json",
			args: []string{"dashboard", "--home", home, "--watch", "--json"},
			want: "dashboard --watch cannot be combined with --json",
		},
		{
			name: "watch without a terminal",
			args: []string{"dashboard", "--home", home, "--watch"},
			want: "dashboard --watch requires a terminal",
		},

		{
			name: "positional argument",
			args: []string{"dashboard", "--home", home, "extra"},
			want: "dashboard does not accept positional arguments",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(tc.args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("Run(%v) = %d, want 2; stderr=%s", tc.args, code, stderr.String())
			}
			if strings.Contains(stderr.String(), "flag provided but not defined") {
				t.Fatalf("Run(%v) exited 2 from the FLAG PARSER, not from the check this case names: %s", tc.args, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("Run(%v) stderr = %q, want it to contain %q", tc.args, stderr.String(), tc.want)
			}
		})
	}
}

// The interval guard sits BEHIND the terminal guard, so a bytes.Buffer stdout
// can never reach it: style.IsTerminal is false for any writer without Stat(),
// so runDashboard exits at "requires a terminal" first. The previous version of
// this case lived in the combos table with the loose want "dashboard --watch",
// which the TERMINAL message also contains - so it passed green while measuring
// the wrong guard, which is precisely the class that round claimed to close
// (#1787 review F1).
//
// Reaching the interval guard needs a stdout that satisfies IsTerminal. This
// case USED TO open /dev/null for that, relying on the #1838 weakness it names.
// With that weakness fixed, /dev/null no longer passes and the old version
// would have SILENTLY SKIPPED - still green, testing nothing. It now uses a
// genuine terminal instead, so it exercises the same production ordering
// without depending on a defect.
func TestDashboardWatchRejectsANonPositiveInterval(t *testing.T) {
	home := dashboardTestHome(t)
	tty := openTerminalForTest(t)
	if !style.IsTerminal(tty) {
		t.Fatal("openTerminalForTest did not return a terminal, so the terminal guard cannot be passed here")
	}
	original := runDashboardWatchFn
	t.Cleanup(func() { runDashboardWatchFn = original })
	started := 0
	runDashboardWatchFn = func(io.Writer, string, bool, time.Duration) int {
		started++
		return 0
	}
	var stderr bytes.Buffer
	code := Run([]string{"dashboard", "--home", home, "--watch", "--interval", "0s"}, tty, &stderr)
	if started != 0 {
		t.Fatal("the watch loop started with a non-positive interval")
	}
	if code != 2 {
		t.Fatalf("Run = %d, want 2; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "dashboard --interval must be greater than zero") {
		t.Fatalf("stderr = %q, want the INTERVAL guard's own message: a substring that the terminal message also matches proves nothing", stderr.String())
	}
}

// The flags deleted with the TUI must still be REJECTED, and that is a separate
// property from the combination checks above - conflating them is what let two
// combination cases pass on a parse error.
func TestDashboardRejectsFlagsRemovedWithTheTUI(t *testing.T) {
	home := dashboardTestHome(t)
	for _, args := range [][]string{
		{"dashboard", "--home", home, "--answer", "p1", "--value", "x"},
		{"dashboard", "--home", home, "--dismiss", "p1"},
		{"dashboard", "--home", home, "--watch", "--answer", "p1"},
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

// The --watch terminal guard must be observable WITHOUT running the watch loop.
// Before the seam, deleting the guard did not fail this suite cleanly: the test
// reached the blocking loop and the package died on its 10m timeout, naming
// whichever test the panic landed in (#1787 review N3). Now the guard's absence
// fails here, in seconds, with the right name on it.
func TestDashboardWatchNeverStartsWithoutATerminal(t *testing.T) {
	home := dashboardTestHome(t)
	original := runDashboardWatchFn
	t.Cleanup(func() { runDashboardWatchFn = original })
	started := 0
	runDashboardWatchFn = func(io.Writer, string, bool, time.Duration) int {
		started++
		return 0
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"dashboard", "--home", home, "--watch"}, &stdout, &stderr)
	if started != 0 {
		t.Fatalf("the watch loop STARTED with a non-terminal stdout; on a real run it would block until interrupted")
	}
	if code != 2 || !strings.Contains(stderr.String(), "requires a terminal") {
		t.Fatalf("Run = %d stderr=%q, want exit 2 naming the terminal requirement", code, stderr.String())
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
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
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

// seedAgentInstanceWithLock seeds a state=running agent_instance with a future
// (within-)lease and a held runtime:<rt>:<ref> session lock owned by ownerPID.
func seedAgentInstanceWithLock(t *testing.T, home, name, ref string, ownerPID int64) {
	t.Helper()
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	future := now.Add(29 * time.Minute).Format("2006-01-02T15:04:05.000000000Z")
	nowStr := now.Format("2006-01-02T15:04:05.000000000Z")
	if err := store.UpsertAgentInstance(context.Background(), db.AgentInstance{
		Name:           name,
		Type:           "researcher",
		Runtime:        "claude",
		RuntimeRef:     ref,
		RepoFullName:   "owner/repo",
		Role:           "researcher",
		AutonomyPolicy: "read-only",
		State:          "running",
		CreatedAt:      nowStr,
		LastUsedAt:     nowStr,
		ExpiresAt:      future,
	}); err != nil {
		t.Fatalf("UpsertAgentInstance returned error: %v", err)
	}
	key, ok := runtimeSessionResourceKey(runtime.Agent{Runtime: "claude", RuntimeRef: ref})
	if !ok {
		t.Fatalf("expected a runtime session key for ref %q", ref)
	}
	acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey:   key,
		OwnerJobID:    "job-" + ref,
		OwnerToken:    "tok-" + ref,
		OwnerPID:      ownerPID,
		OwnerHostname: "", // empty host = treated as this host by the liveness check
		ExpiresAt:     now.Add(29 * time.Minute).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !acquired {
		t.Fatalf("AcquireResourceLock acquired=%v err=%v", acquired, err)
	}
}

// TestRunningSessionStaleWithinLeaseDeadLock is the #505-review regression for the
// within-lease phantom gap: a daemon that crashes SOON after starting a long job
// leaves the instance state=running with a FUTURE lease, so the lease-only check
// treats the dead session as live. The liveness-aware check flags it stale when
// the held runtime-session lock has no live owner, and leaves a genuinely live
// (this-process-owned) session alone.
func TestRunningSessionStaleWithinLeaseDeadLock(t *testing.T) {
	home := dashboardTestHome(t)
	seedAgentInstanceWithLock(t, home, "researcher-bg-crashed", "ref-crashed", deadPID(t))
	seedAgentInstanceWithLock(t, home, "researcher-bg-live", "ref-live", int64(os.Getpid()))

	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	dead, err := store.GetAgentInstance(ctx, "researcher-bg-crashed")
	if err != nil {
		t.Fatalf("GetAgentInstance dead: %v", err)
	}
	live, err := store.GetAgentInstance(ctx, "researcher-bg-live")
	if err != nil {
		t.Fatalf("GetAgentInstance live: %v", err)
	}

	// Lease-only signal misses it (future lease) — this is the gap.
	if dashboardSessionStale(dead.State, dead.ExpiresAt, now) {
		t.Fatalf("within-lease running session should not be lease-stale")
	}
	// Liveness-aware signal flags the dead-owner session...
	if !runningSessionStale(ctx, store, dead, now) {
		t.Fatalf("within-lease running session with a dead-owner lock must be stale")
	}
	// ...but never a live-owner session.
	if runningSessionStale(ctx, store, live, now) {
		t.Fatalf("within-lease running session held by a live owner must not be stale")
	}
}

// TestDashboardFlagsWithinLeasePhantomViaDeadLock binds the within-lease liveness
// fix to the snapshot/render boundary: a running session with a future lease but a
// dead-owner session lock must render as "(stale)", not as a plainly-live session.
func TestDashboardFlagsWithinLeasePhantomViaDeadLock(t *testing.T) {
	home := dashboardTestHome(t)
	seedAgentInstanceWithLock(t, home, "researcher-bg-crashed", "ref-crashed", deadPID(t))

	paths, err := initializedPaths(home)
	if err != nil {
		t.Fatalf("initializedPaths returned error: %v", err)
	}
	snap, err := buildDashboardSnapshot(home, paths)
	if err != nil {
		t.Fatalf("buildDashboardSnapshot returned error: %v", err)
	}
	if len(snap.RuntimeSessions) != 1 || !snap.RuntimeSessions[0].Stale {
		t.Fatalf("within-lease dead-lock session should be stale: %+v", snap.RuntimeSessions)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("dashboard exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "(stale)") {
		t.Fatalf("within-lease phantom not rendered stale:\n%s", stdout.String())
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
