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

func TestRunTaskHoldAndUnholdAppendAuditEventsWithoutChangingState(t *testing.T) {
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	defer store.Close()
	task := db.Task{
		ID: "task-1", RepoFullName: "owner/repo", State: string(workflow.TaskChangesRequested), Branch: "feature/one",
	}
	if err := store.UpsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"task", "hold", task.ID, "--home", home, "--reason", "coordinator inspection", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("hold code=%d stderr=%s", code, stderr.String())
	}
	var held taskHoldOutput
	if err := json.Unmarshal(stdout.Bytes(), &held); err != nil {
		t.Fatalf("decode hold output %q: %v", stdout.String(), err)
	}
	if held.TaskID != task.ID || !held.Held || held.Source != "manual" || held.Reason != "coordinator inspection" {
		t.Fatalf("hold output = %+v", held)
	}
	stored, err := store.GetTask(context.Background(), task.ID)
	if err != nil || stored.State != task.State {
		t.Fatalf("stored task = %+v err=%v", stored, err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"task", "unhold", task.ID, "--home", home, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("unhold code=%d stderr=%s", code, stderr.String())
	}
	var cleared taskHoldOutput
	if err := json.Unmarshal(stdout.Bytes(), &cleared); err != nil {
		t.Fatalf("decode unhold output %q: %v", stdout.String(), err)
	}
	if cleared.TaskID != task.ID || cleared.Held || cleared.Source != "manual" {
		t.Fatalf("unhold output = %+v", cleared)
	}
	events, err := store.ListTaskEvents(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 ||
		events[0].Kind != workflow.TaskHoldSetManualEventKind ||
		events[0].Reason != "coordinator inspection" ||
		events[1].Kind != workflow.TaskHoldClearedManualEventKind {
		t.Fatalf("task events = %+v", events)
	}
}

func TestTaskAndAgentHelpDescribeAdvancementControlsAccurately(t *testing.T) {
	var taskHelp bytes.Buffer
	printTaskUsage(&taskHelp)
	for _, want := range []string{"task hold <id>", "task unhold <id>"} {
		if !strings.Contains(taskHelp.String(), want) {
			t.Fatalf("task help %q missing %q", taskHelp.String(), want)
		}
	}

	var agentHelp bytes.Buffer
	printAgentRunUsage(&agentHelp, "implement")
	if !strings.Contains(agentHelp.String(), "suppresses only competing native reviewer fan-out (#371)") ||
		!strings.Contains(agentHelp.String(), "does not stop review-to-fix advancement") {
		t.Fatalf("agent implement help = %q", agentHelp.String())
	}
}
