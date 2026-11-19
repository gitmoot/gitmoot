package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/reviewlevel"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// noCIBody is the GraphQL answer for one PR with no check at its head, as raw
// JSON fragments. noCIFacts fills every empty field with a repo the reader
// administers, an unprotected base with no ruleset, and no .github/workflows
// on the base, the head or the default branch.
type noCIBody struct {
	Perm          string // viewerPermission
	DefaultBranch string // repository.defaultBranchRef
	BaseTarget    string // baseRef.target
	Rules         string // baseRef.rules
	HeadOid       string // commits(last:1) commit oid
	HeadFile      string // that commit's file(path: ".github/workflows")
	Commits       string // commits.nodes, overriding HeadOid/HeadFile
}

const (
	noWorkflowsDir = `null`
	ciWorkflowDir  = `{"object":{"entries":[{"name":"ci.yml"}]}}`
)

func noCIFacts(t *testing.T, body noCIBody) github.PullRequestFacts {
	t.Helper()
	at := stallNow.Add(-7 * time.Hour).Format(time.RFC3339)
	fill := func(value *string, fallback string) {
		if *value == "" {
			*value = fallback
		}
	}
	fill(&body.Perm, "ADMIN")
	fill(&body.DefaultBranch, `{"target":{"oid":"dddddddddddddddddddddddddddddddddddddddd","file":null}}`)
	fill(&body.BaseTarget, `{"oid":"cccccccccccccccccccccccccccccccccccccccc","file":null}`)
	fill(&body.Rules, `{"pageInfo":{"hasNextPage":false},"nodes":[]}`)
	fill(&body.HeadOid, stallHeadA)
	fill(&body.HeadFile, noWorkflowsDir)
	fill(&body.Commits, `[{"commit":{"oid":"`+body.HeadOid+`","committedDate":"`+at+`","file":`+body.HeadFile+`,"statusCheckRollup":null}}]`)
	raw := `{"data":{"rateLimit":{"cost":1},"repository":{"viewerPermission":"` + body.Perm + `","defaultBranchRef":` + body.DefaultBranch + `,
"pullRequests":{"totalCount":1,"nodes":[{"number":13,"mergeable":"MERGEABLE","headRefOid":"` + stallHeadA + `","baseRefName":"main","createdAt":"` + at + `",
"baseRef":{"target":` + body.BaseTarget + `,"branchProtectionRule":null,"rules":` + body.Rules + `},
"commits":{"nodes":` + body.Commits + `}}]}}}}`
	client := github.NewClientWithRunner(t.TempDir(), fixedResultRunner{result: subprocess.Result{Stdout: raw}})
	facts, err := client.OpenPullRequestFacts(context.Background(), github.Repository{Owner: "owner", Name: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	return facts.PullRequests[0]
}

// TestPRStalledNoCIConfiguredIsNotNoCIOnHead covers #2683: a PR that no
// workflow can run for (none on the base, the head or the default branch) and
// whose base requires no check is not no_ci_on_head. Any workflow, any
// required check, or any of those facts unread keeps it flagged.
func TestPRStalledNoCIConfiguredIsNotNoCIOnHead(t *testing.T) {
	policy := config.DefaultPRStalledPolicy()
	classify := func(pr github.PullRequestFacts) (prStall, bool) {
		return classifyStalledPR(prStallFacts{Repo: "owner/repo", PR: pr, Level: reviewlevel.LevelBackground}, policy, stallNow)
	}
	for name, body := range map[string]noCIBody{
		"no workflows directory anywhere":   {},
		"base workflows directory is empty": {BaseTarget: `{"oid":"cccccccccccccccccccccccccccccccccccccccc","file":{"object":{"entries":[]}}}`},
		"only non-workflow files":           {HeadFile: `{"object":{"entries":[{"name":"README.md"}]}}`},
	} {
		t.Run(name, func(t *testing.T) {
			pr := noCIFacts(t, body)
			if !prNoCIConfigured(pr) {
				t.Fatalf("facts %+v: want no CI configured", pr)
			}
			if stall, ok := classify(pr); ok && stall.State == prStallNoCIOnHead {
				t.Fatalf("classified %+v, want not no_ci_on_head", stall)
			}
		})
	}
	for name, body := range map[string]noCIBody{
		"workflow on the base":                   {BaseTarget: `{"oid":"cccccccccccccccccccccccccccccccccccccccc","file":` + ciWorkflowDir + `}`},
		"workflow only on the head commit":       {HeadFile: ciWorkflowDir},
		"workflow only on the default branch":    {DefaultBranch: `{"target":{"oid":"dddddddddddddddddddddddddddddddddddddddd","file":` + ciWorkflowDir + `}}`},
		"required checks unknown: not an admin":  {Perm: "WRITE"},
		"required checks unknown: rulesets null": {Rules: `null`},
		"head commit unread: no node":            {Commits: `[]`},
		"head commit unread: head moved":         {HeadOid: stallHeadB},
		"default branch unread: null ref":        {DefaultBranch: `null`},
		"default branch unread: null target":     {DefaultBranch: `{"target":null}`},
		"base branch unread: null target":        {BaseTarget: `null`},
	} {
		t.Run(name, func(t *testing.T) {
			pr := noCIFacts(t, body)
			if prNoCIConfigured(pr) {
				t.Fatalf("facts %+v: want CI not ruled out", pr)
			}
			if stall, ok := classify(pr); !ok || stall.State != prStallNoCIOnHead {
				t.Fatalf("classified %+v (stalled=%v), want no_ci_on_head", stall, ok)
			}
		})
	}
	t.Run("no workflows but a required check", func(t *testing.T) {
		pr := noCIFacts(t, noCIBody{})
		pr.Required = []string{"external-ci"}
		if stall, ok := classify(pr); !ok || stall.State != prStallNoCIOnHead {
			t.Fatalf("classified %+v (stalled=%v), want no_ci_on_head", stall, ok)
		}
	})
	t.Run("only gitmoot statuses required", func(t *testing.T) {
		pr := noCIFacts(t, noCIBody{})
		pr.Required = []string{"gitmoot/merge-gate"}
		if !prNoCIConfigured(pr) {
			t.Fatalf("facts %+v: gitmoot's own status is not CI", pr)
		}
	})
	t.Run("pr stalled reports it", func(t *testing.T) {
		home, store, gh := stalledSweepFixture(t)
		gh.pulls = []github.PullRequestFacts{noCIFacts(t, noCIBody{})}
		report, err := evaluateStalledPullRequests(context.Background(), store, newPRStalledEnv(context.Background(), store, home, gh),
			"gitmoot/gitmoot", policy, stallNow)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(report.NoCIConfigured, []int{13}) {
			t.Fatalf("no_ci_configured = %v, want [13]", report.NoCIConfigured)
		}
		for _, row := range report.Stalled {
			if row.State == prStallNoCIOnHead || !row.NoCIConfigured || !strings.Contains(row.Cause, "no CI configured") {
				t.Fatalf("row %+v: want a non-no_ci_on_head state marked no CI configured", row)
			}
		}
		var out bytes.Buffer
		printPRStalled(&out, []prStalledReport{report})
		if !strings.Contains(out.String(), "no CI configured") {
			t.Fatalf("pr stalled output %q does not say no CI configured", out.String())
		}
	})
}

// orphanedStalledFixture is stalledSweepFixture plus PR 8 implemented by
// "gone", a role no longer in the org, and PR 9 with no attribution at all.
func orphanedStalledFixture(t *testing.T) (string, *db.Store, *fakeStalledGitHub) {
	t.Helper()
	home, store, gh := stalledSweepFixture(t)
	if err := store.CreateJob(context.Background(), db.Job{
		ID: "impl-8", Agent: "coder", Type: "implement", State: string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "gitmoot/gitmoot", PullRequest: 8, ActingOrgRole: "gone"}),
	}); err != nil {
		t.Fatal(err)
	}
	return home, store, gh
}

// TestPRStalledOrphanedOwnerGoesToTheCoordinator covers #2685: a PR whose
// recorded seat is gone is alerted to the repo's coordinator ("owner", the
// shallowest seat scoping it), not its deepest seat ("lane"); a PR with no
// attribution still goes to the repo owner ("lane").
func TestPRStalledOrphanedOwnerGoesToTheCoordinator(t *testing.T) {
	home, store, gh := orphanedStalledFixture(t)
	ctx := context.Background()
	gh.pulls = []github.PullRequestFacts{noCIPull(8, stallHeadA, stallNow.Add(-2*time.Hour)), noCIPull(9, stallHeadB, stallNow.Add(-time.Hour))}
	report, err := evaluateStalledPullRequests(ctx, store, newPRStalledEnv(ctx, store, home, gh), "gitmoot/gitmoot", config.DefaultPRStalledPolicy(), stallNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Stalled) != 2 {
		t.Fatalf("stalled = %+v, want PRs 8 and 9", report.Stalled)
	}
	orphan, unattributed := report.Stalled[0], report.Stalled[1]
	if orphan.PullRequest != 8 || orphan.Owner != "owner" || orphan.OwnerSource != ownerSourceCoordinator || orphan.OwnerGone != "gone" {
		t.Fatalf("orphaned PR row = %+v, want owner (coordinator), owner gone: gone", orphan)
	}
	if unattributed.PullRequest != 9 || unattributed.Owner != "lane" || unattributed.OwnerSource != ownerSourceRepoOwner || unattributed.OwnerGone != "" {
		t.Fatalf("unattributed PR row = %+v, want lane (repo owner)", unattributed)
	}
	var out bytes.Buffer
	printPRStalled(&out, []prStalledReport{report})
	if !strings.Contains(out.String(), "owner gone: gone") {
		t.Fatalf("pr stalled output %q does not say owner gone", out.String())
	}

	runStalledSweep(t, home, store, gh, stallNow)
	if got, want := pendingWakeTargetRoles(t, store), []string{"jarvis", "jarvis", "lane", "owner"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("wake targets = %v, want %v", got, want)
	}
}

// TestBlockedTaskOrphanedOwnerGoesToTheCoordinator covers #2685 for the
// blocked-since digest (#2671): a task whose recorded seat is gone reaches the
// coordinator ("owner"), an unattributed one still the repo owner ("lane").
func TestBlockedTaskOrphanedOwnerGoesToTheCoordinator(t *testing.T) {
	home, store := blockedTaskRoutingFixture(t)
	ctx := context.Background()
	const repo = "gitmoot/gitmoot"
	for _, task := range []db.Task{
		{ID: "task-orphan", RepoFullName: repo, State: string(workflow.TaskBlocked)},
		{ID: "task-unattributed", RepoFullName: repo, State: string(workflow.TaskBlocked)},
	} {
		if err := store.UpsertTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: "impl-orphan", Agent: "coder", Type: "implement", State: string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: repo, TaskID: "task-orphan", ActingOrgRole: "gone"}),
	}); err != nil {
		t.Fatal(err)
	}
	sink := &eventRuleSink{store: store, home: home}
	if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, time.Hour, io.Discard, time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatalf("evaluateBlockedTaskEpisodes: %v", err)
	}
	if got, want := pendingWakeTargetRoles(t, store), []string{"jarvis", "jarvis", "lane", "owner"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("blocked task wake targets = %v, want %v", got, want)
	}
}

// TestBlockedTaskClosedUnmergedIsSilentUntilReopened covers #2696: an
// implementation task blocked because its PR closed unmerged is not alerted
// while the PR stays closed, and alerts again once it reopens.
func TestBlockedTaskClosedUnmergedIsSilentUntilReopened(t *testing.T) {
	home, store := blockedTaskRoutingFixture(t)
	ctx := context.Background()
	const repo = "gitmoot/gitmoot"
	const taskID = "adhoc-8e0a6f9e"
	if err := store.UpsertTask(ctx, db.Task{ID: taskID, RepoFullName: repo, Title: "appkit",
		State: string(workflow.TaskChangesRequested), Branch: "feature/appkit"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: repo, Number: 4, HeadBranch: "feature/appkit", HeadSHA: stallHeadA, State: "open"}); err != nil {
		t.Fatal(err)
	}
	if err := (workflow.Engine{Store: store}).HandleReviewPullRequestClosed(ctx, workflow.PullRequestEvent{
		Repo: repo, Branch: "feature/appkit", PullRequest: 4, TaskID: taskID, HeadSHA: stallHeadA, LeadAgent: "github", Sender: "github",
	}, false); err != nil {
		t.Fatalf("HandleReviewPullRequestClosed: %v", err)
	}
	if task, err := store.GetTask(ctx, taskID); err != nil || task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task = %+v, err=%v; want blocked", task, err)
	}
	sink := &eventRuleSink{store: store, home: home}
	evaluate := func(now time.Time) {
		t.Helper()
		if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, time.Hour, io.Discard, now); err != nil {
			t.Fatalf("evaluateBlockedTaskEpisodes: %v", err)
		}
	}
	now := time.Now().UTC().Add(2 * time.Hour)
	evaluate(now)
	evaluate(now.Add(5 * time.Hour))
	if got := pendingWakeTargetRoles(t, store); len(got) != 0 {
		t.Fatalf("closed-unmerged task alerted %v, want none", got)
	}

	// The PR reopens: the daemon's poll records it open again.
	if err := store.UpsertPullRequest(ctx, db.PullRequest{RepoFullName: repo, Number: 4, HeadBranch: "feature/appkit", HeadSHA: stallHeadA, State: "open"}); err != nil {
		t.Fatal(err)
	}
	evaluate(now.Add(6 * time.Hour))
	if got, want := pendingWakeTargetRoles(t, store), []string{"jarvis", "lane"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened task wake targets = %v, want %v", got, want)
	}
}

// prDismissFixture is blockedTaskRoutingFixture with task-1 blocked and
// implemented by "lane" (child of "owner").
func prDismissFixture(t *testing.T) (string, *db.Store) {
	t.Helper()
	home, store := blockedTaskRoutingFixture(t)
	ctx := context.Background()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: "gitmoot/gitmoot", State: string(workflow.TaskBlocked)}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{
		ID: "impl-1", Agent: "coder", Type: "implement", State: string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "gitmoot/gitmoot", TaskID: "task-1", ActingOrgRole: "lane"}),
	}); err != nil {
		t.Fatal(err)
	}
	return home, store
}

func runPRDismissCommand(t *testing.T, home, role string, extra ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"pr", "dismiss", "task-1", "--home", home, "--role", role, "--reason", "closed on purpose"}, extra...)
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestPRDismissRetiresTheTaskWithAnAuditEvent(t *testing.T) {
	home, store := prDismissFixture(t)
	// owner is lane's coordinator.
	code, stdout, stderr := runPRDismissCommand(t, home, "owner", "--json")
	if code != 0 {
		t.Fatalf("pr dismiss exited %d: %s", code, stderr)
	}
	var out prDismissOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if out.ID != "task-1" || out.FromState != "blocked" || out.State != "dismissed" || out.By != "owner" || out.Reason != "closed on purpose" {
		t.Fatalf("output = %+v", out)
	}
	ctx := context.Background()
	if task, err := store.GetTask(ctx, "task-1"); err != nil || task.State != string(workflow.TaskDismissed) {
		t.Fatalf("task = %+v, err=%v; want dismissed", task, err)
	}
	taskEvents, err := store.ListTaskEvents(ctx, "task-1")
	if err != nil {
		t.Fatal(err)
	}
	last := taskEvents[len(taskEvents)-1]
	if last.Kind != TaskEventDismissedManual || last.FromState != "blocked" || last.ToState != "dismissed" ||
		!strings.Contains(last.Reason, "owner") || !strings.Contains(last.Reason, "closed on purpose") {
		t.Fatalf("last task event = %+v, want task_dismissed_manual by owner with the reason", last)
	}

	// Already final: refused.
	if code, _, stderr := runPRDismissCommand(t, home, "owner"); code == 0 || !strings.Contains(stderr, "already dismissed") {
		t.Fatalf("second dismiss exited %d: %s; want refused as already dismissed", code, stderr)
	}
}

func TestPRDismissRefusesALiveJob(t *testing.T) {
	home, store := prDismissFixture(t)
	if err := store.CreateJob(context.Background(), db.Job{
		ID: "impl-live", Agent: "coder", Type: "implement", State: string(workflow.JobQueued),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "gitmoot/gitmoot", TaskID: "task-1", ActingOrgRole: "lane"}),
	}); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runPRDismissCommand(t, home, "lane"); code == 0 || !strings.Contains(stderr, "live job") {
		t.Fatalf("pr dismiss exited %d: %s; want refused for a live job", code, stderr)
	}
	if task, err := store.GetTask(context.Background(), "task-1"); err != nil || task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task = %+v, err=%v; want still blocked", task, err)
	}
}

func TestPRDismissRefusesAnUnrelatedRole(t *testing.T) {
	home, store := prDismissFixture(t)
	// jarvis is lane's sibling: neither its owner, a coordinator nor the repo owner.
	if code, _, stderr := runPRDismissCommand(t, home, "jarvis"); code == 0 || !strings.Contains(stderr, `role "jarvis" may not dismiss task-1`) {
		t.Fatalf("pr dismiss exited %d: %s; want refused for jarvis", code, stderr)
	}
	if task, err := store.GetTask(context.Background(), "task-1"); err != nil || task.State != string(workflow.TaskBlocked) {
		t.Fatalf("task = %+v, err=%v; want still blocked", task, err)
	}
}

// TestPRDismissRefusesASettlingJob covers the liveness FindLiveTaskJob shares
// with the closed-PR retire path: a settled job still owing its parent an
// advance, and a cancellation of a running job not yet settled, keep the task
// live although neither is queued or running.
func TestPRDismissRefusesASettlingJob(t *testing.T) {
	for name, job := range map[string]struct {
		state  string
		events []db.JobEvent
	}{
		"settled job with advance debt": {state: string(workflow.JobSucceeded),
			events: []db.JobEvent{{Kind: "advance_started", Message: "advancing the parent"}}},
		"unsettled cancellation from running": {state: string(workflow.JobCancelled),
			events: []db.JobEvent{{Kind: string(workflow.JobCancelled), Message: "cancel requested from running: by owner"}}},
	} {
		t.Run(name, func(t *testing.T) {
			home, store := prDismissFixture(t)
			ctx := context.Background()
			if err := store.CreateJob(ctx, db.Job{
				ID: "impl-settling", Agent: "coder", Type: "implement", State: job.state,
				Payload: mustJobPayload(t, workflow.JobPayload{Repo: "gitmoot/gitmoot", TaskID: "task-1", ActingOrgRole: "lane"}),
			}); err != nil {
				t.Fatal(err)
			}
			for _, event := range job.events {
				event.JobID = "impl-settling"
				if err := store.AddJobEvent(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			if code, _, stderr := runPRDismissCommand(t, home, "lane"); code == 0 || !strings.Contains(stderr, "impl-settling is "+job.state+" but still settling") {
				t.Fatalf("pr dismiss exited %d: %s; want refused for a settling job", code, stderr)
			}
			if task, err := store.GetTask(ctx, "task-1"); err != nil || task.State != string(workflow.TaskBlocked) {
				t.Fatalf("task = %+v, err=%v; want still blocked", task, err)
			}
		})
	}
}
