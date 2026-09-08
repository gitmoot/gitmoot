package workflow

import (
	"context"
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
		{"review records it and does not", "review", "task-1654-review", "approved", false},
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
			jobID := tc.jobType + "-1654"
			insertCompletedJob(t, store, db.Job{ID: jobID, Agent: agent, Type: tc.jobType}, JobPayload{
				Repo:                   "gitmoot/gitmoot",
				Branch:                 tc.branch,
				PullRequest:            0,
				TaskID:                 tc.branch,
				TaskTitle:              "fanout verb",
				LeadAgent:              "lead",
				Reviewers:              []string{"auditor"},
				SkipNativeReviewFanout: true,
				Result:                 &AgentResult{Decision: tc.decision, Summary: "done"},
			})

			// A review advance may legitimately return an error in this bare
			// fixture; what is being measured is the branch lock, not the advance.
			_ = engine.AdvanceJob(ctx, jobID)

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
