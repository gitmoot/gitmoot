package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

type postMergeIssueCall struct {
	repo, title, body string
	labels            []string
}

func rawPostMergeFinding(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPostMergeReviewFilesFollowUpsWithoutReviewLifecycle(t *testing.T) {
	for _, decision := range []string{"changes_requested", "approved"} {
		t.Run(decision, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-5", RepoFullName: "jerryfane/joltra", Branch: "task-5", State: string(TaskMerged),
			}); err != nil {
				t.Fatalf("UpsertTask: %v", err)
			}
			engine := testEngine(store)
			engine.MergeGate = &fakeMergeGate{onEvaluate: func(MergeRequest) {
				t.Fatal("merge gate evaluated for a post-merge review")
			}}
			var calls []postMergeIssueCall
			engine.PostMergeFollowUpIssue = func(_ context.Context, repo, title, body string, labels []string) (string, error) {
				calls = append(calls, postMergeIssueCall{repo, title, body, labels})
				return "https://github.com/jerryfane/joltra/issues/900", nil
			}
			insertCompletedJob(t, store, db.Job{ID: "post-merge-review", Agent: "audit", Type: "review"}, JobPayload{
				Repo: "jerryfane/joltra", Branch: "task-5", PullRequest: 5, HeadSHA: "head5", TaskID: "task-5",
				WorkflowID: "wf-post-merge", ReviewRequester: "deimos", NoFixTarget: true, PostMergeReview: true,
				Result: &AgentResult{Decision: decision, Summary: "post-merge verdict", Findings: []json.RawMessage{
					rawPostMergeFinding(t, map[string]any{"severity": "P1", "title": "Retry can no longer restart a stuck pass", "detail": "the retry path returns early", "file": "src/retry.ts", "line": 12}),
					rawPostMergeFinding(t, map[string]any{"severity": "P2", "title": "Playhead desyncs on collapsed cuts", "detail": "offset is not projected", "file": "src/player.ts", "line": 40}),
					rawPostMergeFinding(t, map[string]any{"severity": "P3", "title": "Naming nit"}),
					rawPostMergeFinding(t, map[string]any{"severity": "P2", "title": "Withdrawn claim", "state": "withdrawn"}),
				}},
			})

			for range 2 {
				if err := engine.AdvanceJob(ctx, "post-merge-review"); err != nil {
					t.Fatalf("AdvanceJob: %v", err)
				}
			}

			if len(calls) != 1 {
				t.Fatalf("issue calls = %d, want exactly one P2 issue across both advances: %+v", len(calls), calls)
			}
			call := calls[0]
			if call.repo != "jerryfane/joltra" || !strings.HasPrefix(call.title, "[review-p2] Playhead desyncs") ||
				strings.Join(call.labels, ",") != "review-p2,agent:deimos" ||
				!strings.Contains(call.body, "finding-uid: post-merge-review/1") || !strings.Contains(call.body, "src/player.ts:40") {
				t.Fatalf("issue call = %+v", call)
			}

			notes, err := store.ListWorkflowNotes(ctx, "wf-post-merge", 0)
			if err != nil {
				t.Fatalf("ListWorkflowNotes: %v", err)
			}
			var urgent, followup int
			for _, note := range notes {
				switch {
				case strings.Contains(note.Body, "Fix or revert today") && strings.Contains(note.Body, "Retry can no longer restart"):
					urgent++
				case strings.Contains(note.Body, "issues/900"):
					followup++
				}
			}
			if urgent != 1 || followup != 1 {
				t.Fatalf("notes: urgent=%d followup=%d, want 1 each; notes=%+v", urgent, followup, notes)
			}

			task, err := store.GetTask(ctx, "task-5")
			if err != nil {
				t.Fatalf("GetTask: %v", err)
			}
			if task.State != string(TaskMerged) {
				t.Fatalf("task state = %q, want %q unchanged", task.State, TaskMerged)
			}
			if got := countJobEvents(t, store, "post-merge-review", postMergeFollowupsDoneEvent); got != 1 {
				t.Fatalf("%s events = %d, want 1", postMergeFollowupsDoneEvent, got)
			}
		})
	}
}

func TestPostMergeReviewRecordsUnwiredIssueHook(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{ID: "post-merge-unwired", Agent: "audit", Type: "review"}, JobPayload{
		Repo: "jerryfane/joltra", PullRequest: 6, HeadSHA: "head6", PostMergeReview: true, NoFixTarget: true,
		Result: &AgentResult{Decision: "changes_requested", Findings: []json.RawMessage{
			rawPostMergeFinding(t, map[string]any{"severity": "P2", "title": "Needs a follow-up"}),
		}},
	})
	if err := engine.AdvanceJob(ctx, "post-merge-unwired"); err != nil {
		t.Fatalf("AdvanceJob: %v", err)
	}
	if got := countJobEvents(t, store, "post-merge-unwired", postMergeFollowupErrorEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1", postMergeFollowupErrorEvent, got)
	}
	if got := countJobEvents(t, store, "post-merge-unwired", postMergeFollowupUnaddressedEvent); got != 1 {
		t.Fatalf("%s events = %d, want 1 (no role or workflow)", postMergeFollowupUnaddressedEvent, got)
	}
}
