package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// TestRunJobGatesListAndClearAutoResumes drives the full CLI surface (#682): list
// the gates, clear one (still blocked), then clear the last (auto-re-queues the
// blocked stage through RetryJob).
func TestRunJobGatesListAndClearAutoResumes(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedCLIJob(t, store, db.Job{
		ID:      "job-blocked",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobBlocked),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo"}),
	}, "blocked")
	if _, err := store.RecordJobGates(context.Background(), "job-blocked", []string{"API key", "R2 token"}); err != nil {
		t.Fatalf("RecordJobGates returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "gates", "job-blocked", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job gates exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "open\tAPI key") || !strings.Contains(stdout.String(), "2 gate(s), 2 open") {
		t.Fatalf("job gates list output = %q", stdout.String())
	}

	// Clear one -> still blocked, not resumed.
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"job", "gates", "clear", "job-blocked", "--need", "API key", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("clear one exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "not resumed") {
		t.Fatalf("clear one output = %q, want not resumed", stdout.String())
	}
	if job, _ := store.GetJob(context.Background(), "job-blocked"); job.State != string(workflow.JobBlocked) {
		t.Fatalf("state after one clear = %q, want blocked", job.State)
	}

	// Clear the rest -> auto-resume.
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"job", "gates", "clear", "job-blocked", "--all", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("clear all exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "resumed:") {
		t.Fatalf("clear all output = %q, want resumed", stdout.String())
	}
	job, err := store.GetJob(context.Background(), "job-blocked")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != string(workflow.JobQueued) {
		t.Fatalf("state after clearing all = %q, want queued", job.State)
	}
}

// TestRunJobGatesClearRequiresNeedOrAll proves the mutually-exclusive flag guard.
func TestRunJobGatesClearRequiresNeedOrAll(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedCLIJob(t, store, db.Job{
		ID:      "job-blocked",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobBlocked),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo"}),
	}, "blocked")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "gates", "clear", "job-blocked", "--home", home}, &stdout, &stderr); code != 2 {
		t.Fatalf("clear with neither flag exit = %d, want 2; stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "gates", "clear", "job-blocked", "--all", "--need", "x", "--home", home}, &stdout, &stderr); code != 2 {
		t.Fatalf("clear with both flags exit = %d, want 2; stderr=%s", code, stderr.String())
	}
}

// TestRunJobGatesListNoGates prints a friendly line for a job with no gates.
func TestRunJobGatesListNoGates(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedCLIJob(t, store, db.Job{
		ID:      "job-plain",
		Agent:   "audit",
		Type:    "ask",
		State:   string(workflow.JobFailed),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo"}),
	}, "failed")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "gates", "job-plain", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job gates exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no gates recorded") {
		t.Fatalf("output = %q, want no-gates message", stdout.String())
	}
}
