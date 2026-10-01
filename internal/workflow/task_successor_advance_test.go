package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2278 review P1: a review bound to the successor of a stranded task that
// owned the PR branch must be able to advance. The engine resolves a task by
// branch, so while the stranded task kept the branch every verdict failed with
// "task ... is stranded; workflow advancement cannot move it" and retried
// forever. The successor now takes the branch over.
func TestReviewVerdictAdvancesTheSuccessorOfAStrandedBranchOwner(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "gm-review-opus", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("e", 40)
	if err := store.UpsertTask(ctx, db.Task{ID: "review-pr-9-abcd", RepoFullName: "gitmoot/gitmoot", State: string(TaskAwaitingHumanMerge), Branch: "task-9", Title: "Review PR #9"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.DisposeTask(ctx, "review-pr-9-abcd", []string{string(TaskAwaitingHumanMerge)}, string(TaskStranded), "tier4_stranded", "own PR remains open", "", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	successor, _, err := store.CreateTaskSuccessor(ctx, "review-pr-9-abcd", []string{string(TaskStranded)}, string(TaskReviewing), "")
	if err != nil {
		t.Fatal(err)
	}
	insertCompletedJob(t, store, db.Job{ID: "review-succ", Agent: "gm-review-opus", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 9, HeadSHA: head, TaskID: successor.ID,
		Result: &AgentResult{Decision: "changes_requested", Severity: "P2", Summary: "one defect",
			Evidence: EvidenceExecuted, TestsRun: []string{"go test ./... -> ok"}},
	})

	if err := engine.AdvanceJob(ctx, "review-succ"); err != nil {
		t.Fatalf("a successor-bound verdict could not advance: %v", err)
	}
	advanced, err := store.GetTask(ctx, successor.ID)
	if err != nil || advanced.State != string(TaskChangesRequested) {
		t.Fatalf("successor = %+v %v, want changes_requested", advanced, err)
	}
	if stranded, _ := store.GetTask(ctx, "review-pr-9-abcd"); stranded.State != string(TaskStranded) {
		t.Fatalf("stranded task moved to %q", stranded.State)
	}
}
