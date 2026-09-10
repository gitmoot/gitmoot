package db

import (
	"context"
	"path/filepath"
	"testing"
)

func openRepoDefaultBranchTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// The cached default branch is consumed as the BASE BRANCH, and it was written
// from the worktree's CURRENT branch, so records exist naming a feature branch
// (#2145: jerryfane/herdr held `fix/lan-address-portability` for a repository
// whose default is `master`). A wrong value is already durable, so the poll must
// be able to correct it - and must report whether it actually did, or the
// operator sees a correction logged on every tick forever.
func TestUpdateRepoDefaultBranchCorrectsAndReportsChange(t *testing.T) {
	ctx := context.Background()
	store := openRepoDefaultBranchTestStore(t)
	if err := store.UpsertRepo(ctx, Repo{
		Owner: "jerryfane", Name: "herdr",
		DefaultBranch: "fix/lan-address-portability",
		RemoteURL:     "https://github.com/jerryfane/herdr.git",
		CheckoutPath:  "/repo/herdr", PrimaryCheckoutPath: "/repo/herdr",
	}); err != nil {
		t.Fatal(err)
	}

	changed, err := store.UpdateRepoDefaultBranch(ctx, "jerryfane/herdr", "master")
	if err != nil || !changed {
		t.Fatalf("first correction: changed=%v err=%v, want true/nil", changed, err)
	}
	repo, err := store.GetRepo(ctx, "jerryfane/herdr")
	if err != nil {
		t.Fatal(err)
	}
	if repo.DefaultBranch != "master" {
		t.Fatalf("default_branch = %q, want master", repo.DefaultBranch)
	}

	// Idempotent: the poll runs every tick, so an unchanged value must report no
	// change rather than a write.
	changed, err = store.UpdateRepoDefaultBranch(ctx, "jerryfane/herdr", "master")
	if err != nil || changed {
		t.Fatalf("repeat correction: changed=%v err=%v, want false/nil", changed, err)
	}
}

// An unreadable origin/HEAD must not blank the base branch. Turning a wrong base
// into a MISSING base is a worse failure, and it would happen on every repo
// whose symbolic ref is momentarily unresolvable.
func TestUpdateRepoDefaultBranchRejectsEmptyRatherThanBlanking(t *testing.T) {
	ctx := context.Background()
	store := openRepoDefaultBranchTestStore(t)
	if err := store.UpsertRepo(ctx, Repo{
		Owner: "jerryfane", Name: "herdr", DefaultBranch: "master",
		RemoteURL: "https://github.com/jerryfane/herdr.git", CheckoutPath: "/repo/herdr",
	}); err != nil {
		t.Fatal(err)
	}

	for _, branch := range []string{"", "   "} {
		changed, err := store.UpdateRepoDefaultBranch(ctx, "jerryfane/herdr", branch)
		if err == nil || changed {
			t.Fatalf("branch %q: changed=%v err=%v, want an error and no change", branch, changed, err)
		}
	}
	repo, err := store.GetRepo(ctx, "jerryfane/herdr")
	if err != nil {
		t.Fatal(err)
	}
	if repo.DefaultBranch != "master" {
		t.Fatalf("default_branch = %q, want master preserved", repo.DefaultBranch)
	}
}
