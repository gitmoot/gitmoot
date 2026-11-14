//go:build e2e

package cli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestConcurrentOmpReviewsBySameReviewerE2E is #2590's acceptance proof: two
// review jobs queued for ONE reviewer agent on the omp runtime run at the same
// time and both succeed, with no temp worker, no runtime-session lock and no
// agent_instances table.
//
// Temp workers existed to fork a second session when a job found its agent's
// runtime session busy. omp mints a fresh session per job, so it never takes a
// runtime-session lock and nothing is busy: the scheduler admits both jobs in
// one pass and each runs in its own review worktree.
//
// CONCURRENCY PROOF: the fake `omp` touches a marker named after its job's
// worktree, then waits until BOTH markers exist before emitting `approved`. A
// job that waits alone for ~10s emits `failed` instead. So both jobs reach
// JobSucceeded only if both omp processes were live at once.
//
// WHAT TURNS IT RED: anything that serializes the second review behind the
// first. If omp jobs keyed a runtime-session lock again (resumableSessionRuntime
// true for omp), both jobs would share runtime:omp:fresh:reviewer, the selector
// would decline the second ("runtime already taken this pass"), the first would
// time out alone and fail, and the second would stay queued. A worker that
// waited on the lock (runtime_lock_wait) or forked a temp worker is caught by
// the event assertions.
func TestConcurrentOmpReviewsBySameReviewerE2E(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	rendezvous := filepath.Join(root, "rendezvous")
	for _, dir := range []string{binDir, rendezvous} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeRendezvousOmpStub(t, filepath.Join(binDir, "omp"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITMOOT_OMP_RENDEZVOUS", rendezvous)

	rawHome := filepath.Join(root, "home")
	paths, err := pathsFromFlag(rawHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "reviewer", runtime.OmpRuntime, "fresh:reviewer", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	// Two pull requests, each reviewed in its own worktree at its own head, as
	// review dispatch allocates them. Same reviewer agent for both.
	jobIDs := []string{"review-pr-7", "review-pr-8"}
	for index, jobID := range jobIDs {
		branch := fmt.Sprintf("pr-%d", 7+index)
		worktree := filepath.Join(root, "worktrees", jobID)
		runDaemonWorkerGit(t, checkout, "worktree", "add", "-b", branch, worktree, "main")
		head, err := gitutil.NewHostClient(worktree).HeadSHA(ctx)
		if err != nil {
			t.Fatalf("HeadSHA(%s): %v", worktree, err)
		}
		enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
			ID: jobID, Agent: "reviewer", Action: "review", Repo: "owner/repo",
			Branch: branch, PullRequest: 7 + index, HeadSHA: head, WorktreePath: worktree,
		})
	}

	worker := defaultJobWorker(store, io.Discard, rawHome)
	worker.UsePool = true
	worker.CheckoutValidator = func(_ context.Context, _ db.Job, payload workflow.JobPayload, _ runtime.Agent) (string, error) {
		return payload.WorktreePath, nil
	}
	worker.WorkflowFactory = func(string) workflow.Engine { return workflow.Engine{Store: store} }
	worker.CommenterFactory = func(string) github.Client { return &cliPollFakeGitHub{} }

	bothRunning := drivePoolConcurrently(t, ctx, worker, store, jobIDs)

	for _, jobID := range jobIDs {
		job, err := store.GetJob(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		events, err := store.ListJobEvents(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Agent != "reviewer" {
			t.Fatalf("%s ran as agent %q, want the registered reviewer (no forked worker)", jobID, job.Agent)
		}
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("%s state = %q, want succeeded; the reviews did not run concurrently\nevents:\n%s\npayload: %s", jobID, job.State, planModeE2EEventText(events), job.Payload)
		}
		for _, event := range events {
			if strings.HasPrefix(event.Kind, "temp_worker") || strings.HasPrefix(event.Kind, "ephemeral_worker") || event.Kind == "runtime_lock_wait" {
				t.Fatalf("%s recorded %s: %s", jobID, event.Kind, event.Message)
			}
			if event.Kind == "dispatch_declined" && strings.Contains(event.Message, "runtime") {
				t.Fatalf("%s was declined on its runtime session: %s", jobID, event.Message)
			}
		}
	}
	if !bothRunning {
		t.Fatal("the sampler never saw both reviews running at once")
	}
	markers, err := filepath.Glob(filepath.Join(rendezvous, "started-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 2 {
		t.Fatalf("omp rendezvous markers = %v, want one per review", markers)
	}

	locks, err := store.ListResourceLocks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, lock := range locks {
		if strings.HasPrefix(lock.ResourceKey, "runtime:") || strings.HasPrefix(lock.ResourceKey, "agent-type:") {
			t.Fatalf("resource lock %s left behind; omp reviews take no session lock", lock.ResourceKey)
		}
	}
	raw, err := sql.Open("sqlite", paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var tables int
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_instances'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("agent_instances still exists; #2590's migration drops it")
	}
}

// writeRendezvousOmpStub installs a fake omp that answers the adapter's probes
// like writePlanModeOmpStub and, for a real run, blocks on a 2-of-2 rendezvous
// keyed by its working directory (one review worktree per job).
func writeRendezvousOmpStub(t *testing.T, path string) {
	t.Helper()
	const script = `#!/bin/sh
case "$1" in
  --help)
    cat <<'EOF'
Usage: omp [options]
  -p
  --mode
  --approval-mode
  --no-session
EOF
    ;;
  --version)
    printf 'omp stub-17.2.4\n'
    ;;
  *)
    : > "$GITMOOT_OMP_RENDEZVOUS/started-$(basename "$PWD")"
    decision=failed
    i=0
    while [ "$i" -lt 200 ]; do
      if [ "$(ls "$GITMOOT_OMP_RENDEZVOUS"/started-* 2>/dev/null | wc -l)" -ge 2 ]; then
        decision=approved
        break
      fi
      i=$((i+1))
      sleep 0.05
    done
    sleep 0.3
    cat <<EOF
{"type":"session","version":3,"id":"01920000-0000-7000-8000-000000002590","timestamp":"2026-11-14T00:00:00.000Z","cwd":"$PWD"}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"gitmoot_result\":{\"decision\":\"$decision\",\"summary\":\"rendezvous $decision\",\"findings\":[],\"changes_made\":[],\"tests_run\":[],\"needs\":[]}}"}],"usage":{"input":1,"output":1,"totalTokens":2},"stopReason":"stop"}}
{"type":"agent_end","messages":[],"isTerminal":true}
EOF
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}
