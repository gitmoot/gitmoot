package workflow

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// referencedByOtherFullScan is the path_referenced guard exactly as it was
// before #2691: one full read per question.
func referencedByOtherFullScan(t *testing.T, store *db.Store, jobID, expected string) bool {
	t.Helper()
	refs, _, err := store.ListJobWorktreeRefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range refs {
		if other.ID != jobID && filepath.Clean(strings.TrimSpace(other.WorktreePath)) == filepath.Clean(expected) {
			return true
		}
	}
	return false
}

func insertRawWorktreeRefJob(t *testing.T, store *db.Store, id, state, payload string) {
	t.Helper()
	if err := store.ExecForTest(context.Background(),
		`INSERT INTO jobs (id, agent, type, state, payload) VALUES (?, 'a', 'review', ?, ?)`, id, state, payload); err != nil {
		t.Fatal(err)
	}
}

// TestWorktreeRefIndexMatchesFullScanGuard pins that the #2691 index makes the
// same refuse/allow decision as the full-scan guard it replaced, for a fresh
// index and for one already built before the other job was written.
func TestWorktreeRefIndexMatchesFullScanGuard(t *testing.T) {
	const self, path = "self", "/wt/owner/repo/self"
	type fixture struct {
		name, id, state, payload string
		refuse                   bool
	}
	var cases []fixture
	for _, state := range []JobState{JobQueued, JobRunning, JobBlocked, JobSucceeded, JobFailed, JobCancelled} {
		cases = append(cases, fixture{"same path " + string(state), "other", string(state), `{"worktree_path":"` + path + `"}`, true})
	}
	cases = append(cases,
		fixture{"different path", "other", "running", `{"worktree_path":"/wt/owner/repo/other"}`, false},
		fixture{"path prefix only", "other", "running", `{"worktree_path":"` + path + `-2"}`, false},
		fixture{"same job", self, "running", `{"worktree_path":"` + path + `"}`, false},
		fixture{"unparseable payload", "other", "running", `{"worktree_path":"` + path + `"`, false},
		fixture{"empty path", "other", "running", `{"worktree_path":"  "}`, false},
		fixture{"trailing slash", "other", "running", `{"worktree_path":"` + path + `/"}`, true},
		fixture{"dot segment", "other", "running", `{"worktree_path":"/wt/owner/./repo/self"}`, true},
		fixture{"surrounding space", "other", "running", `{"worktree_path":" ` + path + ` "}`, true},
		fixture{"upper-case key", "other", "running", `{"WORKTREE_PATH":"` + path + `"}`, true},
		fixture{"last duplicate key", "other", "running", `{"worktree_path":"","worktree_path":"` + path + `"}`, true},
	)
	for _, tc := range cases {
		for _, expected := range []string{path, path + "/", "/wt/owner/repo/./self"} {
			for _, prebuilt := range []bool{false, true} {
				store := openEngineStore(t)
				ctx := context.Background()
				var index WorktreeRefIndex
				if tc.id != self {
					insertRawWorktreeRefJob(t, store, self, "failed", `{"worktree_path":"`+path+`"}`)
				}
				if prebuilt {
					// Built before the other job exists: it must still see it.
					if got, err := index.referencedByOther(ctx, store, self, expected); err != nil || got {
						t.Fatalf("%s: empty index refused=%t err=%v", tc.name, got, err)
					}
				}
				insertRawWorktreeRefJob(t, store, tc.id, tc.state, tc.payload)
				old := referencedByOtherFullScan(t, store, self, expected)
				got, err := index.referencedByOther(ctx, store, self, expected)
				if err != nil {
					t.Fatal(err)
				}
				if old != tc.refuse || got != old {
					t.Fatalf("%s (expected path %q, prebuilt %t): full scan refused=%t, index refused=%t, want %t",
						tc.name, expected, prebuilt, old, got, tc.refuse)
				}
			}
		}
	}
}

// TestWorktreeRefIndexStaleOnlyRefusesMore pins the freshness rule of a held
// index: a reference that later disappears keeps refusing, and a job inserted
// after the index was built is seen.
func TestWorktreeRefIndexStaleOnlyRefusesMore(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	const path = "/wt/owner/repo/a"
	insertRawWorktreeRefJob(t, store, "a", "failed", `{"worktree_path":"`+path+`"}`)
	insertRawWorktreeRefJob(t, store, "b", "running", `{"worktree_path":"`+path+`"}`)
	var index WorktreeRefIndex
	if got, err := index.referencedByOther(ctx, store, "a", path); err != nil || !got {
		t.Fatalf("co-owned path allowed: %t %v", got, err)
	}
	if err := store.ExecForTest(ctx, `UPDATE jobs SET payload = '{}' WHERE id = 'b'`); err != nil {
		t.Fatal(err)
	}
	if referencedByOtherFullScan(t, store, "a", path) {
		t.Fatal("fixture: the full scan should no longer see b")
	}
	if got, err := index.referencedByOther(ctx, store, "a", path); err != nil || !got {
		t.Fatalf("held index forgot a reference: %t %v", got, err)
	}
	const fresh = "/wt/owner/repo/c"
	insertRawWorktreeRefJob(t, store, "c", "failed", `{"worktree_path":"`+fresh+`"}`)
	if got, err := index.referencedByOther(ctx, store, "c", fresh); err != nil || got {
		t.Fatalf("unreferenced path refused: %t %v", got, err)
	}
	insertRawWorktreeRefJob(t, store, "d", "queued", `{"worktree_path":"`+fresh+`"}`)
	if got, err := index.referencedByOther(ctx, store, "c", fresh); err != nil || !got {
		t.Fatalf("held index missed a job inserted after it was built: %t %v", got, err)
	}
	if scans := store.WorktreeRefScansForTest(); scans != 2 {
		t.Fatalf("full scans = %d, want 2 (one by the index, one by the fixture check)", scans)
	}
}
