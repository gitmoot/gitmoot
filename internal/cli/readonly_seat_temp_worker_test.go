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
// The observable is the staging the wrap performs: with the wrap in place the
// seat's isolated runtime state exists on disk under the config home. Without
// it, nothing is staged, because nothing on the fork path ever asks.
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
	// itself proof the fork path now asks - and the staging below happens inside
	// that same call, before the refusal.
	wrapAttempted := false
	for _, event := range events {
		if strings.Contains(event.Message, "read-only Landlock sandbox") {
			wrapAttempted = true
		}
	}
	if !wrapAttempted {
		t.Error("no evidence the fork path applied the read-only sandbox wrap")
	}

	staged := stagedSeatStateUnder(t, home)
	if staged == "" {
		t.Fatalf("the forked read-only seat staged no isolated runtime state under %q: it ran outside the sandbox and outside the staging policy", home)
	}
	contents, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "gpt-5") {
		t.Errorf("staged seat config = %q, want the host model setting", contents)
	}
}

// stagedSeatStateUnder finds a staged seat config.toml anywhere under home. The
// cache root is chosen by the isolated-tool-cache grant, so the test asks
// whether staging HAPPENED rather than hard-coding where.
func stagedSeatStateUnder(t *testing.T, home string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || found != "" {
			return nil //nolint:nilerr // an unreadable subtree is not a result
		}
		if filepath.Base(path) == "config.toml" && strings.Contains(path, "runtime-state") {
			found = path
		}
		return nil
	})
	return found
}
