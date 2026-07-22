package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/org"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestBlockedSinceAdmissionIsOffByDefaultAndRequiresEnabledRule(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	if sink, err := enabledBlockedSinceEventSink(ctx, store, ""); err != nil || sink != nil {
		t.Fatalf("sink with zero rules = %T, err=%v; want nil", sink, err)
	}
	if err := store.AddEventRule(ctx, db.EventRule{ID: "disabled", OnKind: "blocked", WakeRole: "owner", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if sink, err := enabledBlockedSinceEventSink(ctx, store, ""); err != nil || sink != nil {
		t.Fatalf("sink with disabled rule = %T, err=%v; want nil", sink, err)
	}
	if err := store.AddEventRule(ctx, db.EventRule{ID: "enabled", OnKind: "blocked", WakeRole: "owner", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if sink, err := enabledBlockedSinceEventSink(ctx, store, ""); err != nil || sink == nil {
		t.Fatalf("sink with enabled rule = %T, err=%v; want non-nil", sink, err)
	}
}

func TestEvaluateBlockedTaskEpisodesEmitsOnceAndReopensAfterClear(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	repo := "owner/repo"
	if err := store.UpsertTask(ctx, db.Task{ID: "task-1", RepoFullName: repo, State: string(workflow.TaskBlocked)}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
	sink := &recordingSink{}

	if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, time.Hour, io.Discard, now); err != nil {
		t.Fatalf("evaluateBlockedTaskEpisodes(first) error = %v", err)
	}
	assertBlockedSinceTaskEvent(t, sink, 1)
	if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, time.Hour, io.Discard, now.Add(time.Minute)); err != nil {
		t.Fatalf("evaluateBlockedTaskEpisodes(second) error = %v", err)
	}
	assertBlockedSinceTaskEvent(t, sink, 1)

	changed, _, err := store.CompareAndSwapTaskState(ctx, "task-1", string(workflow.TaskBlocked), string(workflow.TaskMerged))
	if err != nil || !changed {
		t.Fatalf("unblock task: changed=%v err=%v", changed, err)
	}
	if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, time.Hour, io.Discard, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("evaluateBlockedTaskEpisodes(clear) error = %v", err)
	}
	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil || len(episodes) != 0 {
		t.Fatalf("episodes after unblock = %+v, err=%v", episodes, err)
	}

	changed, _, err = store.CompareAndSwapTaskState(ctx, "task-1", string(workflow.TaskMerged), string(workflow.TaskBlocked))
	if err != nil || !changed {
		t.Fatalf("re-block task: changed=%v err=%v", changed, err)
	}
	if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, time.Hour, io.Discard, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("evaluateBlockedTaskEpisodes(re-block) error = %v", err)
	}
	assertBlockedSinceTaskEvent(t, sink, 2)
}

func assertBlockedSinceTaskEvent(t *testing.T, sink *recordingSink, want int) {
	t.Helper()
	blocked := sink.byType(events.EventJobBlocked)
	if len(blocked) != want {
		t.Fatalf("job.blocked events = %d, want %d", len(blocked), want)
	}
	if want == 0 {
		return
	}
	ev := blocked[len(blocked)-1]
	if ev.Cause != "blocked_since" || ev.JobID != "task-1" || ev.RootID != "task-1" || ev.Repo != "owner/repo" || ev.Status != string(workflow.TaskBlocked) {
		t.Fatalf("event = %+v", ev)
	}
}

type fakeBlockedRoleAvailability struct {
	available bool
	calls     int
}

func (f *fakeBlockedRoleAvailability) Available(context.Context) bool {
	f.calls++
	return f.available
}

func TestRunBlockedRoleWakeOnceUsesInjectedProviderAndDedups(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`
[org]
enforce = "warn"
[org.roles."owner"]
scope = ["*"]
merge_rule = "owner"
[org.roles."review"]
parent = "owner"
scope = ["gitmoot/*"]
merge_rule = "self"
[orchestrate]
blocked_role_wake_after = "1h"
`); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	snapshot := org.Snapshot{
		States: map[string]org.RoleLiveState{
			"owner":  {State: org.StateBlocked},
			"review": {State: org.StateIdle},
		},
		ObservedAt: now.Add(-2 * time.Hour), ProviderVersion: "test-v1",
	}
	availability := &fakeBlockedRoleAvailability{available: true}
	sink := &recordingSink{}
	var providerRoles []string
	deps := blockedRoleWakeDependencies{
		availability: availability,
		provider: func(roles []string) org.Provider {
			providerRoles = append([]string(nil), roles...)
			return orgFixtureProvider{snapshot: snapshot}
		},
		eventSink: func(context.Context, *db.Store, string) (events.Sink, error) { return sink, nil },
	}

	if got := resolveBlockedRoleWakeAfter(home); got != time.Hour {
		t.Fatalf("resolveBlockedRoleWakeAfter() = %s, want 1h", got)
	}
	var output bytes.Buffer
	runBlockedRoleWakeOnce(context.Background(), store, home, &output, now, deps)
	blocked := sink.byType(events.EventJobBlocked)
	if len(blocked) != 1 {
		t.Fatalf("job.blocked events = %d, want 1; output=%s", len(blocked), output.String())
	}
	if len(providerRoles) != 2 || providerRoles[0] != "owner" || providerRoles[1] != "review" {
		t.Fatalf("provider roles = %v", providerRoles)
	}
	ev := blocked[0]
	if ev.Cause != "blocked_since" || ev.JobID != "org-blocked:owner" || ev.RootID != "org-blocked:owner" || ev.Repo != "" {
		t.Fatalf("event = %+v", ev)
	}
	runBlockedRoleWakeOnce(context.Background(), store, home, io.Discard, now.Add(time.Minute), deps)
	if got := len(sink.byType(events.EventJobBlocked)); got != 1 {
		t.Fatalf("duplicate job.blocked events = %d, want 1", got)
	}
	if availability.calls != 2 {
		t.Fatalf("availability calls = %d, want 2", availability.calls)
	}
}
