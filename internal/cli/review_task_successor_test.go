package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #2277: a stranded review task on a still-open PR must be able to get a new
// exact-head review. Before the fix dispatch demanded a successor task that no
// command could create. Now `gitmoot task successor` creates it and dispatch
// binds the review to it, without resurrecting the stranded row.
func TestStrandedReviewTaskGetsANewReviewThroughItsSuccessor(t *testing.T) {
	repo := github.Repository{Owner: "jerryfane", Name: "numbra"}
	head := strings.Repeat("a", 40)
	for _, test := range []struct {
		name   string
		branch string
	}{
		{"derived review task, no branch", ""},
		{"task owning the PR branch", "feat/analytics"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			store := openCLIJobStore(t, home)
			defer store.Close()
			strandedID := "review-pr-46-" + shortHash(repo.FullName())
			if err := store.UpsertTask(ctx, db.Task{ID: strandedID, RepoFullName: repo.FullName(), GoalID: "local-review",
				Title: "Review PR #46", State: string(workflow.TaskAwaitingHumanMerge), Branch: test.branch}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.DisposeTask(ctx, strandedID, []string{string(workflow.TaskAwaitingHumanMerge)},
				string(workflow.TaskStranded), "tier4_stranded", "no disposal evidence: own PR remains open", "", "", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			request := localAgentDispatchRequest{Home: home, PullRequest: 46, HeadSHA: head, Branch: test.branch}

			_, err := prepareLocalReviewTask(ctx, store, repo, request)
			want := "gitmoot task successor " + strandedID
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("dispatch before a successor: err=%v, want it to name %q", err, want)
			}

			var stdout, stderr bytes.Buffer
			if code := runTask([]string{"successor", strandedID, "--home", home, "--reason", "PR still open"}, &stdout, &stderr); code != 0 {
				t.Fatalf("task successor exit %d: %s", code, stderr.String())
			}
			successorID := strandedID + "-successor-2"
			if !strings.Contains(stdout.String(), successorID) {
				t.Fatalf("task successor output %q, want %s", stdout.String(), successorID)
			}

			bound, err := prepareLocalReviewTask(ctx, store, repo, request)
			if err != nil {
				t.Fatalf("dispatch after the successor: %v", err)
			}
			if bound.TaskID != successorID {
				t.Fatalf("review bound to %q, want the successor %q", bound.TaskID, successorID)
			}
			stranded, _ := store.GetTask(ctx, strandedID)
			if stranded.State != string(workflow.TaskStranded) {
				t.Fatalf("stranded task resurrected to %q", stranded.State)
			}
			if test.branch == "" {
				successor, _ := store.GetTask(ctx, successorID)
				if successor.State != string(workflow.TaskReviewing) {
					t.Fatalf("successor state %q, want reviewing", successor.State)
				}
			}
		})
	}
}
