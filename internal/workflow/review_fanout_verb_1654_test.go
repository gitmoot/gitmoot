package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1654 / PR #2055. The documentation for --skip-native-review-fanout now states
// that the flag is a control on the IMPLEMENT verb and is recorded but not
// consumed on REVIEW. That is a claim about behaviour, so it is pinned here,
// beside the code that decides it, rather than asserted only in prose.
//
// In AdvanceJob the flag is read inside `case "implement":`, where it writes the
// branch lock; `case "review":` begins after that block and never consults it.
// The two arms below are the same fixture differing only in job type, so a
// failure names WHICH verb changed behaviour.
//
// IF SOMEONE IMPLEMENTS THE REVIEW-SIDE CONSUMER, THE REVIEW ARM MUST FAIL. That
// is intended: the docs would then be wrong, and this test is what forces them
// to be updated in the same change.
func TestSkipNativeReviewFanoutIsAnImplementControlNotAReviewOne(t *testing.T) {
	for _, tc := range []struct {
		name       string
		jobType    string
		branch     string
		decision   string
		wantOnLock bool
	}{
		{"implement consumes it", "implement", "task-1654-impl", "implemented", true},
		{"review records it and does not", "review", "task-1654-review", "changes_requested", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
			seedAgent(t, store, "auditor", []string{"review"}, "gitmoot/gitmoot")
			engine := testEngine(store)

			if acquired, err := store.AcquireLock(ctx, db.BranchLock{
				RepoFullName: "gitmoot/gitmoot", Branch: tc.branch, Owner: "lead",
			}); err != nil || !acquired {
				t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
			}

			agent := "lead"
			if tc.jobType == "review" {
				agent = "auditor"
			}
			// THE REVIEW ARM MUST REACH REVIEW ADVANCEMENT. With PullRequest=0 it
			// exited at engine_run_budgets.go:817 through advance_skipped_no_pr,
			// before any review machinery ran, so the arm proved only that an
			// early return writes no lock (#2055 review F2). The review arm now
			// carries a real pull request and a changes_requested decision, which
			// is the path that reaches reviewDecisionAgent and dispatchFix.
			pr := 0
			if tc.jobType == "review" {
				pr = 1654
				if err := store.UpsertPullRequest(ctx, db.PullRequest{
					RepoFullName: "gitmoot/gitmoot", Number: int64(pr), HeadBranch: tc.branch,
					BaseBranch: "main", HeadSHA: strings.Repeat("d", 40), State: "open",
				}); err != nil {
					t.Fatalf("UpsertPullRequest returned error: %v", err)
				}
				if err := store.UpsertTask(ctx, db.Task{
					ID: tc.branch, RepoFullName: "gitmoot/gitmoot", GoalID: "goal-1",
					Title: "fanout verb", State: string(TaskReviewing), Branch: tc.branch,
				}); err != nil {
					t.Fatalf("UpsertTask returned error: %v", err)
				}
				// THE ARM MUST REACH dispatchFix, not stop at the report-only
				// default (#2055 review round 2, F1). engine_run_budgets.go:849-861
				// only calls dispatchFixWhenHeadHasSettled when a policy is
				// CONFIGURED and not disabled, so without this opt-in the fixture
				// exercised the shortest possible path through the review arm and
				// the branch-lock assertion could pass because nothing ran.
				if err := store.SetPullRequestAutoFixPolicy(ctx, "gitmoot/gitmoot", pr, false, "gm-findings", "test opts into the unattended chain"); err != nil {
					t.Fatalf("SetPullRequestAutoFixPolicy returned error: %v", err)
				}
				// dispatchFix resolves auto-fix OWNERSHIP, and with no implement job
				// recorded for the task it blocks on the attribution gap rather than
				// running. Seeding the implementer is what lets the arm reach the
				// dispatch it is meant to exercise - and reaching that gate at all is
				// the proof the fixture now travels the real path.
				insertCompletedJob(t, store, db.Job{ID: "implement-for-" + tc.branch, Agent: "lead", Type: "implement"}, JobPayload{
					Repo: "gitmoot/gitmoot", Branch: tc.branch, PullRequest: pr,
					HeadSHA: strings.Repeat("d", 40), TaskID: tc.branch, TaskTitle: "fanout verb",
					LeadAgent: "lead",
					Result:    &AgentResult{Decision: "implemented", Summary: "implemented"},
				})
			}
			jobID := tc.jobType + "-1654"
			insertCompletedJob(t, store, db.Job{ID: jobID, Agent: agent, Type: tc.jobType}, JobPayload{
				Repo:                   "gitmoot/gitmoot",
				Branch:                 tc.branch,
				PullRequest:            pr,
				HeadSHA:                strings.Repeat("d", 40),
				TaskID:                 tc.branch,
				TaskTitle:              "fanout verb",
				LeadAgent:              "lead",
				Reviewers:              []string{"auditor"},
				SkipNativeReviewFanout: true,
				Result:                 &AgentResult{Decision: tc.decision, Summary: "done"},
			})

			// THE ERROR IS NOT DISCARDED (#2055 review round 2, F1). Swallowing it
			// let an errored advance satisfy the branch-lock assertion for a reason
			// that has nothing to do with the review case.
			if err := engine.AdvanceJob(ctx, jobID); err != nil {
				t.Fatalf("AdvanceJob returned error: %v", err)
			}

			// POSITIVE PROOF that review advancement ran, not merely the absence of
			// one early-return event. setTaskState(TaskChangesRequested) happens
			// inside the changes_requested arm before the auto-fix policy check, so
			// the task's state is a marker only that path can set. Checking for the
			// ABSENCE of advance_skipped_no_pr cannot distinguish "ran" from "took a
			// different early return"; this can.
			if tc.jobType == "review" {
				task, taskErr := store.GetTask(ctx, tc.branch)
				if taskErr != nil {
					t.Fatalf("GetTask returned error: %v", taskErr)
				}
				if task.State != string(TaskChangesRequested) {
					t.Fatalf("task state = %q, want changes_requested: the review arm did not reach the decision switch, so the branch-lock assertion below proves nothing", task.State)
				}
			}

			// CONTROL for #2055 review F2: prove the review arm did not exit early
			// again. advance_skipped_no_pr is the guard that used to make this arm
			// vacuous, so its ABSENCE is what makes the assertion below mean
			// anything.
			if tc.jobType == "review" {
				events, evErr := store.ListJobEvents(ctx, jobID)
				if evErr != nil {
					t.Fatalf("ListJobEvents returned error: %v", evErr)
				}
				for _, event := range events {
					if event.Kind == "advance_skipped_no_pr" {
						t.Fatalf("the review arm exited through advance_skipped_no_pr and never reached review advancement, so it proves nothing about the review case")
					}
				}
			}

			// THE FIX JOB ITSELF MUST BE ASSERTED (#2055 review round 3). Reaching
			// dispatchFixWhenHeadHasSettled is not the same as proving what it
			// dispatched: stopping at TaskChangesRequested plus an unchanged lock
			// left two mutants alive - deleting the dispatch call, and making
			// dispatchFix copy the review payload's SkipNativeReviewFanout into its
			// parentless implement request. Both are exactly the documented
			// behaviour, so both must be pinned.
			if tc.jobType == "review" {
				implementJobs, listErr := store.ListJobsByType(ctx, "implement")
				if listErr != nil {
					t.Fatalf("ListJobsByType returned error: %v", listErr)
				}
				var fix *db.Job
				candidates := 0
				for i := range implementJobs {
					// The seeded implementer satisfies the ownership gate; the fix
					// job is the one dispatchFix created.
					if implementJobs[i].ID != "implement-for-"+tc.branch {
						fix = &implementJobs[i]
						candidates++
					}
				}
				if fix == nil {
					t.Fatalf("no fix job was dispatched: dispatchFixWhenHeadHasSettled did not run, so this arm proves nothing about what a review dispatches")
				}
				// IDENTIFICATION BY EXCLUSION IS ONLY SOUND IF EXACTLY ONE REMAINS.
				// With more than one, the loop silently kept the last and the
				// assertions below would describe an arbitrary job.
				if candidates != 1 {
					t.Fatalf("expected exactly one dispatched fix job, found %d: identification by exclusion is not sound here", candidates)
				}
				var fixPayload JobPayload
				if err := json.Unmarshal([]byte(fix.Payload), &fixPayload); err != nil {
					t.Fatalf("Unmarshal fix payload returned error: %v", err)
				}
				// The documentation states dispatchFix builds a PARENTLESS implement
				// request that does not inherit the bit. If it ever does inherit it,
				// a review-dispatched fix would suppress native fan-out on the
				// branch, which is precisely the effect the docs say review cannot
				// have.
				// PARENTLESSNESS ASSERTED DIRECTLY (#2055 review round 4). The
				// documentation says dispatchFix builds a PARENTLESS implement
				// request; a false fanout bit only shows the bit is false, which is
				// also true of a parented request that simply did not inherit. The
				// distinction matters because inheritance at the enqueue chokepoint
				// REQUIRES a parent, so parentlessness is the mechanism the doc
				// names, not the symptom.
				if strings.TrimSpace(fix.ParentJobID) != "" {
					t.Fatalf("the fix job %q carries ParentJobID %q; the documentation states dispatchFix builds a PARENTLESS request, and a parented one would inherit the bit at the enqueue chokepoint", fix.ID, fix.ParentJobID)
				}
				// The dispatched fix must target the pull request under review.
				// Dropping PullRequest from dispatchFix's JobRequest still enqueues
				// a job whose fanout bit is false, so the bit alone cannot see it.
				// THE REVIEWED HEAD TRAVELS WITH THE FIX (#2055 review round 5). Removing
				// `HeadSHA: payload.HeadSHA` from dispatchFix's request left this test
				// and the entire internal/workflow package green, so a fix could be
				// dispatched against no head at all: it would run wherever the branch
				// happened to be rather than at the head whose review demanded it.
				if strings.TrimSpace(fixPayload.HeadSHA) != strings.Repeat("d", 40) {
					t.Fatalf("the fix job carries head %q, want the reviewed head: a fix dispatched without its head fixes whatever the branch has drifted to", fixPayload.HeadSHA)
				}
				if fixPayload.PullRequest != pr {
					t.Fatalf("the fix job targets pull request %d, want %d: the dispatch lost the PR it was fixing", fixPayload.PullRequest, pr)
				}
				// THE REVIEWER ROSTER TRAVELS WITH THE FIX (#2055 review round 6).
				// Deleting `Reviewers: e.requiredReviewers(payload)` from dispatchFix
				// left this test and the whole package green: the fixture supplied
				// reviewer "auditor" and never checked that the fix carried it, so a
				// fix could be dispatched with an EMPTY roster. That matters because
				// allRequiredReviewersApproved reads the same field to decide when the
				// fix is done; an empty roster is not "nobody must approve" by
				// accident, it is a different completion rule.
				//
				// The engine here sets no RequiredReviewers, so the roster can only
				// have come from the review payload. Asserting that precondition too,
				// or a global fallback would satisfy this without any propagation.
				if len(engine.RequiredReviewers) != 0 {
					t.Fatalf("the engine has a global reviewer fallback %v, so this assertion cannot prove the roster propagated",
						engine.RequiredReviewers)
				}
				if len(fixPayload.Reviewers) != 1 || fixPayload.Reviewers[0] != "auditor" {
					t.Fatalf("the fix job carries reviewers %v, want [auditor]: the dispatch lost the roster that decides when the fix is complete",
						fixPayload.Reviewers)
				}
				if fixPayload.SkipNativeReviewFanout {
					t.Fatalf("the fix job %q inherited SkipNativeReviewFanout from the review payload. "+
						"CLI.md and website/docs/reference/cli.md state that dispatchFix does not inherit it; "+
						"if that changed deliberately, update both docs", fix.ID)
				}
			}

			lock, err := store.GetBranchLock(ctx, "gitmoot/gitmoot", tc.branch)
			if err != nil {
				t.Fatalf("GetBranchLock returned error: %v", err)
			}
			if lock.SkipNativeReviewFanout != tc.wantOnLock {
				if tc.wantOnLock {
					t.Fatalf("implement advance did not persist the flag onto the branch lock; the implement-side control is broken")
				}
				t.Fatalf("a REVIEW advance persisted skip_native_review_fanout onto the branch lock. " +
					"The documentation states review records the bit and does not consume it. " +
					"If the review-side consumer was implemented deliberately, update CLI.md and " +
					"website/docs/reference/cli.md in the same change")
			}
		})
	}
}

// The documentation states one propagation path that DOES carry the flag past a
// review job: every delegation child inherits it, for any parent job type,
// because the intent is an operator command about the whole tree (#1236). A
// review parent that delegates an implement leg therefore produces a child that
// does consume it.
//
// TestDelegationChildInheritsSkipNativeReviewFanout already pins this for an
// `ask` parent. This arm pins it for a REVIEW parent specifically, because that
// is the sentence the CLI documentation now makes, and a doc claim about review
// should not rest on a fixture about ask.
func TestReviewParentStillPropagatesTheFlagToADelegationChild(t *testing.T) {
	store := openEngineStore(t)
	engine := testEngine(store)

	parent := db.Job{ID: "review-parent-1654", Agent: "auditor", Type: "review"}
	payload := JobPayload{
		Repo:                   "gitmoot/gitmoot",
		Branch:                 "task-1654-delegating-review",
		TaskID:                 "task-1654-delegating-review",
		LeadAgent:              "lead",
		SkipNativeReviewFanout: true,
	}

	child := engine.delegationRequest(context.Background(), parent, payload, Delegation{
		ID:     "fix-leg",
		Agent:  "lead",
		Action: "implement",
		Prompt: "fix what the review found",
	})

	if !child.SkipNativeReviewFanout {
		t.Fatalf("a review parent's delegation child dropped SkipNativeReviewFanout. " +
			"CLI.md and website/docs/reference/cli.md state that delegation children inherit it " +
			"regardless of parent type; if that propagation was removed deliberately, update both docs")
	}
}

// #2055 review F3. The constructor test above checks delegationRequest's return
// value, which is not what the child actually gets: Mailbox.prepareEnqueue
// independently inherits the bit from the STORED parent
// (internal/workflow/mailbox.go:682-688). Removing the constructor assignment
// could leave real behaviour intact while failing that test, and passing it
// proves nothing about an enqueued child.
//
// This asserts the observable thing: a child enqueued under a stored REVIEW
// parent carries the bit in its persisted payload.
func TestEnqueuedChildOfAReviewParentCarriesTheFanoutBit(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "auditor", []string{"review"}, "gitmoot/gitmoot")

	parentPayload := JobPayload{
		Repo:                   "gitmoot/gitmoot",
		Branch:                 "task-1654-enqueue",
		TaskID:                 "task-1654-enqueue",
		LeadAgent:              "lead",
		SkipNativeReviewFanout: true,
	}
	insertCompletedJob(t, store, db.Job{ID: "review-parent-enqueue", Agent: "auditor", Type: "review"}, parentPayload)

	mailbox := NewMailbox(store, UnavailableDeliveryWorktreeResolver("test"))
	child, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "review-parent-enqueue/delegation/fix-leg",
		Agent:        "lead",
		Action:       "implement",
		Repo:         "gitmoot/gitmoot",
		Branch:       "task-1654-enqueue",
		TaskID:       "task-1654-enqueue",
		ParentJobID:  "review-parent-enqueue",
		Instructions: "fix what the review found",
	})
	if err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	var got JobPayload
	if err := json.Unmarshal([]byte(stored.Payload), &got); err != nil {
		t.Fatalf("Unmarshal child payload returned error: %v", err)
	}
	if !got.SkipNativeReviewFanout {
		t.Fatalf("a child enqueued under a review parent did not persist SkipNativeReviewFanout. " +
			"CLI.md and website/docs/reference/cli.md state that delegation children inherit it " +
			"regardless of parent type; if that changed deliberately, update both docs")
	}
}
