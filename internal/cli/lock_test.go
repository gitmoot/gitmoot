package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestRunLockListShowRelease(t *testing.T) {
	home := t.TempDir()
	store := openCLILockStore(t, home)
	defer store.Close()
	acquireCLILock(t, store, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-9", Owner: "lead"})
	acquireCLILock(t, store, db.BranchLock{RepoFullName: "owner/other", Branch: "task-1", Owner: "audit"})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"lock", "list", "--home", home, "--repo", "owner/repo"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("lock list exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "owner/repo\ttask-9\tlead") || strings.Contains(stdout.String(), "owner/other") {
		t.Fatalf("lock list output = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"lock", "show", "owner/repo", "task-9", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("lock show exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{"repo: owner/repo", "branch: task-9", "owner: lead"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("lock show missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"lock", "release", "owner/repo", "task-9", "--home", home, "--owner", "lead"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("lock release exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "released lock owner/repo task-9 owned by lead") {
		t.Fatalf("lock release output = %q", stdout.String())
	}
	events, err := store.ListBranchLockEvents(context.Background(), "owner/repo", "task-9")
	if err != nil {
		t.Fatalf("ListBranchLockEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "released" || events[0].Owner != "lead" {
		t.Fatalf("events = %+v", events)
	}
}

func TestRunLockReleaseRequiresOwnerUnlessForced(t *testing.T) {
	home := t.TempDir()
	store := openCLILockStore(t, home)
	defer store.Close()
	acquireCLILock(t, store, db.BranchLock{RepoFullName: "owner/repo", Branch: "task-9", Owner: "lead"})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"lock", "release", "owner/repo", "task-9", "--home", home, "--owner", "other"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("wrong owner exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "owned by lead, not other") {
		t.Fatalf("wrong owner stderr = %q", stderr.String())
	}
	if _, err := store.GetBranchLock(context.Background(), "owner/repo", "task-9"); err != nil {
		t.Fatalf("lock should still exist after wrong owner: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"lock", "release", "owner/repo", "task-9", "--home", home}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("missing owner exit code = %d, stderr=%s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"lock", "release", "owner/repo", "task-9", "--home", home, "--force"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("force release exit code = %d, stderr=%s", code, stderr.String())
	}
	events, err := store.ListBranchLockEvents(context.Background(), "owner/repo", "task-9")
	if err != nil {
		t.Fatalf("ListBranchLockEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "force_released" || events[0].Owner != "lead" {
		t.Fatalf("force events = %+v", events)
	}
}

func openCLILockStore(t *testing.T, home string) *db.Store {
	t.Helper()
	return openCLIJobStore(t, home)
}

func acquireCLILock(t *testing.T, store *db.Store, lock db.BranchLock) {
	t.Helper()
	acquired, err := store.AcquireLock(context.Background(), lock)
	if err != nil {
		t.Fatalf("AcquireLock returned error: %v", err)
	}
	if !acquired {
		t.Fatalf("AcquireLock did not acquire %+v", lock)
	}
}
