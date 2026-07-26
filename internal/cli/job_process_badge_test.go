package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestDeriveWorktreeProcessActive(t *testing.T) {
	withWorktree := mustJobPayload(t, workflow.JobPayload{WorktreePath: "/tmp/job-worktree"})
	tests := []struct {
		name       string
		job        db.Job
		live       bool
		known      bool
		want       bool
		wantProbes int
	}{
		{name: "succeeded live", job: db.Job{State: string(workflow.JobSucceeded), Payload: withWorktree}, live: true, known: true, want: true, wantProbes: 1},
		{name: "failed live", job: db.Job{State: string(workflow.JobFailed), Payload: withWorktree}, live: true, known: true, want: true, wantProbes: 1},
		{name: "cancelled live", job: db.Job{State: string(workflow.JobCancelled), Payload: withWorktree}, live: true, known: true, want: true, wantProbes: 1},
		{name: "blocked live", job: db.Job{State: string(workflow.JobBlocked), Payload: withWorktree}, live: true, known: true, want: true, wantProbes: 1},
		{name: "unknown liveness", job: db.Job{State: string(workflow.JobSucceeded), Payload: withWorktree}, live: false, known: false, wantProbes: 1},
		{name: "known inactive", job: db.Job{State: string(workflow.JobSucceeded), Payload: withWorktree}, live: false, known: true, wantProbes: 1},
		{name: "running short circuits", job: db.Job{State: string(workflow.JobRunning), Payload: withWorktree}, live: true, known: true},
		{name: "queued short circuits", job: db.Job{State: string(workflow.JobQueued), Payload: withWorktree}, live: true, known: true},
		{name: "no worktree", job: db.Job{State: string(workflow.JobSucceeded), Payload: mustJobPayload(t, workflow.JobPayload{})}},
		{name: "malformed payload", job: db.Job{State: string(workflow.JobSucceeded), Payload: "{"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probes := 0
			got := deriveWorktreeProcessActive(tc.job, func(path string) (bool, bool) {
				probes++
				if path != "/tmp/job-worktree" {
					t.Fatalf("probe path = %q", path)
				}
				return tc.live, tc.known
			})
			if got != tc.want {
				t.Fatalf("deriveWorktreeProcessActive = %v, want %v", got, tc.want)
			}
			if probes != tc.wantProbes {
				t.Fatalf("probe calls = %d, want %d", probes, tc.wantProbes)
			}
		})
	}
}

func TestRunJobListShowSurfaceWorktreeProcessBadge(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	jobs := []struct {
		id    string
		state workflow.JobState
		path  string
	}{
		{id: "live-terminal", state: workflow.JobSucceeded, path: "/tmp/live-worktree"},
		{id: "inactive-terminal", state: workflow.JobFailed, path: "/tmp/inactive-worktree"},
		{id: "unknown-terminal", state: workflow.JobCancelled, path: "/tmp/unknown-worktree"},
		{id: "blocked-live", state: workflow.JobBlocked, path: "/tmp/blocked-worktree"},
		{id: "running-live", state: workflow.JobRunning, path: "/tmp/running-worktree"},
		{id: "no-worktree", state: workflow.JobFailed},
	}
	for _, item := range jobs {
		seedCLIJob(t, store, db.Job{
			ID:      item.id,
			Agent:   "worker",
			Type:    "ask",
			State:   string(item.state),
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", WorktreePath: item.path}),
		}, string(item.state))
	}

	probes := map[string]int{}
	previous := jobWorktreeLiveness
	jobWorktreeLiveness = func(path string) (bool, bool) {
		probes[path]++
		switch path {
		case "/tmp/live-worktree", "/tmp/blocked-worktree":
			return true, true
		case "/tmp/inactive-worktree":
			return false, true
		case "/tmp/unknown-worktree":
			return false, false
		case "/tmp/running-worktree":
			t.Fatalf("running job unexpectedly invoked liveness probe")
			return true, true
		default:
			t.Fatalf("unexpected liveness probe path %q", path)
			return false, false
		}
	}
	t.Cleanup(func() { jobWorktreeLiveness = previous })

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list --json exit = %d, stderr=%s", code, stderr.String())
	}
	var entries []jobListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("decode job list --json: %v\n%s", err, stdout.String())
	}
	var rawEntries []map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &rawEntries); err != nil {
		t.Fatalf("decode raw job list --json: %v", err)
	}
	for i, entry := range entries {
		_, fieldPresent := rawEntries[i]["process_active"]
		want := entry.ID == "live-terminal" || entry.ID == "blocked-live"
		if entry.ProcessActive != want || fieldPresent != want {
			t.Fatalf("entry %s process_active = %v, present=%v, want %v", entry.ID, entry.ProcessActive, fieldPresent, want)
		}
	}
	if probes["/tmp/running-worktree"] != 0 {
		t.Fatalf("running worktree probe calls = %d", probes["/tmp/running-worktree"])
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list exit = %d, stderr=%s", code, stderr.String())
	}
	lines := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		id, _, _ := strings.Cut(line, "\t")
		lines[id] = line
	}
	for _, id := range []string{"live-terminal", "blocked-live"} {
		if !strings.Contains(lines[id], "LIVE PROCESS: worktree still has an active process") {
			t.Fatalf("%s line missing live-process badge: %q", id, lines[id])
		}
	}
	if got := lines["inactive-terminal"]; got != "inactive-terminal\tfailed\task\tworker\towner/repo\t#0" {
		t.Fatalf("healthy terminal line changed: %q", got)
	}
	for _, id := range []string{"unknown-terminal", "running-live", "no-worktree"} {
		if strings.Contains(lines[id], "LIVE PROCESS:") {
			t.Fatalf("%s line unexpectedly has live-process badge: %q", id, lines[id])
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", "live-terminal", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("live job show --json exit = %d, stderr=%s", code, stderr.String())
	}
	var shown jobShowOutput
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
		t.Fatalf("decode live job show --json: %v\n%s", err, stdout.String())
	}
	if !shown.ProcessActive || !strings.Contains(stdout.String(), `"process_active": true`) {
		t.Fatalf("live job show missing process_active: %+v\n%s", shown, stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", "inactive-terminal", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("inactive job show --json exit = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "process_active") {
		t.Fatalf("inactive job show must omit process_active:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", "live-terminal", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("live job show exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "process_active: worktree still has an active process") {
		t.Fatalf("live job show missing process_active line:\n%s", stdout.String())
	}
}
