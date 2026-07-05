package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func seedSessionAgentRepo(t *testing.T, store *db.Store) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", Enabled: true}); err != nil {
		t.Fatalf("UpsertRepo returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:         "lead",
		Role:         "agent",
		Runtime:      runtime.ShellRuntime,
		RuntimeRef:   "printf ok",
		RepoScope:    "owner/repo",
		Capabilities: []string{"ask", "review", "implement"},
		HealthStatus: "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
}

// TestJobOpenCreatesRunningSessionJob proves `job open` creates a running,
// externally_driven job with no queued row (no dispatch).
func TestJobOpenCreatesRunningSessionJob(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--title", "lead session", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
	}
	var out jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode job open JSON: %v (%s)", err, stdout.String())
	}
	if out.State != string(workflow.JobRunning) || !out.ExternallyDriven || out.Type != "ask" || out.Repo != "owner/repo" {
		t.Fatalf("job open output = %+v", out)
	}

	stored, err := store.GetJob(context.Background(), out.JobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(workflow.JobRunning) || !stored.ExternallyDriven {
		t.Fatalf("stored job = %+v, want running externally_driven", stored)
	}
	queued, err := store.ListQueuedJobs(context.Background())
	if err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	}
	if len(queued) != 0 {
		t.Fatalf("queued = %d, want 0", len(queued))
	}
}

// TestJobCloseAppliesDecision proves open+close moves the job to its terminal state
// with the session's result.
func TestJobCloseAppliesDecision(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "review", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
	}
	var opened jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &opened); err != nil {
		t.Fatalf("decode open JSON: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "close", opened.JobID, "--home", home, "--decision", "changes_requested", "--summary", "needs work", "--pr", "9", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job close exit = %d, stderr=%s", code, stderr.String())
	}
	var closed jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &closed); err != nil {
		t.Fatalf("decode close JSON: %v", err)
	}
	if closed.State != string(workflow.JobSucceeded) || closed.Decision != "changes_requested" || closed.PullRequest != 9 {
		t.Fatalf("close output = %+v", closed)
	}
	stored, err := store.GetJob(context.Background(), opened.JobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(workflow.JobSucceeded) {
		t.Fatalf("stored state = %q, want succeeded", stored.State)
	}
}

// TestJobRecordOneShotTerminal proves `job record` creates an already-terminal job.
func TestJobRecordOneShotTerminal(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"job", "record", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "implement", "--decision", "implemented", "--summary", "done", "--pr", "12", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("job record exit = %d, stderr=%s", code, stderr.String())
	}
	var out jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode record JSON: %v", err)
	}
	if out.State != string(workflow.JobSucceeded) || out.Decision != "implemented" || !out.ExternallyDriven {
		t.Fatalf("record output = %+v", out)
	}
	stored, err := store.GetJob(context.Background(), out.JobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(workflow.JobSucceeded) || !stored.ExternallyDriven {
		t.Fatalf("stored job = %+v", stored)
	}
}

// TestJobSessionValidationErrors proves the CLI rejects bad input cleanly.
func TestJobSessionValidationErrors(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"invalid type", []string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "bogus"}, 2},
		{"invalid decision", []string{"job", "record", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--decision", "bogus"}, 2},
		{"missing agent", []string{"job", "open", "--home", home, "--repo", "owner/repo", "--type", "ask"}, 2},
		{"unknown agent", []string{"job", "open", "--home", home, "--agent", "ghost", "--repo", "owner/repo", "--type", "ask"}, 1},
		{"untracked repo", []string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/nope", "--type", "ask"}, 1},
		{"close unknown id", []string{"job", "close", "no-such-job", "--home", home, "--decision", "approved"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(tc.args, &stdout, &stderr); code != tc.want {
				t.Fatalf("exit = %d, want %d; stderr=%s", code, tc.want, stderr.String())
			}
		})
	}
}

// TestJobCloseDoubleCloseFails proves a session job can be closed exactly once.
func TestJobCloseDoubleCloseFails(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedSessionAgentRepo(t, store)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "open", "--home", home, "--agent", "lead", "--repo", "owner/repo", "--type", "ask", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job open exit = %d, stderr=%s", code, stderr.String())
	}
	var opened jobSessionOutput
	if err := json.Unmarshal(stdout.Bytes(), &opened); err != nil {
		t.Fatalf("decode open JSON: %v", err)
	}
	if code := Run([]string{"job", "close", opened.JobID, "--home", home, "--decision", "approved"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("first close failed")
	}
	stderr.Reset()
	if code := Run([]string{"job", "close", opened.JobID, "--home", home, "--decision", "approved"}, &bytes.Buffer{}, &stderr); code != 1 {
		t.Fatalf("second close exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already been closed") {
		t.Fatalf("double-close stderr = %q, want already-been-closed", stderr.String())
	}
}
