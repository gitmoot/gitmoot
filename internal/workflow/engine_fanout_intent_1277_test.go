package workflow

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1277, the escape g7-review reproduced against the first version of this fix.
// The branch lock cannot rescue a multi-generation tree, because at the moment
// the intent is dropped NO intent-bearing implement job has advanced yet, so
// nothing has published the intent onto the lock.
//
// The path: an intent-bearing coordinator delegates a child; the child's
// continuation is built by a constructor that does NOT set the flag (there are
// nine such constructors and none set it); that continuation then delegates the
// implement leg, which opens the PR and re-arms the fanout.
//
// Enqueue-time inheritance closes it for every constructor at once, which is why
// the continuation request below deliberately leaves SkipNativeReviewFanout
// unset — exactly as engine_continuation_synthesis.go builds it.
func TestIntentSurvivesContinuationThenImplementChild(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"ask", "review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	mailbox := Mailbox{store: store}

	root, err := mailbox.Enqueue(ctx, JobRequest{
		ID:                     "root-coordinator",
		Agent:                  "coordinator",
		Action:                 "ask",
		Repo:                   "gitmoot/gitmoot",
		Branch:                 "task-9",
		TaskID:                 "task-9",
		Instructions:           "coordinate",
		SkipNativeReviewFanout: true,
	})
	if err != nil {
		t.Fatalf("Enqueue root returned error: %v", err)
	}

	// Generation 2: a continuation constructor that drops the flag.
	continuation, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "continuation-1",
		Agent:        "coordinator",
		Action:       "ask",
		Repo:         "gitmoot/gitmoot",
		Branch:       "task-9",
		TaskID:       "task-9",
		Instructions: "synthesize",
		ParentJobID:  root.ID,
	})
	if err != nil {
		t.Fatalf("Enqueue continuation returned error: %v", err)
	}
	continuationPayload, err := ParseJobPayload(continuation.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(continuation) returned error: %v", err)
	}
	if !continuationPayload.SkipNativeReviewFanout {
		t.Fatalf("continuation dropped the intent: %+v", continuationPayload)
	}

	// Generation 3: the implement child that would open the PR.
	child, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "implement-child",
		Agent:        "impl",
		Action:       "implement",
		Repo:         "gitmoot/gitmoot",
		Branch:       "task-9",
		TaskID:       "task-9",
		Instructions: "do the work",
		ParentJobID:  continuation.ID,
	})
	if err != nil {
		t.Fatalf("Enqueue implement child returned error: %v", err)
	}
	childPayload, err := ParseJobPayload(child.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(child) returned error: %v", err)
	}
	if !childPayload.SkipNativeReviewFanout {
		t.Fatalf("implement child after continuation re-armed native fanout: %+v", childPayload)
	}
}

func assertNoReviewJobs(t *testing.T, ctx context.Context, store *db.Store) {
	t.Helper()
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "review" {
			t.Fatalf("expected no review jobs, found %+v", job)
		}
	}
}
