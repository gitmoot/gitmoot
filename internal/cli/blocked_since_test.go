package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
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
	store, err := db.Open(paths.Database)
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
	store, err := db.Open(paths.Database)
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
	nudgeAt(base, 1)                            // crosses threshold → emit #1
	nudgeAt(base.Add(30*time.Minute), 1)        // within interval → no re-emit
	nudgeAt(base.Add(wakeAfter+time.Minute), 2) // past interval, still blocked → re-nudge #2

	// Repeats must be self-identifying: both re-nudges of the SAME episode carry the
	// SAME first_since, so a consumer never reads a re-nudge as a fresh episode.
	blocked := sink.byType(events.EventJobBlocked)
	sinceOf := func(detail string) string {
		if i := strings.Index(detail, "(since "); i >= 0 {
			return detail[i:]
		}
		return ""
	}
	if s0, s1 := sinceOf(blocked[0].Detail), sinceOf(blocked[1].Detail); s0 == "" || s0 != s1 {
		t.Fatalf("re-nudges not self-identifying by first_since: %q vs %q", blocked[0].Detail, blocked[1].Detail)
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
