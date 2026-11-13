//go:build e2e

package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2618. A remote review's worker returned while its row was still running:
// the run's terminal write was lost on a contended store, and nothing else
// settled the row, so `daemon drain` waited on it for over an hour. Here a
// trigger loses the run's own terminal writes the same way (RAISE(IGNORE): the
// UPDATE matches no row) and counts them in job_events; later writes land.
// Either way the worker must not leave the row running.
func TestRemoteReviewWorkerExitSettlesItsClaimedRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		script    string
		lose      int // terminal writes the store loses before one lands
		wantState string
		wantKind  string
		wantText  string
	}{
		{
			name:      "result_produced",
			script:    `printf '{"gitmoot_result":{"decision":"approved","evidence":"executed","summary":"reviewed 2618","findings":[],"changes_made":[],"tests_run":["true"],"needs":[],"delegations":[]}}'`,
			lose:      1,
			wantState: string(workflow.JobSucceeded),
			wantKind:  string(workflow.JobSucceeded),
		},
		{
			name:   "no_result",
			script: `echo "runtime died before a result" >&2; exit 3`,
			// Mailbox.fail writes twice: with the payload, then state only.
			lose:      2,
			wantState: string(workflow.JobFailed),
			wantKind:  jobRecoveryFailedEvent,
			wantText:  "worker exited without settling its claimed run",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			t.Setenv("HERDR_BIN", filepath.Join(t.TempDir(), "herdr-must-not-run"))
			harness := newRemoteLifecycleHarness(t)
			home, paths, store := heartbeatLoopE2EHome(t)
			writeRemoteLifecycleConfig(t, paths, harness.control.URL)
			checkout := createDaemonWorkerGitCheckout(t, "main")
			makeReviewFixOriginFetchable(t, checkout, "main")
			runDaemonWorkerGit(t, checkout, "switch", "-c", "feature")
			writeReviewBaseFile(t, checkout, "under-review.txt", "review subject\n")
			runDaemonWorkerGit(t, checkout, "add", "-A")
			runDaemonWorkerGit(t, checkout, "commit", "-m", "review subject")
			head := daemonWorkerHeadSHA(t, checkout)
			serveReviewSourceFixture(t, paths, checkout, "ghs_unused2618")

			const repo = "owner/repo"
			jobID := "unsettled-exit-2618-" + tc.name
			seedDaemonWorkerRepo(t, store, repo, filepath.Join(home, "unavailable-host-checkout"))
			seedDaemonWorkerAgent(t, store, "settle-reviewer", runtime.ShellRuntime, tc.script, []string{"review"}, repo)
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName: repo, Number: 2618, URL: "https://example.invalid/owner/repo/pull/2618",
				HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, State: "open",
			}); err != nil {
				t.Fatal(err)
			}
			remote := "remote"
			job, err := workflow.NewMailbox(store).Enqueue(ctx, workflow.JobRequest{
				ID: jobID, Agent: "settle-reviewer", Action: "review",
				Repo: repo, PullRequest: 2618, HeadSHA: head, Branch: "feature",
				ReviewPurpose: db.DefaultReviewPurpose, Instructions: "review it",
				ExecBackend: &remote,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ExecForTest(ctx, fmt.Sprintf(`CREATE TRIGGER lose_terminal_writes BEFORE UPDATE OF state ON jobs
				WHEN OLD.state = 'running' AND NEW.state IN ('succeeded', 'failed', 'blocked')
				AND (SELECT COUNT(*) FROM job_events WHERE job_id = OLD.id AND kind = 'test_terminal_write_lost') < %d
				BEGIN
					INSERT INTO job_events (job_id, kind, message) VALUES (OLD.id, 'test_terminal_write_lost', NEW.state);
					SELECT RAISE(IGNORE);
				END`, tc.lose)); err != nil {
				t.Fatalf("install lost-write trigger: %v", err)
			}

			worker := executionBackendJobWorker(store, io.Discard, home)
			worker.RemoteEnvdEndpointResolver = func(string, int) string { return harness.envd.URL }
			worker.ExecutionBackendFactory = worker.defaultExecutionBackend
			worker.ReviewAdmissionGitHubFactory = func(string) remoteReviewAdmissionGitHub {
				return remoteReviewAdmissionGitHubStub{
					pull:   github.PullRequest{Number: 2618, HeadSHA: head, BaseRef: "main"},
					checks: []github.PullRequestCheck{{Name: "build", Bucket: "pass"}},
				}
			}
			worker.CommenterFactory = func(string) github.Client { return &cliPollFakeGitHub{} }
			runErr := worker.run(ctx, job)

			finished, err := store.GetJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := workflow.ParseJobPayload(finished.Payload)
			if err != nil {
				t.Fatal(err)
			}
			events, err := store.ListJobEvents(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			var trail []string
			var kindMessages []string
			lost := 0
			for _, event := range events {
				trail = append(trail, event.Kind)
				if event.Kind == "test_terminal_write_lost" {
					lost++
				}
				if event.Kind == tc.wantKind {
					kindMessages = append(kindMessages, event.Message)
				}
			}
			if lost != tc.lose {
				t.Fatalf("lost terminal writes = %d, want the injected %d (events %v)", lost, tc.lose, trail)
			}
			if finished.State != tc.wantState {
				t.Fatalf("state = %s after the worker returned (run err %v), want %s; events %v", finished.State, runErr, tc.wantState, trail)
			}
			if len(kindMessages) == 0 || !strings.Contains(strings.Join(kindMessages, "\n"), tc.wantText) {
				t.Fatalf("%s events = %q, want one containing %q; events %v", tc.wantKind, kindMessages, tc.wantText, trail)
			}
			if tc.wantState == string(workflow.JobSucceeded) {
				if payload.Result == nil || payload.Result.Summary != "reviewed 2618" {
					t.Fatalf("stored result = %+v, want the reviewer's result", payload.Result)
				}
				// The advancement RunJob never reached is left to the advance-retry pass.
				needsRetry, err := worker.jobNeedsAdvanceRetry(ctx, job.ID)
				if err != nil || !needsRetry {
					t.Fatalf("advance retry pending = %v (err %v), want true; events %v", needsRetry, err, trail)
				}
			}
		})
	}
}
