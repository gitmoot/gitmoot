package db

import (
	"context"
	"path/filepath"
	"testing"
)

func openPresetStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestAgentPresetDeliveryDefaultsFull proves the additive column defaults to
// 'full' for an agent that never sets it, so every existing row and every
// non-opted-in agent reads back byte-identical behavior.
func TestAgentPresetDeliveryDefaultsFull(t *testing.T) {
	store := openPresetStore(t)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, Agent{Name: "audit", Role: "reviewer", Runtime: "codex", RuntimeRef: "last", RepoScope: "o/r"}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	got, err := store.GetAgent(ctx, "audit")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.PresetDelivery != PresetDeliveryFull {
		t.Fatalf("PresetDelivery = %q, want %q", got.PresetDelivery, PresetDeliveryFull)
	}
}

// TestUpsertAndUpdateAgentPresetDelivery covers persisting the mode on subscribe
// (upsert), an in-place update, and the invalid/blank -> full normalization.
func TestUpsertAndUpdateAgentPresetDelivery(t *testing.T) {
	store := openPresetStore(t)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, Agent{Name: "audit", Role: "reviewer", Runtime: "codex", RuntimeRef: "last", RepoScope: "o/r", PresetDelivery: "REFERENCED"}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	got, err := store.GetAgent(ctx, "audit")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.PresetDelivery != PresetDeliveryReferenced {
		t.Fatalf("upsert normalized mode = %q, want %q", got.PresetDelivery, PresetDeliveryReferenced)
	}

	if err := store.UpdateAgentPresetDelivery(ctx, "audit", "auto"); err != nil {
		t.Fatalf("UpdateAgentPresetDelivery: %v", err)
	}
	got, _ = store.GetAgent(ctx, "audit")
	if got.PresetDelivery != PresetDeliveryAuto {
		t.Fatalf("after update mode = %q, want %q", got.PresetDelivery, PresetDeliveryAuto)
	}

	// An invalid mode is coerced to full rather than stored verbatim.
	if err := store.UpdateAgentPresetDelivery(ctx, "audit", "nonsense"); err != nil {
		t.Fatalf("UpdateAgentPresetDelivery(nonsense): %v", err)
	}
	got, _ = store.GetAgent(ctx, "audit")
	if got.PresetDelivery != PresetDeliveryFull {
		t.Fatalf("invalid mode not coerced; mode = %q, want %q", got.PresetDelivery, PresetDeliveryFull)
	}

	if err := store.UpdateAgentPresetDelivery(ctx, "missing", "auto"); err == nil {
		t.Fatalf("UpdateAgentPresetDelivery on missing agent should error")
	}
}

// TestPresetSessionStateRecordAndMatch covers the exact-match contract and the
// commit-invalidation invariant: a marker matches only the exact tuple, a commit
// change overwrites the prior row (so a stale commit never matches), and delete
// clears the preset.
func TestPresetSessionStateRecordAndMatch(t *testing.T) {
	store := openPresetStore(t)
	ctx := context.Background()

	// No rows until recorded (off by default).
	if has, err := store.HasPresetSessionState(ctx, "codex", "sess-1", "thermo", "c1"); err != nil || has {
		t.Fatalf("HasPresetSessionState empty = (%v, %v), want (false, nil)", has, err)
	}

	if err := store.RecordPresetSessionState(ctx, "codex", "sess-1", "thermo", "c1"); err != nil {
		t.Fatalf("RecordPresetSessionState: %v", err)
	}
	if has, err := store.HasPresetSessionState(ctx, "codex", "sess-1", "thermo", "c1"); err != nil || !has {
		t.Fatalf("exact match = (%v, %v), want (true, nil)", has, err)
	}
	// Any component mismatch must not match.
	for _, m := range []struct{ rt, s, p, c string }{
		{"claude", "sess-1", "thermo", "c1"},
		{"codex", "sess-2", "thermo", "c1"},
		{"codex", "sess-1", "other", "c1"},
		{"codex", "sess-1", "thermo", "c2"},
	} {
		if has, err := store.HasPresetSessionState(ctx, m.rt, m.s, m.p, m.c); err != nil || has {
			t.Fatalf("mismatch %+v matched unexpectedly", m)
		}
	}

	// A commit change overwrites the prior row for the (runtime, session, preset)
	// tuple: the old commit no longer matches, only the new one does.
	if err := store.RecordPresetSessionState(ctx, "codex", "sess-1", "thermo", "c2"); err != nil {
		t.Fatalf("re-record new commit: %v", err)
	}
	if has, _ := store.HasPresetSessionState(ctx, "codex", "sess-1", "thermo", "c1"); has {
		t.Fatalf("stale commit c1 still matches after commit change")
	}
	if has, _ := store.HasPresetSessionState(ctx, "codex", "sess-1", "thermo", "c2"); !has {
		t.Fatalf("new commit c2 does not match")
	}

	// Empty components are rejected on write and never match on read.
	if err := store.RecordPresetSessionState(ctx, "", "sess-1", "thermo", "c2"); err == nil {
		t.Fatalf("RecordPresetSessionState with empty runtime should error")
	}

	// DeletePresetSessionStateForPreset clears every session/commit for a preset.
	if err := store.RecordPresetSessionState(ctx, "codex", "sess-9", "thermo", "c9"); err != nil {
		t.Fatalf("record another session: %v", err)
	}
	n, err := store.DeletePresetSessionStateForPreset(ctx, "thermo")
	if err != nil {
		t.Fatalf("DeletePresetSessionStateForPreset: %v", err)
	}
	if n < 2 {
		t.Fatalf("deleted %d rows, want >= 2", n)
	}
	if has, _ := store.HasPresetSessionState(ctx, "codex", "sess-1", "thermo", "c2"); has {
		t.Fatalf("preset state survived delete")
	}
}
