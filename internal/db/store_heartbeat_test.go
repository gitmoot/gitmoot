package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openHeartbeatStore(t *testing.T) *Store {
	t.Helper()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestHeartbeatStateMissingIsNotError proves a fresh schedule reads back as "not
// found" (the daemon treats that as "due now"), keeping the table off-by-default:
// no row exists until a heartbeat first fires.
func TestHeartbeatStateMissingIsNotError(t *testing.T) {
	store := openHeartbeatStore(t)
	ctx := context.Background()
	state, found, err := store.GetHeartbeatState(ctx, "repo-maintainer", "daily")
	if err != nil {
		t.Fatalf("GetHeartbeatState: %v", err)
	}
	if found {
		t.Fatalf("expected no row for a fresh heartbeat, got %+v", state)
	}
}

func TestUpsertAndGetHeartbeatState(t *testing.T) {
	store := openHeartbeatStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	due := now.Add(24 * time.Hour)
	if err := store.UpsertHeartbeatState(ctx, HeartbeatState{
		Agent: "repo-maintainer", Name: "daily",
		LastRunAt: now, NextDueAt: due, LastJobID: "job-1", LastStatus: "enqueued",
	}); err != nil {
		t.Fatalf("UpsertHeartbeatState: %v", err)
	}
	state, found, err := store.GetHeartbeatState(ctx, "repo-maintainer", "daily")
	if err != nil {
		t.Fatalf("GetHeartbeatState: %v", err)
	}
	if !found {
		t.Fatalf("expected row after upsert")
	}
	if !state.LastRunAt.Equal(now) || !state.NextDueAt.Equal(due) || state.LastJobID != "job-1" || state.LastStatus != "enqueued" {
		t.Fatalf("roundtrip mismatch: %+v", state)
	}
	// Upsert again (same key) replaces in place — one row per (agent, name).
	later := due.Add(24 * time.Hour)
	if err := store.UpsertHeartbeatState(ctx, HeartbeatState{
		Agent: "repo-maintainer", Name: "daily",
		LastRunAt: due, NextDueAt: later, LastJobID: "job-2", LastStatus: "enqueue_failed",
	}); err != nil {
		t.Fatalf("UpsertHeartbeatState replace: %v", err)
	}
	state, _, err = store.GetHeartbeatState(ctx, "repo-maintainer", "daily")
	if err != nil {
		t.Fatalf("GetHeartbeatState after replace: %v", err)
	}
	if state.LastJobID != "job-2" || state.LastStatus != "enqueue_failed" || !state.NextDueAt.Equal(later) {
		t.Fatalf("replace mismatch: %+v", state)
	}
}

func TestCountActiveJobsByFingerprint(t *testing.T) {
	store := openHeartbeatStore(t)
	ctx := context.Background()
	const fp = "heartbeat:repo-maintainer/daily"

	if count, err := store.CountActiveJobsByFingerprint(ctx, fp); err != nil || count != 0 {
		t.Fatalf("empty store: count=%d err=%v", count, err)
	}

	mustJob := func(id, state, payload string) {
		if err := store.CreateJob(ctx, Job{ID: id, Agent: "repo-maintainer", Type: "ask", State: state, Payload: payload}); err != nil {
			t.Fatalf("CreateJob %s: %v", id, err)
		}
	}
	mustJob("j1", "queued", `{"fingerprint":"heartbeat:repo-maintainer/daily"}`)
	mustJob("j2", "running", `{"fingerprint":"heartbeat:repo-maintainer/daily"}`)
	mustJob("j3", "succeeded", `{"fingerprint":"heartbeat:repo-maintainer/daily"}`) // terminal: not counted
	mustJob("j4", "queued", `{"fingerprint":"heartbeat:other/x"}`)                  // different fp
	mustJob("j5", "queued", `not-json`)                                             // malformed: tolerated, skipped

	count, err := store.CountActiveJobsByFingerprint(ctx, fp)
	if err != nil {
		t.Fatalf("CountActiveJobsByFingerprint: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 active jobs for %q, got %d", fp, count)
	}

	// An empty fingerprint matches nothing (never counts every active job).
	if count, err := store.CountActiveJobsByFingerprint(ctx, ""); err != nil || count != 0 {
		t.Fatalf("empty fingerprint: count=%d err=%v", count, err)
	}
}
