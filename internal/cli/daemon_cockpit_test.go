package cli

import (
	"context"
	"io"
	"testing"

	"github.com/jerryfane/gitmoot/internal/cockpit"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
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
