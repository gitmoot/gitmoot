package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// gitmoot#1745: two polls at an UNCHANGED PR head must enqueue ONE review job,
// not two, and the second must not error.
//
// THIS TEST IS EXPECTED TO PASS, and that expectation is the deliverable rather
// than a weakness. Read through origin/main at
// 2752a4d91057d82ad71b6e76122970a12b6095b1, which contains d3fd2877:
// engine_pr_lifecycle.go:312-380 both diagnoses this class and carries its fix.
// The mechanism it records: a re-poll at the same head re-derives the SAME
// deterministic job id - Engine.jobID at engine.go:728 hashes Repo, Branch,
// PullRequest, TaskID, Agent, Action, ReviewRound and Instructions, and
// notably NOT HeadSHA - while the worker had mutated Instructions and
// WorktreePath after enqueue, so existingJobMatchesRequest no longer matched
// the stored payload and the raw "UNIQUE constraint failed: jobs.id" surfaced
// out of HandlePullRequestOpened. That commit allocates the read-only worktree
// BEFORE enqueue, deciding idempotence ahead of the side-effecting
// `git worktree add`.
//
// A PASS says the 2026-08-29 40-job cluster is historical and bounded by a
// merged fix, and #1745 can close citing this test. A FAIL would be the sharper
// result - the class surviving its own documented fix - and only then does a
// dispatch-boundary change come into scope.
//
// THE ENGINE USES THE REAL jobID DERIVATION ON PURPOSE. The sibling fanout
// tests inject a readable JobID func; that would test a fixture's id scheme
// rather than the FNV derivation this defect is about, so this one leaves
// Engine.JobID nil and counts jobs instead of naming one.
//
// WHAT THIS TEST IS AND IS NOT SENSITIVE TO, measured rather than assumed,
// because the comment above names a mechanism and a reader would otherwise
// assume this test guards it:
//
//   - KILLED: forcing a DIFFERENT review round on the second call
//     (nextReviewRound no longer reusing the unchanged head's round) produces a
//     second review leg at the same head, and this test fails with both ids.
//     So the discriminating layer here is the ROUND DERIVATION.
//   - SURVIVED: disabling the head-round reuse guard (reviewLegsAtHead) alone.
//     With the round unchanged the id is identical and the enqueue absorbs it.
//   - SURVIVED: making existingJobMatchesRequest judge every review job a
//     non-match - the payload-mismatch mechanism d3fd2877's comment describes.
//     Even sabotaged that way, no duplicate and no UNIQUE-constraint error
//     appears at this head.
//
// Those last two survivals are not weaknesses of the assertion, they are the
// finding: the observable contract is protected by SEVERAL independent layers,
// which is why the 2026-08-29 class is bounded rather than merely fixed in one
// place. But this test does NOT distinguish d3fd2877's before-enqueue
// allocation specifically, and it should not be cited as proof of that.
//
// Offline by construction: an isolated t.TempDir store (never /root/.gitmoot),
// a fake GitHub client, and SHELL-runtime agents, so no LLM and no network is
// reachable from this path.
//
// Credit: apps-coc rows 117668 and 117671 reported the cluster; row 117695
// corrects their coordinates, which had been read from a divergent tree.
func TestReviewRedispatchAtUnchangedHeadEnqueuesOnce_1745_E2E(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	repo := github.Repository{Owner: "gitmoot", Name: "gitmoot"}

	// The branch holder. handlePullRequestWorkflow reads the branch lock and
	// returns without fanning out when it is absent, and HandlePullRequestOpened
	// then runs ensureAgentAllowed against this owner, so it needs the implement
	// capability and a write-granting policy or admission is refused.
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "shell-lead", Role: "lead", Runtime: runtime.ShellRuntime,
		RepoScope: repo.FullName(), Capabilities: []string{"implement"},
		AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite, HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent lead: %v", err)
	}
	// The reviewer. workflowReviewers selects on repo access plus the "review"
	// capability, and eligibleReviewers drops anything it considers unfit for
	// THIS head - an empty roster silently records a baseline and dispatches
	// nothing, which is why the control at the end of this test exists.
	if err := store.UpsertAgent(ctx, db.Agent{
		Name: "shell-audit", Role: "reviewer", Runtime: runtime.ShellRuntime,
		RepoScope: repo.FullName(), Capabilities: []string{"review"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto, HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent reviewer: %v", err)
	}
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{
		RepoFullName: repo.FullName(), Branch: "task-1745", Owner: "shell-lead",
	}); err != nil || !acquired {
		t.Fatalf("AcquireLock acquired=%v err=%v", acquired, err)
	}
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-1745", GoalID: "goal-1745", Title: "redispatch probe",
		State: string(workflow.TaskPlanned), Branch: "task-1745",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	client := &fakeGitHub{
		pulls:    []github.PullRequest{},
		comments: map[int64][]github.IssueComment{1745: {}},
	}
	// Engine.JobID deliberately nil: the production FNV derivation is the thing
	// under test.
	engine := workflow.Engine{Store: store}
	daemon := Daemon{Repo: repo, Store: store, GitHub: client, Workflow: &engine}

	// THE HEAD IS IDENTICAL ACROSS BOTH CALLS. The defect was never about a
	// moved head; it was a re-derived id at the SAME head failing a payload
	// match, so holding the head fixed is the whole premise.
	const stableHead = "1745deadbeef1745deadbeef1745deadbeef1745"
	pull := github.PullRequest{
		Number: 1745, Title: "redispatch probe", State: "open",
		URL:     "https://github.com/gitmoot/gitmoot/pull/1745",
		HeadRef: "task-1745", BaseRef: "main", HeadSHA: stableHead,
	}

	if err := daemon.handlePullRequestWorkflow(ctx, pull, nil); err != nil {
		t.Fatalf("first poll returned error: %v", err)
	}
	afterFirst := reviewJobIDsAtHead1745(t, store, stableHead)

	// POLL TWO, same head, same event. This is the assertion #1745 asks for.
	if err := daemon.handlePullRequestWorkflow(ctx, pull, nil); err != nil {
		// The historical symptom was exactly this: a raw UNIQUE-constraint error
		// out of HandlePullRequestOpened, so it is named rather than folded into
		// a generic failure.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			t.Fatalf("second poll at an unchanged head reproduced #1745's symptom: %v", err)
		}
		t.Fatalf("second poll returned error: %v", err)
	}
	afterSecond := reviewJobIDsAtHead1745(t, store, stableHead)

	if len(afterSecond) != len(afterFirst) {
		t.Fatalf("review jobs after poll 1 = %v, after poll 2 = %v; a re-poll at an unchanged head must add none", afterFirst, afterSecond)
	}

	// THE CONTROL, and it is the reason this test is worth running. Without it a
	// build that fans out NOTHING passes identically - 0 == 0 - which is the
	// shape that makes an idempotence assertion worthless. It fired for real
	// while this test was being written: four separate admission gates (a
	// missing branch lock, an unsubscribed owner, a missing implement
	// capability, and a policy that grants no write) each produced zero jobs and
	// zero errors, and only this line distinguished them from a pass.
	if len(afterFirst) != 1 {
		t.Fatalf("review jobs after poll 1 = %v, want exactly 1; the idempotence assertion above proves nothing otherwise", afterFirst)
	}
}

// reviewJobIDsAtHead1745 lists persisted review-job ids bound to head. It reads
// the STORE, not a return value: the defect was a duplicate ROW.
func reviewJobIDsAtHead1745(t *testing.T, store *db.Store, head string) []string {
	t.Helper()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	ids := []string{}
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		var payload workflow.JobPayload
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			t.Fatalf("parse payload %q: %v", job.ID, err)
		}
		if strings.EqualFold(strings.TrimSpace(payload.HeadSHA), strings.TrimSpace(head)) {
			ids = append(ids, job.ID)
		}
	}
	return ids
}
