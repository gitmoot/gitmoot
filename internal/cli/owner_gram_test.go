package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/org"
)

// fakeOwnerGram records every send; no real Gram is ever sent from a test.
type fakeOwnerGram struct {
	mu     sync.Mutex
	texts  []string
	result ownerGramResult
	// started, when set, receives once per send; release, when set, blocks the
	// send until closed; delay simulates a slow Herdr.
	started chan struct{}
	release chan struct{}
	delay   time.Duration
}

func (f *fakeOwnerGram) send(_ context.Context, text string) ownerGramResult {
	f.mu.Lock()
	f.texts = append(f.texts, text)
	started, release, delay, result := f.started, f.release, f.delay, f.result
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	time.Sleep(delay)
	return result
}

func (f *fakeOwnerGram) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

func ownerGramResolver(store *db.Store, sink synchronousEventRuleTestSink, gram *fakeOwnerGram) replyWakeDeliveryResolver {
	return func(ctx context.Context) (replyWakeDelivery, error) {
		rules, err := store.ListEventRules(ctx)
		if err != nil {
			return replyWakeDelivery{}, err
		}
		return replyWakeDelivery{sink: sink, rules: rules, ownerGram: gram.send}, nil
	}
}

// insertOwnerEscalation stores a job.needs_attention escalation addressed to
// the owner, the shape that matched no event rule and stayed pending forever.
func insertOwnerEscalation(t *testing.T, store *db.Store, jobID, repo string) {
	t.Helper()
	event := events.Event{
		SchemaVersion: 1, Type: events.EventJobNeedsAttention, JobID: jobID, RootID: jobID,
		Repo: repo, Status: "stranded", Timestamp: time.Now().UTC().Format(time.RFC3339),
		Detail: "task " + jobID + " stranded: own PR remains open and awaiting_human_merge is protected",
		Cause:  "task_disposal_stranded",
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertWakeOutbox(context.Background(), db.WakeOutboxSourceEscalation, string(encoded), db.WakeOutboxKindEscalation, []string{"owner"}); err != nil {
		t.Fatal(err)
	}
}

func wakeRowsByRole(t *testing.T, store *db.Store) map[string]db.WakeOutboxEntry {
	t.Helper()
	rows, err := store.ListWakeOutbox(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	byRole := make(map[string]db.WakeOutboxEntry, len(rows))
	for _, row := range rows {
		byRole[row.TargetRole] = row
	}
	return byRole
}

func TestOwnerEscalationIsSentAsOneGramWithReceiptAndNeverResent(t *testing.T) {
	store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}, {"lane", "w1:p1"}})
	ctx := context.Background()
	insertOwnerEscalation(t, store, "review-pr-46-abc", "acme/widget")
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/lane", Author: "owner", Body: "status", AddressedTarget: "lane",
	}); err != nil {
		t.Fatal(err)
	}
	gram := &fakeOwnerGram{result: ownerGramResult{Outcome: db.OwnerGramAccepted, GramID: "gram-test-1"}}
	resolve := ownerGramResolver(store, sink, gram)
	due := time.Now().UTC().Add(replyWakeCoalescingWindow + time.Second)

	if _, err := drainReplyWakeOutboxWithHealth(ctx, store, due, replyWakeCoalescingWindow, resolve); err != nil {
		t.Fatalf("drain: %v", err)
	}
	sent := gram.calls()
	if len(sent) != 1 {
		t.Fatalf("gram sends = %d, want 1: %q", len(sent), sent)
	}
	for _, want := range []string{"Gitmoot: 1 item needs you", "acme/widget", "stranded", "gitmoot job show review-pr-46-abc"} {
		if !strings.Contains(sent[0], want) {
			t.Fatalf("gram text = %q, want it to contain %q", sent[0], want)
		}
	}
	rows := wakeRowsByRole(t, store)
	if rows["owner"].State != db.WakeOutboxStateDelivered || !strings.Contains(rows["owner"].LastError, "gram-test-1") {
		t.Fatalf("owner row = %+v, want delivered naming its gram", rows["owner"])
	}
	// The non-owner note keeps the ordinary pane path.
	if rows["lane"].State != db.WakeOutboxStateDelivered || wake.promptCalls != 1 || wake.pane != "w1:p1" {
		t.Fatalf("lane row = %+v prompts=%d pane=%q, want ordinary pane delivery", rows["lane"], wake.promptCalls, wake.pane)
	}
	receipts, err := store.ListOwnerGramReceipts(ctx)
	if err != nil || len(receipts) != 1 || receipts[0].GramID != "gram-test-1" ||
		receipts[0].Outcome != db.OwnerGramAccepted || receipts[0].WakeOutboxID != rows["owner"].ID {
		t.Fatalf("receipts = %+v err=%v, want one accepted receipt for the owner row", receipts, err)
	}

	// Later drains, even after the spacing interval, never send it again.
	for _, at := range []time.Time{due.Add(time.Second), due.Add(ownerGramMinInterval + time.Minute)} {
		if _, err := drainReplyWakeOutboxWithHealth(ctx, store, at, replyWakeCoalescingWindow, resolve); err != nil {
			t.Fatalf("later drain: %v", err)
		}
	}
	if got := len(gram.calls()); got != 1 {
		t.Fatalf("gram sends after later drains = %d, want still 1", got)
	}
}

func TestOwnerGramUnknownOutcomeIsNotResentAndShowsInOrgHealth(t *testing.T) {
	store, sink, _, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	ctx := context.Background()
	insertOwnerEscalation(t, store, "review-pr-7-def", "acme/widget")
	gram := &fakeOwnerGram{result: ownerGramResult{Outcome: db.OwnerGramUnknown, Detail: "herdr gram send timed out; it may have been sent"}}
	resolve := ownerGramResolver(store, sink, gram)
	due := time.Now().UTC().Add(replyWakeCoalescingWindow + time.Second)

	health, err := drainReplyWakeOutboxWithHealth(ctx, store, due, replyWakeCoalescingWindow, resolve)
	if err == nil || health.blocked != 1 || health.unknown != 1 {
		t.Fatalf("health = %s err = %v, want the unknown owner send reported as blocked/unknown", health, err)
	}
	for _, at := range []time.Time{due.Add(time.Minute), due.Add(time.Hour)} {
		_, _ = drainReplyWakeOutboxWithHealth(ctx, store, at, replyWakeCoalescingWindow, resolve)
	}
	if got := len(gram.calls()); got != 1 {
		t.Fatalf("gram sends = %d, want 1: an unknown outcome must never be resent", got)
	}
	row := wakeRowsByRole(t, store)["owner"]
	if row.State != db.WakeOutboxStateDeliveryUnknown {
		t.Fatalf("owner row = %+v, want delivery_unknown", row)
	}
	if db.WakeProvenUnsent(row) {
		t.Fatal("an unknown owner send was classified as provably unsent")
	}

	paths := config.PathsForHome(home)
	shared, err := loadOrgSharedState(ctx, paths, store, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	statusRows, err := buildOrgStatusRows(ctx, &shared, func(context.Context, config.OrgConfig) (map[string]org.RoleLiveState, time.Time, string, error) {
		return map[string]org.RoleLiveState{"owner": {State: org.StateIdle}}, time.Time{}, "fixture", nil
	}, "status", false)
	if err != nil {
		t.Fatal(err)
	}
	var flag string
	for _, statusRow := range statusRows {
		if statusRow.Role == "owner" {
			flag = orgMissedWakeFlag(statusRow)
		}
	}
	if !strings.Contains(flag, "1 owner alerts not delivered") || !strings.Contains(flag, "unknown=1") {
		t.Fatalf("owner org status flag = %q, want the undelivered owner alert", flag)
	}
}

func TestOwnerGramRefusalFailsRowAsProvenUnsent(t *testing.T) {
	store, sink, _, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	ctx := context.Background()
	insertOwnerEscalation(t, store, "review-pr-8-aaa", "acme/widget")
	gram := &fakeOwnerGram{result: ownerGramResult{Outcome: db.OwnerGramRefused, Detail: "herdr server_not_running: no herdr server"}}
	due := time.Now().UTC().Add(replyWakeCoalescingWindow + time.Second)
	_, _ = drainReplyWakeOutboxWithHealth(ctx, store, due, replyWakeCoalescingWindow, ownerGramResolver(store, sink, gram))
	row := wakeRowsByRole(t, store)["owner"]
	if row.State != db.WakeOutboxStateFailed || !db.WakeProvenUnsent(row) {
		t.Fatalf("owner row = %+v, want failed and provably unsent (retryable by an operator)", row)
	}
	alerts, err := store.OwnerAlertHealth(ctx, "owner")
	if err != nil || alerts.Failed != 1 {
		t.Fatalf("owner alert health = %+v err=%v, want one failed", alerts, err)
	}
}

func TestOwnerGramsAreSpacedAndBatched(t *testing.T) {
	store, sink, _, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	ctx := context.Background()
	insertOwnerEscalation(t, store, "review-pr-1-aaa", "acme/one")
	insertOwnerEscalation(t, store, "review-pr-2-bbb", "acme/two")
	gram := &fakeOwnerGram{result: ownerGramResult{Outcome: db.OwnerGramAccepted, GramID: "gram-batch"}}
	resolve := ownerGramResolver(store, sink, gram)
	due := time.Now().UTC().Add(replyWakeCoalescingWindow + time.Second)
	if _, err := drainReplyWakeOutboxWithHealth(ctx, store, due, replyWakeCoalescingWindow, resolve); err != nil {
		t.Fatal(err)
	}
	sent := gram.calls()
	if len(sent) != 1 || !strings.Contains(sent[0], "Gitmoot: 2 items need you") ||
		!strings.Contains(sent[0], "acme/one") || !strings.Contains(sent[0], "acme/two") {
		t.Fatalf("gram sends = %q, want both alerts in one gram", sent)
	}

	insertOwnerEscalation(t, store, "review-pr-3-ccc", "acme/three")
	// The receipt was stamped at real time; a third alert due shortly after is
	// held back by the spacing interval, then sent once it elapses.
	soon := time.Now().UTC().Add(replyWakeCoalescingWindow + time.Second)
	if _, err := drainReplyWakeOutboxWithHealth(ctx, store, soon, replyWakeCoalescingWindow, resolve); err != nil {
		t.Fatal(err)
	}
	if got := len(gram.calls()); got != 1 {
		t.Fatalf("gram sends inside the spacing interval = %d, want 1", got)
	}
	if _, err := drainReplyWakeOutboxWithHealth(ctx, store, soon.Add(ownerGramMinInterval), replyWakeCoalescingWindow, resolve); err != nil {
		t.Fatal(err)
	}
	sent = gram.calls()
	if len(sent) != 2 || !strings.Contains(sent[1], "acme/three") || strings.Contains(sent[1], "acme/one") {
		t.Fatalf("gram sends = %q, want the third alert alone in a second gram", sent)
	}
}

func TestConcurrentDrainsSendOwnerGramOnce(t *testing.T) {
	store, sink, _, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	ctx := context.Background()
	insertOwnerEscalation(t, store, "review-pr-9-ddd", "acme/widget")
	gram := &fakeOwnerGram{result: ownerGramResult{Outcome: db.OwnerGramAccepted, GramID: "gram-once"}}
	resolve := ownerGramResolver(store, sink, gram)
	due := time.Now().UTC().Add(replyWakeCoalescingWindow + time.Second)
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			_, _ = drainReplyWakeOutboxWithHealth(ctx, store, due, replyWakeCoalescingWindow, resolve)
		})
	}
	group.Wait()
	if got := len(gram.calls()); got != 1 {
		t.Fatalf("gram sends across concurrent drains = %d, want 1", got)
	}
}

func TestOwnerEscalationWithoutRuleIsNotInertWhenGramIsWired(t *testing.T) {
	store, sink, _, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	ctx := context.Background()
	insertOwnerEscalation(t, store, "review-pr-5-eee", "acme/widget")
	now := time.Now().UTC()
	gram := &fakeOwnerGram{result: ownerGramResult{Outcome: db.OwnerGramAccepted, GramID: "gram-x"}}
	health, _ := wakeOutboxObligationHealth(ctx, store, now.Add(-replyWakeAttemptedUnknownAfter), now, replyWakeCoalescingWindow, ownerGramResolver(store, sink, gram))
	if health.inert != 0 || health.held != 1 {
		t.Fatalf("health = %s, want the owner escalation held (deliverable), not inert", health)
	}
	if len(gram.calls()) != 0 {
		t.Fatal("health classification sent a gram")
	}
}

func TestClassifyHerdrGramSend(t *testing.T) {
	tests := []struct {
		name    string
		run     herdrGramRun
		outcome string
		gramID  string
	}{
		{"accepted", herdrGramRun{started: true, stdout: []byte(`{"id":"cli:gram:send","result":{"type":"gram_sent","message":{"id":"gram-1a2b","text":"x"}}}`)}, db.OwnerGramAccepted, "gram-1a2b"},
		{"success without id", herdrGramRun{started: true, stdout: []byte(`ok`)}, db.OwnerGramUnknown, ""},
		{"server refused", herdrGramRun{started: true, exitCode: 1, stderr: []byte(`{"id":"cli:gram:send","error":{"code":"server_not_running","message":"no herdr server is running"}}`)}, db.OwnerGramRefused, ""},
		{"exit 1 without error body", herdrGramRun{started: true, exitCode: 1, stderr: []byte(`Error: broken pipe`)}, db.OwnerGramUnknown, ""},
		{"usage refused", herdrGramRun{started: true, exitCode: 2, stderr: []byte(`usage: herdr gram send`)}, db.OwnerGramRefused, ""},
		{"timed out", herdrGramRun{started: true, timedOut: true, exitCode: -1}, db.OwnerGramUnknown, ""},
		{"cannot start", herdrGramRun{startErr: errors.New("executable file not found")}, db.OwnerGramRefused, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyHerdrGramSend(test.run)
			if got.Outcome != test.outcome || got.GramID != test.gramID {
				t.Fatalf("classifyHerdrGramSend = %+v, want outcome %q gram %q", got, test.outcome, test.gramID)
			}
		})
	}
}

// The owner path must stay off unless the daemon wired a sender: tests and
// one-shot drains keep the ordinary rule path for the owner role.
func TestOwnerWakeWithoutGramSenderKeepsOrdinaryPath(t *testing.T) {
	store, sink, _, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	ctx := context.Background()
	insertOwnerEscalation(t, store, "review-pr-6-fff", "acme/widget")
	due := time.Now().UTC().Add(replyWakeCoalescingWindow + time.Second)
	health, _ := drainReplyWakeOutboxWithHealth(ctx, store, due, replyWakeCoalescingWindow, replyWakeTestDeliveryResolver(sink))
	if health.inert != 1 {
		t.Fatalf("health = %s, want the rule-less owner escalation inert without a gram sender", health)
	}
	receipts, err := store.ListOwnerGramReceipts(ctx)
	if err != nil || len(receipts) != 0 {
		t.Fatalf("receipts = %+v err=%v, want none", receipts, err)
	}
}

// Two drainers with DISJOINT batches (more than ownerGramMaxItems due rows)
// must not both send: the first claim reserves the owner Gram slot durably
// before its send starts.
func TestOverlappingDrainsWithDisjointOwnerBatchesSendOneGram(t *testing.T) {
	store, sink, _, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	ctx := context.Background()
	for index := range ownerGramMaxItems + 5 {
		insertOwnerEscalation(t, store, fmt.Sprintf("review-pr-%d-ovl", index+1), "acme/widget")
	}
	gram := &fakeOwnerGram{
		result:  ownerGramResult{Outcome: db.OwnerGramAccepted, GramID: "gram-first"},
		started: make(chan struct{}, 4), release: make(chan struct{}),
	}
	resolve := ownerGramResolver(store, sink, gram)
	due := time.Now().UTC().Add(replyWakeCoalescingWindow + time.Second)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = drainReplyWakeOutboxWithHealth(ctx, store, due, replyWakeCoalescingWindow, resolve)
	}()
	select {
	case <-gram.started:
	case <-time.After(10 * time.Second):
		t.Fatal("first drain never started its send")
	}
	// The first send is still in flight and has written no receipt yet.
	_, _ = drainReplyWakeOutboxWithHealth(ctx, store, due.Add(time.Second), replyWakeCoalescingWindow, resolve)
	if got := len(gram.calls()); got != 1 {
		close(gram.release)
		t.Fatalf("gram sends while the first send was in flight = %d, want 1", got)
	}
	close(gram.release)
	<-firstDone
	pending, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil || len(pending) != 5 {
		t.Fatalf("pending owner rows = %d err=%v, want the 5 rows beyond the first gram", len(pending), err)
	}
}

// The spacing interval runs from when a send finished, not from when the drain
// started it, so a slow Herdr cannot shorten the gap between owner Grams.
func TestOwnerGramSpacingRunsFromSendCompletion(t *testing.T) {
	store, sink, _, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p0"}})
	ctx := context.Background()
	for index := range ownerGramMaxItems + 1 {
		insertOwnerEscalation(t, store, fmt.Sprintf("review-pr-%d-slow", index+1), "acme/widget")
	}
	const sendTook = 1500 * time.Millisecond
	gram := &fakeOwnerGram{result: ownerGramResult{Outcome: db.OwnerGramAccepted, GramID: "gram-slow"}, delay: sendTook}
	resolve := ownerGramResolver(store, sink, gram)
	due := time.Now().UTC().Add(replyWakeCoalescingWindow + time.Second)
	if _, err := drainReplyWakeOutboxWithHealth(ctx, store, due, replyWakeCoalescingWindow, resolve); err != nil {
		t.Fatal(err)
	}
	receipts, err := store.ListOwnerGramReceipts(ctx)
	if err != nil || len(receipts) != ownerGramMaxItems {
		t.Fatalf("receipts = %d err=%v", len(receipts), err)
	}
	sentAt, err := time.Parse(db.BlockedEpisodeTimeLayout, receipts[0].SentAt)
	if err != nil || sentAt.Sub(due) < sendTook {
		t.Fatalf("receipt sent_at = %s, drain started %s: want the send completion time", receipts[0].SentAt, due)
	}
	gram.mu.Lock()
	gram.delay = 0
	gram.mu.Unlock()
	// Two minutes after the drain STARTED, but not after the send finished.
	if _, err := drainReplyWakeOutboxWithHealth(ctx, store, due.Add(ownerGramMinInterval+sendTook/2), replyWakeCoalescingWindow, resolve); err != nil {
		t.Fatal(err)
	}
	if got := len(gram.calls()); got != 1 {
		t.Fatalf("gram sends before two minutes after completion = %d, want 1", got)
	}
	if _, err := drainReplyWakeOutboxWithHealth(ctx, store, sentAt.Add(ownerGramMinInterval), replyWakeCoalescingWindow, resolve); err != nil {
		t.Fatal(err)
	}
	if got := len(gram.calls()); got != 2 {
		t.Fatalf("gram sends two minutes after completion = %d, want 2", got)
	}
}
