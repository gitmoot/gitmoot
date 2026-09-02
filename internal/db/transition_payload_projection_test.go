package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
)

// storedJobProjection reads the DERIVED COLUMNS back out of the row, not out of a
// helper's return value. That distinction is the whole point: a helper can be correct
// while the statement that was supposed to persist its output never ran (#1673).
type storedJobProjection struct {
	state                  string
	payload                string
	resultHash             string
	repo                   string
	pullRequest            int
	blockerRetryAt         string
	blockerSuggestedAction string
}

func readStoredJobProjection(t *testing.T, store *Store, id string) storedJobProjection {
	t.Helper()
	var got storedJobProjection
	row := store.db.QueryRowContext(context.Background(),
		`SELECT state, payload, COALESCE(result_hash, ''), COALESCE(repo, ''), COALESCE(pull_request, 0),
		        COALESCE(blocker_retry_at, ''), COALESCE(blocker_suggested_action, '')
		 FROM jobs WHERE id = ?`, id)
	if err := row.Scan(&got.state, &got.payload, &got.resultHash, &got.repo, &got.pullRequest,
		&got.blockerRetryAt, &got.blockerSuggestedAction); err != nil {
		t.Fatalf("read stored projection for %s: %v", id, err)
	}
	return got
}

func expectedResultHash(t *testing.T, payload string) string {
	t.Helper()
	var projection struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(payload), &projection); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	sum := sha256.Sum256(projection.Result)
	return hex.EncodeToString(sum[:])
}

// TestTransitionWithPayloadKeepsDerivedColumnsConsistent is the round-3 P1 of PR #1763.
// The payload-carrying transition installs a synthetic result, so every column a reader
// projects from jobs.payload must move with it IN THE SAME STATEMENT: result_hash (the
// terminal result's proof-integrity receipt and its memory-harvest key), plus repo,
// pull_request, blocker_retry_at and blocker_suggested_action.
//
// The three other payload writers in store_jobs.go already recompute all five, so a
// writer that skips them is one statement diverging from a file-wide convention.
//
// SEMANTIC REVERSION THIS KILLS: drop the derived fields from the payload arm and the
// stored hash stays at its previous value while the payload claims a result.
func TestTransitionWithPayloadKeepsDerivedColumnsConsistent(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	// A queued job with NO result: result_hash starts empty, which is the state that
	// makes a missed update invisible unless the column is read back.
	initial, err2 := json.Marshal(map[string]any{
		"repo":         "gitmoot/gitmoot",
		"pull_request": 7,
		"workflow_id":  "",
	})
	if err2 != nil {
		t.Fatalf("marshal initial payload: %v", err2)
	}
	if err := store.CreateJobWithEvent(ctx, Job{
		ID: "child", Agent: "api", Type: "review", State: "queued", Payload: string(initial),
	}, JobEvent{Kind: "queued", Message: "job queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	before := readStoredJobProjection(t, store, "child")
	if before.resultHash != "" {
		t.Fatalf("precondition: stored result_hash = %q, want empty", before.resultHash)
	}

	terminal, err3 := json.Marshal(map[string]any{
		"repo":                     "gitmoot/gitmoot",
		"pull_request":             7,
		"blocker_retry_at":         "2026-09-02T00:00:00Z",
		"blocker_suggested_action": "reopen the pull request",
		"result": map[string]any{
			"decision": "failed",
			"summary":  "pull request #7 is no longer open",
		},
	})
	if err3 != nil {
		t.Fatalf("marshal terminal payload: %v", err3)
	}

	transitioned, err := store.TransitionJobStateWithPayloadAndEvents(ctx, "child", "queued", "failed",
		terminal, JobEvent{Kind: "superseded_pr_closed", Message: "pull request #7 is no longer open"})
	if err != nil {
		t.Fatalf("TransitionJobStateWithPayloadAndEvents: %v", err)
	}
	if !transitioned {
		t.Fatal("transitioned = false, want the queued -> failed transition to commit")
	}

	after := readStoredJobProjection(t, store, "child")
	if after.state != "failed" {
		t.Fatalf("stored state = %q, want failed", after.state)
	}
	if after.payload != string(terminal) {
		t.Fatalf("stored payload was not replaced:\n got %s\nwant %s", after.payload, terminal)
	}
	// THE ASSERTION THAT WAS MISSING: the hash is read from the COLUMN and compared
	// against the payload it must describe.
	want := expectedResultHash(t, string(terminal))
	if after.resultHash != want {
		t.Fatalf("stored result_hash = %q, want %q: the payload claims a result the hash does not describe",
			after.resultHash, want)
	}
	if after.repo != "gitmoot/gitmoot" || after.pullRequest != 7 {
		t.Fatalf("stored projections repo=%q pull_request=%d, want gitmoot/gitmoot and 7", after.repo, after.pullRequest)
	}
	if after.blockerRetryAt != "2026-09-02T00:00:00Z" || after.blockerSuggestedAction != "reopen the pull request" {
		t.Fatalf("stored blocker projections = %q / %q, want them carried from the payload",
			after.blockerRetryAt, after.blockerSuggestedAction)
	}
}

// TestTransitionWithoutPayloadLeavesDerivedColumnsAlone is the success-path control for
// the same change, and the half that stops it from becoming a bound that rejects valid
// input: the events-only wrapper must remain byte-equivalent to the original primitive,
// touching neither the payload nor anything projected from it.
func TestTransitionWithoutPayloadLeavesDerivedColumnsAlone(t *testing.T) {
	ctx := context.Background()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	payload, err2 := json.Marshal(map[string]any{
		"repo":         "gitmoot/gitmoot",
		"pull_request": 9,
		"result":       map[string]any{"decision": "implemented", "summary": "done"},
	})
	if err2 != nil {
		t.Fatalf("marshal payload: %v", err2)
	}
	if err := store.CreateJobWithEvent(ctx, Job{
		ID: "job-keep", Agent: "api", Type: "review", State: "running", Payload: string(payload),
		ResultHash: jobResultHashFromPayload(string(payload)),
	}, JobEvent{Kind: "running", Message: "job running"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	before := readStoredJobProjection(t, store, "job-keep")

	transitioned, err := store.TransitionJobStateWithEvents(ctx, "job-keep", "running", "succeeded",
		JobEvent{Kind: "succeeded", Message: "done"})
	if err != nil {
		t.Fatalf("TransitionJobStateWithEvents: %v", err)
	}
	if !transitioned {
		t.Fatal("transitioned = false")
	}

	after := readStoredJobProjection(t, store, "job-keep")
	if after.state != "succeeded" {
		t.Fatalf("stored state = %q, want succeeded", after.state)
	}
	if after.payload != before.payload || after.resultHash != before.resultHash ||
		after.repo != before.repo || after.pullRequest != before.pullRequest ||
		after.blockerRetryAt != before.blockerRetryAt || after.blockerSuggestedAction != before.blockerSuggestedAction {
		t.Fatalf("the events-only wrapper changed derived state:\nbefore %+v\nafter  %+v", before, after)
	}
}
