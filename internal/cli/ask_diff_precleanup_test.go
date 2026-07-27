package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestComposeBeforeReadOnlyWorktreeCleanupHooksRunsEveryHook(t *testing.T) {
	firstErr := errors.New("first collector failed")
	secondErr := errors.New("second collector failed")
	var calls []string
	hook := composeBeforeReadOnlyWorktreeCleanupHooks(
		func(context.Context, string, string, workflow.JobPayload) error {
			calls = append(calls, "first")
			return firstErr
		},
		nil,
		func(context.Context, string, string, workflow.JobPayload) error {
			calls = append(calls, "second")
			return secondErr
		},
	)

	err := hook(context.Background(), "job-1", "ask", workflow.JobPayload{})
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("composed error = %v, want both collector errors", err)
	}
	if got := strings.Join(calls, ","); got != "first,second" {
		t.Fatalf("collector calls = %q, want first,second", got)
	}
}

func TestAskReviewDiffPrecleanupHookEmptyAndTruncated(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	checkout, _ := gitFixtureRepo(t, "clean\n")

	seedCLIJob(t, store, db.Job{
		ID:      "clean-ask",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", WorktreePath: checkout, ReadOnlyWorktree: true}),
	}, "succeeded")
	if err := askReviewDiffPrecleanupHook(store)(ctx, "clean-ask", "ask", workflow.JobPayload{
		Repo: "owner/repo", WorktreePath: checkout, ReadOnlyWorktree: true,
	}); err != nil {
		t.Fatalf("clean precleanup hook: %v", err)
	}
	cleanJob, err := store.GetJob(ctx, "clean-ask")
	if err != nil {
		t.Fatal(err)
	}
	cleanPayload, err := daemonJobPayload(cleanJob)
	if err != nil {
		t.Fatal(err)
	}
	if cleanPayload.ReadOnlyWorktreeDiff != "" || cleanPayload.ReadOnlyWorktreeDiffTruncated || cleanPayload.ReadOnlyWorktreeDiffError != "" {
		t.Fatalf("clean payload has bogus diff metadata: %+v", cleanPayload)
	}

	large := strings.Repeat("x", readOnlyWorktreeDiffMaxBytes+1024)
	if err := os.WriteFile(filepath.Join(checkout, "marker.txt"), []byte(large), 0o600); err != nil {
		t.Fatal(err)
	}
	seedCLIJob(t, store, db.Job{
		ID:      "large-review",
		Agent:   "audit",
		Type:    "review",
		State:   string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", WorktreePath: checkout, ReadOnlyWorktree: true}),
	}, "succeeded")
	if err := askReviewDiffPrecleanupHook(store)(ctx, "large-review", "review", workflow.JobPayload{
		Repo: "owner/repo", WorktreePath: checkout, ReadOnlyWorktree: true,
	}); err != nil {
		t.Fatalf("large precleanup hook: %v", err)
	}
	largeJob, err := store.GetJob(ctx, "large-review")
	if err != nil {
		t.Fatal(err)
	}
	largePayload, err := daemonJobPayload(largeJob)
	if err != nil {
		t.Fatal(err)
	}
	if !largePayload.ReadOnlyWorktreeDiffTruncated {
		t.Fatal("large diff was not marked truncated")
	}
	if !strings.Contains(largePayload.ReadOnlyWorktreeDiff, "[gitmoot: read-only worktree diff truncated; omitted ") {
		t.Fatalf("large diff lacks visible truncation marker: tail=%q", largePayload.ReadOnlyWorktreeDiff[len(largePayload.ReadOnlyWorktreeDiff)-200:])
	}
	if len(largePayload.ReadOnlyWorktreeDiff) > readOnlyWorktreeDiffMaxBytes {
		t.Fatalf("captured diff len = %d, max = %d", len(largePayload.ReadOnlyWorktreeDiff), readOnlyWorktreeDiffMaxBytes)
	}
}

func TestAskReviewDiffPrecleanupHookPersistsFailureMarker(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	notRepo := t.TempDir()
	payload := workflow.JobPayload{Repo: "owner/repo", WorktreePath: notRepo, ReadOnlyWorktree: true}
	seedCLIJob(t, store, db.Job{
		ID: "broken-ask", Agent: "audit", Type: "ask", State: string(workflow.JobSucceeded),
		Payload: mustJobPayload(t, payload),
	}, "succeeded")

	if err := askReviewDiffPrecleanupHook(store)(ctx, "broken-ask", "ask", payload); err == nil {
		t.Fatal("capture in a non-repository returned nil error")
	}
	job, err := store.GetJob(ctx, "broken-ask")
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ReadOnlyWorktreeDiffError == "" {
		t.Fatal("capture failure did not persist a durable error marker")
	}
	if persisted.ReadOnlyWorktreeDiff != "" || persisted.ReadOnlyWorktreeDiffTruncated {
		t.Fatalf("capture failure retained bogus diff data: %+v", persisted)
	}
}
