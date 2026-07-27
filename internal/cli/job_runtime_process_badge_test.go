package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestRunJobListShowSurfaceRecordedRuntimeProcessLiveness(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	for _, item := range []struct {
		id       string
		state    workflow.JobState
		pid      int
		identity string
	}{
		{id: "alive", state: workflow.JobRunning, pid: 101, identity: "start-alive"},
		{id: "dead", state: workflow.JobRunning, pid: 202, identity: "start-dead"},
		{id: "no-pid", state: workflow.JobRunning},
		{id: "terminal", state: workflow.JobSucceeded, pid: 303, identity: "start-terminal"},
	} {
		seedCLIJob(t, store, db.Job{
			ID:    item.id,
			Agent: "worker",
			Type:  "ask",
			State: string(item.state),
			Payload: mustJobPayload(t, workflow.JobPayload{
				Repo:                "owner/repo",
				RuntimePID:          item.pid,
				RuntimePIDStartTime: item.identity,
			}),
		}, string(item.state))
	}

	probes := 0
	previous := jobRuntimeProcessLiveness
	jobRuntimeProcessLiveness = func(pid int, identity string) (bool, bool) {
		probes++
		switch pid {
		case 101:
			if identity != "start-alive" {
				t.Fatalf("alive identity = %q", identity)
			}
			return true, true
		case 202:
			if identity != "start-dead" {
				t.Fatalf("dead identity = %q", identity)
			}
			return false, true
		default:
			t.Fatalf("unexpected PID probe %d", pid)
			return false, false
		}
	}
	t.Cleanup(func() { jobRuntimeProcessLiveness = previous })

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list --json exit = %d, stderr=%s", code, stderr.String())
	}
	var entries []jobListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("decode job list: %v\n%s", err, stdout.String())
	}
	byID := make(map[string]jobListEntry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}
	if got := byID["alive"].RuntimeProcessActive; got == nil || !*got {
		t.Fatalf("alive runtime_process_active = %v, want true", got)
	}
	if got := byID["dead"].RuntimeProcessActive; got == nil || *got {
		t.Fatalf("dead runtime_process_active = %v, want false", got)
	}
	for _, id := range []string{"no-pid", "terminal"} {
		if got := byID[id].RuntimeProcessActive; got != nil {
			t.Fatalf("%s runtime_process_active = %v, want omitted", id, *got)
		}
	}
	if probes != 2 {
		t.Fatalf("PID probe calls = %d, want 2", probes)
	}

	for _, tc := range []struct {
		id      string
		wantNil bool
		want    bool
	}{
		{id: "alive", want: true},
		{id: "dead", want: false},
		{id: "no-pid", wantNil: true},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := Run([]string{"job", "show", tc.id, "--home", home, "--json"}, &stdout, &stderr); code != 0 {
			t.Fatalf("job show %s exit = %d, stderr=%s", tc.id, code, stderr.String())
		}
		var shown jobShowOutput
		if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
			t.Fatalf("decode job show %s: %v\n%s", tc.id, err, stdout.String())
		}
		if tc.wantNil {
			if shown.RuntimeProcessActive != nil {
				t.Fatalf("%s runtime_process_active = %v, want omitted", tc.id, *shown.RuntimeProcessActive)
			}
			continue
		}
		if shown.RuntimeProcessActive == nil || *shown.RuntimeProcessActive != tc.want {
			t.Fatalf("%s runtime_process_active = %v, want %v", tc.id, shown.RuntimeProcessActive, tc.want)
		}
	}
}
