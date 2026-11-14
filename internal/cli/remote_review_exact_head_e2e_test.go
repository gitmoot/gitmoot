//go:build e2e

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// remoteReviewWorkspaceProbe is the review runtime. It reports what an
// exact-head reviewer can establish inside the instance with ordinary git and
// the GITMOOT_PRIOR_VERDICTS file, and nothing else.
const remoteReviewWorkspaceProbe = `head=$(git rev-parse HEAD)
base=$(git rev-parse "@{upstream}")
files=$(git diff --name-only "@{upstream}" HEAD | tr '\n' ',')
prior=absent
if [ -n "${GITMOOT_PRIOR_VERDICTS:-}" ] && grep -q PRIOR-TITLE-2281 "$GITMOOT_PRIOR_VERDICTS" && grep -q PRIOR-DETAIL-2281 "$GITMOOT_PRIOR_VERDICTS"; then
  prior=present
fi
printf '{"gitmoot_result":{"decision":"approved","evidence":"executed","summary":"head=%s base=%s files=%s prior=%s","findings":[],"changes_made":[],"tests_run":["git rev-parse HEAD"],"needs":[]}}' "$head" "$base" "$files" "$prior"`

// #2281: a policy-routed remote review ended blocked because the instance
// had neither the exact head commit nor the prior findings ("commit <head> is
// absent locally, GitHub access returned 401, prior review findings
// unavailable"). This drives a review job through jobWorker.run onto the real
// remote backend and checks what the runtime itself can see there.
func TestRemoteReviewRunsOnExactHeadWithBaseAndPriorVerdicts(t *testing.T) {
	ctx := context.Background()
	const secret = "ghs_GITMOOT2281hostCREDENTIAL"
	t.Setenv("HERDR_BIN", filepath.Join(t.TempDir(), "herdr-must-not-run"))
	harness := newRemoteLifecycleHarness(t)
	home, paths, store := daemonE2EHome(t)
	writeRemoteLifecycleConfig(t, paths, harness.control.URL)
	contents, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.Replace(string(contents), `backend = "remote"`, `backend = "local"`, 1))
	if err := os.WriteFile(paths.ConfigFile, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	checkout := createDaemonWorkerGitCheckout(t, "main")
	base := daemonWorkerHeadSHA(t, checkout)
	makeReviewFixOriginFetchable(t, checkout, "main")
	runDaemonWorkerGit(t, checkout, "switch", "-c", "feature")
	writeReviewBaseFile(t, checkout, "under-review.txt", "review subject\n")
	runDaemonWorkerGit(t, checkout, "add", "-A")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "review subject")
	head := daemonWorkerHeadSHA(t, checkout)
	runDaemonWorkerGit(t, checkout, "config", "http.https://github.com/.extraheader", "AUTHORIZATION: basic "+secret)
	serveReviewSourceFixture(t, paths, checkout, secret)
	appendConfig(t, paths, "\n[disk_guard]\nmin_free_bytes = 10000000000\nmin_free_percent = 0\nremote_reviews = true\nremote_reviews_below_bytes = 20000000000\n")
	enableJanitorCapacity(t, paths)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 500_000_000_000, FreeBytes: 19_999_999_999}, nil
	})
	device, err := janitorDeviceOf(paths.Home)
	if err != nil {
		t.Fatal(err)
	}
	previousStatus := runJanitorStatus
	runJanitorStatus = func(context.Context, string, time.Duration) ([]byte, error) {
		volume := fmt.Sprintf(`{"filesystem_key":"device:%d","device":%d,"state":"cleanup_needed","admission_hold":false,"available_bytes":19999999999,"reserve_bytes":10000000000,"target_bytes":20000000000,"reason":"warning band"}`, device, device)
		return []byte(janitorStatusJSON(time.Now(), time.Minute, janitorTestPolicyHash, volume)), nil
	}
	t.Cleanup(func() { runJanitorStatus = previousStatus })

	const repo = "owner/repo"
	seedDaemonWorkerRepo(t, store, repo, filepath.Join(home, "unavailable-host-checkout"))
	seedDaemonWorkerAgent(t, store, "remote-review-agent", runtime.ShellRuntime, remoteReviewWorkspaceProbe, []string{"review"}, repo)
	// The summary deliberately does not enumerate the finding: only the
	// individual finding carries its title and detail, as on PR #1202 where a
	// three-finding verdict summarised one failure.
	priorPayload, err := json.Marshal(workflow.JobPayload{
		Repo: repo, PullRequest: 2281, HeadSHA: base,
		Result: &workflow.AgentResult{
			Decision: "changes_requested", Severity: "P2", Evidence: workflow.EvidenceExecuted,
			Summary: "one failure",
			Findings: []json.RawMessage{
				json.RawMessage(`{"severity":"P2","title":"PRIOR-TITLE-2281","location":"main.go:1","description":"PRIOR-DETAIL-2281 the base is not refreshed"}`),
				json.RawMessage(`"a second, bare finding"`),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "prior-review-2281", Agent: "earlier-reviewer", Type: "review",
		State: string(workflow.JobSucceeded), Payload: string(priorPayload),
	}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "verdict"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo, Number: 2281, URL: "https://example.invalid/owner/repo/pull/2281",
		HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	job, err := workflow.NewMailbox(store).Enqueue(ctx, workflow.JobRequest{
		ID: "remote-review-exact-head", Agent: "remote-review-agent", Action: "review",
		Repo: repo, PullRequest: 2281, HeadSHA: head, Branch: "feature",
		ReviewPurpose: db.DefaultReviewPurpose, Instructions: "review the exact head",
	})
	if err != nil {
		t.Fatal(err)
	}

	worker := executionBackendJobWorker(store, io.Discard, home)
	worker.RemoteEnvdEndpointResolver = func(string, int) string { return harness.envd.URL }
	worker.ExecutionBackendFactory = worker.defaultExecutionBackend
	worker.ReviewAdmissionGitHubFactory = func(string) remoteReviewAdmissionGitHub {
		return remoteReviewAdmissionGitHubStub{
			pull:   github.PullRequest{Number: 2281, HeadSHA: head, BaseRef: "main"},
			checks: []github.PullRequestCheck{{Name: "build", Bucket: "pass"}},
		}
	}
	worker.CommenterFactory = func(string) github.Client { return &cliPollFakeGitHub{} }
	if pending := pendingIDs(t, ctx, worker); strings.Join(pending, ",") != job.ID {
		t.Fatalf("warning-band dispatch = %v, want only %s", pending, job.ID)
	}
	job, err = store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	routed, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if routed.ExecBackend != string(execbackend.Remote) || !routed.DiskGuardRouted || routed.WorktreePath != "" {
		t.Fatalf("warning-band review did not route to E2B before local preparation: %+v", routed)
	}
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
	if payload.Result == nil {
		events, _ := store.ListJobEvents(ctx, job.ID)
		t.Fatalf("review job %s produced no result; events: %+v", finished.State, events)
	}
	want := "head=" + head + " base=" + base + " files=under-review.txt, prior=present"
	if payload.Result.Summary != want {
		t.Fatalf("what the remote reviewer saw = %q\nwant %q", payload.Result.Summary, want)
	}

	// Nothing the host could authenticate to GitHub with reached the instance.
	_, deleted, _, _, _ := harness.snapshot()
	if len(deleted) != 1 {
		t.Fatalf("provider deletes = %v, want the review instance destroyed", deleted)
	}
	harness.mu.Lock()
	uploads := make(map[string][]byte, len(harness.uploads))
	for name, data := range harness.uploads {
		uploads[name] = data
	}
	harness.mu.Unlock()
	if _, exists := uploads["/home/user/.gitmoot-sync.tar.gz"]; exists {
		t.Fatal("remote source review uploaded a host workspace archive")
	}
	for name, content := range uploads {
		if bytes.Contains(content, []byte(secret)) {
			t.Fatalf("upload %q carries the host credential", name)
		}
	}
}
