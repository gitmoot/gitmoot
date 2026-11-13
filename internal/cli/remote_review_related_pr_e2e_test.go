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

// remoteRelatedProbe reports what the reviewer sees of the related tree in
// its sandbox workspace, after an optional extra step.
const remoteRelatedProbe = `seen=$(cat .gitmoot-related/pr-7/db/schema.sql 2>/dev/null | tr -d '\n' || echo absent)
%s
printf '{"gitmoot_result":{"decision":"approved","evidence":"executed","summary":"related=%%s","findings":[],"changes_made":[],"tests_run":["cat related"],"needs":[],"delegations":[]}}' "$seen"`

// #2509 / #2534 review P2-3. A related tree reaches a remote review's sandbox
// at its pinned digest, and a reviewer that restores write bits and edits it
// gets its verdict rejected after delivery.
func TestRemoteReviewRelatedPRIsVerifiedInsideTheSandbox(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate string
		wantOK bool
	}{
		{name: "delivered", wantOK: true},
		{name: "altered_in_sandbox", mutate: `chmod -R u+w .gitmoot-related/pr-7 && printf 'DROP TABLE sibling;\n' > .gitmoot-related/pr-7/db/schema.sql`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			t.Setenv("HERDR_BIN", filepath.Join(t.TempDir(), "herdr-must-not-run"))
			origin, _ := relatedPROrigin(t)
			related, err := resolveReviewRelatedPRs(ctx, origin, "owner/repo", []int{7})
			if err != nil {
				t.Fatal(err)
			}
			harness := newRemoteLifecycleHarness(t)
			restoreWriteBitsOnCleanup(t, filepath.Dir(harness.workspace))
			home, paths, store := heartbeatLoopE2EHome(t)
			writeRemoteLifecycleConfig(t, paths, harness.control.URL)
			checkout := createDaemonWorkerGitCheckout(t, "main")
			makeReviewFixOriginFetchable(t, checkout, "main")
			runDaemonWorkerGit(t, checkout, "switch", "-c", "feature")
			writeReviewBaseFile(t, checkout, "under-review.txt", "review subject\n")
			runDaemonWorkerGit(t, checkout, "add", "-A")
			runDaemonWorkerGit(t, checkout, "commit", "-m", "review subject")
			head := daemonWorkerHeadSHA(t, checkout)
			serveReviewSourceFixture(t, paths, checkout, "ghs_unused2509")
			const repo = "owner/repo"
			seedDaemonWorkerRepo(t, store, repo, filepath.Join(home, "unavailable-host-checkout"))
			record, err := store.GetRepo(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			record.RemoteURL = origin
			if err := store.UpsertRepo(ctx, record); err != nil {
				t.Fatal(err)
			}
			seedDaemonWorkerAgent(t, store, "related-reviewer", runtime.ShellRuntime, fmt.Sprintf(remoteRelatedProbe, tc.mutate), []string{"review"}, repo)
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName: repo, Number: 2509, URL: "https://example.invalid/owner/repo/pull/2509",
				HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, State: "open",
			}); err != nil {
				t.Fatal(err)
			}
			remote := "remote"
			job, err := workflow.NewMailbox(store).Enqueue(ctx, workflow.JobRequest{
				ID: "related-review", Agent: "related-reviewer", Action: "review",
				Repo: repo, PullRequest: 2509, HeadSHA: head, Branch: "feature",
				ReviewPurpose: db.DefaultReviewPurpose, Instructions: "check against the sibling",
				ExecBackend: &remote, ReviewRelatedPRs: related,
			})
			if err != nil {
				t.Fatal(err)
			}
			worker := executionBackendJobWorker(store, io.Discard, home)
			worker.RemoteEnvdEndpointResolver = func(string, int) string { return harness.envd.URL }
			worker.ExecutionBackendFactory = worker.defaultExecutionBackend
			worker.ReviewAdmissionGitHubFactory = func(string) remoteReviewAdmissionGitHub {
				return remoteReviewAdmissionGitHubStub{
					pull:   github.PullRequest{Number: 2509, HeadSHA: head, BaseRef: "main"},
					checks: []github.PullRequestCheck{{Name: "build", Bucket: "pass"}},
				}
			}
			worker.CommenterFactory = func(string) github.Client { return &cliPollFakeGitHub{} }
			if err := worker.run(ctx, job); err != nil {
				t.Fatalf("worker.run: %v", err)
			}
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
			kinds := map[string][]string{}
			for _, event := range events {
				kinds[event.Kind] = append(kinds[event.Kind], event.Message)
			}
			installed := strings.Join(kinds[reviewRelatedPRInstalledEventKind], " ")
			if !strings.Contains(installed, related[0].TreeSHA256) || !strings.Contains(installed, `"backend":"remote"`) {
				t.Fatalf("no remote install receipt with the pinned digest: %v", kinds)
			}
			if !tc.wantOK {
				if finished.State == string(workflow.JobSucceeded) || len(kinds[reviewRelatedPRAlteredEventKind]) == 0 {
					t.Fatalf("altered related tree: state=%s events=%v; want the verdict rejected", finished.State, kinds)
				}
				return
			}
			if finished.State != string(workflow.JobSucceeded) || payload.Result == nil {
				t.Fatalf("review state=%s result=%v events=%v", finished.State, payload.Result, kinds)
			}
			if payload.Result.Summary != "related=CREATE TABLE sibling (id INT);" {
				t.Fatalf("what the reviewer saw = %q", payload.Result.Summary)
			}
			if len(kinds[reviewRelatedPRVerifiedEventKind]) == 0 {
				t.Fatalf("no post-delivery verification: %v", kinds)
			}
		})
	}
}
