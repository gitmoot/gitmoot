package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestReviewRequesterFactDoesNotRequireOptionalRoute(t *testing.T) {
	store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}, {"lane", "w1:p1"}})
	insertAwaitedFactWakeForTest(t, store, db.AwaitedFactWakePayload{
		ID: 1, WaiterRole: "lane", SubjectKind: db.AwaitedFactSubjectReviewVerdict,
		SubjectKey: "acme/widget#46@head", State: db.AwaitedFactStateSatisfied,
		Detail: "review approved for exact head",
	})
	drainReplyWakeAfterAllRowsAreDue(t, store, sink)
	delivered, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStateDelivered)
	if err != nil || len(delivered) != 1 || delivered[0].TargetRole != "lane" || wake.promptCalls != 1 {
		t.Fatalf("requester notification: rows=%+v calls=%d err=%v", delivered, wake.promptCalls, err)
	}
	_, err = drainReplyWakeOutboxWithHealth(context.Background(), store, time.Now().Add(time.Hour), time.Second,
		func(context.Context) (replyWakeDelivery, error) { return replyWakeDelivery{sink: sink}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if wake.promptCalls != 1 {
		t.Fatalf("completion replayed: %d prompts", wake.promptCalls)
	}
}

func TestUncertainWakeRemainsVisibleWithoutResending(t *testing.T) {
	store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	wake.stalled = true
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{WorkflowID: "delivery/unknown", Author: "worker", Body: "do this once", AddressedTarget: "owner"}); err != nil {
		t.Fatal(err)
	}
	// Exercise repeated production drain ticks, not just response classification.
	pending, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	created, err := time.Parse(time.RFC3339Nano, pending[0].CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		_, err := drainReplyWakeOutboxWithHealth(context.Background(), store, created.Add(time.Duration(i+1)*time.Hour), time.Second,
			func(context.Context) (replyWakeDelivery, error) { return replyWakeDelivery{sink: sink}, nil })
		if err == nil {
			t.Fatal("uncertain mandatory notice disappeared from health")
		}
	}
	unknown, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStateDeliveryUnknown)
	if err != nil || len(unknown) != 1 || unknown[0].AttemptCount != 1 || wake.promptCalls != 1 {
		t.Fatalf("uncertain delivery: rows=%+v calls=%d err=%v", unknown, wake.promptCalls, err)
	}
}

func TestExplicitFactMuteDoesNotDeliverOrReportLostMail(t *testing.T) {
	store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	if err := store.AddEventRule(context.Background(), db.EventRule{ID: "muted", OnKind: "fact", WakeRole: "owner", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	insertAwaitedFactWakeForTest(t, store, db.AwaitedFactWakePayload{ID: 1, WaiterRole: "owner", SubjectKind: db.AwaitedFactSubjectReviewVerdict, SubjectKey: "acme/widget#46@head", State: db.AwaitedFactStateSatisfied})
	rules, err := store.ListEventRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = drainReplyWakeOutboxWithHealth(context.Background(), store, time.Now().Add(time.Hour), time.Second,
		func(context.Context) (replyWakeDelivery, error) {
			return replyWakeDelivery{sink: sink, rules: rules}, nil
		})
	if err != nil || wake.promptCalls != 0 {
		t.Fatalf("intentional mute: prompts=%d err=%v", wake.promptCalls, err)
	}
}

func TestRegisteredJoblessWorkflowMessagesAcrossAuthorizedRelationships(t *testing.T) {
	for _, roles := range [][2]string{{"jarvis", "owner"}, {"owner", "jarvis"}, {"gm-omp-nag", "gm-omp-impl"}} {
		t.Run(roles[0]+"-"+roles[1], func(t *testing.T) {
			home := orgMessageTestHome(t)
			var out, diag bytes.Buffer
			if code := runWorkflowJournal([]string{"register", "delivery/fresh", "coordinate delivery repair", "--home", home}, &out, &diag); code != 0 {
				t.Fatalf("register code=%d: %s", code, diag.String())
			}
			if code := runOrg([]string{"message", "send", "--home", home, "--org-role", roles[0], "--to", roles[1], "--workflow", "delivery/fresh", "please inspect the review"}, &out, &diag); code != 0 {
				t.Fatalf("send code=%d: %s", code, diag.String())
			}
			store, err := dbtest.Open(t, config.PathsForHome(home).Database)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			count, err := store.CountJobsByWorkflow(context.Background(), "delivery/fresh")
			if err != nil || count != 0 {
				t.Fatalf("registration fabricated jobs: count=%d err=%v", count, err)
			}
			notes, err := store.ListWorkflowNotes(context.Background(), "delivery/fresh", 0)
			if err != nil || len(notes) != 1 {
				t.Fatalf("notes=%v err=%v", notes, err)
			}
			from, to, label, body, ok := workflow.ParseOrgMessageNote(notes[0].Body)
			if !ok || from != roles[0] || to != roles[1] || label != "delivery/fresh" || body != "please inspect the review" {
				t.Fatalf("wrong addressed content: %+v", notes[0])
			}
			rows, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
			if err != nil || len(rows) != 1 || rows[0].TargetRole != roles[1] || rows[0].SourceID != fmt.Sprint(notes[0].ID) {
				t.Fatalf("queued=%+v err=%v", rows, err)
			}
			if code := runWorkflowNote([]string{"delivery/fresh", "to=owner journal only", "--home", home, "--no-auto"}, &out, &diag); code != 0 {
				t.Fatalf("note code=%d: %s", code, diag.String())
			}
			rows, err = store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
			if err != nil || len(rows) != 1 {
				t.Fatalf("journal created notification: rows=%+v err=%v", rows, err)
			}
		})
	}
}

func TestWakeRecoveryRejectsUnknownAndSupersedesWithAudit(t *testing.T) {
	store, _, _, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{WorkflowID: "delivery/recovery", Author: "worker", Body: "uncertain", AddressedTarget: "owner"}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	claimed, err := store.ClaimWakeOutbox(context.Background(), rows[0].ID, nil, time.Now().Add(-time.Hour))
	if err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if _, err := store.ExpireAgedWakeOutbox(context.Background(), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	var out, diag bytes.Buffer
	if code := runOrg([]string{"wake", "retry", fmt.Sprint(rows[0].ID), "--reason", "binding fixed", "--home", home}, &out, &diag); code != 1 {
		t.Fatalf("uncertain retry code=%d err=%s", code, diag.String())
	}
	out.Reset()
	diag.Reset()
	if code := runOrg([]string{"wake", "supersede", fmt.Sprint(rows[0].ID), "--reason", "review head obsolete", "--home", home}, &out, &diag); code != 0 {
		t.Fatalf("supersede code=%d err=%s", code, diag.String())
	}
	superseded, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStateSuperseded)
	if err != nil || len(superseded) != 1 || !strings.Contains(superseded[0].LastError, "review head obsolete") {
		t.Fatalf("superseded=%+v err=%v", superseded, err)
	}
	events, err := store.ListJobEvents(context.Background(), fmt.Sprintf("wake-outbox:%d", rows[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "wake_recovery_superseded" {
			return
		}
	}
	t.Fatal(errors.New("recovery lacks durable audit"))
}
