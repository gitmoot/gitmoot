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

// TestReviewCommandsCheckTypeInTheHomeTheyRead pins the #2647 review finding:
// the review-type check must run against the run the shared handler reads, in
// the home its flag set resolves. A token scan read `--home=B` out of the
// --log-path value, found a review there, and then the watch emitted the ask
// stored under the same id in home A.
func TestReviewCommandsCheckTypeInTheHomeTheyRead(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	storeA := openCLIJobStore(t, homeA)
	defer storeA.Close()
	storeB := openCLIJobStore(t, homeB)
	defer storeB.Close()
	payload := mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 7})
	seedCLIJob(t, storeA, db.Job{ID: "shared-id", Agent: "audit", Type: "ask", State: string(workflow.JobBlocked), Payload: payload}, "blocked")
	seedCLIJob(t, storeB, db.Job{ID: "shared-id", Agent: "audit", Type: "review", State: string(workflow.JobSucceeded), Payload: payload}, "succeeded")

	for _, args := range [][]string{
		{"review", "watch", "shared-id", "--home", homeA, "--log-path", "--home=" + homeB, "--json"},
		{"review", "show", "shared-id", "--home", homeA, "--json"},
		{"review", "cancel", "shared-id", "--home", homeA},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(args, &stdout, &stderr)
		if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "shared-id is a ask run, not a review") {
			t.Fatalf("%v: exit = %d stdout=%q stderr=%q, want a refusal of home A's ask", args[:2], code, stdout.String(), stderr.String())
		}
	}
	if job, err := storeA.GetJob(context.Background(), "shared-id"); err != nil || job.State != string(workflow.JobBlocked) {
		t.Fatalf("home A ask after refused review cancel = %+v, %v; want still blocked", job, err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"review", "watch", "shared-id", "--home", homeB, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("review watch in home B: exit = %d stderr=%q", code, stderr.String())
	}
	var watched struct {
		Run map[string]any `json:"run"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &watched); err != nil || watched.Run["Type"] != "review" {
		t.Fatalf("review watch in home B = %q (%v), want home B's review", stdout.String(), err)
	}
}

// TestRunShowAndWatchJSONUseRunKeys pins the #2601 --json projection: the
// run object and its events carry the renamed keys and not the old ones,
// while the keys fleet scripts read (ID, State, UpdatedAt) are unchanged.
func TestRunShowAndWatchJSONUseRunKeys(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	payload := mustJobPayload(t, workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 7, WorkflowID: "camp-1"})
	seedCLIJob(t, store, db.Job{ID: "parent-run", Agent: "audit", Type: "review", State: string(workflow.JobSucceeded), Payload: payload}, "succeeded")
	seedCLIJob(t, store, db.Job{ID: "child-run", Agent: "audit", Type: "review", State: string(workflow.JobSucceeded), Payload: payload, ParentJobID: "parent-run", WorkflowID: "camp-1"}, "succeeded")

	for _, command := range []string{"show", "watch"} {
		var stdout, stderr bytes.Buffer
		if code := Run([]string{"run", command, "child-run", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
			t.Fatalf("run %s --json exit = %d stderr=%q", command, code, stderr.String())
		}
		var decoded struct {
			Run    map[string]any   `json:"run"`
			Events []map[string]any `json:"events"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
			t.Fatalf("run %s --json decode: %v\n%s", command, err, stdout.String())
		}
		want := map[string]any{"ID": "child-run", "State": "succeeded", "Reviewer": "audit", "ParentRunID": "parent-run", "campaign": "camp-1"}
		for key, value := range want {
			if decoded.Run[key] != value {
				t.Fatalf("run %s --json run[%q] = %v, want %v\n%s", command, key, decoded.Run[key], value, stdout.String())
			}
		}
		if _, ok := decoded.Run["UpdatedAt"]; !ok {
			t.Fatalf("run %s --json lost run.UpdatedAt\n%s", command, stdout.String())
		}
		for _, old := range []string{"Agent", "ParentJobID", "workflow_id"} {
			if _, ok := decoded.Run[old]; ok {
				t.Fatalf("run %s --json still has run[%q]\n%s", command, old, stdout.String())
			}
		}
		if command == "watch" {
			if len(decoded.Events) == 0 {
				t.Fatalf("run watch --json has no events\n%s", stdout.String())
			}
			for _, event := range decoded.Events {
				if event["RunID"] != "child-run" {
					t.Fatalf("run watch --json event RunID = %v, want child-run", event["RunID"])
				}
				if _, ok := event["JobID"]; ok {
					t.Fatalf("run watch --json event still has JobID: %v", event)
				}
			}
		}
	}
}
