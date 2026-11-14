package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestMailboxRefusesReviewDelegationsInEveryMode pins #2588: reviewers are no
// longer offered delegation, so a review result carrying delegations fails the
// job with a clear result-check message whatever the result_checks mode,
// instead of being dispatched or silently dropped.
func TestMailboxRefusesReviewDelegationsInEveryMode(t *testing.T) {
	const delegating = `{"gitmoot_result":{"decision":"approved","summary":"fanning out to a lens","evidence":"static_only","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[{"id":"lens","agent":"other","action":"review","prompt":"look"}]}}`
	for i, mode := range []ResultCheckMode{ResultChecksOff, ResultChecksWarn, ResultChecksBlock} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			mailbox := Mailbox{store: store, resultCheckMode: mode}
			id := fmt.Sprintf("job-review-delegations-%d", i)
			if _, err := mailbox.Enqueue(ctx, JobRequest{ID: id, Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			agent := shellAgent()
			agent.Role = "reviewer"
			_, runErr := mailbox.Run(ctx, id, agent, &fakeDelivery{outputs: []string{delegating}})
			var checksErr *ResultChecksError
			if !errors.As(runErr, &checksErr) {
				t.Fatalf("run err = %v, want a ResultChecksError", runErr)
			}
			job, err := store.GetJob(ctx, id)
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			if job.State != string(JobFailed) {
				t.Fatalf("state = %q, want %q", job.State, JobFailed)
			}
			payload, err := unmarshalPayload(job.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if len(payload.ResultChecks) != 1 || payload.ResultChecks[0].ID != "review-delegates-nothing" {
				t.Fatalf("stored checks = %+v, want review-delegates-nothing", payload.ResultChecks)
			}
			events, err := store.ListJobEvents(ctx, id)
			if err != nil {
				t.Fatalf("ListJobEvents: %v", err)
			}
			explained := false
			for _, event := range events {
				if event.Kind == ResultChecksFailedEventKind && strings.Contains(event.Message, "reviewers can no longer delegate") {
					explained = true
				}
			}
			if !explained {
				t.Fatalf("events %+v carry no result_checks_failed message explaining the refusal", events)
			}
		})
	}
}
