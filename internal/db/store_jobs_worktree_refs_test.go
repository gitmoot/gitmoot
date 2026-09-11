package db

import (
	"context"
	"testing"
)

func insertWorktreeRefJob(t *testing.T, store *Store, id, state, payload string) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(),
		`INSERT INTO jobs (id, agent, type, state, payload, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?)`,
		id, "a", "review", state, payload, "2026-09-01 00:00:00", "2026-09-02 00:00:00"); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

func refsByID(t *testing.T, store *Store) map[string]JobWorktreeRef {
	t.Helper()
	refs, err := store.ListJobWorktreeRefs(context.Background())
	if err != nil {
		t.Fatalf("ListJobWorktreeRefs: %v", err)
	}
	byID := make(map[string]JobWorktreeRef, len(refs))
	for _, ref := range refs {
		byID[ref.ID] = ref
	}
	return byID
}

// TestListJobWorktreeRefsMatchesEncodingJSONNotJSONExtract is the regression
// for #2149's review finding, and it is a SAFETY test rather than a
// formatting one.
//
// The first version of this query asked SQLite for the path with
// json_extract. That is not encoding/json: json_extract returns the FIRST
// duplicate key and is case-SENSITIVE, while encoding/json keeps the LAST
// duplicate and matches field names case-insensitively. Both payloads below
// are valid JSON, and both were dropped by the json_extract predicate while
// the decoder the caller replaced would have seen them.
//
// A dropped row is a co-owner the reclaim guard cannot see. If it is still
// running, its checkout is deleted underneath it.
func TestListJobWorktreeRefsMatchesEncodingJSONNotJSONExtract(t *testing.T) {
	store := openStoreOperationsTestStore(t)

	// json_extract returns "" for this; encoding/json returns /live/dup.
	insertWorktreeRefJob(t, store, "duplicate-key", "running",
		`{"worktree_path":"","worktree_path":"/live/dup"}`)
	// json_extract returns NULL for this; encoding/json returns /live/case.
	insertWorktreeRefJob(t, store, "upper-case-key", "running",
		`{"WORKTREE_PATH":"/live/case"}`)
	insertWorktreeRefJob(t, store, "canonical", "succeeded",
		`{"worktree_path":"/plain/one","prompt":"x"}`)

	refs := refsByID(t, store)
	for _, want := range []struct{ id, path string }{
		{"duplicate-key", "/live/dup"},
		{"upper-case-key", "/live/case"},
		{"canonical", "/plain/one"},
	} {
		got, ok := refs[want.id]
		if !ok {
			t.Errorf("%s is missing: a co-owner invisible to the guard is a worktree deleted underneath a live job", want.id)
			continue
		}
		if got.WorktreePath != want.path {
			t.Errorf("%s path = %q, want %q (encoding/json semantics, not json_extract's)", want.id, got.WorktreePath, want.path)
		}
	}
}

// TestListJobWorktreeRefsSkipsRowsWithNoUsablePath keeps the query from
// handing the guard rows it would only discard, and pins that an unparseable
// payload is skipped rather than failing the call - which is what the caller
// did with a ParseJobPayload error.
func TestListJobWorktreeRefsSkipsRowsWithNoUsablePath(t *testing.T) {
	store := openStoreOperationsTestStore(t)

	insertWorktreeRefJob(t, store, "no-key", "succeeded", `{"prompt":"x"}`)
	insertWorktreeRefJob(t, store, "empty-value", "succeeded", `{"worktree_path":""}`)
	insertWorktreeRefJob(t, store, "blank-value", "succeeded", `{"worktree_path":"   "}`)
	// Mentions the key but only inside a nested object, so the top-level field
	// is empty. The text prefilter selects it; the decode must discard it.
	insertWorktreeRefJob(t, store, "nested-only", "succeeded", `{"meta":{"worktree_path":"/nested"}}`)
	// Not JSON, but it DOES mention the key, so the prefilter selects it and
	// the decoder is the thing that must cope. The first version of this
	// fixture omitted the key, so SQL dropped the row and the skip branch was
	// never reached - its mutant survived, returning an error instead of
	// skipping, without failing a single test.
	insertWorktreeRefJob(t, store, "not-json", "succeeded", `{"worktree_path": "/truncated`)
	insertWorktreeRefJob(t, store, "real", "running", `{"worktree_path":"/real"}`)

	refs := refsByID(t, store)
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want only the usable one: %+v", len(refs), refs)
	}
	got, ok := refs["real"]
	if !ok {
		t.Fatalf("the usable row is missing: %+v", refs)
	}
	if got.WorktreePath != "/real" || got.State != "running" {
		t.Errorf("ref = %+v, want /real in state running", got)
	}
	if got.UpdatedAt == "" || got.CreatedAt == "" {
		t.Errorf("ref = %+v, want the age fields the guard needs to compare against the cutoff", got)
	}
}

// TestListJobWorktreeRefsPrefilterIsASuperset pins the property the whole
// design rests on: SQL must never decide that a row has no path. It may only
// drop rows that cannot mention one.
//
// Asserted by construction rather than by reading the plan: every row whose
// payload contains the key in ANY case must survive the prefilter and reach
// the decoder, and the decoder alone decides.
func TestListJobWorktreeRefsPrefilterIsASuperset(t *testing.T) {
	store := openStoreOperationsTestStore(t)

	insertWorktreeRefJob(t, store, "mixed-case", "running", `{"WorkTree_Path":"/mixed"}`)
	insertWorktreeRefJob(t, store, "trailing-ws", "running", `{"worktree_path":"/ws   "}`)
	// A row with no mention of the key at all is the ONLY thing SQL may drop.
	insertWorktreeRefJob(t, store, "unrelated", "running", `{"prompt":"nothing here"}`)

	refs := refsByID(t, store)
	if _, ok := refs["mixed-case"]; !ok {
		t.Error("a mixed-case key was dropped before the decoder saw it")
	}
	if got := refs["trailing-ws"].WorktreePath; got != "/ws   " {
		t.Errorf("path = %q, want the stored value verbatim: trimming belongs to the caller's comparison, not here", got)
	}
	if _, ok := refs["unrelated"]; ok {
		t.Error("a row that never mentions the key was returned")
	}
}
