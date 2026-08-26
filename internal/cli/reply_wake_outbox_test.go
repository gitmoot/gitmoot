package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestBlockedEventProducesDurableWakeOutboxObligation(t *testing.T) {
	store, deliverySink, wake, _ := replyWakeTestHarness(
		t,
		[]replyWakeTestRole{{name: "owner", pane: "w1:p1"}},
	)
	ctx := context.Background()
	if err := store.AddEventRule(ctx, db.EventRule{
		ID: "blocked-owner", OnKind: "blocked", WakeRole: "owner", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	event := events.NewEvent(
		events.EventJobBlocked,
		"job-blocked",
		"root-blocked",
		"owner/repo",
		"blocked",
		"needs operator input",
		time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
		workflow.RedactCommentText,
	)
	deliverySink.sink.Emit(ctx, event)

	pending, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("blocked durable obligations = %d, want 1: %+v", len(pending), pending)
	}
	if pending[0].SourceKind != "blocked" ||
		pending[0].TargetRole != "owner" ||
		pending[0].CoalesceKey != "blocked:owner" {
		t.Fatalf("blocked durable obligation = %+v", pending[0])
	}

	drainReplyWakeAfterAllRowsAreDue(t, store, deliverySink)
	if wake.promptCalls != 1 || !strings.Contains(wake.prompt, "gitmoot blocked event") {
		t.Fatalf("blocked wake = calls=%d prompt=%q, want one blocked wake", wake.promptCalls, wake.prompt)
	}
	delivered, err := store.ListWakeOutbox(ctx, db.WakeOutboxStateDelivered)
	if err != nil || len(delivered) != 1 {
		t.Fatalf("delivered blocked obligations = %+v, err=%v", delivered, err)
	}
}

func TestWakeOutboxCoalescesPerKindAndRole(t *testing.T) {
	store, deliverySink, wake, _ := replyWakeTestHarness(
		t,
		[]replyWakeTestRole{{name: "owner", pane: "w1:p1"}},
	)
	ctx := context.Background()
	if err := store.AddEventRule(ctx, db.EventRule{
		ID: "blocked-owner", OnKind: "blocked", WakeRole: "owner", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 2; index++ {
		event := events.NewEvent(
			events.EventJobBlocked,
			fmt.Sprintf("job-blocked-%d", index),
			fmt.Sprintf("root-blocked-%d", index),
			"owner/repo",
			"blocked",
			"needs operator input",
			base.Add(time.Duration(index)*time.Second),
			workflow.RedactCommentText,
		)
		deliverySink.sink.Emit(ctx, event)
	}
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID:      "release/coalesce-kinds",
		Author:          "worker",
		Body:            "reply for owner",
		AddressedTarget: "owner",
	}); err != nil {
		t.Fatal(err)
	}

	pending, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("durable obligations = %d, want 3: %+v", len(pending), pending)
	}
	keysByKind := map[string]map[string]bool{}
	for _, entry := range pending {
		if keysByKind[entry.SourceKind] == nil {
			keysByKind[entry.SourceKind] = map[string]bool{}
		}
		keysByKind[entry.SourceKind][entry.CoalesceKey] = true
	}
	if len(keysByKind["blocked"]) != 1 || !keysByKind["blocked"]["blocked:owner"] {
		t.Fatalf("blocked coalesce keys = %v, want one blocked:owner key", keysByKind["blocked"])
	}
	if len(keysByKind[db.WakeOutboxSourceWorkflowNote]) != 1 ||
		!keysByKind[db.WakeOutboxSourceWorkflowNote]["reply:owner"] {
		t.Fatalf("reply coalesce keys = %v, want one reply:owner key", keysByKind[db.WakeOutboxSourceWorkflowNote])
	}
	if keysByKind["blocked"]["reply:owner"] {
		t.Fatalf("blocked and reply obligations collided: %+v", pending)
	}

	drainReplyWakeAfterAllRowsAreDue(t, store, deliverySink)
	if wake.promptCalls != 2 {
		t.Fatalf("wake calls = %d, want one blocked and one reply wake: %q", wake.promptCalls, wake.prompts)
	}
	var blockedWakes, replyWakes int
	for _, prompt := range wake.prompts {
		switch {
		case strings.Contains(prompt, "gitmoot blocked event"):
			blockedWakes++
		case strings.Contains(prompt, "gitmoot reply event"):
			replyWakes++
		}
	}
	if blockedWakes != 1 || replyWakes != 1 {
		t.Fatalf("wake kinds = blocked:%d reply:%d, prompts=%q", blockedWakes, replyWakes, wake.prompts)
	}
}

func TestWakeOutboxTickHealthIncludesBlockedAndEscalation(t *testing.T) {
	store := daemonWorkerStore(t)
	ctx := context.Background()
	for _, rule := range []db.EventRule{
		{ID: "blocked-owner", OnKind: "blocked", WakeRole: "owner", Enabled: true},
		{ID: "escalation-owner", OnKind: "escalation", WakeRole: "owner", Enabled: true},
	} {
		if err := store.AddEventRule(ctx, rule); err != nil {
			t.Fatal(err)
		}
	}
	sink := &eventRuleSink{store: store}
	now := time.Now().UTC()
	blocked := events.NewEvent(
		events.EventJobBlocked, "job-blocked", "root-blocked", "owner/repo",
		"blocked", "blocked", now, workflow.RedactCommentText,
	)
	blocked.WakeTargetRole = "owner"
	escalation := events.NewEvent(
		events.EventJobNeedsAttention, "job-escalated", "root-escalated", "owner/repo",
		"awaiting_human", "answer required", now, workflow.RedactCommentText,
	)
	escalation.Cause = "escalation"
	escalation.WakeTargetRole = "owner"
	sink.Emit(ctx, blocked)
	sink.Emit(ctx, escalation)

	pending, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("blocked/escalation obligations = %d, want 2: %+v", len(pending), pending)
	}
	kinds := map[string]bool{}
	for _, entry := range pending {
		kinds[entry.SourceKind] = true
	}
	if !kinds["blocked"] || !kinds["escalation"] {
		t.Fatalf("pending source kinds = %v, want blocked and escalation", kinds)
	}
	health, err := wakeOutboxObligationHealth(
		ctx,
		store,
		now.Add(-replyWakeAttemptedUnknownAfter),
		func(ctx context.Context) (replyWakeDelivery, error) {
			rules, err := store.ListEventRules(ctx)
			return replyWakeDelivery{rules: rules}, err
		},
	)
	if err == nil || health.pending != 2 || health.inert != 0 ||
		!strings.Contains(err.Error(), "pending=2 inert=0 route_removed=0 aged_attempted=0") {
		t.Fatalf("tick health = %s err=%v, want two routable outstanding obligations", health, err)
	}
}

func TestWakeOutboxHealthDistinguishesRemovedRouteFromNeverConfigured(t *testing.T) {
	store := daemonWorkerStore(t)
	ctx := context.Background()
	if err := store.AddEventRule(ctx, db.EventRule{
		ID: "reply-owner", OnKind: "reply", WakeRole: "owner", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"owner", "worker"} {
		if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: "release/route-history", Author: "worker", Body: target,
			AddressedTarget: target,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DeleteEventRule(ctx, "reply-owner"); err != nil {
		t.Fatal(err)
	}
	health, err := wakeOutboxObligationHealth(
		ctx,
		store,
		time.Now().UTC().Add(-replyWakeAttemptedUnknownAfter),
		func(context.Context) (replyWakeDelivery, error) {
			return replyWakeDelivery{}, nil
		},
	)
	if err == nil || health.pending != 0 || health.inert != 1 || health.routeRemoved != 1 ||
		!strings.Contains(err.Error(), "pending=0 inert=1 route_removed=1 aged_attempted=0") {
		t.Fatalf("route-history health = %s err=%v", health, err)
	}
}

func TestReplyWakeOutboxMalformedRowDoesNotBlockOtherRoleDelivery(t *testing.T) {
	store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{
		{"owner", "w1:p1"},
		{"zulu", "w1:p2"},
	})
	ctx := context.Background()
	for _, target := range []string{"owner", "zulu"} {
		if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: "release/malformed-isolation", Author: "worker", Body: target,
			AddressedTarget: target,
		}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE wake_outbox SET source_kind = 'bogus_kind' WHERE target_role = 'zulu'`); err != nil {
		t.Fatal(err)
	}

	err = drainReplyWakeAfterAllRowsAreDueResult(t, store, sink)
	if err == nil || !strings.Contains(err.Error(), `unsupported source kind "bogus_kind"`) {
		t.Fatalf("drain error = %v, want malformed zulu row after owner delivery", err)
	}
	if wake.promptCalls != 1 || len(wake.panes) != 1 || wake.panes[0] != "w1:p1" {
		t.Fatalf("wake calls=%d panes=%v prompts=%v, want only owner delivered", wake.promptCalls, wake.panes, wake.prompts)
	}
	delivered, err := store.ListWakeOutbox(ctx, db.WakeOutboxStateDelivered)
	if err != nil || len(delivered) != 1 || delivered[0].TargetRole != "owner" {
		t.Fatalf("delivered=%+v err=%v, want owner only", delivered, err)
	}
	pending, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil || len(pending) != 1 || pending[0].TargetRole != "zulu" {
		t.Fatalf("pending=%+v err=%v, want malformed zulu retained", pending, err)
	}
}

func TestReplyWakeOutboxDrainFailureDoesNotAbortRepoWork(t *testing.T) {
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "unused", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-after-unhealthy-wake", Agent: "audit", Action: "ask",
		Repo: "owner/repo", Branch: "main", PullRequest: 1,
	})
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release/drain-isolation", Author: "worker", Body: "matching route without a delivery sink",
		AddressedTarget: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "reply-owner", OnKind: "reply", WakeRole: "owner", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	worker := poolSchedulerWorker(t, store, &cliWorkerFakeAdapter{output: poolSchedulerAskResult}, false)
	if err := runEnabledRepoWorkerTicksTracked(
		context.Background(), store, worker, 1, "", &stdout, time.Now().UTC(), nil, nil,
	); err != nil {
		t.Fatalf("fleet tick aborted on reply wake drain failure: %v", err)
	}
	job, err := store.GetJob(context.Background(), "job-after-unhealthy-wake")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("repo job state = %q, want succeeded after unhealthy wake drain", job.State)
	}
	if !strings.Contains(stdout.String(), "reply wake outbox drain unhealthy:") {
		t.Fatalf("fleet tick log = %q, want unhealthy wake drain", stdout.String())
	}
}

func TestReplyWakeOutboxInertHealthDoesNotEscalateSingleRepoLoop(t *testing.T) {
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release/drain-streak", Author: "worker", Body: "persistently unroutable",
		AddressedTarget: "owner",
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := defaultJobWorker(store, io.Discard, t.TempDir())
	live := newDaemonReloadableConfig(30*time.Second, 1, false)
	tracker := newInflightJobTracker(ctx)
	var checkoutLock sync.Mutex
	var stdout syncBuffer
	errCh := startSingleRepoWorkerLoop(
		ctx, 100*time.Microsecond, store, worker, live, &checkoutLock, tracker,
		"owner/repo", "", &stdout,
	)

	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(stdout.String(), "reply wake outbox drain health:") <= maxConsecutiveWorkerTickFailures {
		select {
		case err := <-errCh:
			t.Fatalf("single-repo loop escalated persistent inert wake health: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("single-repo loop did not report inert health beyond %d ticks; log=%q", maxConsecutiveWorkerTickFailures, stdout.String())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-errCh:
		t.Fatalf("single-repo loop exited while reporting inert health: %v", err)
	default:
	}

	cancel()
	select {
	case err, ok := <-errCh:
		if ok && err != nil {
			t.Fatalf("single-repo loop shutdown surfaced error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("single-repo loop did not stop after cancellation")
	}
	if strings.Contains(stdout.String(), "consecutive failures, escalating") {
		t.Fatalf("inert reply wake health reached escalation ladder: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "reply wake outbox drain unhealthy:") ||
		!strings.Contains(stdout.String(), "pending=0 inert=1 route_removed=0 aged_attempted=0") {
		t.Fatalf("inert reply wake health log = %q", stdout.String())
	}
}

func TestReplyWakeOutboxBurstCoalescesToExactlyOneWake(t *testing.T) {
	store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	ctx := context.Background()
	// Duplicate matching rules must not inject duplicate prompts into the same
	// serial role pane for one coalesced window.
	if err := store.AddEventRule(ctx, db.EventRule{
		ID: "reply-duplicate", OnKind: "reply", WakeRole: "owner", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	var oldestID int64
	for index := 0; index < 10; index++ {
		note, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: "release/burst", Author: "worker",
			Body: fmt.Sprintf("addressed item %d", index+1), AddressedTarget: "owner",
		})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			oldestID = note.ID
		}
	}
	drainReplyWakeAfterAllRowsAreDue(t, store, sink)

	if wake.promptCalls != 1 {
		t.Fatalf("wake calls = %d, want exactly one; prompts=%q", wake.promptCalls, wake.prompts)
	}
	want := fmt.Sprintf("10 new items, oldest id %d", oldestID)
	if !strings.Contains(wake.prompt, want) {
		t.Fatalf("wake prompt = %q, want %q", wake.prompt, want)
	}
	delivered, err := store.ListWakeOutbox(ctx, db.WakeOutboxStateDelivered)
	if err != nil || len(delivered) != 10 {
		t.Fatalf("delivered rows = %+v, err=%v", delivered, err)
	}
}

func TestReplyWakeOutboxDeliversAddressedChatMessage(t *testing.T) {
	store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	ctx := context.Background()
	thread, err := store.CreateChatThread(ctx, db.ChatThread{Slug: "wake-chat", Repo: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := store.AddChatMessage(ctx, db.ChatMessage{
		ThreadID: thread.ID, AuthorName: "human", Kind: db.ChatKindChat,
		Body: "@owner please inspect", Mentions: []string{"owner"},
	})
	if err != nil {
		t.Fatal(err)
	}

	drainReplyWakeAfterAllRowsAreDue(t, store, sink)

	if wake.promptCalls != 1 || !strings.Contains(wake.prompt, "1 new items, oldest id "+message.ID) {
		t.Fatalf("chat wake = calls=%d prompt=%q", wake.promptCalls, wake.prompt)
	}
	delivered, err := store.ListWakeOutbox(ctx, db.WakeOutboxStateDelivered)
	if err != nil || len(delivered) != 1 || delivered[0].SourceKind != db.WakeOutboxSourceChatMessage {
		t.Fatalf("delivered chat rows = %+v, err=%v", delivered, err)
	}
}

func TestReplyWakeOutboxKeepsRolesSeparate(t *testing.T) {
	store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{
		{"owner", "w1:p1"},
		{"reviewer", "w1:p2"},
	})
	ctx := context.Background()
	for index := 0; index < 3; index++ {
		for _, role := range []string{"owner", "reviewer"} {
			if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
				WorkflowID: "release/roles", Author: "worker",
				Body: role, AddressedTarget: role,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	drainReplyWakeAfterAllRowsAreDue(t, store, sink)
	if wake.promptCalls != 2 {
		t.Fatalf("wake calls = %d, want one per role; panes=%v prompts=%v", wake.promptCalls, wake.panes, wake.prompts)
	}
	gotPane := map[string]bool{}
	for _, pane := range wake.panes {
		gotPane[pane] = true
	}
	if !gotPane["w1:p1"] || !gotPane["w1:p2"] {
		t.Fatalf("wake panes = %v, want both role panes", wake.panes)
	}
	for _, prompt := range wake.prompts {
		if !strings.Contains(prompt, "3 new items, oldest id ") {
			t.Fatalf("cross-contaminated prompt = %q", prompt)
		}
	}
}

func TestReplyWakeOutboxStartsNewBatchAfterWindow(t *testing.T) {
	store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	ctx := context.Background()
	first, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/windows", Author: "worker", Body: "first", AddressedTarget: "owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/windows", Author: "worker", Body: "second", AddressedTarget: "owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	setWakeOutboxCreatedAt(t, store.DatabasePath(), fmt.Sprint(first.ID), base)
	setWakeOutboxCreatedAt(t, store.DatabasePath(), fmt.Sprint(second.ID), base.Add(replyWakeCoalescingWindow))

	if err := drainReplyWakeOutbox(ctx, store, base.Add(2*replyWakeCoalescingWindow+time.Second), replyWakeTestDeliveryResolver(sink)); err != nil {
		t.Fatal(err)
	}
	if wake.promptCalls != 2 {
		t.Fatalf("wake calls = %d, want two rolling windows; prompts=%v", wake.promptCalls, wake.prompts)
	}
	for _, prompt := range wake.prompts {
		if !strings.Contains(prompt, "1 new items, oldest id ") {
			t.Fatalf("window prompt = %q", prompt)
		}
	}
}

func TestReplyWakeOutboxFleetDrainRunsWithZeroEnabledRepos(t *testing.T) {
	store, sink, wake, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	ctx := context.Background()
	for index := 0; index < 4; index++ {
		if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: "release/tail", Author: "worker", Body: fmt.Sprint(index),
			AddressedTarget: "owner",
		}); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil {
		t.Fatal(err)
	}
	oldestAt, err := time.Parse(time.RFC3339Nano, pending[0].CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	err = drainReplyWakeOutbox(ctx, store, oldestAt.Add(replyWakeCoalescingWindow-time.Millisecond), replyWakeTestDeliveryResolver(sink))
	if err == nil || !strings.Contains(err.Error(), "outstanding obligations: pending=4") {
		t.Fatalf("pre-window drain health = %v, want four pending obligations", err)
	}
	if wake.promptCalls != 0 {
		t.Fatalf("tail woke before window closed: %v", wake.prompts)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	installReplyWakeProductionSink(t, worker, sink.sink)
	repos, err := store.ListRepos(ctx)
	if err != nil || len(repos) != 0 {
		t.Fatalf("repos = %+v, err=%v; test requires a zero-repo fleet", repos, err)
	}
	if err := runEnabledRepoWorkerTicksTracked(
		ctx, store, worker, 0, "", io.Discard,
		oldestAt.Add(replyWakeCoalescingWindow), nil, nil,
	); err != nil {
		t.Fatalf("fleet daemon tick: %v", err)
	}
	if wake.promptCalls != 1 || !strings.Contains(wake.prompt, "4 new items, oldest id ") {
		t.Fatalf("tail wake = calls=%d prompt=%q", wake.promptCalls, wake.prompt)
	}
}

func TestReplyWakeOutboxRecordsExistingDeliveryOutcomeStates(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeEventWake)
		wantState string
		wantErr   bool
	}{
		{name: "stalled", configure: func(w *fakeEventWake) { w.stalled = true }, wantState: db.WakeOutboxStateStalled},
		{name: "transport failure", configure: func(w *fakeEventWake) { w.promptErr = errors.New("transport down") }, wantState: db.WakeOutboxStateFailed, wantErr: true},
		{name: "odd non delivery", configure: func(w *fakeEventWake) { w.oddNonDelivery = true }, wantState: db.WakeOutboxStateFailed, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, sink, wake, _ := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
			test.configure(wake)
			if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
				WorkflowID: "release/outcome", Author: "worker", Body: test.name,
				AddressedTarget: "owner",
			}); err != nil {
				t.Fatal(err)
			}
			drainErr := drainReplyWakeAfterAllRowsAreDueResult(t, store, sink)
			if (drainErr != nil) != test.wantErr {
				t.Fatalf("drain error = %v, wantErr=%v", drainErr, test.wantErr)
			}
			rows, err := store.ListWakeOutbox(context.Background(), test.wantState)
			if err != nil || len(rows) != 1 || rows[0].AttemptCount != 1 || rows[0].FinishedAt == "" {
				t.Fatalf("%s rows = %+v, err=%v", test.wantState, rows, err)
			}
		})
	}
}

func TestReplyWakeOutboxReadFailureLogsUnhealthyWithoutAbortingFleetTick(t *testing.T) {
	store, _, _, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release/read-failure", Author: "worker", Body: "must stay visible",
		AddressedTarget: "owner",
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`DROP TABLE wake_outbox`); err != nil {
		t.Fatal(err)
	}

	worker := defaultJobWorker(store, io.Discard, home)
	var stdout bytes.Buffer
	err = runEnabledRepoWorkerTicksTracked(
		context.Background(), store, worker, 0, "", &stdout,
		time.Now().UTC(), nil, nil,
	)
	if err != nil {
		t.Fatalf("daemon tick aborted after the wake outbox became unreadable: %v", err)
	}
	if !strings.Contains(stdout.String(), "reply wake outbox drain unhealthy:") ||
		!strings.Contains(stdout.String(), "no such table: wake_outbox") {
		t.Fatalf("daemon tick log = %q, want explicit unreadable wake outbox cause", stdout.String())
	}
}

func TestReplyWakeOutboxHealthFailsClosedWhenEventRulesAreUnreadable(t *testing.T) {
	store, _, _, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release/rules-read-failure", Author: "worker", Body: "must stay pending",
		AddressedTarget: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	pendingBefore, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(pendingBefore) != 1 {
		t.Fatalf("pending before event_rules failure = %+v, err=%v", pendingBefore, err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, pendingBefore[0].CreatedAt)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`DROP TABLE event_rules`); err != nil {
		t.Fatal(err)
	}

	worker := defaultJobWorker(store, io.Discard, home)
	var stdout bytes.Buffer
	tickErr := runEnabledRepoWorkerTicksTracked(
		context.Background(), store, worker, 0, "", &stdout,
		createdAt.Add(replyWakeCoalescingWindow-time.Millisecond), nil, nil,
	)
	pending, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending rows after event_rules read failure = %+v, err=%v", pending, err)
	}
	if tickErr != nil {
		t.Fatalf("daemon tick aborted after sink resolution skipped a pending wake: %v", tickErr)
	}
	if !strings.Contains(stdout.String(), "classify wake outbox obligations") ||
		!strings.Contains(stdout.String(), "no such table: event_rules") ||
		strings.Contains(stdout.String(), "inert=") {
		t.Fatalf("daemon tick log = %q, want explicit event_rules read cause", stdout.String())
	}
}

func TestReplyWakeOutboxZeroRulesReportsInertWithoutUnhealthy(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release/zero-rules", Author: "worker", Body: "must stay pending",
		AddressedTarget: "owner",
	}); err != nil {
		t.Fatal(err)
	}

	worker := defaultJobWorker(store, io.Discard, home)
	var stdout bytes.Buffer
	tickErr := runEnabledRepoWorkerTicksTracked(
		context.Background(), store, worker, 0, "", &stdout,
		time.Now().UTC(), nil, nil,
	)
	if tickErr != nil {
		t.Fatalf("fleet tick aborted on inert pending-obligation health: %v", tickErr)
	}
	if !strings.Contains(stdout.String(), "reply wake outbox drain health: pending=0 inert=1 route_removed=0 aged_attempted=0") ||
		strings.Contains(stdout.String(), "reply wake outbox drain unhealthy:") {
		t.Fatalf("fleet tick log = %q, want visible inert-only health", stdout.String())
	}
	pending, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending rows after zero-rule refusal = %+v, err=%v", pending, err)
	}
}

func TestReplyWakeOutboxUnrelatedEnabledRuleReportsPendingObligationInert(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "unrelated-blocked", OnKind: "blocked", WakeRole: "owner", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release/unrelated-rule", Author: "worker", Body: "must remain visible",
		AddressedTarget: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, err=%v", pending, err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, pending[0].CreatedAt)
	if err != nil {
		t.Fatal(err)
	}

	worker := defaultJobWorker(store, io.Discard, home)
	var stdout bytes.Buffer
	tickErr := runEnabledRepoWorkerTicksTracked(
		context.Background(), store, worker, 0, "", &stdout,
		createdAt.Add(replyWakeCoalescingWindow+time.Second), nil, nil,
	)
	if tickErr != nil {
		t.Fatalf("fleet tick aborted on inert pending-obligation health: %v", tickErr)
	}
	if !strings.Contains(stdout.String(), "reply wake outbox drain health: pending=0 inert=1 route_removed=0 aged_attempted=0") ||
		strings.Contains(stdout.String(), "reply wake outbox drain unhealthy:") {
		t.Fatalf("fleet tick log = %q, want visible inert-only health", stdout.String())
	}
	pending, err = store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(pending) != 1 || pending[0].AttemptCount != 0 {
		t.Fatalf("pending after unrelated rule = %+v, err=%v", pending, err)
	}
}

func TestReplyWakeOutboxAgedAttemptedExpiresDeliveryUnknownWithoutDuplicateWake(t *testing.T) {
	store, sink, wake, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release/crash-recovery", Author: "worker", Body: "crash residue",
		AddressedTarget: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, err=%v", pending, err)
	}
	attemptedAt := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	claimed, err := store.ClaimWakeOutbox(context.Background(), []int64{pending[0].ID}, attemptedAt)
	if err != nil || !claimed {
		t.Fatalf("simulate pre-crash claim = %v, err=%v", claimed, err)
	}

	worker := defaultJobWorker(store, io.Discard, home)
	installReplyWakeProductionSink(t, worker, sink.sink)
	recoveryAt := attemptedAt.Add(replyWakeAttemptedUnknownAfter)
	var stdout bytes.Buffer
	tickErr := runEnabledRepoWorkerTicksTracked(
		context.Background(), store, worker, 0, "", &stdout,
		recoveryAt, nil, nil,
	)
	if tickErr != nil {
		t.Fatalf("crash-recovery tick aborted on delivery-unknown health failure: %v", tickErr)
	}
	if !strings.Contains(stdout.String(), "delivery unknown") {
		t.Fatalf("crash-recovery tick log = %q, want delivery-unknown health failure", stdout.String())
	}
	if wake.promptCalls != 0 {
		t.Fatalf("aged attempted row was blindly re-emitted: calls=%d prompts=%v", wake.promptCalls, wake.prompts)
	}
	unknown, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStateDeliveryUnknown)
	if err != nil || len(unknown) != 1 || unknown[0].ID != pending[0].ID {
		t.Fatalf("delivery unknown rows = %+v, err=%v", unknown, err)
	}
	events, err := store.ListJobEvents(context.Background(), fmt.Sprintf("wake-outbox:%d", pending[0].ID))
	if err != nil || len(events) != 1 || events[0].Kind != db.WakeOutboxDeliveryUnknownEventKind {
		t.Fatalf("delivery unknown events = %+v, err=%v", events, err)
	}

	if err := runEnabledRepoWorkerTicksTracked(
		context.Background(), store, worker, 0, "", io.Discard,
		recoveryAt.Add(time.Second), nil, nil,
	); err != nil {
		t.Fatalf("post-resolution fleet tick = %v", err)
	}
	if wake.promptCalls != 0 {
		t.Fatalf("resolved delivery-unknown row emitted on later tick: calls=%d", wake.promptCalls)
	}
	events, err = store.ListJobEvents(context.Background(), fmt.Sprintf("wake-outbox:%d", pending[0].ID))
	if err != nil || len(events) != 1 {
		t.Fatalf("delivery unknown event duplicated: %+v, err=%v", events, err)
	}
}

func TestReplyWakeOutboxRuleDeletedMidDrainRefusesLaterBatch(t *testing.T) {
	store, sink, wake, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	ctx := context.Background()
	first, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/rule-generation", Author: "worker", Body: "first",
		AddressedTarget: "owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: "release/rule-generation", Author: "worker", Body: "second",
		AddressedTarget: "owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	setWakeOutboxCreatedAt(t, store.DatabasePath(), fmt.Sprint(first.ID), base)
	setWakeOutboxCreatedAt(t, store.DatabasePath(), fmt.Sprint(second.ID), base.Add(replyWakeCoalescingWindow))
	wake.onPrompt = func() error {
		return store.DeleteEventRule(ctx, "reply-0")
	}

	worker := defaultJobWorker(store, io.Discard, home)
	installReplyWakeProductionSink(t, worker, sink.sink)
	var stdout bytes.Buffer
	tickErr := runEnabledRepoWorkerTicksTracked(
		ctx, store, worker, 0, "", &stdout,
		base.Add(2*replyWakeCoalescingWindow+time.Second), nil, nil,
	)
	if tickErr != nil {
		t.Fatalf("fleet tick aborted on later batch refusal: %v", tickErr)
	}
	if !strings.Contains(stdout.String(), "reply wake outbox drain unhealthy:") ||
		!strings.Contains(stdout.String(), "pending=0 inert=0 route_removed=1 aged_attempted=0") {
		t.Fatalf("fleet tick log = %q, want later batch refusal", stdout.String())
	}
	if wake.promptCalls != 1 {
		t.Fatalf("wake calls = %d, want only the authorized first batch; prompts=%v", wake.promptCalls, wake.prompts)
	}
	delivered, err := store.ListWakeOutbox(ctx, db.WakeOutboxStateDelivered)
	if err != nil || len(delivered) != 1 || delivered[0].SourceID != fmt.Sprint(first.ID) {
		t.Fatalf("delivered = %+v, err=%v", delivered, err)
	}
	pending, err := store.ListWakeOutbox(ctx, db.WakeOutboxStatePending)
	if err != nil || len(pending) != 1 || pending[0].SourceID != fmt.Sprint(second.ID) || pending[0].AttemptCount != 0 {
		t.Fatalf("pending later batch = %+v, err=%v", pending, err)
	}

	stdout.Reset()
	tickErr = runEnabledRepoWorkerTicksTracked(
		ctx, store, worker, 0, "", &stdout,
		base.Add(2*replyWakeCoalescingWindow+2*time.Second), nil, nil,
	)
	if tickErr != nil {
		t.Fatalf("second fleet tick aborted on durable route removal: %v", tickErr)
	}
	if !strings.Contains(stdout.String(), "reply wake outbox drain unhealthy:") ||
		!strings.Contains(stdout.String(), "pending=0 inert=0 route_removed=1 aged_attempted=0") {
		t.Fatalf("second fleet tick log = %q, want durable later-batch refusal", stdout.String())
	}
	if wake.promptCalls != 1 {
		t.Fatalf("second tick re-emitted refused batch: calls=%d prompts=%v", wake.promptCalls, wake.prompts)
	}
}

func TestReplyWakeOutboxFinishFailureFailsFleetTickAfterClaimWithoutSinkOverride(t *testing.T) {
	store, sink, _, home := replyWakeTestHarness(t, []replyWakeTestRole{{"owner", "w1:p1"}})
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release/finish-failure", Author: "worker", Body: "must surface finish failure",
		AddressedTarget: "owner",
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending rows = %+v, err=%v", pending, err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, pending[0].CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`
CREATE TRIGGER reject_wake_outbox_finish
BEFORE UPDATE OF state ON wake_outbox
WHEN OLD.state = 'attempted' AND NEW.state = 'delivered'
BEGIN
	SELECT RAISE(ABORT, 'forced wake outbox finish failure');
END`); err != nil {
		t.Fatal(err)
	}

	worker := defaultJobWorker(store, io.Discard, home)
	installReplyWakeProductionSink(t, worker, sink.sink)
	var stdout bytes.Buffer
	tickErr := runEnabledRepoWorkerTicksTracked(
		context.Background(), store, worker, 0, "", &stdout,
		createdAt.Add(replyWakeCoalescingWindow+time.Second), nil, nil,
	)
	if tickErr != nil {
		t.Fatalf("fleet tick aborted on post-claim finish failure: %v", tickErr)
	}
	if !strings.Contains(stdout.String(), "finish wake outbox as delivered") ||
		!strings.Contains(stdout.String(), "forced wake outbox finish failure") {
		t.Fatalf("fleet tick log = %q, want surfaced post-claim finish failure", stdout.String())
	}
	attempted, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStateAttempted)
	if err != nil || len(attempted) != 1 {
		t.Fatalf("attempted rows after finish failure = %+v, err=%v", attempted, err)
	}
}

const replyWakeProducerDatabaseEnv = "GITMOOT_TEST_REPLY_WAKE_PRODUCER_DATABASE"

func TestReplyWakeOutboxSurvivesProducerProcessExitAndDrainsOnLaterDaemonTick(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddEventRule(context.Background(), db.EventRule{
		ID: "reply-process-exit", OnKind: "reply", WakeRole: "owner", Enabled: true,
	}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	producer := exec.Command(executable, "-test.run", "^TestReplyWakeOutboxProducerProcess$")
	producer.Env = append(os.Environ(), replyWakeProducerDatabaseEnv+"="+paths.Database)
	if output, err := producer.CombinedOutput(); err != nil {
		t.Fatalf("producer process: %v\n%s", err, output)
	}

	store, err = dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pending, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending after producer exit = %+v, err=%v", pending, err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, pending[0].CreatedAt)
	if err != nil {
		t.Fatal(err)
	}

	wake := &blockingReplyWake{started: make(chan struct{}), release: make(chan struct{})}
	worker := defaultJobWorker(store, io.Discard, home)
	installReplyWakeProductionSink(t, worker, &eventRuleSink{wake: wake})
	tickDone := make(chan error, 1)
	go func() {
		tickDone <- runEnabledRepoWorkerTicksTracked(
			context.Background(), store, worker, 0, "", io.Discard,
			createdAt.Add(replyWakeCoalescingWindow+time.Second), nil, nil,
		)
	}()

	select {
	case <-wake.started:
	case <-time.After(10 * time.Second):
		t.Fatal("later daemon tick did not begin durable wake delivery")
	}
	select {
	case err := <-tickDone:
		t.Fatalf("daemon tick returned before durable delivery completed: %v", err)
	case <-time.After(2 * time.Second):
	}
	close(wake.release)
	select {
	case err := <-tickDone:
		if err != nil {
			t.Fatalf("later daemon tick: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("later daemon tick did not finish after delivery")
	}

	delivered, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStateDelivered)
	if err != nil || len(delivered) != 1 || delivered[0].SourceID != pending[0].SourceID {
		t.Fatalf("delivered after producer exit = %+v, err=%v", delivered, err)
	}
}

func TestReplyWakeOutboxProducerProcess(t *testing.T) {
	databasePath := strings.TrimSpace(os.Getenv(replyWakeProducerDatabaseEnv))
	if databasePath == "" {
		return
	}
	store, err := dbtest.Open(t, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
		WorkflowID: "release/process-exit", Author: "worker", Body: "survive exit",
		AddressedTarget: "owner",
	}); err != nil {
		t.Fatal(err)
	}
}

type blockingReplyWake struct {
	started chan struct{}
	release chan struct{}
}

func (w *blockingReplyWake) Available(context.Context) bool {
	return true
}

func (w *blockingReplyWake) AgentPrompt(ctx context.Context, _, _, _ string) (bool, bool, error) {
	close(w.started)
	select {
	case <-w.release:
		return true, false, nil
	case <-ctx.Done():
		return false, false, ctx.Err()
	}
}

func (w *blockingReplyWake) ResolvePaneByLabel(_ context.Context, label string) (string, bool) {
	return label, true
}

type replyWakeTestRole struct {
	name string
	pane string
}

func replyWakeTestHarness(t *testing.T, roles []replyWakeTestRole) (*db.Store, synchronousEventRuleTestSink, *fakeEventWake, string) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for index, role := range roles {
		fmt.Fprintf(&body, "[org.roles.%q]\nscope=[\"*\"]\npane=%q\n", role.name, role.pane)
		if index > 0 {
			fmt.Fprintf(&body, "parent=%q\n", roles[0].name)
		}
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for index, role := range roles {
		if err := store.AddEventRule(context.Background(), db.EventRule{
			ID: fmt.Sprintf("reply-%d", index), OnKind: "reply", WakeRole: role.name, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	wake := &fakeEventWake{}
	ruleSink := &eventRuleSink{store: store, home: home, wake: wake}
	return store, synchronousEventRuleTestSink{sink: ruleSink}, wake, home
}

func drainReplyWakeAfterAllRowsAreDue(t *testing.T, store *db.Store, sink synchronousEventRuleTestSink) {
	t.Helper()
	if err := drainReplyWakeAfterAllRowsAreDueResult(t, store, sink); err != nil {
		t.Fatal(err)
	}
}

func drainReplyWakeAfterAllRowsAreDueResult(t *testing.T, store *db.Store, sink synchronousEventRuleTestSink) error {
	t.Helper()
	pending, err := store.ListWakeOutbox(context.Background(), db.WakeOutboxStatePending)
	if err != nil || len(pending) == 0 {
		t.Fatalf("pending rows = %+v, err=%v", pending, err)
	}
	latest := time.Time{}
	for _, entry := range pending {
		createdAt, err := time.Parse(time.RFC3339Nano, entry.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}
		if createdAt.After(latest) {
			latest = createdAt
		}
	}
	return drainReplyWakeOutbox(context.Background(), store, latest.Add(replyWakeCoalescingWindow+time.Second), replyWakeTestDeliveryResolver(sink))
}

func replyWakeTestDeliveryResolver(sink synchronousEventRuleTestSink) replyWakeDeliveryResolver {
	return func(ctx context.Context) (replyWakeDelivery, error) {
		rules, err := sink.sink.store.ListEventRules(ctx)
		if err != nil {
			return replyWakeDelivery{}, err
		}
		return replyWakeDelivery{sink: sink, rules: rules}, nil
	}
}

func installReplyWakeProductionSink(t *testing.T, worker jobWorker, sink *eventRuleSink) {
	t.Helper()
	home := worker.workflowHome()
	key := home + "\x00" + worker.Store.DatabasePath()
	sink.store = worker.Store
	sink.home = home
	eventSinkCache.Lock()
	oldSink, hadSink := eventSinkCache.rules[key]
	eventSinkCache.rules[key] = sink
	eventSinkCache.Unlock()
	t.Cleanup(func() {
		eventSinkCache.Lock()
		if hadSink {
			eventSinkCache.rules[key] = oldSink
		} else {
			delete(eventSinkCache.rules, key)
		}
		delete(eventSinkCache.webhooks, home)
		eventSinkCache.Unlock()
	})
}

func setWakeOutboxCreatedAt(t *testing.T, databasePath, sourceID string, at time.Time) {
	t.Helper()
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	stamp := at.UTC().Format(db.BlockedEpisodeTimeLayout)
	if _, err := raw.Exec(`UPDATE wake_outbox SET created_at = ?, updated_at = ? WHERE source_kind = ? AND source_id = ?`,
		stamp, stamp, db.WakeOutboxSourceWorkflowNote, sourceID); err != nil {
		t.Fatal(err)
	}
}
