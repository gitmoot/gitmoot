package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestDaemonWorkerLocksTheRegisteredSessionForAReadOnlySeat pins the invariant
// this PR exists to protect, ON THE DAEMON PATH.
//
// jobWorker.run copies the agent into sessionLockAgent BEFORE applyReadOnlySeat
// rewrites RuntimeRef to the seat's fresh ref, and acquires the lock with that
// copy. Lock with the rewritten agent instead and a read-only seat stops
// serializing behind the reviewer's registered session (#684).
//
// The existing coverage pins the same invariant on the dispatch path and, in
// TestApplyReadOnlySeatLeavesTheSessionLockAgentIntact, on a LOCAL copy of the
// idiom. Neither enters this function, which is why moving the acquisition here
// onto the post-seat agent survives the whole package. A test that pins the
// twin is not a test of this path.
//
// Method: hold the REGISTERED session's lock under a foreign owner, then drive
// the real jobWorker.run. Keying the lock off the registered ref must observe
// that conflict; keying it off the seat's per-job fresh ref cannot, because
// nothing else can ever hold that key.
func TestDaemonWorkerLocksTheRegisteredSessionForAReadOnlySeat(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	const registeredRef = "019fa4c8-69c1-7bc2-8628-00ade8fa43c5"
	// same_session = queue makes the busy outcome deterministic; the default
	// fork_temp_session forks a temp worker instead, which is its own finding.
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("config.Initialize: %v", err)
	}
	queued := strings.Replace(config.DefaultConfig(paths), `same_session = "fork_temp_session"`, `same_session = "queue"`, 1)
	if err := os.WriteFile(paths.ConfigFile, []byte(queued), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	checkout := t.TempDir()
	runGit(t, checkout, "init", "-b", "main")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	runGit(t, checkout, "commit", "--allow-empty", "-m", "init")
	if err := store.UpsertRepoForce(ctx, db.Repo{Owner: "owner", Name: "repo", CheckoutPath: checkout, PrimaryCheckoutPath: checkout}); err != nil {
		t.Fatalf("UpsertRepoForce: %v", err)
	}
	headSHA := strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "HEAD"))
	registeredAgent := db.Agent{
		Name:       "seat-reviewer",
		Runtime:    runtime.CodexRuntime,
		RuntimeRef: registeredRef,
		RepoScope:  "owner/repo",
	}

	registeredKey, ok := runtimeSessionResourceKey(runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeRef: registeredRef})
	if !ok {
		t.Fatal("a registered codex session must have a lock key")
	}

	// A foreign owner holds the registered session, exactly as a running
	// reviewer would.
	release, acquired, _, _, err := acquireRuntimeSessionLockWithKey(ctx, store, "other-job", registeredKey, true, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatalf("seed the registered session lock: %v", err)
	}
	if !acquired {
		t.Fatal("seeding the registered session lock did not acquire it")
	}
	defer func() { _ = release(context.Background()) }()

	job := db.Job{
		ID:    "local-review-seat-lock",
		Agent: "seat-reviewer",
		Type:  "review",
		State: string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{
			Repo:         "owner/repo",
			Branch:       "main",
			PullRequest:  7,
			HeadSHA:      headSHA,
			ReadOnlySeat: true,
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
		// The adapter is built before the lock is acquired, so the test needs
		// one; it must never be delivered to, which the assertions below imply.
		AdapterFactory: func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
			return &cliWorkerFakeAdapter{}, nil
		},
	}
	runErr := worker.run(ctx, job)

	if !errors.Is(runErr, errRuntimeSessionBusy) {
		t.Fatalf("run err = %v, want errRuntimeSessionBusy: the seat must contend for the REGISTERED session %q, not its own per-job fresh ref", runErr, registeredKey)
	}

	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var waited string
	for _, event := range events {
		if event.Kind == "runtime_lock_wait" {
			waited = event.Message
		}
	}
	if waited == "" {
		t.Fatal("no runtime_lock_wait event: the daemon path recorded no contention for the registered session")
	}
	if !strings.Contains(waited, registeredKey) {
		t.Fatalf("runtime_lock_wait names %q, want the registered session key %q", waited, registeredKey)
	}
	if strings.Contains(waited, job.ID) {
		t.Fatalf("runtime_lock_wait names the seat's own job-scoped ref (%q): the lock moved onto the post-seat agent", waited)
	}
}
