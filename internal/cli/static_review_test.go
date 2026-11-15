package cli

import (
	"context"
	"errors"
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

// hostCIStub is the host's GitHub view of a review's exact head.
type hostCIStub struct {
	head   string
	checks []github.PullRequestCheck
	err    error
}

func (s hostCIStub) ListCheckRunsForRef(_ context.Context, _ github.Repository, ref string) ([]github.PullRequestCheck, error) {
	if s.err != nil {
		return nil, s.err
	}
	if ref != s.head {
		return nil, nil
	}
	return s.checks, nil
}

func (hostCIStub) ActionsJobLog(context.Context, github.Repository, int64) (string, error) {
	return "", nil
}

// runExecutedReviewWithHostCI runs an ordinary (executed) review of owner/repo
// at its checkout's head through jobWorker.run and returns the prompts the
// runtime was given.
func runExecutedReviewWithHostCI(t *testing.T, ci func(head string) hostCIStub) []string {
	t.Helper()
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	head := daemonWorkerHeadSHA(t, checkout)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgentWithPolicy(t, store, "shell-reviewer", runtime.ShellRuntime,
		"true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "host-ci-review", Agent: "shell-reviewer", Action: "review", Repo: "owner/repo",
		PullRequest: 43, HeadSHA: head, WorktreePath: checkout, Instructions: "review the exact head",
	})
	adapter := &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","evidence":"executed","summary":"reviewed","findings":[],"changes_made":[],"tests_run":["go test ./... -> ok"],"needs":[]}}`}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return checkout, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return adapter, nil
	}
	worker.StaticReviewCIGitHubFactory = func(string) staticReviewCIGitHub { return ci(head) }
	job, err := store.GetJob(ctx, "host-ci-review")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.run(ctx, job); err != nil {
		t.Fatalf("worker.run: %v", err)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.prompts) == 0 {
		events, _ := store.ListJobEvents(ctx, job.ID)
		t.Fatalf("review was never delivered; events=%+v", events)
	}
	return append([]string(nil), adapter.prompts...)
}

// TestExecutedReviewPromptCarriesHostFetchedCI is #2664: a read-only seat has
// no GitHub credential, so it cannot read a private repository's Actions runs,
// and an executed review blocked on a check it could not run even though the
// exact-head CI was green. The host fetches that CI for every review, not only
// a no-execution one, and the reviewer gets it with the run and job ids it
// must cite.
func TestExecutedReviewPromptCarriesHostFetchedCI(t *testing.T) {
	const link = "https://github.com/owner/repo/actions/runs/43688504726/job/126845298023"
	prompts := runExecutedReviewWithHostCI(t, func(head string) hostCIStub {
		return hostCIStub{head: head, checks: []github.PullRequestCheck{{Name: "ci / test", State: "success", Link: link}}}
	})
	prompt := prompts[0]
	for _, want := range []string{
		"HOST-FETCHED EXACT-HEAD CI EVIDENCE (fetched by the Gitmoot host",
		"- ci / test: success (" + link + ")",
		"you may cite it",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("executed review prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "NO-EXECUTION REVIEW") {
		t.Fatalf("executed review was told it cannot run anything:\n%s", prompt)
	}
}

// TestReviewCIFetchErrorStillDispatchesReview guards #2664: CI the host
// could not fetch is stated as unavailable and the review runs anyway.
func TestReviewCIFetchErrorStillDispatchesReview(t *testing.T) {
	prompts := runExecutedReviewWithHostCI(t, func(head string) hostCIStub {
		return hostCIStub{head: head, err: errors.New("HTTP 502 from GitHub")}
	})
	if want := "- CI evidence unavailable: listing check runs failed: HTTP 502 from GitHub"; !strings.Contains(prompts[0], want) {
		t.Fatalf("review prompt lacks %q:\n%s", want, prompts[0])
	}
}

// TestWorkerEnforcesRepositoryNoExecutionPolicy: a repository declaring
// execution = "none" makes EVERY review of it a no-execution review, whatever
// queued the job, so a review that cannot run under that contract (here a
// shell reviewer) is failed before any runtime launches. A review of the same
// shape with no declaration, or with an unreadable one, shows both sides of
// the decision.
func TestWorkerEnforcesRepositoryNoExecutionPolicy(t *testing.T) {
	cases := []struct {
		name         string
		config       string
		wantLaunched bool
		wantFailure  string
	}{
		{name: "undeclared runs", config: "", wantLaunched: true},
		{name: "declared none refuses a non-omp reviewer", config: "[repos.\"owner/repo\".review]\nexecution = \"none\"\n", wantFailure: "not supported on runtime \"shell\""},
		{name: "unreadable declaration refuses", config: "[repos.\"owner/repo\".review]\nexecution = \"sometimes\"\n", wantFailure: "review execution policy is unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, home := blockerE2EHome(t)
			if tc.config != "" {
				paths, err := pathsFromFlag(home)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
					t.Fatal(err)
				}
				existing, _ := os.ReadFile(paths.ConfigFile)
				if err := os.WriteFile(paths.ConfigFile, append(existing, []byte("\n"+tc.config)...), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
			seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
			seedDaemonWorkerAgentWithPolicy(t, store, "shell-reviewer", runtime.ShellRuntime,
				"true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
			enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
				ID: "policy-review", Agent: "shell-reviewer", Action: "review", Repo: "owner/repo",
				WorktreePath: checkout, ReadOnlySeat: true,
			})
			launched := false
			worker := defaultJobWorker(store, io.Discard, home)
			worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
				return checkout, nil
			}
			worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
				launched = true
				return &cliWorkerFakeAdapter{output: `{"gitmoot_result":{"decision":"approved","summary":"reviewed","findings":[],"changes_made":[],"tests_run":[],"needs":[]}}`}, nil
			}
			job, err := store.GetJob(ctx, "policy-review")
			if err != nil {
				t.Fatal(err)
			}
			if err := worker.run(ctx, job); err != nil {
				t.Fatal(err)
			}
			if launched != tc.wantLaunched {
				t.Fatalf("runtime launched = %v, want %v", launched, tc.wantLaunched)
			}
			if tc.wantFailure == "" {
				return
			}
			job, err = store.GetJob(ctx, "policy-review")
			if err != nil {
				t.Fatal(err)
			}
			if job.State != string(workflow.JobFailed) {
				t.Fatalf("state = %q, want failed", job.State)
			}
			events, err := store.ListJobEvents(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Kind == string(workflow.JobFailed) && strings.Contains(event.Message, tc.wantFailure) {
					return
				}
			}
			t.Fatalf("no failure naming %q; events=%+v", tc.wantFailure, events)
		})
	}
}
