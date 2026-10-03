package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
)

func TestMessageReviewNotificationUsesRequesterAndExactPurpose(t *testing.T) {
	for _, policy := range []string{"no optional rule", "observer muted", "addressed muted", "post-merge subscriber"} {
		t.Run(policy, func(t *testing.T) {
			ctx := context.Background()
			store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
			head := strings.Repeat("a", 40)
			if err := store.CreateJobWithEvent(ctx, db.Job{ID: "review-source", Agent: "reviewer", Type: "review", State: "succeeded", Payload: `{"repo":"acme/widget","pull_request":42,"head_sha":"` + head + `","review_purpose":"code","post_merge_review":true,"result":{"decision":"approved"}}`}, db.JobEvent{Kind: "succeeded", Message: "review completed"}); err != nil {
				t.Fatal(err)
			}
			switch policy {
			case "observer muted", "addressed muted":
				scope := db.EventRuleScopeObserver
				if policy == "addressed muted" {
					scope = db.EventRuleScopeAddressed
				}
				if err := store.AddEventRule(ctx, db.EventRule{ID: "muted-review", OnKind: "review-verdict", WakeRole: "owner", Scope: scope, Enabled: false}); err != nil {
					t.Fatal(err)
				}
			case "post-merge subscriber":
				key, err := db.ReviewRequestSubjectKey("acme/widget", 42, head, db.ReviewRequestPurpose("code", true))
				if err != nil {
					t.Fatal(err)
				}
				fact, _, err := store.SubscribeAwaitedFact(ctx, db.AwaitedFactSubscription{WaiterRole: "owner", SubjectKind: db.AwaitedFactSubjectReviewVerdict, SubjectKey: key, Deadline: time.Now().UTC().Add(time.Hour)})
				if err != nil {
					t.Fatal(err)
				}
				if fact.State != db.AwaitedFactStateSatisfied {
					t.Fatalf("completed exact-head review did not satisfy subscriber: %+v", fact)
				}
			}
			event := events.Event{Type: events.EventJobFinished, JobID: "review-source", Repo: "acme/widget", PullRequest: 42, Cause: events.EventCauseReviewVerdict, ReviewDecision: "approved", Status: "succeeded", WakeTargetRole: "owner", Detail: "Review complete", Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
			sink.sink.Emit(ctx, event)
			event.Timestamp = time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
			sink.sink.Emit(ctx, event)
			inbox, err := store.ListMessages(ctx, "owner", 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(inbox) != 1 {
				t.Fatalf("completion produced %d inbox items: %+v", len(inbox), inbox)
			}
			var notice struct {
				Sender        string `json:"from"`
				Kind          string `json:"kind"`
				Repo          string `json:"repo"`
				PullRequest   int    `json:"pull_request"`
				HeadSHA       string `json:"head_sha"`
				ReviewPurpose string `json:"review_purpose"`
				SourceKind    string `json:"source_kind"`
			}
			wire, err := json.Marshal(inbox[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(wire, &notice); err != nil {
				t.Fatal(err)
			}
			if notice.Sender != "@system" || notice.Kind != "review" || notice.Repo != "acme/widget" || notice.PullRequest != 42 || notice.HeadSHA != head || notice.ReviewPurpose != db.ReviewRequestPurpose("code", true) {
				t.Fatalf("incorrect trusted provenance: %+v", notice)
			}
			if policy == "post-merge subscriber" && notice.SourceKind != db.WakeOutboxSourceAwaitedFact {
				t.Fatalf("duplicate legacy review route won: %+v", notice)
			}
			_ = drainReplyWakeAfterAllRowsAreDueResult(t, store, sink)
			expected := 1
			if policy == "addressed muted" {
				expected = 0
			}
			if wake.promptCalls != expected {
				t.Fatalf("wake calls=%d, want %d for %s", wake.promptCalls, expected, policy)
			}
			_, _ = drainReplyWakeOutboxWithHealth(ctx, store, time.Now().UTC().Add(time.Hour), 0, replyWakeTestDeliveryResolver(sink))
			if wake.promptCalls != expected {
				t.Fatal("replayed review notification on a later drain")
			}
		})
	}
}
