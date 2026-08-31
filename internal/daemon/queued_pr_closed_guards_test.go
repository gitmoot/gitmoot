package daemon

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestPollOnceLeavesRetriedJobAloneAfterSupersede pins the sixth exemption class. The
// sweep terminates a queued leg; an operator runs `gitmoot job retry`, which accepts
// `cancelled` and writes the row back to `queued` with a `retry_queued` event. Without
// this the next poll cancelled it again, forever: an explicit instruction silently
// undone in a loop, which is the failure this sweep exists to remove rather than
// create. The test is ORDER, not presence — a retry NEWER than the newest supersede
// means "I know, do it anyway".
func TestPollOnceLeavesRetriedJobAloneAfterSupersede(t *testing.T) {
	ctx := context.Background()
	store, repo, client := closedPullRequestSweepFixture(t)
	seedQueuedJob(t, store, "fanout-review", "audit", "review", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7",
		LeadAgent: "lead", Sender: "github",
	})
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce 1: %v", err)
	}
	if job, err := store.GetJob(ctx, "fanout-review"); err != nil || job.State != string(workflow.JobCancelled) {
		t.Fatalf("after poll 1 state = %+v err=%v, want cancelled", job.State, err)
	}

	// The operator disagrees.
	if _, err := workflow.RetryJob(ctx, store, "fanout-review"); err != nil {
		t.Fatalf("RetryJob: %v", err)
	}
	if job, err := store.GetJob(ctx, "fanout-review"); err != nil || job.State != string(workflow.JobQueued) {
		t.Fatalf("after retry state = %+v err=%v, want queued", job.State, err)
	}

	for poll := range 3 {
		if err := daemon.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", poll+2, err)
		}
	}
	job, err := store.GetJob(ctx, "fanout-review")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("state after 3 more polls = %q, want queued: the sweep undid an operator's retry", job.State)
	}
	// Exactly one supersede on the record — the pre-retry one.
	events, err := store.ListJobEvents(ctx, "fanout-review")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	superseded := 0
	for _, event := range events {
		if event.Kind == workflow.JobEventSupersededPullRequestClosed {
			superseded++
		}
	}
	if superseded != 1 {
		t.Fatalf("%s events = %d, want 1 (the one before the retry)", workflow.JobEventSupersededPullRequestClosed, superseded)
	}
}

// TestPollOnceIsUnaffectedByAnotherRepoUndecodablePayload pins the regression the
// verdict measured on both sides: the sweep used to decode every queued payload in the
// HOME before filtering by repo, so one undecodable row in any repo failed every
// watched repo's poll — permanently, because the condition never clears.
func TestPollOnceIsUnaffectedByAnotherRepoUndecodablePayload(t *testing.T) {
	ctx := context.Background()
	store, repo, client := closedPullRequestSweepFixture(t)
	// Valid JSON, undecodable into JobPayload: pull_request is a string.
	if err := store.CreateJob(ctx, db.Job{
		ID: "foreign-bad-payload", Agent: "audit", Type: "review", State: string(workflow.JobQueued),
		Payload: `{"repo":"gitmoot/other","pull_request":"7"}`,
	}); err != nil {
		t.Fatalf("CreateJob foreign: %v", err)
	}
	seedQueuedJob(t, store, "mine-review", "audit", "review", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7",
		LeadAgent: "lead", Sender: "github",
	})
	// Same repo, different casing on the payload. Forge repository identity is
	// case-insensitive, so this is this daemon's work and must be swept; a
	// case-sensitive comparison left rows like it stranded forever.
	seedQueuedJob(t, store, "case-variant-review", "audit", "review", workflow.JobPayload{
		Repo: "Gitmoot/Gitmoot", Branch: "task-7", PullRequest: 7, TaskID: "task-7",
		LeadAgent: "lead", Sender: "github",
	})
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	for poll := range 3 {
		if err := daemon.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v (another repo's row must not fail this repo's poll)", poll+1, err)
		}
	}
	if job, err := store.GetJob(ctx, "mine-review"); err != nil || job.State != string(workflow.JobCancelled) {
		t.Fatalf("this repo's leg = %+v err=%v, want cancelled", job.State, err)
	}
	// The foreign row is untouched: not this daemon's business.
	if job, err := store.GetJob(ctx, "foreign-bad-payload"); err != nil || job.State != string(workflow.JobQueued) {
		t.Fatalf("foreign row = %+v err=%v, want queued and untouched", job.State, err)
	}
	if job, err := store.GetJob(ctx, "case-variant-review"); err != nil || job.State != string(workflow.JobCancelled) {
		t.Fatalf("case-variant row = %+v err=%v, want cancelled: repo identity is case-insensitive", job.State, err)
	}
}

// TestPollOnceAsksTheForgeOncePerNumberPerPoll pins the cost bound. Several queued jobs
// bound to the same unrecorded number must cost ONE forge answer per poll, not one per
// job: an issue-bound job can never be terminated, so an undeduped lookup repeats
// forever, once per job per poll.
func TestPollOnceAsksTheForgeOncePerNumberPerPoll(t *testing.T) {
	ctx := context.Background()
	store, repo, client := closedPullRequestSweepFixture(t)
	for _, id := range []string{"issue-child-a", "issue-child-b", "issue-child-c"} {
		seedQueuedJob(t, store, id, "audit", "ask", workflow.JobPayload{
			// An ISSUE number: absent from the PR listing for a reason that has nothing
			// to do with being closed, and absent from pull_requests too.
			Repo: repo.FullName(), Branch: "task-12", PullRequest: 12, TaskID: "task-12",
			LeadAgent: "lead", Sender: "github", ParentJobID: "issue-coordinator", DelegationID: id,
		})
	}
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	const polls = 4
	for poll := range polls {
		if err := daemon.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", poll+1, err)
		}
	}
	asked := 0
	for _, number := range client.getPullRequestCalls {
		if number == 12 {
			asked++
		}
	}
	if asked != polls {
		t.Fatalf("forge asked about #12 %d times across %d polls with 3 jobs, want exactly one per poll", asked, polls)
	}
	for _, id := range []string{"issue-child-a", "issue-child-b", "issue-child-c"} {
		if job, err := store.GetJob(ctx, id); err != nil || job.State != string(workflow.JobQueued) {
			t.Fatalf("%s = %+v err=%v, want queued: an issue number is not a closed PR", id, job.State, err)
		}
	}
}

func TestPollOnceDoesNotReuseForgeAnswersAcrossPolls(t *testing.T) {
	ctx := context.Background()
	store, repo, client := closedPullRequestSweepFixture(t)
	client.pullsByNumber[7] = github.PullRequest{
		Number: 7, State: "open", HeadRef: "task-7", BaseRef: "main", HeadSHA: "head-seven",
	}
	seedQueuedJob(t, store, "review", "audit", "review", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7",
		LeadAgent: "lead", Sender: "github",
	})
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce with open pull request: %v", err)
	}
	if job, err := store.GetJob(ctx, "review"); err != nil || job.State != string(workflow.JobQueued) {
		t.Fatalf("job after open answer = %+v err=%v, want queued", job.State, err)
	}

	client.pullsByNumber[7] = github.PullRequest{
		Number: 7, State: "closed", HeadRef: "task-7", BaseRef: "main", HeadSHA: "head-seven",
	}
	if err := daemon.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce with closed pull request: %v", err)
	}
	if job, err := store.GetJob(ctx, "review"); err != nil || job.State != string(workflow.JobCancelled) {
		t.Fatalf("job after closed answer = %+v err=%v, want cancelled", job.State, err)
	}

	asked := 0
	for _, number := range client.getPullRequestCalls {
		if number == 7 {
			asked++
		}
	}
	if asked != 2 {
		t.Fatalf("forge asked about #7 %d times across two polls, want once per poll", asked)
	}
}

// closedPullRequestSweepFixture seeds the minimum a closed-PR sweep needs: agents, a
// recorded CLOSED pull request #7, and a forge with no open PRs.
func closedPullRequestSweepFixture(t *testing.T) (*db.Store, github.Repository, *fakeGitHub) {
	t.Helper()
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}
	for _, agent := range []db.Agent{
		{Name: "lead", Role: "lead", Runtime: "codex", RuntimeRef: "last", RepoScope: repo.FullName(), Capabilities: []string{"implement"}, AutonomyPolicy: "workspace-write", HealthStatus: "ok"},
		{Name: "audit", Role: "reviewer", Runtime: "codex", RuntimeRef: "last", RepoScope: repo.FullName(), Capabilities: []string{"review"}, AutonomyPolicy: "auto", HealthStatus: "ok"},
	} {
		if err := store.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent %s: %v", agent.Name, err)
		}
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo.FullName(), Number: 7, HeadBranch: "task-7", BaseBranch: "main",
		HeadSHA: "head-seven", State: "closed",
	}); err != nil {
		t.Fatalf("UpsertPullRequest 7: %v", err)
	}
	// The forge is the authority on "is this a pull request, and is it not open".
	// #7 is closed; #12 is absent, which is what an issue number looks like.
	return store, repo, &fakeGitHub{
		comments:      map[int64][]github.IssueComment{},
		pullsByNumber: map[int64]github.PullRequest{7: {Number: 7, State: "closed", HeadRef: "task-7", BaseRef: "main", HeadSHA: "head-seven"}},
	}
}

// TestPollOnceLeavesQueuedWorkAloneWhenThePullRequestReopens pins the snapshot race.
// openPullNumbers is captured at the top of PollOnce, so a PR reopened before the
// sweep runs still looks absent from it. Absence is only a prefilter: the forge's
// CURRENT state decides, or live work would be cancelled out from under a reopened PR.
func TestPollOnceLeavesQueuedWorkAloneWhenThePullRequestReopens(t *testing.T) {
	ctx := context.Background()
	store, repo, client := closedPullRequestSweepFixture(t)
	// Reopened after the listing this poll will use: absent from the open set, open
	// on the forge.
	client.pullsByNumber[7] = github.PullRequest{
		Number: 7, State: "open", HeadRef: "task-7", BaseRef: "main", HeadSHA: "head-seven",
	}
	seedQueuedJob(t, store, "reopened-review", "audit", "review", workflow.JobPayload{
		Repo: repo.FullName(), Branch: "task-7", PullRequest: 7, TaskID: "task-7",
		LeadAgent: "lead", Sender: "github",
	})
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	for poll := range 3 {
		if err := daemon.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce %d: %v", poll+1, err)
		}
	}
	job, err := store.GetJob(ctx, "reopened-review")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("state = %q, want queued: the PR is open on the forge, whatever the poll's snapshot said", job.State)
	}
}
