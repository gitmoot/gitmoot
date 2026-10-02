package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
)

func TestReviewVerdictDoesNotDuplicateRequesterFactNotice(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	head := strings.Repeat("a", 40)
	key, err := db.ReviewRequestSubjectKey("acme/widget", 42, head, "code")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SubscribeAwaitedFact(ctx, db.AwaitedFactSubscription{WaiterRole: "requester", SubjectKind: db.AwaitedFactSubjectReviewVerdict, SubjectKey: key, Deadline: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateJob(ctx, db.Job{ID: "review-notice", Agent: "audit", Type: "review", State: string(JobSucceeded), Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	engine := testEngine(store)
	engine.EventSink = sink
	engine.mailbox().emitTerminal(ctx, "review-notice", JobSucceeded, JobPayload{Repo: "acme/widget", PullRequest: 42, HeadSHA: head, ActingOrgRole: "requester", Result: &AgentResult{Decision: "approved", Summary: "exact head approved"}})
	finished := sink.byType(events.EventJobFinished)
	if len(finished) != 1 {
		t.Fatalf("terminal events=%+v", finished)
	}
	if finished[0].WakeTargetRole == "requester" {
		t.Fatal("legacy event duplicates requester fact")
	}
	for _, role := range finished[0].WakeTargetRoles {
		if role == "requester" {
			t.Fatal("requester appears in legacy multi-target wake")
		}
	}
	// A new head without this subscription retains its own legacy route; do not
	// suppress another review just because the recipient once requested one.
	engine.mailbox().emitTerminal(ctx, "review-notice", JobSucceeded, JobPayload{Repo: "acme/widget", PullRequest: 42, HeadSHA: strings.Repeat("b", 40), ActingOrgRole: "requester", Result: &AgentResult{Decision: "approved", Summary: "new head"}})
	finished = sink.byType(events.EventJobFinished)
	if len(finished) != 2 {
		t.Fatalf("terminal events=%+v", finished)
	}
	for _, role := range finished[1].WakeTargetRoles {
		if role == "requester" {
			return
		}
	}
	t.Fatal("unsubscribed new head lost requester notification")
}
