package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestTaskListQueriesStrandedDisposal(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, stderr.String())
	}
	store, err := dbtest.Open(t, filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTask(context.Background(), db.Task{ID: "task-stranded", RepoFullName: "owner/repo", State: string(workflow.TaskBlocked)}); err != nil {
		t.Fatal(err)
	}
	if changed, _, err := store.DisposeTask(context.Background(), "task-stranded", []string{string(workflow.TaskBlocked)}, string(workflow.TaskStranded), "tier4_stranded", "no disposal evidence; escalation unroutable: no live ancestor", "", "", time.Now()); err != nil || !changed {
		t.Fatalf("DisposeTask changed=%v err=%v", changed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"task", "list", "--home", home, "--state", "stranded", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("task list stranded code=%d stderr=%s", code, stderr.String())
	}
	var listed []taskListOutput
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "task-stranded" || listed[0].DisposalTier != "tier4_stranded" || !strings.Contains(listed[0].DisposalReason, "no disposal evidence") {
		t.Fatalf("stranded task list = %+v", listed)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"task", "list", "--home", home, "--state", "blocked", "--json"}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "[]" {
		t.Fatalf("blocked task list code=%d stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestTaskListFindsStrandedTaskWhenEscalationWriteFails(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"init", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, stderr.String())
	}
	store, err := dbtest.Open(t, filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.UpsertTask(ctx, db.Task{ID: "task-escalation-failed", RepoFullName: "owner/repo", State: string(workflow.TaskBlocked)}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER reject_task_disposal_escalation
		BEFORE INSERT ON wake_outbox
		WHEN NEW.source_kind = 'escalation'
		BEGIN
			SELECT RAISE(ABORT, 'forced task disposal escalation failure');
		END;`); err != nil {
		t.Fatal(err)
	}
	changed, state, disposalErr := store.DisposeTask(ctx, "task-escalation-failed",
		[]string{string(workflow.TaskBlocked)}, string(workflow.TaskStranded), "tier4_stranded",
		"no disposal evidence", "owner", `{"kind":"task_disposal"}`, time.Now())
	if !changed || state != string(workflow.TaskStranded) || disposalErr == nil ||
		!strings.Contains(disposalErr.Error(), "forced task disposal escalation failure") {
		t.Fatalf("DisposeTask changed=%v state=%q err=%v", changed, state, disposalErr)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"task", "list", "--home", home, "--state", "stranded", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("task list stranded code=%d stderr=%s", code, stderr.String())
	}
	var stranded []taskListOutput
	if err := json.Unmarshal(stdout.Bytes(), &stranded); err != nil {
		t.Fatal(err)
	}
	if len(stranded) != 1 || stranded[0].ID != "task-escalation-failed" ||
		!strings.Contains(stranded[0].DisposalReason, "escalation write failed") ||
		!strings.Contains(stranded[0].DisposalReason, "forced task disposal escalation failure") {
		t.Fatalf("stranded task list = %+v", stranded)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"task", "list", "--home", home, "--state", "blocked", "--json"}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "[]" {
		t.Fatalf("blocked task list code=%d stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
}
