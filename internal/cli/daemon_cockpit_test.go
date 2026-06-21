package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/cockpit"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// cockpitStubAdapter is a no-op DeliveryAdapter used as the "inner" adapter so
// the wrap-vs-passthrough decision can be checked by pointer identity.
type cockpitStubAdapter struct{}

func (cockpitStubAdapter) Deliver(context.Context, runtime.Agent, runtime.Job) (runtime.Result, error) {
	return runtime.Result{}, nil
}

// sameAdapter reports whether two DeliveryAdapters are the same underlying value
// (i.e. inner was returned untouched, not wrapped).
func sameAdapter(a, b workflow.DeliveryAdapter) bool {
	pa, okA := a.(*cockpitStubAdapter)
	pb, okB := b.(*cockpitStubAdapter)
	return okA && okB && pa == pb
}

func TestMaybeWrapCockpitDecision(t *testing.T) {
	inner := &cockpitStubAdapter{}
	meta := cockpit.JobMeta{JobID: "job-1"}

	// A real cockpit constructed against a HERDR_BIN that does not exist on the
	// test host: Available is false, so requested+available is exercised as the
	// unavailable branch (CI has no herdr). The wrap-when-available branch is
	// covered by the cockpit package's own tests against a fake runner.
	unavailableCockpit := cockpit.New(cockpit.Options{HerdrBin: "herdr-does-not-exist-for-tests"}, nil)

	tests := []struct {
		name            string
		cp              *cockpit.Cockpit
		requested       bool
		modeOff         bool
		wantWrapped     bool
		wantUnavailable bool
	}{
		{
			name:            "not requested is a passthrough with no event",
			cp:              unavailableCockpit,
			requested:       false,
			modeOff:         false,
			wantWrapped:     false,
			wantUnavailable: false,
		},
		{
			name:            "mode off skips entirely with no event",
			cp:              unavailableCockpit,
			requested:       true,
			modeOff:         true,
			wantWrapped:     false,
			wantUnavailable: false,
		},
		{
			name:            "requested with nil cockpit is unavailable",
			cp:              nil,
			requested:       true,
			modeOff:         false,
			wantWrapped:     false,
			wantUnavailable: true,
		},
		{
			name:            "requested with herdr absent is unavailable",
			cp:              unavailableCockpit,
			requested:       true,
			modeOff:         false,
			wantWrapped:     false,
			wantUnavailable: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, unavailable := maybeWrapCockpit(tc.cp, tc.requested, tc.modeOff, inner, meta)
			if unavailable != tc.wantUnavailable {
				t.Fatalf("unavailable = %v, want %v", unavailable, tc.wantUnavailable)
			}
			passthrough := sameAdapter(got, inner)
			if tc.wantWrapped && passthrough {
				t.Fatalf("expected a wrapped adapter, got the inner adapter untouched")
			}
			if !tc.wantWrapped && !passthrough {
				t.Fatalf("expected the inner adapter untouched, got a wrapped adapter")
			}
		})
	}

	// Wrap path: when herdr is genuinely available on the host, a requested
	// non-off job must produce a wrapped adapter (not the inner one) and report
	// available. Skipped on hosts without a reachable herdr server so the test is
	// deterministic everywhere.
	live := cockpit.New(cockpit.Options{}, nil)
	if !live.Available(context.Background()) {
		t.Skip("herdr not available on this host; wrap-path branch is covered by the cockpit package tests")
	}
	got, unavailable := maybeWrapCockpit(live, true, false, inner, meta)
	if unavailable {
		t.Fatalf("unavailable = true with a live herdr, want false")
	}
	if sameAdapter(got, inner) {
		t.Fatalf("expected a wrapped adapter with a live herdr, got the inner adapter untouched")
	}
}

func TestCockpitJobMetaPaneKeyMode(t *testing.T) {
	job := db.Job{ID: "job-42", Type: "implement", Agent: "lead"}
	payload := workflow.JobPayload{RootJobID: "root-1", Branch: "feat/x", DelegationDepth: 2}
	agent := runtime.Agent{Name: "lead"}
	checkout := "/tmp/worktree"

	jobMeta := cockpitJobMeta(job, payload, agent, checkout, "job")
	if jobMeta.PaneKey != "job-42" {
		t.Fatalf("job-mode PaneKey = %q, want job id", jobMeta.PaneKey)
	}
	if jobMeta.JobID != "job-42" || jobMeta.RootJobID != "root-1" || jobMeta.Agent != "lead" ||
		jobMeta.Action != "implement" || jobMeta.Branch != "feat/x" || jobMeta.Worktree != checkout ||
		jobMeta.Depth != 2 {
		t.Fatalf("unexpected meta: %+v", jobMeta)
	}

	seatMeta := cockpitJobMeta(job, payload, agent, checkout, "seat")
	if seatMeta.PaneKey != "lead" {
		t.Fatalf("seat-mode PaneKey = %q, want agent name", seatMeta.PaneKey)
	}
}

// TestCockpitJobMetaRootCoordinatorUsesOwnID guards that a root coordinator job
// (empty payload.RootJobID) is its own root, so roots do not collide into one
// herdr workspace keyed by the empty string.
func TestCockpitJobMetaRootCoordinatorUsesOwnID(t *testing.T) {
	agent := runtime.Agent{Name: "coord"}
	checkout := "/tmp/worktree"

	root := cockpitJobMeta(db.Job{ID: "root-job", Type: "ask"}, workflow.JobPayload{}, agent, checkout, "job")
	if root.RootJobID != "root-job" {
		t.Fatalf("root coordinator RootJobID = %q, want its own id %q", root.RootJobID, "root-job")
	}

	// A whitespace-only RootJobID is also treated as a root (mirrors the engine).
	blank := cockpitJobMeta(db.Job{ID: "root-2", Type: "ask"}, workflow.JobPayload{RootJobID: "   "}, agent, checkout, "job")
	if blank.RootJobID != "root-2" {
		t.Fatalf("blank-root RootJobID = %q, want own id %q", blank.RootJobID, "root-2")
	}

	// A child carries the inherited root through unchanged.
	child := cockpitJobMeta(db.Job{ID: "child-job", Type: "implement"}, workflow.JobPayload{RootJobID: "root-job"}, agent, checkout, "job")
	if child.RootJobID != "root-job" {
		t.Fatalf("child RootJobID = %q, want inherited %q", child.RootJobID, "root-job")
	}
}

// TestWorkerEmitsCockpitUnavailableEvent drives the real worker path: a job that
// requested --cockpit on a host without herdr runs unwrapped (the fake adapter
// still delivers) and the worker records exactly one cockpit_unavailable event.
func TestWorkerEmitsCockpitUnavailableEvent(t *testing.T) {
	// Force herdr absent regardless of the host so the unavailable path is
	// deterministic (this box may have a live herdr server).
	t.Setenv("HERDR_BIN", "herdr-does-not-exist-for-tests")
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "task-cockpit")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:      "job-cockpit",
		Agent:   "lead",
		Action:  "implement",
		Repo:    "owner/repo",
		Branch:  "task-cockpit",
		Cockpit: true,
	})

	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return &cliPollFakeGitHub{}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	if adapter.calls != 1 {
		t.Fatalf("adapter delivery calls = %d, want 1 (job runs unwrapped)", adapter.calls)
	}

	events, err := store.ListJobEvents(ctx, "job-cockpit")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	count := 0
	for _, e := range events {
		if e.Kind == "cockpit_unavailable" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("cockpit_unavailable events = %d, want exactly 1 (events: %+v)", count, events)
	}
}

// TestMaybeWrapCockpitAvailable checks the single source of truth the daemon uses
// to decide whether to set up the per-job tee log: it is true only when requested,
// not opted-off, the cockpit is non-nil, AND herdr is reachable. CI has no herdr
// so every here-listed case is false; the available branch is covered by the
// cockpit package's fake-runner tests + the live skip in TestMaybeWrapCockpitDecision.
func TestMaybeWrapCockpitAvailable(t *testing.T) {
	unavailableCockpit := cockpit.New(cockpit.Options{HerdrBin: "herdr-does-not-exist-for-tests"}, nil)
	cases := []struct {
		name      string
		cp        *cockpit.Cockpit
		requested bool
		modeOff   bool
	}{
		{"not requested", unavailableCockpit, false, false},
		{"mode off", unavailableCockpit, true, true},
		{"nil cockpit", nil, true, false},
		{"herdr absent", unavailableCockpit, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if maybeWrapCockpitAvailable(tc.cp, tc.requested, tc.modeOff) {
				t.Fatalf("maybeWrapCockpitAvailable = true, want false")
			}
		})
	}
}

// TestCockpitTeeAdapterCreatesLogAndTees proves the wrapping-path log setup: it
// creates+truncates <home>/logs/jobs/<jobid>.log, returns the path + an open file
// + a tee-instrumented adapter, and the adapter's Deliver streams the child's
// stdout into that log (the file a pane tails). Result capture is unchanged.
func TestCockpitTeeAdapterCreatesLogAndTees(t *testing.T) {
	home := t.TempDir()
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard, home)
	agent := runtime.Agent{Name: "lead", Role: "builder", Runtime: runtime.ShellRuntime, RuntimeRef: "echo streamed-line"}

	adapter, logPath, logFile := worker.cockpitTeeAdapter(agent, t.TempDir(), "job-tee")
	if logFile == nil {
		t.Fatal("expected a non-nil log file on the wrapping path")
	}
	defer logFile.Close()

	wantPath := filepath.Join(config.PathsForHome(home).Logs, "jobs", "job-tee.log")
	if logPath != wantPath {
		t.Fatalf("logPath = %q, want %q", logPath, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("log file not created: %v", err)
	}

	// The adapter must carry a TeeRunner whose Out is the log file (the seam that
	// streams live with zero adapter change) and whose inner preserves group-kill.
	shell, ok := adapter.(runtime.ShellAdapter)
	if !ok {
		t.Fatalf("adapter type = %T, want runtime.ShellAdapter", adapter)
	}
	tee, ok := shell.Runner.(subprocess.TeeRunner)
	if !ok {
		t.Fatalf("adapter Runner = %T, want subprocess.TeeRunner", shell.Runner)
	}
	if _, ok := tee.Inner.(subprocess.GroupRunner); !ok {
		t.Fatalf("tee inner = %T, want subprocess.GroupRunner (group-kill preserved)", tee.Inner)
	}

	// Delivering tees the child's stdout into the log; the buffered Result is intact.
	res, err := adapter.Deliver(context.Background(), agent, runtime.Job{Prompt: "go"})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !strings.Contains(res.Raw, "streamed-line") {
		t.Fatalf("result raw missing output: %q", res.Raw)
	}
	logged, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logged), "streamed-line") {
		t.Fatalf("log missing teed output: %q", string(logged))
	}
}

// TestCockpitTeeAdapterTruncatesExistingLog: each run starts fresh, so stale
// output from a prior run of the same job id is not shown in the pane.
func TestCockpitTeeAdapterTruncatesExistingLog(t *testing.T) {
	home := t.TempDir()
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard, home)
	logPath := filepath.Join(config.PathsForHome(home).Logs, "jobs", "job-trunc.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("STALE OUTPUT\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	agent := runtime.Agent{Name: "lead", Role: "builder", Runtime: runtime.ShellRuntime, RuntimeRef: "true"}
	_, _, logFile := worker.cockpitTeeAdapter(agent, t.TempDir(), "job-trunc")
	if logFile == nil {
		t.Fatal("expected a non-nil log file")
	}
	defer logFile.Close()
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if strings.Contains(string(logged), "STALE") {
		t.Fatalf("log was not truncated: %q", string(logged))
	}
}

// TestCockpitTeeAdapterFailOpenUnsupportedRuntime: an unsupported runtime fails
// the adapter rebuild, so the helper returns a nil file (no leaked log) and the
// caller falls back to the P0 pane — fail-open, never failing the job.
func TestCockpitTeeAdapterFailOpenUnsupportedRuntime(t *testing.T) {
	home := t.TempDir()
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard, home)
	agent := runtime.Agent{Name: "x", Runtime: "nope-not-a-runtime"}
	adapter, logPath, logFile := worker.cockpitTeeAdapter(agent, t.TempDir(), "job-bad")
	if adapter != nil || logPath != "" || logFile != nil {
		t.Fatalf("expected fail-open nils, got adapter=%v path=%q file=%v", adapter, logPath, logFile)
	}
}

// TestWorkerNoCockpitEventWhenNotRequested confirms a job that did not request
// --cockpit records no cockpit_unavailable event (off/absent is byte-identical).
func TestWorkerNoCockpitEventWhenNotRequested(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := createDaemonWorkerGitCheckout(t, "task-plain")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"implement"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID:     "job-plain",
		Agent:  "lead",
		Action: "implement",
		Repo:   "owner/repo",
		Branch: "task-plain",
	})

	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"implemented","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`}
	worker := defaultJobWorker(store, io.Discard)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.CommenterFactory = func(string) github.Client {
		return &cliPollFakeGitHub{}
	}

	if err := runQueuedJobs(ctx, worker, 1); err != nil {
		t.Fatalf("runQueuedJobs returned error: %v", err)
	}

	events, err := store.ListJobEvents(ctx, "job-plain")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if daemonWorkerHasEvent(events, "cockpit_unavailable") {
		t.Fatalf("unexpected cockpit_unavailable event for non-cockpit job: %+v", events)
	}
}

func cockpitTestJobPayload(t *testing.T, p workflow.JobPayload) string {
	t.Helper()
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(encoded)
}

// TestRootTreeTerminal: a root coordination tree is terminal only when every job
// sharing the root (the root coordinator + every child/continuation by payload
// RootJobID) is in a terminal state.
func TestRootTreeTerminal(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worker := defaultJobWorker(store, io.Discard)

	// Root coordinator (its own id is the root) + two children carrying RootJobID.
	mustCreate := func(id, state string, payload workflow.JobPayload) {
		if err := store.CreateJob(ctx, db.Job{ID: id, Agent: "a", Type: "implement", State: state, Payload: cockpitTestJobPayload(t, payload)}); err != nil {
			t.Fatalf("CreateJob %s: %v", id, err)
		}
	}
	mustCreate("root-1", string(workflow.JobSucceeded), workflow.JobPayload{})
	mustCreate("child-a", string(workflow.JobSucceeded), workflow.JobPayload{RootJobID: "root-1"})
	mustCreate("child-b", string(workflow.JobRunning), workflow.JobPayload{RootJobID: "root-1"})

	// child-b still running -> not terminal.
	if done, err := worker.rootTreeTerminal(ctx, "root-1"); err != nil || done {
		t.Fatalf("rootTreeTerminal with a running child = (%v, %v), want (false, nil)", done, err)
	}

	// Finish child-b -> terminal.
	if _, err := store.TransitionJobState(ctx, "child-b", string(workflow.JobRunning), string(workflow.JobFailed)); err != nil {
		t.Fatalf("transition child-b: %v", err)
	}
	if done, err := worker.rootTreeTerminal(ctx, "root-1"); err != nil || !done {
		t.Fatalf("rootTreeTerminal with all children terminal = (%v, %v), want (true, nil)", done, err)
	}

	// A cancelled job counts as terminal.
	mustCreate("child-c", string(workflow.JobCancelled), workflow.JobPayload{RootJobID: "root-1"})
	if done, err := worker.rootTreeTerminal(ctx, "root-1"); err != nil || !done {
		t.Fatalf("rootTreeTerminal with a cancelled child = (%v, %v), want (true, nil)", done, err)
	}

	// An unknown root with no jobs is treated as terminal (already pruned).
	if done, err := worker.rootTreeTerminal(ctx, "root-none"); err != nil || !done {
		t.Fatalf("rootTreeTerminal for an empty root = (%v, %v), want (true, nil)", done, err)
	}
	// An empty root id is never terminal.
	if done, _ := worker.rootTreeTerminal(ctx, ""); done {
		t.Fatal("rootTreeTerminal for an empty root id must be false")
	}
}

func TestCockpitJobStateTerminal(t *testing.T) {
	terminal := []string{
		string(workflow.JobSucceeded), string(workflow.JobFailed),
		string(workflow.JobBlocked), string(workflow.JobCancelled),
	}
	for _, s := range terminal {
		if !cockpitJobStateTerminal(s) {
			t.Errorf("cockpitJobStateTerminal(%q) = false, want true", s)
		}
	}
	for _, s := range []string{string(workflow.JobQueued), string(workflow.JobRunning), "", "weird"} {
		if cockpitJobStateTerminal(s) {
			t.Errorf("cockpitJobStateTerminal(%q) = true, want false", s)
		}
	}
}

// TestIsReconveneContinuation: a finalize continuation, a continuation that
// returned no delegations, and a root coordinator with no children are reconvene
// points; a coordinator that returned delegations is not.
func TestIsReconveneContinuation(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	worker := defaultJobWorker(store, io.Discard)

	// finalize continuation -> reconvene.
	if !worker.isReconveneContinuation(ctx, db.Job{ID: "fin"}, workflow.JobPayload{DelegationFinalize: true}) {
		t.Fatal("finalize continuation must be a reconvene point")
	}
	// continuation (has parent) with no delegations -> reconvene.
	if !worker.isReconveneContinuation(ctx, db.Job{ID: "cont"}, workflow.JobPayload{ParentJobID: "coord", Result: &workflow.AgentResult{}}) {
		t.Fatal("a childless continuation must be a reconvene point")
	}
	// continuation that returned delegations -> NOT reconvene.
	withDel := workflow.JobPayload{ParentJobID: "coord", Result: &workflow.AgentResult{Delegations: []workflow.Delegation{{ID: "d1", Agent: "x", Action: "implement", Prompt: "go"}}}}
	if worker.isReconveneContinuation(ctx, db.Job{ID: "cont2"}, withDel) {
		t.Fatal("a continuation that returned delegations is not a reconvene point")
	}
	// root coordinator with NO children -> reconvene.
	if err := store.CreateJob(ctx, db.Job{ID: "root-solo", Agent: "a", Type: "ask", State: string(workflow.JobSucceeded), Payload: cockpitTestJobPayload(t, workflow.JobPayload{})}); err != nil {
		t.Fatal(err)
	}
	if !worker.isReconveneContinuation(ctx, db.Job{ID: "root-solo"}, workflow.JobPayload{}) {
		t.Fatal("a childless root coordinator must be a reconvene point")
	}
	// root coordinator WITH a child -> not reconvene (the children, not this job,
	// carry the work; the terminal continuation is the reconvene point).
	if err := store.CreateJob(ctx, db.Job{ID: "root-par", Agent: "a", Type: "ask", State: string(workflow.JobSucceeded), Payload: cockpitTestJobPayload(t, workflow.JobPayload{})}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "child-1", Agent: "a", Type: "implement", State: string(workflow.JobSucceeded), ParentJobID: "root-par", Payload: cockpitTestJobPayload(t, workflow.JobPayload{RootJobID: "root-par", ParentJobID: "root-par"})}); err != nil {
		t.Fatal(err)
	}
	if worker.isReconveneContinuation(ctx, db.Job{ID: "root-par"}, workflow.JobPayload{}) {
		t.Fatal("a root coordinator with children is not itself the reconvene point")
	}
}

// TestCockpitSeatLogAdapterAppends: seat mode uses a STABLE per-seat append log so
// the seat's pane tails one accumulating file across rounds. The path is
// <home>/logs/seats/<rootShort>/<seatSlug>.log, opened O_APPEND (prior output is
// preserved, not truncated), and the adapter tees the child's stdout into it.
func TestCockpitSeatLogAdapterAppends(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GITMOOT_HOME", home)
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard, home)
	cp := cockpit.New(cockpit.Options{HerdrBin: "herdr-does-not-exist-for-tests", PaneKeyMode: config.CockpitPaneKeySeat}, nil)

	wantPath := cp.SeatLogPath("root-seat", "builder")
	if wantPath == "" {
		t.Fatal("expected a non-empty seat-log path with GITMOOT_HOME set")
	}
	// Pre-seed prior-round history that the append log must preserve.
	if err := os.MkdirAll(filepath.Dir(wantPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wantPath, []byte("ROUND1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	agent := runtime.Agent{Name: "builder", Role: "builder", Runtime: runtime.ShellRuntime, RuntimeRef: "echo ROUND2"}
	adapter, logPath, logFile := worker.cockpitSeatLogAdapter(cp, agent, t.TempDir(), "job-r2", "root-seat", "builder")
	if logFile == nil {
		t.Fatal("expected a non-nil seat log file")
	}
	defer logFile.Close()
	if logPath != wantPath {
		t.Fatalf("seat logPath = %q, want %q", logPath, wantPath)
	}

	if _, err := adapter.Deliver(context.Background(), agent, runtime.Job{Prompt: "go"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	logged, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read seat log: %v", err)
	}
	got := string(logged)
	if !strings.Contains(got, "ROUND1") {
		t.Fatalf("seat log must preserve prior-round history (append, not truncate); got: %q", got)
	}
	if !strings.Contains(got, "ROUND2") {
		t.Fatalf("seat log must contain this round's teed output; got: %q", got)
	}

	// The seat-log adapter carries a group-kill-preserving TeeRunner (result capture
	// unchanged).
	shell, ok := adapter.(runtime.ShellAdapter)
	if !ok {
		t.Fatalf("adapter type = %T, want runtime.ShellAdapter", adapter)
	}
	tee, ok := shell.Runner.(subprocess.TeeRunner)
	if !ok {
		t.Fatalf("adapter Runner = %T, want subprocess.TeeRunner", shell.Runner)
	}
	if _, ok := tee.Inner.(subprocess.GroupRunner); !ok {
		t.Fatalf("tee inner = %T, want subprocess.GroupRunner", tee.Inner)
	}
}

// TestCockpitSeatLogAdapterFailOpenNoHome: with no resolvable home, the seat-log
// path is empty and the adapter falls back to the P0 pane (nil file) — fail-open.
func TestCockpitSeatLogAdapterFailOpenNoHome(t *testing.T) {
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard)
	// An empty GITMOOT_HOME makes the cockpit's home empty, so SeatLogPath returns "".
	t.Setenv("GITMOOT_HOME", "")
	cp := cockpit.New(cockpit.Options{HerdrBin: "herdr-does-not-exist-for-tests", PaneKeyMode: config.CockpitPaneKeySeat}, nil)
	agent := runtime.Agent{Name: "builder", Runtime: runtime.ShellRuntime, RuntimeRef: "true"}
	adapter, logPath, logFile := worker.cockpitSeatLogAdapter(cp, agent, t.TempDir(), "job-x", "root-seat", "builder")
	if adapter != nil || logPath != "" || logFile != nil {
		t.Fatalf("expected fail-open nils with no home, got adapter=%v path=%q file=%v", adapter, logPath, logFile)
	}
}

// TestCockpitLogAdapterPicksLogPerMode: job mode uses the per-job truncate log
// (<home>/logs/jobs/<jobid>.log); seat mode uses the stable per-seat append log
// (<home>/logs/seats/<rootShort>/<seatSlug>.log). The dispatch is what keeps job
// mode byte-identical to P1.
func TestCockpitLogAdapterPicksLogPerMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GITMOOT_HOME", home)
	worker := defaultJobWorker(daemonWorkerStore(t), io.Discard, home)
	cp := cockpit.New(cockpit.Options{HerdrBin: "herdr-does-not-exist-for-tests", PaneKeyMode: config.CockpitPaneKeySeat}, nil)
	agent := runtime.Agent{Name: "builder", Role: "builder", Runtime: runtime.ShellRuntime, RuntimeRef: "true"}

	// Job mode -> per-job log under logs/jobs.
	_, jobLog, jobFile := worker.cockpitLogAdapter(cp, agent, t.TempDir(), "job-1", "root-1", "builder", false)
	if jobFile == nil {
		t.Fatal("job-mode log file is nil")
	}
	jobFile.Close()
	wantJob := filepath.Join(config.PathsForHome(home).Logs, "jobs", "job-1.log")
	if jobLog != wantJob {
		t.Fatalf("job-mode log = %q, want %q", jobLog, wantJob)
	}

	// Seat mode -> stable per-seat log under logs/seats/<rootShort>.
	_, seatLog, seatFile := worker.cockpitLogAdapter(cp, agent, t.TempDir(), "job-1", "root-1", "builder", true)
	if seatFile == nil {
		t.Fatal("seat-mode log file is nil")
	}
	seatFile.Close()
	wantSeat := cp.SeatLogPath("root-1", "builder")
	if seatLog != wantSeat {
		t.Fatalf("seat-mode log = %q, want %q", seatLog, wantSeat)
	}
	if !strings.Contains(seatLog, filepath.Join("logs", "seats")) {
		t.Fatalf("seat-mode log %q is not under logs/seats", seatLog)
	}
}
