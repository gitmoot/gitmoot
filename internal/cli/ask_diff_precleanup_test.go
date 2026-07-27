package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	notRepo := filepath.Join(t.TempDir(), "token="+secret)
	if err := os.Mkdir(notRepo, 0o700); err != nil {
		t.Fatal(err)
	}
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
	if strings.Contains(persisted.ReadOnlyWorktreeDiffError, secret) || !strings.Contains(persisted.ReadOnlyWorktreeDiffError, "token=[REDACTED]") {
		t.Fatalf("capture failure marker did not redact hostile content: %q", persisted.ReadOnlyWorktreeDiffError)
	}
	if persisted.ReadOnlyWorktreeDiff != "" || persisted.ReadOnlyWorktreeDiffTruncated {
		t.Fatalf("capture failure retained bogus diff data: %+v", persisted)
	}
}

func TestCaptureReadOnlyWorktreeDiffDoesNotRunConfiguredGitHelpers(t *testing.T) {
	ctx := context.Background()
	checkout, _ := gitFixtureRepo(t, "initial\n")
	attributes := "marker.txt filter=hostile diff=hostile\n"
	if err := os.WriteFile(filepath.Join(checkout, ".gitattributes"), []byte(attributes), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, checkout, "add", ".gitattributes")
	runGit(t, checkout, "commit", "-m", "add attributes")

	probeDir := t.TempDir()
	fsmonitorSentinel := filepath.Join(probeDir, "fsmonitor-fired")
	cleanSentinel := filepath.Join(probeDir, "clean-fired")
	textconvSentinel := filepath.Join(probeDir, "textconv-fired")
	fsmonitor := writeExecutableGitHelper(t, probeDir, "fsmonitor.sh",
		fmt.Sprintf("#!/bin/sh\nprintf fired > %q\nprintf 'token\\0'\n", fsmonitorSentinel))
	clean := writeExecutableGitHelper(t, probeDir, "clean.sh",
		fmt.Sprintf("#!/bin/sh\nprintf fired > %q\ncat\n", cleanSentinel))
	textconv := writeExecutableGitHelper(t, probeDir, "textconv.sh",
		fmt.Sprintf("#!/bin/sh\nprintf fired > %q\nprintf HOSTILE-TEXTCONV\ncat \"$1\"\n", textconvSentinel))

	runGit(t, checkout, "config", "core.fsmonitor", fsmonitor)
	runGit(t, checkout, "config", "filter.hostile.clean", clean)
	runGit(t, checkout, "config", "diff.hostile.textconv", textconv)
	if err := os.WriteFile(filepath.Join(checkout, "marker.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Prove the fixture is live and would execute all three helpers under ordinary
	// repository-configured Git commands. Each probe is isolated so an fsmonitor
	// protocol warning cannot mask the attribute-selected helper checks.
	runGitProbeIgnoringFailure(checkout, "status", "--short")
	assertPathExists(t, fsmonitorSentinel, "ordinary git status did not invoke the configured fsmonitor")
	_ = os.Remove(fsmonitorSentinel)
	if err := os.WriteFile(filepath.Join(checkout, "marker.txt"), []byte("changed again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitProbeIgnoringFailure(checkout, "-c", "core.fsmonitor=false", "status", "--short")
	assertPathExists(t, cleanSentinel, "ordinary git status did not invoke the configured clean filter")
	_ = os.Remove(cleanSentinel)
	runGitProbeIgnoringFailure(checkout, "-c", "core.fsmonitor=false", "diff", "--textconv", "HEAD", "--", "marker.txt")
	assertPathExists(t, textconvSentinel, "ordinary git diff did not invoke the configured textconv")
	_ = os.Remove(textconvSentinel)
	_ = os.Remove(cleanSentinel)

	snapshot, truncated, err := captureReadOnlyWorktreeDiff(ctx, checkout)
	if err != nil {
		t.Fatalf("isolated capture: %v", err)
	}
	if truncated {
		t.Fatal("small adversarial fixture was unexpectedly truncated")
	}
	if !strings.Contains(snapshot, "marker.txt") || !strings.Contains(snapshot, "+changed again") {
		t.Fatalf("isolated capture lost the actual edit:\n%s", snapshot)
	}
	for _, sentinel := range []string{fsmonitorSentinel, cleanSentinel, textconvSentinel} {
		if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
			t.Fatalf("repository-configured helper executed during isolated capture: sentinel=%s stat=%v", sentinel, err)
		}
	}
	if strings.Contains(snapshot, "HOSTILE-TEXTCONV") {
		t.Fatalf("configured textconv output leaked into captured payload:\n%s", snapshot)
	}
}

func writeExecutableGitHelper(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func runGitProbeIgnoringFailure(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	_ = cmd.Run()
}

func assertPathExists(t *testing.T, path, message string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s: %v", message, err)
	}
}
