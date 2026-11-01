//go:build e2e

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// remoteEvidenceProbe reports a digest of the evidence directory exactly as
// the reviewer runtime sees it inside the instance.
const remoteEvidenceProbe = `ev=absent
if [ -n "${GITMOOT_REVIEW_EVIDENCE_DIR:-}" ]; then ev=$(ls "$GITMOOT_REVIEW_EVIDENCE_DIR" | tr '\n' ','):$(cat "$GITMOOT_REVIEW_EVIDENCE_DIR"/* | sha256sum | cut -c1-16); fi
%s
printf '{"gitmoot_result":{"decision":"approved","evidence":"executed","summary":"evidence=%%s","findings":[],"changes_made":[],"tests_run":["ls evidence"],"needs":[],"delegations":[]}}' "$ev"`

// #2476. Pinned private evidence reaches a remote review's sandbox, is read
// back there before the reviewer starts, and is re-verified after delivery:
// host drift refuses provisioning, and a change inside the sandbox rejects the
// verdict.
func TestRemoteReviewEvidenceIsVerifiedInsideTheSandbox(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    string // extra probe step run by the reviewer
		driftHost bool   // rewrite a host file after the request pinned it
		wantKind  string
		wantOK    bool
	}{
		{name: "delivered", wantKind: reviewEvidenceVerifiedEventKind, wantOK: true},
		{name: "altered_in_sandbox", mutate: `for f in "$GITMOOT_REVIEW_EVIDENCE_DIR"/01-*; do chmod u+w "$f" && printf x >> "$f"; done`, wantKind: reviewEvidenceAlteredEventKind},
		{name: "drifted_on_host", driftHost: true},
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
			serveReviewSourceFixture(t, paths, checkout, "ghs_unused2476")

			evidenceDir := t.TempDir()
			files := map[string]string{"native.log": "native log line\n", "screen shot.png": "\x89PNG fake image bytes"}
			var entries []map[string]any
			for _, name := range []string{"native.log", "screen shot.png"} {
				target := filepath.Join(evidenceDir, name)
				if err := os.WriteFile(target, []byte(files[name]), 0o600); err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256([]byte(files[name]))
				entries = append(entries, map[string]any{"path": target, "bytes": len(files[name]), "sha256": hex.EncodeToString(sum[:])})
			}
			manifest, _ := json.Marshal(map[string]any{"schema_version": 1, "head_sha": head, "files": entries})
			manifestPath := filepath.Join(evidenceDir, "manifest.json")
			if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
				t.Fatal(err)
			}
			evidence, err := workflow.LoadReviewEvidence(manifestPath, head)
			if err != nil {
				t.Fatal(err)
			}

			const repo = "owner/repo"
			seedDaemonWorkerRepo(t, store, repo, filepath.Join(home, "unavailable-host-checkout"))
			seedDaemonWorkerAgent(t, store, "evidence-reviewer", runtime.ShellRuntime, fmt.Sprintf(remoteEvidenceProbe, tc.mutate), []string{"review"}, repo)
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName: repo, Number: 2476, URL: "https://example.invalid/owner/repo/pull/2476",
				HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, State: "open",
			}); err != nil {
				t.Fatal(err)
			}
			remote := "remote"
			job, err := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("worker owns checkout")).Enqueue(ctx, workflow.JobRequest{
				ID: "evidence-review", Agent: "evidence-reviewer", Action: "review",
				Repo: repo, PullRequest: 2476, HeadSHA: head, Branch: "feature",
				ReviewPurpose: db.DefaultReviewPurpose, Instructions: "inspect the evidence",
				ExecBackend: &remote, ReviewEvidence: evidence,
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.driftHost {
				if err := os.WriteFile(filepath.Join(evidenceDir, "native.log"), []byte("native log line edited\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			worker := executionBackendJobWorker(store, io.Discard, home)
			worker.RemoteEnvdEndpointResolver = func(string, int) string { return harness.envd.URL }
			worker.ExecutionBackendFactory = worker.defaultExecutionBackend
			worker.ReviewAdmissionGitHubFactory = func(string) remoteReviewAdmissionGitHub {
				return remoteReviewAdmissionGitHubStub{
					pull:   github.PullRequest{Number: 2476, HeadSHA: head, BaseRef: "main"},
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
			if tc.driftHost {
				if finished.State == string(workflow.JobSucceeded) || payload.Result != nil || len(kinds[reviewEvidenceVerifiedEventKind]) != 0 {
					t.Fatalf("host drift: state=%s result=%v verified=%v; want refusal before the reviewer ran", finished.State, payload.Result, kinds[reviewEvidenceVerifiedEventKind])
				}
				failure := strings.Join(kinds[string(workflow.JobFailed)], " ")
				if !strings.Contains(failure, "01-native.log changed") {
					t.Fatalf("host drift failure = %q, want it to name the changed sandbox file", failure)
				}
				// The failure becomes the job event, PR comment and dashboard text.
				for _, event := range events {
					if strings.Contains(event.Message, evidenceDir) {
						t.Fatalf("event %s publishes the host evidence path: %q", event.Kind, event.Message)
					}
				}
				return
			}
			delivered := kinds[reviewEvidenceVerifiedEventKind]
			if len(delivered) == 0 || !strings.Contains(delivered[0], `"phase":"delivered"`) || !strings.Contains(delivered[0], evidence.ManifestSHA256) {
				t.Fatalf("no in-sandbox delivery receipt: %v", kinds)
			}
			var receipt reviewEvidenceReceipt
			if err := json.Unmarshal([]byte(delivered[0]), &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.HeadSHA != head || receipt.JobID != job.ID || len(receipt.Files) != 2 || !receipt.Files[0].Match || !receipt.Files[1].Match {
				t.Fatalf("delivery receipt = %+v", receipt)
			}
			if !tc.wantOK {
				if finished.State == string(workflow.JobSucceeded) || len(kinds[reviewEvidenceAlteredEventKind]) == 0 {
					t.Fatalf("altered evidence: state=%s events=%v; want the verdict rejected", finished.State, kinds)
				}
				return
			}
			if finished.State != string(workflow.JobSucceeded) || payload.Result == nil {
				t.Fatalf("review state=%s result=%v events=%v", finished.State, payload.Result, kinds)
			}
			wantDigest := sha256.Sum256([]byte(files["native.log"] + files["screen shot.png"]))
			want := "evidence=01-native.log,02-screen_shot.png,:" + hex.EncodeToString(wantDigest[:])[:16]
			if payload.Result.Summary != want {
				t.Fatalf("what the reviewer saw = %q, want %q", payload.Result.Summary, want)
			}
			after := kinds[reviewEvidenceVerifiedEventKind]
			if len(after) < 2 || !strings.Contains(after[len(after)-1], `"phase":"after_delivery"`) {
				t.Fatalf("no post-delivery re-verification: %v", after)
			}
		})
	}
}
