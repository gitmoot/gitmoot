package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestTempWorkerForkStillSandboxesAReadOnlySeat drives the FORK path, not the
// wrap helper.
//
// When the registered session is busy and parallel_sessions.same_session is
// fork_temp_session, control leaves jobWorker.run before the delivery-site
// sandbox wrap, and startTempWorker copies the delivery agent - so ReadOnlySeat
// survives into a path that never wrapped it. A read-only seat therefore ran
// outside Landlock and outside this PR's staging policy.
//
// The observable is the fail-closed wrapper refusal. The fake delivery adapter
// cannot be Landlock-wrapped, so reaching that refusal proves the fork path
// applied the sandbox policy before any runtime launch.
func TestTempWorkerForkStillSandboxesAReadOnlySeat(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	// The host profile the seat must stage a narrowed copy of.
	hostCodex := filepath.Join(home, "host-codex")
	if err := os.MkdirAll(hostCodex, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostCodex, "config.toml"), []byte("model = \"gpt-5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	checkout := t.TempDir()
	runGit(t, checkout, "init", "-b", "main")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runGit(t, checkout, "commit", "--allow-empty", "-m", "init")
	headSHA := strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "HEAD"))
	if err := store.UpsertRepoForce(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: checkout, PrimaryCheckoutPath: checkout}); err != nil {
		t.Fatalf("UpsertRepoForce: %v", err)
	}

	const registeredRef = "019fa4c8-69c1-7bc2-8628-00ade8fa43d1"
	registeredAgent := db.Agent{
		Name:       "seat-reviewer",
		Role:       "reviewer",
		Runtime:    runtime.CodexRuntime,
		RuntimeRef: registeredRef,
		RepoScope:  "owner/repo",
		Model:      "gpt-5",
	}
	if err := store.UpsertAgent(ctx, registeredAgent); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	// Hold the registered session so the fork path is the one taken. The
	// default config leaves same_session = fork_temp_session.
	registeredKey, ok := runtimeSessionResourceKey(runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeRef: registeredRef})
	if !ok {
		t.Fatal("registered codex session must have a lock key")
	}
	release, acquired, _, _, err := acquireRuntimeSessionLockWithKey(ctx, store, "other-job", registeredKey, true, time.Now().UTC(), time.Hour)
	if err != nil || !acquired {
		t.Fatalf("seed registered lock: err=%v acquired=%v", err, acquired)
	}
	defer func() { _ = release(context.Background()) }()

	job := db.Job{
		ID:    "local-review-seat-fork",
		Agent: "seat-reviewer",
		Type:  "review",
		State: string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo:             "owner/repo",
			Branch:           "main",
			PullRequest:      11,
			HeadSHA:          headSHA,
			ReadOnlySeat:     true,
			RuntimeConfigDir: hostCodex,
		}),
	}
	if err := store.CreateJobWithEvent(ctx, job, db.JobEvent{Kind: string(workflow.JobQueued), Message: "seed"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}

	var output bytes.Buffer
	worker := jobWorker{
		Store:              store,
		ConfigHome:         home,
		ConfigHomeExplicit: true,
		Stdout:             &output,
		AgentLookup: func(context.Context, string) (db.Agent, error) {
			return registeredAgent, nil
		},
		AdapterFactory: func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
			return &cliWorkerFakeAdapter{}, nil
		},
		StartAdapterFactory: func(execbackend.Backend, string, string) (runtime.Adapter, error) {
			return &cliWorkerFakeAdapter{}, nil
		},
	}
	_ = worker.run(ctx, job)

	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	forked := false
	for _, event := range events {
		if event.Kind == "temp_worker_eligible" {
			forked = true
		}
	}
	if !forked {
		t.Fatalf("the run never took the temp-worker fork, so this test is not exercising the path it names; events: %+v", events)
	}

	// The fake adapter cannot be Landlock-wrapped, so the wrap REFUSING it is
	// proof the fork path now asks. The private state is removed with the failed
	// setup rather than left behind as a second, stale observable.
	wrapAttempted := false
	for _, event := range events {
		if strings.Contains(event.Message, "read-only Landlock sandbox") {
			wrapAttempted = true
		}
	}
	if !wrapAttempted {
		t.Error("no evidence the fork path applied the read-only sandbox wrap")
	}
}
