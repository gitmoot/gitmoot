package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TestReviewRunCommandsAreLimitedToReviews pins #2601: `review list|show`
// are the run commands restricted to runs of type review, and the old
// `job` command prefix still reaches the run commands with a notice.
func TestReviewRunCommandsAreLimitedToReviews(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	payload := mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 7})
	seedCLIJob(t, store, db.Job{ID: "review-run", Agent: "audit", Type: "review", State: string(workflow.JobSucceeded), Payload: payload}, "succeeded")
	seedCLIJob(t, store, db.Job{ID: "ask-run", Agent: "audit", Type: "ask", State: string(workflow.JobSucceeded), Payload: payload}, "succeeded")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"review", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("review list exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "review-run") || strings.Contains(stdout.String(), "ask-run") {
		t.Fatalf("review list output = %q, want only the review run", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"review", "show", "review-run", "--home", home}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "id: review-run") {
		t.Fatalf("review show exit = %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"review", "show", "ask-run", "--home", home}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "not a review; use gitmoot run show") {
		t.Fatalf("review show of an ask run: exit = %d stderr=%q, want a refusal naming run show", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", "ask-run", "--home", home}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "id: ask-run") || !strings.Contains(stderr.String(), `"job" is now "run"`) {
		t.Fatalf("job show alias: exit = %d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
