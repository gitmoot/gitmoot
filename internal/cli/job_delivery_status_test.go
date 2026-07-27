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

func TestRunJobListShowSurfaceDeliveryStatus(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()

	jobs := []struct {
		id          string
		jobType     string
		pullRequest int
		events      []string
		want        string
	}{
		{id: "completed", jobType: "implement", events: []string{"advance_started", "advance_completed"}, want: jobDeliveryStatusDelivered},
		{id: "pull-request", jobType: "implement", pullRequest: 42, want: jobDeliveryStatusDelivered},
		{id: "pending-started", jobType: "implement", events: []string{"advance_started"}, want: jobDeliveryStatusPending},
		{id: "pending-retry", jobType: "implement", events: []string{"advance_started", "advance_retry"}, want: jobDeliveryStatusPending},
		{id: "blocked", jobType: "implement", events: []string{"advance_started", "advance_retry", "advance_blocked"}, want: jobDeliveryStatusBlocked},
		{id: "old-style", jobType: "implement"},
		{id: "non-implement", jobType: "review", pullRequest: 42, events: []string{"advance_completed"}},
	}
	for _, item := range jobs {
		seedCLIJob(t, store, db.Job{
			ID:    item.id,
			Agent: "worker",
			Type:  item.jobType,
			State: string(workflow.JobSucceeded),
			Payload: mustJobPayload(t, workflow.JobPayload{
				Repo:        "owner/repo",
				PullRequest: item.pullRequest,
			}),
		}, "succeeded")
		for _, kind := range item.events {
			if err := store.AddJobEvent(context.Background(), db.JobEvent{JobID: item.id, Kind: kind, Message: kind}); err != nil {
				t.Fatalf("AddJobEvent(%s, %s): %v", item.id, kind, err)
			}
		}
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list --json exit = %d, stderr=%s", code, stderr.String())
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("decode job list --json: %v\n%s", err, stdout.String())
	}
	listStatuses := make(map[string]string, len(entries))
	listPresence := make(map[string]bool, len(entries))
	for _, entry := range entries {
		var id string
		if err := json.Unmarshal(entry["id"], &id); err != nil {
			t.Fatalf("decode job id: %v", err)
		}
		raw, present := entry["delivery_status"]
		listPresence[id] = present
		if present {
			var status string
			if err := json.Unmarshal(raw, &status); err != nil {
				t.Fatalf("decode %s delivery_status: %v", id, err)
			}
			listStatuses[id] = status
		}
	}
	for _, item := range jobs {
		if got := listStatuses[item.id]; got != item.want {
			t.Errorf("job list %s delivery_status = %q, want %q", item.id, got, item.want)
		}
		if got := listPresence[item.id]; got != (item.want != "") {
			t.Errorf("job list %s delivery_status presence = %v, want %v", item.id, got, item.want != "")
		}
	}

	for _, item := range jobs {
		stdout.Reset()
		stderr.Reset()
		if code := Run([]string{"job", "show", item.id, "--home", home, "--json"}, &stdout, &stderr); code != 0 {
			t.Fatalf("job show %s --json exit = %d, stderr=%s", item.id, code, stderr.String())
		}
		var shown map[string]json.RawMessage
		if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
			t.Fatalf("decode job show %s --json: %v\n%s", item.id, err, stdout.String())
		}
		raw, present := shown["delivery_status"]
		if present != (item.want != "") {
			t.Errorf("job show %s delivery_status presence = %v, want %v", item.id, present, item.want != "")
		}
		if present {
			var got string
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode job show %s delivery_status: %v", item.id, err)
			}
			if got != item.want {
				t.Errorf("job show %s delivery_status = %q, want %q", item.id, got, item.want)
			}
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", "pending-retry", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job show pending-retry exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "delivery_status: pending") {
		t.Fatalf("job show missing delivery status:\n%s", stdout.String())
	}
}

func TestDeriveJobDeliveryStatusLatestMarkerWinsAndUnknownStaysSilent(t *testing.T) {
	job := db.Job{Type: "implement"}
	payload := workflow.JobPayload{}

	tests := []struct {
		name   string
		events []db.JobEvent
		want   string
	}{
		{name: "blocked after retry", events: []db.JobEvent{{Kind: "advance_retry"}, {Kind: "advance_blocked"}}, want: jobDeliveryStatusBlocked},
		{name: "retry after blocked", events: []db.JobEvent{{Kind: "advance_blocked"}, {Kind: "advance_retry"}}, want: jobDeliveryStatusPending},
		{name: "completed after retry", events: []db.JobEvent{{Kind: "advance_retry"}, {Kind: "advance_completed"}}, want: jobDeliveryStatusDelivered},
		{name: "retry queued suppresses stale pending", events: []db.JobEvent{{Kind: "advance_retry"}, {Kind: "retry_queued"}}},
		{name: "awaiting human stays unknown", events: []db.JobEvent{{Kind: "advance_retry"}, {Kind: "advance_awaiting_human"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			latest, ok := latestDeliveryStatusEvent(tc.events)
			if got := deriveJobDeliveryStatus(job, payload, latest, ok); got != tc.want {
				t.Fatalf("deriveJobDeliveryStatus = %q, want %q", got, tc.want)
			}
		})
	}
}
