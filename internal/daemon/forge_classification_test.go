package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestPollOnceClassifiesForgeFailuresByAnswerNotByTransience pins which forge failures
// are an ANSWER and which are merely a failure to read.
//
// The first version of this split keyed the quiet arm on "not transient". Review measured
// what that swallowed: 403 permission loss, 401 bad credentials, 429 rate limit,
// context.Canceled and gh parse errors ALL wrote a permanent "this number is not a pull
// request" record about a possibly real PR and returned a clean poll. transientSignatures
// is deliberately narrow - transport, DNS, TLS, gateway 5xx, and by its own comment NOT
// rate limits - so "not transient" is not the complement of "definitive".
//
// The rule this pins is fail-loud on ambiguity: only a 404-shaped answer is definitive.
// Anything else surfaces, because a red poll that clears costs an operator a look, while
// the opposite mistake strands a job silently and forever.
func TestPollOnceClassifiesForgeFailuresByAnswerNotByTransience(t *testing.T) {
	for _, test := range []struct {
		name        string
		err         error
		wantPollErr bool
		wantRecord  int
	}{
		{name: "404 is an answer", err: errors.New("gh: Not Found (HTTP 404)"), wantPollErr: false, wantRecord: 1},
		{name: "403 is not an answer", err: errors.New("gh: Resource not accessible by integration (HTTP 403)"), wantPollErr: true, wantRecord: 0},
		{name: "401 is not an answer", err: errors.New("gh: Bad credentials (HTTP 401)"), wantPollErr: true, wantRecord: 0},
		{name: "429 is not an answer", err: errors.New("gh: API rate limit exceeded (HTTP 429)"), wantPollErr: true, wantRecord: 0},
		{name: "cancellation is not an answer", err: context.Canceled, wantPollErr: true, wantRecord: 0},
		{name: "parse failure is not an answer", err: errors.New("parse gh output: invalid character 'x'"), wantPollErr: true, wantRecord: 0},
		{name: "transient stays transient", err: errors.New("dial tcp: connection refused"), wantPollErr: true, wantRecord: 0},
		// THESE TWO REACH THE COMPETING-STATUS FILTER, which every row above returns
		// before: they carry the words "not found" AND a competing status. Review measured
		// that deleting the filter entirely left this package green, because no existing
		// row got past the earlier `not found` guard - a live production loop with zero
		// coverage, and it is the exact heuristic I flagged as my risky over-correction.
		{name: "429 body mentioning not found is not an answer", err: errors.New("gh: API rate limit exceeded, pull request not found (HTTP 429)"), wantPollErr: true, wantRecord: 0},
		{name: "403 body mentioning not found is not an answer", err: errors.New("gh: Resource not accessible, not found (HTTP 403)"), wantPollErr: true, wantRecord: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, repo, client := closedPullRequestSweepFixture(t)
			client.getPullRequestErr = test.err
			seedQueuedJob(t, store, "subject", "audit", "review", workflow.JobPayload{
				Repo: repo.FullName(), Branch: "task-12", PullRequest: 12, TaskID: "task-12",
				LeadAgent: "lead", Sender: "github", ParentJobID: "issue-coordinator", DelegationID: "subject",
			})
			engine := workflow.Engine{Store: store}
			daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

			err := daemon.PollOnce(ctx)
			if test.wantPollErr && err == nil {
				t.Fatalf("PollOnce = nil, want an error: %q is not an answer about the number, so classifying it as one writes a false permanent claim", test.err)
			}
			if !test.wantPollErr && err != nil {
				t.Fatalf("PollOnce = %v, want nil: a definitive not-found must not red the poll on every tick", err)
			}
			if got := countJobEventKind(t, store, "subject", pullRequestUnresolvedEvent); got != test.wantRecord {
				t.Fatalf("%s events = %d, want %d", pullRequestUnresolvedEvent, got, test.wantRecord)
			}
			// FAIL-CLOSED IN EVERY ARM: no classification may terminate the job.
			if job, gerr := store.GetJob(ctx, "subject"); gerr != nil || job.State != string(workflow.JobQueued) {
				t.Fatalf("job state = %q err=%v, want queued in every arm", job.State, gerr)
			}
		})
	}
}

// TestTransientSignatureIsNotTheComplementOfDefinitive is the measurement behind the
// test above, asserted directly so a future edit cannot quietly reintroduce the
// "not transient means definitive" equivalence.
func TestTransientSignatureIsNotTheComplementOfDefinitive(t *testing.T) {
	for _, text := range []string{
		"gh: Resource not accessible by integration (HTTP 403)",
		"gh: Bad credentials (HTTP 401)",
		"gh: API rate limit exceeded (HTTP 429)",
	} {
		err := errors.New(text)
		if github.IsTransientMessage(text) {
			t.Fatalf("%q reads as transient; this test exists because it does NOT", text)
		}
		if isPullRequestNotFound(err) {
			t.Fatalf("%q classified as a definitive not-found: it is neither transient nor an answer, which is exactly the gap", text)
		}
	}
}
