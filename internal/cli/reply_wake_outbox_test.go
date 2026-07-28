package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

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

	if err := drainReplyWakeOutbox(ctx, store, sink, base.Add(2*replyWakeCoalescingWindow+time.Second)); err != nil {
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

func TestReplyWakeOutboxTailFlushesWithoutAnotherNote(t *testing.T) {
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
	if err := drainReplyWakeOutbox(ctx, store, sink, oldestAt.Add(replyWakeCoalescingWindow-time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if wake.promptCalls != 0 {
		t.Fatalf("tail woke before window closed: %v", wake.prompts)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	worker.EventSinkOverride = sink
	if err := runDaemonWorkerTickTracked(
		ctx, store, worker, 0, false, "", "", io.Discard,
		oldestAt.Add(replyWakeCoalescingWindow), nil, nil,
	); err != nil {
		t.Fatalf("daemon tick: %v", err)
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
	}{
		{name: "stalled", configure: func(w *fakeEventWake) { w.stalled = true }, wantState: db.WakeOutboxStateStalled},
		{name: "transport failure", configure: func(w *fakeEventWake) { w.promptErr = errors.New("transport down") }, wantState: db.WakeOutboxStateFailed},
		{name: "odd non delivery", configure: func(w *fakeEventWake) { w.oddNonDelivery = true }, wantState: db.WakeOutboxStateFailed},
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
			drainReplyWakeAfterAllRowsAreDue(t, store, sink)
			rows, err := store.ListWakeOutbox(context.Background(), test.wantState)
			if err != nil || len(rows) != 1 || rows[0].AttemptCount != 1 || rows[0].FinishedAt == "" {
				t.Fatalf("%s rows = %+v, err=%v", test.wantState, rows, err)
			}
		})
	}
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
	store, err := db.Open(paths.Database)
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
	if err := drainReplyWakeOutbox(context.Background(), store, sink, latest.Add(replyWakeCoalescingWindow+time.Second)); err != nil {
		t.Fatal(err)
	}
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
