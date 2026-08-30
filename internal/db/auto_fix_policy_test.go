package db

import (
	"context"
	"path/filepath"
	"testing"
)

func openAutoFixPolicyTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestPullRequestAutoFixPolicyPersistsDisableAndEnable(t *testing.T) {
	ctx := context.Background()
	store := openAutoFixPolicyTestStore(t)

	if policy, configured, err := store.PullRequestAutoFixPolicyFor(ctx, "owner/repo", 1686); err != nil {
		t.Fatalf("PullRequestAutoFixPolicyFor unset: %v", err)
	} else if configured {
		t.Fatalf("unset policy = %+v, configured=true", policy)
	}

	if err := store.SetPullRequestAutoFixPolicy(ctx, " Owner/Repo ", 1686, true, "gitmoot", "coordinator declined this round"); err != nil {
		t.Fatalf("disable policy: %v", err)
	}
	policy, configured, err := store.PullRequestAutoFixPolicyFor(ctx, "owner/repo", 1686)
	if err != nil {
		t.Fatalf("read disabled policy: %v", err)
	}
	if !configured || !policy.Disabled || policy.RepoFullName != "owner/repo" || policy.PullRequest != 1686 || policy.Actor != "gitmoot" || policy.Reason != "coordinator declined this round" || policy.CreatedAt == "" || policy.UpdatedAt == "" {
		t.Fatalf("disabled policy = %+v, configured=%v", policy, configured)
	}

	if err := store.SetPullRequestAutoFixPolicy(ctx, "owner/repo", 1686, false, "gitmoot", "owner resumed automatic fixes"); err != nil {
		t.Fatalf("enable policy: %v", err)
	}
	policy, configured, err = store.PullRequestAutoFixPolicyFor(ctx, "OWNER/REPO", 1686)
	if err != nil {
		t.Fatalf("read enabled policy: %v", err)
	}
	if !configured || policy.Disabled || policy.Actor != "gitmoot" || policy.Reason != "owner resumed automatic fixes" {
		t.Fatalf("enabled policy = %+v, configured=%v", policy, configured)
	}
}

func TestPullRequestAutoFixPolicyRejectsUnattributedDecision(t *testing.T) {
	ctx := context.Background()
	store := openAutoFixPolicyTestStore(t)

	for _, tc := range []struct {
		name   string
		repo   string
		pr     int
		actor  string
		reason string
	}{
		{name: "bad repo", repo: "owner", pr: 1, actor: "role", reason: "reason"},
		{name: "bad pr", repo: "owner/repo", pr: 0, actor: "role", reason: "reason"},
		{name: "missing actor", repo: "owner/repo", pr: 1, reason: "reason"},
		{name: "missing reason", repo: "owner/repo", pr: 1, actor: "role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.SetPullRequestAutoFixPolicy(ctx, tc.repo, tc.pr, true, tc.actor, tc.reason); err == nil {
				t.Fatal("SetPullRequestAutoFixPolicy succeeded, want validation error")
			}
		})
	}
}
