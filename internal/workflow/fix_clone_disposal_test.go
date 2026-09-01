package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SetAsideFixClone is the ONLY disposal primitive fix-clone paths may use, so its
// contract is pinned directly: it renames, never deletes, and the result lands in
// the survivor family every existing scan already reports.
//
// MUTATION PROOF: replace the rename with os.RemoveAll and both the surviving
// content and the survivor listing assertions fail.
func TestSetAsideFixClonePreservesContent(t *testing.T) {
	root := t.TempDir()
	clone := filepath.Join(root, "job-1")
	if err := os.MkdirAll(filepath.Join(clone, "src"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(clone, "src", "only-copy.txt"), []byte("unique\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	aside, err := SetAsideFixClone(clone)
	if err != nil {
		t.Fatalf("SetAsideFixClone: %v", err)
	}
	if aside == "" || aside == clone {
		t.Fatalf("set-aside path = %q, want a fresh sibling", aside)
	}
	if _, err := os.Stat(clone); !os.IsNotExist(err) {
		t.Fatalf("managed path was not freed: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(aside, "src", "only-copy.txt"))
	if err != nil || string(content) != "unique\n" {
		t.Fatalf("content = %q (err %v), want it moved intact", content, err)
	}
	// Reported by the same survivor scan the daemon, the reclaim and doctor use, so
	// an operator sees it without a second mechanism.
	survivors, err := FixCloneQuarantines(clone)
	if err != nil || len(survivors) != 1 || survivors[0] != aside {
		t.Fatalf("survivors = %v (err %v), want the set-aside clone %s", survivors, err, aside)
	}
	if !strings.Contains(filepath.Base(aside), "orphaned") {
		t.Fatalf("set-aside name %q does not say what it is", filepath.Base(aside))
	}

	// Two set-asides of the same managed path must not collide.
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatalf("MkdirAll second clone: %v", err)
	}
	second, err := SetAsideFixClone(clone)
	if err != nil || second == aside {
		t.Fatalf("second set-aside = %q (err %v), want a distinct name", second, err)
	}
	// An absent path is a no-op rather than an error: the caller's cleanup runs on
	// paths that may already be gone.
	if aside, err := SetAsideFixClone(filepath.Join(root, "never-existed")); err != nil || aside != "" {
		t.Fatalf("set-aside of an absent path = (%q, %v), want a no-op", aside, err)
	}
}
