package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
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

func TestEvaluateBlockedTaskEpisodesDigestsDueTasksAndMarksEachEpisode(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	repo := "owner/repo"
	now := time.Now().UTC().Truncate(time.Second).Add(4 * time.Hour)
	wakeAfter := time.Hour
	tasks := []struct {
		id           string
		blockedSince time.Time
	}{
		{id: "task-newest", blockedSince: now.Add(-2 * time.Hour)},
		{id: "task-oldest", blockedSince: now.Add(-4 * time.Hour)},
		{id: "task-middle", blockedSince: now.Add(-3 * time.Hour)},
	}
	for _, task := range tasks {
		if err := store.UpsertTask(ctx, db.Task{ID: task.id, RepoFullName: repo, State: string(workflow.TaskBlocked)}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertBlockedEpisode(ctx, taskEpisodeSubject(repo, task.id), task.blockedSince, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	sink := &recordingSink{}
	if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, wakeAfter, io.Discard, now); err != nil {
		t.Fatalf("evaluateBlockedTaskEpisodes() error = %v", err)
	}
	blocked := sink.byType(events.EventJobBlocked)
	if len(blocked) != 1 {
		t.Fatalf("job.blocked events = %d, want one digest", len(blocked))
	}
	ev := blocked[0]
	if ev.JobID != "task-oldest" || ev.RootID != "task-oldest" || ev.Repo != repo || ev.Cause != "blocked_since" {
		t.Fatalf("digest event = %+v", ev)
	}
	wantSince := tasks[1].blockedSince.UTC().Format(time.RFC3339) // task-oldest, the digest anchor
	for _, want := range []string{"3 tasks blocked", "oldest 4h0m0s", "(since " + wantSince + ")", "task-oldest", "(+2 more)"} {
		if !strings.Contains(ev.Detail, want) {
			t.Fatalf("digest detail %q missing %q", ev.Detail, want)
		}
	}

	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != len(tasks) {
		t.Fatalf("blocked episodes = %d, want %d", len(episodes), len(tasks))
	}
	wantEmittedAt := now.Format(db.BlockedEpisodeTimeLayout)
	for _, episode := range episodes {
		if episode.EmittedAt != wantEmittedAt {
			t.Errorf("episode %q emitted_at = %q, want %q", episode.Subject, episode.EmittedAt, wantEmittedAt)
		}
	}

	if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, wakeAfter, io.Discard, now.Add(time.Minute)); err != nil {
		t.Fatalf("evaluateBlockedTaskEpisodes(subsequent) error = %v", err)
	}
	if got := len(sink.byType(events.EventJobBlocked)); got != 1 {
		t.Fatalf("job.blocked events after subsequent pass = %d, want 1", got)
	}
}

func TestEvaluateBlockedTaskEpisodesSingleTaskUsesOneItemDigest(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	repo := "owner/repo"
	taskID := "task-only"
	now := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
	if err := store.UpsertTask(ctx, db.Task{ID: taskID, RepoFullName: repo, State: string(workflow.TaskBlocked)}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBlockedEpisode(ctx, taskEpisodeSubject(repo, taskID), now.Add(-2*time.Hour), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	sink := &recordingSink{}
	if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, time.Hour, io.Discard, now); err != nil {
		t.Fatalf("evaluateBlockedTaskEpisodes() error = %v", err)
	}
	blocked := sink.byType(events.EventJobBlocked)
	if len(blocked) != 1 {
		t.Fatalf("job.blocked events = %d, want 1", len(blocked))
	}
	wantSince := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	wantDetail := "1 tasks blocked — oldest 2h0m0s (since " + wantSince + ") — task-only"
	if ev := blocked[0]; ev.Detail != wantDetail || strings.Contains(ev.Detail, "(+") {
		t.Fatalf("one-item digest detail = %q, want %q", ev.Detail, wantDetail)
	}
}

func TestEvaluateBlockedTaskEpisodesCapsAlertsWithoutDisposingTask(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	repo := "owner/repo"
	taskID := "task-exhausted-alerts"
	wakeAfter := time.Hour
	firstSweep := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
	blockedSince := firstSweep.Add(-8 * time.Hour)
	if err := store.UpsertTask(ctx, db.Task{ID: taskID, RepoFullName: repo, State: string(workflow.TaskBlocked)}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBlockedEpisode(ctx, taskEpisodeSubject(repo, taskID), blockedSince, blockedSince); err != nil {
		t.Fatal(err)
	}

	sink := &recordingSink{}
	for pass := 0; pass < blockedTaskMaxNudges+1; pass++ {
		now := firstSweep.Add(time.Duration(pass) * (wakeAfter + time.Second))
		if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, wakeAfter, io.Discard, now); err != nil {
			t.Fatalf("evaluateBlockedTaskEpisodes(pass %d) error = %v", pass+1, err)
		}
	}
	if got := len(sink.byType(events.EventJobBlocked)); got != blockedTaskMaxNudges {
		t.Fatalf("job.blocked nudges = %d, want capped %d", got, blockedTaskMaxNudges)
	}
	escalations := sink.byType(events.EventJobNeedsAttention)
	if len(escalations) != 1 {
		t.Fatalf("terminal escalations = %d, want 1", len(escalations))
	}
	if ev := escalations[0]; ev.Cause != "escalation" || ev.JobID != taskID ||
		ev.RootID != taskID || !strings.Contains(ev.Detail, "alert ladder exhausted") ||
		!strings.Contains(ev.Detail, taskID) {
		t.Fatalf("terminal escalation = %+v", ev)
	}

	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil || len(episodes) != 1 {
		t.Fatalf("blocked episodes = %+v, err=%v", episodes, err)
	}
	if got := episodes[0].TaskEmitCount; got != blockedTaskMaxNudges {
		t.Fatalf("task_emit_count = %d, want %d", got, blockedTaskMaxNudges)
	}
	if episodes[0].TaskExhaustedAt == "" {
		t.Fatal("task_exhausted_at is empty; exhausted alert ladder is not queryable")
	}
	blocked, err := store.ListTasksByRepoState(ctx, repo, string(workflow.TaskBlocked))
	if err != nil || len(blocked) != 1 || blocked[0].ID != taskID {
		t.Fatalf("blocked tasks after alert exhaustion = %+v, err=%v", blocked, err)
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

func blockedRoleWakeTestStore(t *testing.T) (string, *db.Store) {
	t.Helper()
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
pane = "Gitmoot"
[org.roles."review"]
parent = "owner"
scope = ["gitmoot/*"]
merge_rule = "self"
pane = "Gitmoot Review"
[orchestrate]
blocked_role_wake_after = "1h"
`); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return home, store
}

func TestRunBlockedRoleWakeOnceUsesInjectedProviderAndDedups(t *testing.T) {
	home, store := blockedRoleWakeTestStore(t)

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
	var providerRoles []config.OrgRole
	deps := blockedRoleWakeDependencies{
		availability: availability,
		provider: func(roles []config.OrgRole) org.Provider {
			providerRoles = append([]config.OrgRole(nil), roles...)
			return orgFixtureProvider{snapshot: snapshot}
		},
		eventSink: func(context.Context, *db.Store, string) (events.Sink, error) { return sink, nil },
	}

	if got := resolveBlockedRoleWakeAfter(home); got != time.Hour {
		t.Fatalf("resolveBlockedRoleWakeAfter() = %s, want 1h", got)
	}
	var output bytes.Buffer
	runBlockedRoleWakeOnce(context.Background(), store, home, &output, now, deps)
	livePresence, err := store.ListRoleLivePresence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(livePresence) != 2 || livePresence[0].Role != "owner" || livePresence[0].State != string(org.StateBlocked) ||
		livePresence[1].Role != "review" || livePresence[1].State != string(org.StateIdle) {
		t.Fatalf("persisted live presence = %+v", livePresence)
	}
	blocked := sink.byType(events.EventJobBlocked)
	if len(blocked) != 1 {
		t.Fatalf("job.blocked events = %d, want 1; output=%s", len(blocked), output.String())
	}
	if len(providerRoles) != 2 ||
		providerRoles[0].Name != "owner" || providerRoles[0].Pane != "Gitmoot" ||
		providerRoles[1].Name != "review" || providerRoles[1].Pane != "Gitmoot Review" {
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

	snapshot.States = map[string]org.RoleLiveState{"owner": {State: org.StateWorking}}
	deps.provider = func([]config.OrgRole) org.Provider { return orgFixtureProvider{snapshot: snapshot} }
	runBlockedRoleWakeOnce(context.Background(), store, home, io.Discard, now.Add(2*time.Minute), deps)
	livePresence, err = store.ListRoleLivePresence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(livePresence) != 1 || livePresence[0].Role != "owner" || livePresence[0].State != string(org.StateWorking) {
		t.Fatalf("persisted live presence after reap = %+v", livePresence)
	}
}

func TestRunBlockedRoleWakeOnceDoesNotWakePausedSeat(t *testing.T) {
	home, store := blockedRoleWakeTestStore(t)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	snapshot := org.Snapshot{
		States: map[string]org.RoleLiveState{
			"owner":  {State: org.StateBlocked},
			"review": {State: org.StateIdle},
		},
		ObservedAt: now.Add(-2 * time.Hour), ProviderVersion: "test-v1",
	}
	sink := &recordingSink{}
	var providerRoles []config.OrgRole
	deps := blockedRoleWakeDependencies{
		availability: &fakeBlockedRoleAvailability{available: true},
		provider: func(roles []config.OrgRole) org.Provider {
			providerRoles = append([]config.OrgRole(nil), roles...)
			return orgFixtureProvider{snapshot: snapshot}
		},
		eventSink: func(context.Context, *db.Store, string) (events.Sink, error) { return sink, nil },
		roster: func(_ context.Context, _ *db.Store, cfg config.OrgConfig) orgRoster {
			if _, ok := cfg.Role("owner"); !ok {
				t.Fatal("test org role owner missing")
			}
			if _, ok := cfg.Role("review"); !ok {
				t.Fatal("test org role review missing")
			}
			// Rosters are constructed only through the resolver (round 3:
			// the fields are unexported outside internal/config).
			return resolveOrgRoster(cfg, nil, map[string]string{"owner": "test pause"})
		},
	}

	runBlockedRoleWakeOnce(context.Background(), store, home, io.Discard, now, deps)
	if got := len(sink.byType(events.EventJobBlocked)); got != 0 {
		t.Fatalf("paused owner job.blocked events = %d, want 0", got)
	}
	if len(providerRoles) != 2 || providerRoles[0].Name != "owner" || providerRoles[1].Name != "review" {
		t.Fatalf("provider roles = %v, want Members() including paused owner", providerRoles)
	}
	presence, err := store.ListRoleLivePresence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(presence) != 2 || presence[0].Role != "owner" || presence[0].State != string(org.StateBlocked) {
		t.Fatalf("persisted live presence = %+v, want paused owner retained", presence)
	}
}

// TestRunBlockedRoleWakeOncePersistsPresenceWithWakeDisabled guards #1118: org
// live-presence must populate even when blocked-role WAKING is off
// (blocked_role_wake_after=0, the default), i.e. presence is decoupled from wake.
func TestRunBlockedRoleWakeOncePersistsPresenceWithWakeDisabled(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// No blocked_role_wake_after override → defaults to 0 (waking disabled).
	if _, err := file.WriteString(`
[org]
enforce = "warn"
[org.roles."owner"]
scope = ["*"]
merge_rule = "owner"
pane = "Gitmoot"
`); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if got := resolveBlockedRoleWakeAfter(home); got != 0 {
		t.Fatalf("resolveBlockedRoleWakeAfter() = %s, want 0 (disabled)", got)
	}

	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	snapshot := org.Snapshot{
		States:     map[string]org.RoleLiveState{"owner": {State: org.StateWorking}},
		ObservedAt: now, ProviderVersion: "test-v1",
	}
	availability := &fakeBlockedRoleAvailability{available: true}
	sink := &recordingSink{}
	deps := blockedRoleWakeDependencies{
		availability: availability,
		provider:     func([]config.OrgRole) org.Provider { return orgFixtureProvider{snapshot: snapshot} },
		eventSink:    func(context.Context, *db.Store, string) (events.Sink, error) { return sink, nil },
	}

	var output bytes.Buffer
	runBlockedRoleWakeOnce(context.Background(), store, home, &output, now, deps)

	// Presence persists despite waking being disabled — the #1118 decoupling.
	livePresence, err := store.ListRoleLivePresence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(livePresence) != 1 || livePresence[0].Role != "owner" || livePresence[0].State != string(org.StateWorking) {
		t.Fatalf("presence not persisted with wake disabled: %+v (output=%s)", livePresence, output.String())
	}
	// Waking is disabled → the snapshot was still taken (availability probed) but
	// NO blocked-role evaluation ran, so no events were emitted.
	if availability.calls != 1 {
		t.Fatalf("availability calls = %d, want 1", availability.calls)
	}
	if got := len(sink.byType(events.EventJobBlocked)); got != 0 {
		t.Fatalf("blocked events with waking disabled = %d, want 0", got)
	}
}

// TestRunBlockedRoleWakeOnceSkipsSilentlyWithNoOrgRolesConfigured guards the
// regression a high review caught in #1118's first pass: a herdr-less/no-org
// deployment (the common OSS/automated-user case) must NEVER probe herdr or log
// on this once-a-minute loop — with zero org roles configured there is nothing
// to snapshot, so the availability check (and its diagnostic line) must not run.
func TestRunBlockedRoleWakeOnceSkipsSilentlyWithNoOrgRolesConfigured(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	availability := &fakeBlockedRoleAvailability{available: false}
	deps := blockedRoleWakeDependencies{
		availability: availability,
		provider: func([]config.OrgRole) org.Provider {
			t.Fatal("provider factory must not be called with zero org roles configured")
			return nil
		},
		eventSink: func(context.Context, *db.Store, string) (events.Sink, error) {
			t.Fatal("event sink must not be resolved with zero org roles configured")
			return nil, nil
		},
	}

	var output bytes.Buffer
	runBlockedRoleWakeOnce(context.Background(), store, home, &output, time.Now().UTC(), deps)

	if availability.calls != 0 {
		t.Fatalf("availability.Available calls = %d, want 0 (herdr must not be probed)", availability.calls)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q, want empty (no log spam for an unconfigured daemon)", output.String())
	}
}

// TestBlockedSinceReNudgesOncePerIntervalWhileStillBlocked pins the self-healing
// semantic: while a subject stays blocked it is re-nudged at most once per
// wakeAfter, not a single durable one-shot (so a dropped wake recovers).
func TestBlockedSinceReNudgesOncePerIntervalWhileStillBlocked(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	repo := "owner/repo"
	if err := store.UpsertTask(ctx, db.Task{ID: "task-nudge", RepoFullName: repo, State: string(workflow.TaskBlocked)}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
	wakeAfter := time.Hour
	sink := &recordingSink{}
	nudgeAt := func(at time.Time, want int) {
		t.Helper()
		if err := evaluateBlockedTaskEpisodes(ctx, store, sink, repo, wakeAfter, io.Discard, at); err != nil {
			t.Fatalf("evaluate at %s: %v", at, err)
		}
		if got := len(sink.byType(events.EventJobBlocked)); got != want {
			t.Fatalf("blocked events at %s = %d, want %d", at, got, want)
		}
	}
	nudgeAt(base, 1) // crosses threshold → emit #1
	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil || len(episodes) != 1 {
		t.Fatalf("episodes after first nudge = %+v, err=%v", episodes, err)
	}
	firstSince := episodes[0].BlockedSince
	nudgeAt(base.Add(30*time.Minute), 1)        // within interval → no re-emit
	nudgeAt(base.Add(wakeAfter+time.Minute), 2) // past interval, still blocked → re-nudge #2

	// The digest carries only the anchor item's first_since, not every item's, but
	// the durable per-task episode must retain that stable identity across re-nudges.
	episodes, err = store.ListBlockedEpisodes(ctx)
	if err != nil || len(episodes) != 1 || episodes[0].BlockedSince != firstSince {
		t.Fatalf("re-nudge changed episode identity: episodes=%+v first_since=%q err=%v", episodes, firstSince, err)
	}
	blocked := sink.byType(events.EventJobBlocked)
	for _, ev := range blocked {
		if !strings.Contains(ev.Detail, "1 tasks blocked — oldest") || strings.Contains(ev.Detail, "(+") {
			t.Fatalf("re-nudge event is not a one-item digest: %q", ev.Detail)
		}
	}
}

func TestEvaluateBlockedRoleEpisodesStillEmitsPerRole(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	sink := &recordingSink{}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	snapshot := org.Snapshot{
		States: map[string]org.RoleLiveState{
			"owner":  {State: org.StateBlocked},
			"review": {State: org.StateBlocked},
			"worker": {State: org.StateBlocked},
		},
		ObservedAt: now.Add(-2 * time.Hour),
	}
	if err := evaluateBlockedRoleEpisodes(ctx, store, sink, snapshot, time.Hour, io.Discard, now); err != nil {
		t.Fatalf("evaluateBlockedRoleEpisodes() error = %v", err)
	}
	blocked := sink.byType(events.EventJobBlocked)
	if len(blocked) != 3 {
		t.Fatalf("job.blocked role events = %d, want one per role", len(blocked))
	}
	for _, ev := range blocked {
		if ev.Cause != "blocked_since" || !strings.HasPrefix(ev.JobID, "org-blocked:") || !strings.HasPrefix(ev.Detail, "role ") {
			t.Fatalf("role event changed shape: %+v", ev)
		}
	}
}

// TestBlockedRoleEpisodeSurvivesTransientUnknownSnapshot pins the fix that a
// momentary StateUnknown (or absent) role observation must NOT clear the episode
// or reset its accrued blocked duration; only a definitive non-blocked state does.
func TestBlockedRoleEpisodeSurvivesTransientUnknownSnapshot(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	sink := &recordingSink{}
	wakeAfter := time.Hour
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	snap := func(state org.LifecycleState, observedAt time.Time) org.Snapshot {
		return org.Snapshot{States: map[string]org.RoleLiveState{"owner": {State: state}}, ObservedAt: observedAt}
	}
	// Blocked, first observed 2h ago → crosses threshold → emit #1, episode open.
	if err := evaluateBlockedRoleEpisodes(ctx, store, sink, snap(org.StateBlocked, base.Add(-2*time.Hour)), wakeAfter, io.Discard, base); err != nil {
		t.Fatal(err)
	}
	if got := len(sink.byType(events.EventJobBlocked)); got != 1 {
		t.Fatalf("emit after first blocked tick = %d, want 1", got)
	}
	// Transient StateUnknown → episode MUST survive.
	if err := evaluateBlockedRoleEpisodes(ctx, store, sink, snap(org.StateUnknown, base.Add(time.Minute)), wakeAfter, io.Discard, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 || episodes[0].Subject != "role:owner" {
		t.Fatalf("StateUnknown cleared/altered the episode: %+v", episodes)
	}
	// A definitive idle observation clears it.
	if err := evaluateBlockedRoleEpisodes(ctx, store, sink, snap(org.StateIdle, base.Add(2*time.Minute)), wakeAfter, io.Discard, base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	episodes, err = store.ListBlockedEpisodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 0 {
		t.Fatalf("StateIdle did not clear the episode: %+v", episodes)
	}
}

// TestBlockedRoleEpisodeReapedWhenStaleThenReblockStartsFresh pins the staleness
// reap: a role that goes permanently unknown/absent is NOT leaked forever (its
// row is reaped once it stops being re-observed blocked past the gap), and a
// later re-block under the same label starts a FRESH episode rather than reusing
// the stale blocked_since to fire a spurious inflated-duration wake.
func TestBlockedRoleEpisodeReapedWhenStaleThenReblockStartsFresh(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	sink := &recordingSink{}
	wakeAfter := time.Hour
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	blocked := func(observedAt time.Time) org.Snapshot {
		return org.Snapshot{States: map[string]org.RoleLiveState{"gone": {State: org.StateBlocked}}, ObservedAt: observedAt}
	}
	absent := func(observedAt time.Time) org.Snapshot {
		return org.Snapshot{States: map[string]org.RoleLiveState{}, ObservedAt: observedAt}
	}
	run := func(snap org.Snapshot, now time.Time) {
		t.Helper()
		if err := evaluateBlockedRoleEpisodes(ctx, store, sink, snap, wakeAfter, io.Discard, now); err != nil {
			t.Fatalf("evaluate at %s: %v", now, err)
		}
	}
	run(blocked(base.Add(-2*time.Hour)), base) // blocked 2h → emit #1, episode open
	if got := len(sink.byType(events.EventJobBlocked)); got != 1 {
		t.Fatalf("first emit = %d, want 1", got)
	}
	run(absent(base.Add(time.Minute)), base.Add(time.Minute)) // vanished, within gap → survives
	if eps, _ := store.ListBlockedEpisodes(ctx); len(eps) != 1 {
		t.Fatalf("episode reaped within stale gap: %+v", eps)
	}
	past := base.Add(blockedEpisodeStaleGap + time.Minute)
	run(absent(past), past) // past the gap → reaped, no leak
	if eps, _ := store.ListBlockedEpisodes(ctx); len(eps) != 0 {
		t.Fatalf("stale absent-role episode was not reaped (leak): %+v", eps)
	}
	reblock := past.Add(time.Minute)
	run(blocked(reblock), reblock) // fresh incarnation blocks → fresh episode, no spurious wake
	if got := len(sink.byType(events.EventJobBlocked)); got != 1 {
		t.Fatalf("re-block fired a spurious wake: emits = %d, want still 1", got)
	}
	eps, _ := store.ListBlockedEpisodes(ctx)
	if len(eps) != 1 {
		t.Fatalf("re-block episode set = %+v, want exactly 1", eps)
	}
	if got, want := eps[0].BlockedSince, reblock.UTC().Format(db.BlockedEpisodeTimeLayout); got != want {
		t.Fatalf("re-block blocked_since = %q, want fresh %q (not the stale 2h-old instant)", got, want)
	}
}

func TestInputPendingRoleEpisodeDueReNudgeClearAndStale(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	sink := &recordingSink{}
	wakeAfter := time.Hour
	base := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	snap := func(states map[string]org.RoleLiveState, observedAt time.Time) org.Snapshot {
		return org.Snapshot{States: states, ObservedAt: observedAt}
	}
	pending := func(observedAt time.Time) org.Snapshot {
		return snap(map[string]org.RoleLiveState{"owner": {State: org.StateInputPending}}, observedAt)
	}
	run := func(snapshot org.Snapshot, now time.Time) {
		t.Helper()
		if err := evaluateInputPendingRoleEpisodes(ctx, store, sink, snapshot, wakeAfter, io.Discard, now); err != nil {
			t.Fatalf("evaluate at %s: %v", now, err)
		}
	}

	run(pending(base.Add(-2*time.Hour)), base)
	if got := len(sink.byType(events.EventOrgInputPending)); got != 1 {
		t.Fatalf("first input-pending emit = %d, want 1", got)
	}
	episodes, err := store.ListBlockedEpisodes(ctx)
	if err != nil || len(episodes) != 1 || episodes[0].Subject != "input-pending:role:owner" {
		t.Fatalf("input-pending episode = %+v, err=%v", episodes, err)
	}
	firstSince := episodes[0].BlockedSince

	run(pending(base.Add(30*time.Minute)), base.Add(30*time.Minute))
	if got := len(sink.byType(events.EventOrgInputPending)); got != 1 {
		t.Fatalf("emit within repeat interval = %d, want 1", got)
	}
	run(pending(base.Add(wakeAfter+time.Minute)), base.Add(wakeAfter+time.Minute))
	if got := len(sink.byType(events.EventOrgInputPending)); got != 2 {
		t.Fatalf("emit after repeat interval = %d, want 2", got)
	}
	episodes, err = store.ListBlockedEpisodes(ctx)
	if err != nil || len(episodes) != 1 || episodes[0].BlockedSince != firstSince {
		t.Fatalf("re-nudge changed input-pending episode identity: %+v, err=%v", episodes, err)
	}
	for _, ev := range sink.byType(events.EventOrgInputPending) {
		if ev.Cause != "input_pending_since" || ev.JobID != "org-input-pending:owner" ||
			ev.RootID != ev.JobID || ev.Status != string(org.StateInputPending) ||
			!strings.Contains(ev.Detail, "role owner input pending") {
			t.Fatalf("input-pending event changed shape: %+v", ev)
		}
	}

	// Unknown is ambiguous and preserves the episode; a concrete state clears it.
	run(snap(map[string]org.RoleLiveState{"owner": {State: org.StateUnknown}}, base.Add(62*time.Minute)), base.Add(62*time.Minute))
	if episodes, _ = store.ListBlockedEpisodes(ctx); len(episodes) != 1 {
		t.Fatalf("unknown cleared input-pending episode: %+v", episodes)
	}
	run(snap(map[string]org.RoleLiveState{"owner": {State: org.StateWorking}}, base.Add(63*time.Minute)), base.Add(63*time.Minute))
	if episodes, _ = store.ListBlockedEpisodes(ctx); len(episodes) != 0 {
		t.Fatalf("working did not clear input-pending episode: %+v", episodes)
	}

	// A fresh episode that disappears stays through the grace, then reaps stale.
	fresh := base.Add(64 * time.Minute)
	run(pending(fresh), fresh)
	run(snap(map[string]org.RoleLiveState{}, fresh.Add(time.Minute)), fresh.Add(time.Minute))
	if episodes, _ = store.ListBlockedEpisodes(ctx); len(episodes) != 1 {
		t.Fatalf("absent input-pending episode reaped within grace: %+v", episodes)
	}
	past := fresh.Add(blockedEpisodeStaleGap + time.Minute)
	run(snap(map[string]org.RoleLiveState{}, past), past)
	if episodes, _ = store.ListBlockedEpisodes(ctx); len(episodes) != 0 {
		t.Fatalf("stale input-pending episode was not reaped: %+v", episodes)
	}
}

type synchronousEventRuleTestSink struct {
	sink *eventRuleSink
}

func (s synchronousEventRuleTestSink) Emit(ctx context.Context, event events.Event) {
	_ = s.sink.evaluate(ctx, event)
}

func (s synchronousEventRuleTestSink) emitWakeOutbox(ctx context.Context, event events.Event, rules []db.EventRule) error {
	return s.sink.emitWakeOutbox(ctx, event, rules)
}

func TestPaneInputPendingRuleObservationWakesAndTracksDelivery(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"]\nscope=[\"*\"]\npane=\"w1:p1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runOrg([]string{"events", "rule", "add", "--home", home, "--on", "pane_input_pending", "--wake", "owner"}, &stdout, &stderr); code != 0 {
		t.Fatalf("add pane_input_pending rule code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}

	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wake := &fakeEventWake{stalled: true}
	ruleSink := &eventRuleSink{store: store, home: home, wake: wake}
	sink := synchronousEventRuleTestSink{sink: ruleSink}
	wakeAfter := time.Hour
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	snapshot := org.Snapshot{
		States:     map[string]org.RoleLiveState{"owner": {State: org.StateInputPending}},
		ObservedAt: now.Add(-2 * time.Hour),
	}
	if err := evaluateInputPendingRoleEpisodes(context.Background(), store, sink, snapshot, wakeAfter, io.Discard, now); err != nil {
		t.Fatal(err)
	}
	if wake.promptCalls != 1 || wake.pane != "w1:p1" || !strings.Contains(wake.prompt, "pane_input_pending event for job org-input-pending:owner") {
		t.Fatalf("input-pending wake = %+v", wake)
	}
	misses, err := store.ListRoleMissedWakes(context.Background())
	if err != nil || len(misses) != 1 || misses[0].Role != "owner" || misses[0].Consecutive != 1 {
		t.Fatalf("missed-wake tracking after stalled input-pending wake = %+v, err=%v", misses, err)
	}

	wake.stalled = false
	reNudgeAt := now.Add(wakeAfter + time.Minute)
	snapshot.ObservedAt = reNudgeAt
	if err := evaluateInputPendingRoleEpisodes(context.Background(), store, sink, snapshot, wakeAfter, io.Discard, reNudgeAt); err != nil {
		t.Fatal(err)
	}
	if wake.promptCalls != 2 {
		t.Fatalf("input-pending re-nudge wake calls = %d, want 2", wake.promptCalls)
	}
	misses, err = store.ListRoleMissedWakes(context.Background())
	if err != nil || len(misses) != 0 {
		t.Fatalf("delivered input-pending wake did not reset counter: %+v, err=%v", misses, err)
	}
}
