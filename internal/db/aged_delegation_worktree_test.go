package db

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestJobIDsWithAgedTerminalDelegationWorktree locks the behavior of the #1111
// CTE-materialized rewrite across every branch: owner still active vs terminal-
// and-old, an existing cleanup event, aged vs recent, worktree presence, the
// delegation-vs-read-only payload predicate, a resumable blocked job, and two
// aged jobs sharing one worktree. Running on the modernc driver also proves it
// accepts the `WITH ... AS MATERIALIZED` CTE hint the rewrite depends on.
func TestJobIDsWithAgedTerminalDelegationWorktree(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)      // <= cutoff (aged)
	recent := now.Add(-1 * time.Minute) // > cutoff (still fresh)
	cutoff := now.Add(-30 * time.Minute)

	wt := func(path, extra string) string {
		if extra == "" {
			return fmt.Sprintf(`{"worktree_path":%q}`, path)
		}
		return fmt.Sprintf(`{"worktree_path":%q,%s}`, path, extra)
	}
	seeds := []struct {
		id      string
		state   string
		payload string
		updated time.Time
		events  []string
		want    bool
	}{
		{id: "aged-delegation", state: "succeeded", payload: wt("/wt/a", `"delegation_id":"d1"`), updated: old, want: true},
		{id: "aged-readonly", state: "failed", payload: wt("/wt/f", `"read_only_worktree":true`), updated: old, want: true},
		// owned-by-active shares /wt/b with a still-running owner -> not reclaimable.
		{id: "owned-by-active", state: "cancelled", payload: wt("/wt/b", `"delegation_id":"d2"`), updated: old, want: false},
		{id: "active-owner", state: "running", payload: wt("/wt/b", `"delegation_id":"d2b"`), updated: recent, want: false},
		// has-cleanup already emitted a removal event -> excluded.
		{id: "has-cleanup", state: "succeeded", payload: wt("/wt/d", `"delegation_id":"d3"`), updated: old, events: []string{"delegation_worktree_removed"}, want: false},
		{id: "no-worktree", state: "succeeded", payload: `{"delegation_id":"d4"}`, updated: old, want: false},
		{id: "not-aged", state: "succeeded", payload: wt("/wt/g", `"delegation_id":"d5"`), updated: recent, want: false},
		{id: "blocked-resumable", state: "blocked", payload: wt("/wt/h", `"delegation_id":"d6"`), updated: old, want: false},
		// ordinary task worktree: no delegation_id and not read-only -> excluded.
		{id: "ordinary-task", state: "succeeded", payload: wt("/wt/i", ""), updated: old, want: false},
		// two aged terminal jobs sharing one worktree: neither owner is active or
		// newer than cutoff, so BOTH are reclaim candidates (matches the old query).
		{id: "shared-both-aged-1", state: "succeeded", payload: wt("/wt/s", `"delegation_id":"s1"`), updated: old, want: true},
		{id: "shared-both-aged-2", state: "failed", payload: wt("/wt/s", `"delegation_id":"s2"`), updated: old, want: true},
	}

	want := map[string]bool{}
	for _, s := range seeds {
		if err := store.CreateJob(ctx, Job{ID: s.id, Agent: "producer", Type: "implement", State: s.state, Payload: s.payload}); err != nil {
			t.Fatalf("CreateJob(%s): %v", s.id, err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET updated_at=? WHERE id=?`,
			s.updated.Format("2006-01-02 15:04:05"), s.id); err != nil {
			t.Fatalf("set updated_at(%s): %v", s.id, err)
		}
		for _, k := range s.events {
			if err := store.AddJobEvent(ctx, JobEvent{JobID: s.id, Kind: k, Message: "m"}); err != nil {
				t.Fatalf("AddJobEvent(%s,%s): %v", s.id, k, err)
			}
		}
		if s.want {
			want[s.id] = true
		}
	}

	// Invalid/empty payload is a tolerated store state ('' is the schema default,
	// and the write path swallows marshal errors). The eager json_valid-guarded
	// CTE must silently ignore these rows, not error the whole reclaim pass
	// (#1111 finder). Neither is a candidate. Without the guard this query errors.
	for _, bad := range []struct{ id, payload string }{
		{id: "bad-empty-payload", payload: ""},
		{id: "bad-garbage-payload", payload: "not json at all"},
	} {
		if err := store.CreateJob(ctx, Job{ID: bad.id, Agent: "producer", Type: "implement", State: "succeeded", Payload: "{}"}); err != nil {
			t.Fatalf("CreateJob(%s): %v", bad.id, err)
		}
		if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET payload=?, updated_at=? WHERE id=?`,
			bad.payload, old.Format("2006-01-02 15:04:05"), bad.id); err != nil {
			t.Fatalf("force payload(%s): %v", bad.id, err)
		}
	}

	got, err := store.JobIDsWithAgedTerminalDelegationWorktree(ctx, cutoff)
	if err != nil {
		t.Fatalf("JobIDsWithAgedTerminalDelegationWorktree: %v", err)
	}
	gotSet := map[string]bool{}
	for _, id := range got {
		if gotSet[id] {
			t.Fatalf("duplicate id %q in %v", id, got)
		}
		gotSet[id] = true
	}
	if len(gotSet) != len(want) {
		t.Fatalf("result = %v, want exactly %v", got, want)
	}
	for id := range want {
		if !gotSet[id] {
			t.Fatalf("expected %q in result, got %v", id, got)
		}
	}
}
