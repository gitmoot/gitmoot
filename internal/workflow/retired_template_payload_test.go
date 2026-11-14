package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestRetryKeepsRetiredTemplateSnapshotFields pins #2589's review round 1:
// agent templates are gone, but stored payloads still carry their snapshot
// (template_* and the older preset_* spelling). A typed read-modify-write
// such as RetryJob must keep that history byte-for-byte as unknown members,
// never drop it.
func TestRetryKeepsRetiredTemplateSnapshotFields(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	retired := []string{
		`"preset_id":"reviewer"`,
		`"preset_resolved_commit":"abc123"`,
		`"preset_content":"legacy prompt"`,
		`"template_id":"review-panel"`,
		`"template_resolved_commit":"def456"`,
		`"template_content":"newer prompt"`,
	}
	stored := `{"repo":"gitmoot/gitmoot","pull_request":9,"head_sha":"head123",` + strings.Join(retired, ",") + `}`
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "review-legacy", Agent: "audit", Type: "review", State: string(JobFailed), Payload: stored,
	}, db.JobEvent{Kind: string(JobFailed), Message: "failed"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	job, err := RetryJob(ctx, store, "review-legacy")
	if err != nil {
		t.Fatalf("RetryJob returned error: %v", err)
	}
	for _, member := range retired {
		if !strings.Contains(job.Payload, member) {
			t.Fatalf("payload after retry = %s, lost retired member %s", job.Payload, member)
		}
	}
}
