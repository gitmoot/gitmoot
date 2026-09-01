package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestAllocateFixWorktreeSetsAsideInterruptedCloneInsteadOfDeleting enters through
// the PRODUCTION allocation path, which is where a caller-level re-enable-delete
// mutant lives: a helper-only test passes with os.RemoveAll restored here.
//
// An interrupted pre-enqueue allocation has no job row, so the allocator used to
// delete it and re-clone. It is still a standalone object database, so it is now
// moved aside and its bytes survive.
//
// MUTATION PROOF: replace the SetAsideFixClone call in allocateFixWorktreeForRunner
// with os.RemoveAll and the surviving-bytes assertion fails.
func TestAllocateFixWorktreeSetsAsideInterruptedCloneInsteadOfDeleting(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	path, err := workflow.FixWorktreePath(home, "owner/repo", "fix-interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	// The bytes an interrupted allocation may be holding.
	if err := os.WriteFile(filepath.Join(path, "only-copy.txt"), []byte("unique\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No job row exists, so this is the interrupted pre-enqueue branch. The clone
	// step then fails against a checkout that is not a repository, which is fine:
	// the branch under test runs before it.
	_, allocErr := allocateFixWorktreeForRunner(ctx, store, home, t.TempDir(), workflow.FixWorktreeRequest{
		JobID: "fix-interrupted", Repo: "owner/repo", Branch: "feature/fix",
	}, subprocess.ExecRunner{})
	if allocErr == nil {
		t.Log("allocation unexpectedly succeeded; the set-aside assertion below still applies")
	}

	if _, err := os.Stat(filepath.Join(path, "only-copy.txt")); err == nil {
		t.Fatal("the managed path was reused in place; allocation must start from a fresh clone")
	}
	survivors, err := workflow.FixCloneQuarantines(path)
	if err != nil {
		t.Fatalf("FixCloneQuarantines: %v", err)
	}
	if len(survivors) != 1 {
		t.Fatalf("survivors = %v, want the interrupted clone moved aside, not deleted", survivors)
	}
	content, err := os.ReadFile(filepath.Join(survivors[0], "only-copy.txt"))
	if err != nil || string(content) != "unique\n" {
		t.Fatalf("moved-aside content = %q (err %v), want it intact", content, err)
	}
	if !strings.Contains(filepath.Base(survivors[0]), "orphaned") {
		t.Fatalf("survivor name %q does not say what it is", filepath.Base(survivors[0]))
	}
}
