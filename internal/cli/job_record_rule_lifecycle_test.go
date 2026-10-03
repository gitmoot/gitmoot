package cli

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/events"
)

func TestEventRuleProducerPersistsBeforeStoreCloses(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	store, err := dbtest.Open(t, path)
	if err != nil {
		t.Fatal(err)
	}
	wake := &fakeEventWake{}
	sink := &eventRuleSink{store: store, wake: wake}
	sink.Emit(ctx, events.Event{Type: events.EventJobFinished, JobID: "producer-job", WakeTargetRole: "owner", Detail: "Finished while recipient is offline"})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := dbtest.Open(t, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	messages, err := reopened.ListMessages(ctx, "owner", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	wakes, err := reopened.ListWakeOutbox(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(wakes) != 1 || wakes[0].State != db.WakeOutboxStatePending || wake.promptCalls != 0 {
		t.Fatalf("producer must save without typing: messages=%+v wakes=%+v prompts=%d", messages, wakes, wake.promptCalls)
	}
}

func TestGenuineRuleLookupFailureIsStillReported(t *testing.T) {
	store, err := dbtest.Open(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE event_rules`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	var logbuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logbuf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	(&eventRuleSink{store: store}).Emit(context.Background(), events.Event{Type: events.EventJobFinished, JobID: "broken-routing", WakeTargetRole: "owner"})
	if !strings.Contains(logbuf.String(), "org event rules list failed") {
		t.Fatalf("routing failure was hidden: %s", logbuf.String())
	}
}

func TestJobRecordPersistsNotificationBeforeReleasingStore(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	seedSessionAgentRepo(t, store)
	if err := store.AddEventRule(context.Background(), db.EventRule{ID: "watcher", OnKind: "job-terminal", WakeRole: "watcher-a", Scope: db.EventRuleScopeObserver, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "record", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "review", "--decision", "approved", "--summary", "durable producer result", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job record exit=%d: %s", code, stderr.String())
	}
	store, err = dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	inbox, err := store.ListMessages(context.Background(), "watcher-a", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 || inbox[0].Status != "queued" {
		t.Fatalf("job record returned without durable notification: %+v", inbox)
	}
}
