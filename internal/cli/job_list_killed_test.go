package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestJobListKilledShowsOnlyAbandonedDeliveries drives the production CLI entry.
//
// #1726, reproduced on an isolated home: a daemon SIGTERM exits in 0.6s, kills
// its in-flight child, and nothing requeues the row - not even the daemon when
// it comes back. Historically 38 of 39 signal-shaped deaths were never
// recovered. Before this filter that population was reachable only by grepping
// failure messages for two different renderings of one fact, which is why
// nobody had counted it.
//
// The filter identifies; it does not requeue. Automatic requeue is deliberately
// refused a layer up, because a killed delivery may already have pushed a
// branch or posted a PR comment.
func TestJobListKilledShowsOnlyAbandonedDeliveries(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	cases := []struct {
		id     string
		events []string
		want   bool
	}{
		{
			id:     "killed-by-sigterm",
			events: []string{workflow.DeliverySignalKilledEvent},
			want:   true,
		},
		{
			id:     "killed-by-sighup",
			events: []string{workflow.DeliverySignalKilledEvent},
			want:   true,
		},
		{
			// An ordinary failure. The whole point of the filter is that these are
			// the majority and must not be listed.
			id:     "failed-for-its-own-reasons",
			events: nil,
		},
		{
			// A timeout. It failed, it may even render an exit status in the 128+N
			// range, and it is emphatically NOT abandoned work: it burned its full
			// wall. The classifier excludes it upstream, so no event exists here.
			id:     "timed-out",
			events: []string{"job_timeout"},
		},
	}
	for _, item := range cases {
		seedCLIJob(t, store, db.Job{
			ID:      item.id,
			Agent:   "worker",
			Type:    "review",
			State:   string(workflow.JobFailed),
			Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo"}),
		}, "failed")
		for _, kind := range item.events {
			if err := store.AddJobEvent(context.Background(), db.JobEvent{JobID: item.id, Kind: kind, Message: kind}); err != nil {
				t.Fatalf("AddJobEvent(%s, %s): %v", item.id, kind, err)
			}
		}
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "list", "--home", home, "--killed", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list --killed exit = %d, stderr=%s", code, stderr.String())
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("decode job list --killed --json: %v\n%s", err, stdout.String())
	}
	listed := map[string]bool{}
	for _, entry := range entries {
		var id string
		if raw, ok := entry["id"]; ok {
			_ = json.Unmarshal(raw, &id)
		}
		listed[id] = true
	}
	for _, item := range cases {
		if listed[item.id] != item.want {
			t.Errorf("--killed listed %q = %v, want %v (listed set: %v)", item.id, listed[item.id], item.want, listed)
		}
	}

	// The unfiltered listing must still show everything, or the filter has
	// changed the default rather than added a view.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list exit = %d, stderr=%s", code, stderr.String())
	}
	var all []map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &all); err != nil {
		t.Fatalf("decode job list --json: %v", err)
	}
	if len(all) != len(cases) {
		t.Fatalf("unfiltered job list returned %d entries, want %d: --killed must add a view, not change the default", len(all), len(cases))
	}
}

// TestJobListKilledOnAnEmptyStoreListsNothing pins the direction a filter must
// fail in. An absent evidence set means NO matching jobs, never all of them,
// and "no evidence yet" is the state every home starts in.
func TestJobListKilledOnAnEmptyStoreListsNothing(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	seedCLIJob(t, store, db.Job{
		ID:      "ordinary",
		Agent:   "worker",
		Type:    "review",
		State:   string(workflow.JobFailed),
		Payload: mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo"}),
	}, "failed")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "list", "--home", home, "--killed"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list --killed exit = %d, stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "ordinary") {
		t.Fatalf("--killed listed a job with no signal-kill evidence:\n%s", stdout.String())
	}
}
