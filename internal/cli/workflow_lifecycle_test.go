package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

func TestRunWorkflowAutoSettleOnce(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+`
[workflow]
auto_settle_after = "24h"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	seedWorkflowAutoSettleCLI(t, store, "release/merged", 41, "merged")
	seedWorkflowAutoSettleCLI(t, store, "release/open", 42, "open")
	now := time.Now().UTC().Add(72 * time.Hour)

	var stdout bytes.Buffer
	if err := runWorkflowAutoSettleOnce(ctx, paths, store, now, &stdout); err != nil {
		t.Fatalf("runWorkflowAutoSettleOnce: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "auto-settled workflow release/merged") ||
		strings.Contains(got, "auto-settled workflow release/open") {
		t.Fatalf("stdout = %q", got)
	}
	meta, err := store.GetWorkflowMeta(ctx, "release/merged")
	if err != nil || meta.Status != string(db.WorkflowStatusSettled) {
		t.Fatalf("merged workflow meta = %+v, err=%v", meta, err)
	}
	notes, err := store.ListWorkflowNotes(ctx, "release/merged", 0)
	if err != nil || len(notes) != 2 ||
		notes[1].Body != "[auto:workflow:settled] merged/closed PRs, quiet ≥ 24h0m0s" {
		t.Fatalf("merged workflow notes = %+v, err=%v", notes, err)
	}
	allMeta, err := store.ListWorkflowMeta(ctx)
	if err != nil {
		t.Fatalf("ListWorkflowMeta: %v", err)
	}
	if open := allMeta["release/open"]; open.Status == string(db.WorkflowStatusSettled) {
		t.Fatalf("open workflow unexpectedly settled: %+v", open)
	}

	seedWorkflowAutoSettleCLI(t, store, "release/disabled", 43, "merged")
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[workflow]
auto_settle_after = "0"
`), 0o600); err != nil {
		t.Fatalf("write disabled config: %v", err)
	}
	stdout.Reset()
	if err := runWorkflowAutoSettleOnce(ctx, paths, store, now, &stdout); err != nil {
		t.Fatalf("disabled runWorkflowAutoSettleOnce: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("disabled stdout = %q", stdout.String())
	}
	allMeta, err = store.ListWorkflowMeta(ctx)
	if err != nil {
		t.Fatalf("ListWorkflowMeta after disabled sweep: %v", err)
	}
	if disabled := allMeta["release/disabled"]; disabled.Status == string(db.WorkflowStatusSettled) {
		t.Fatalf("disabled workflow unexpectedly settled: %+v", disabled)
	}
}

func seedWorkflowAutoSettleCLI(t *testing.T, store *db.Store, workflowID string, pullRequest int, prState string) {
	t.Helper()
	ctx := context.Background()
	payload := fmt.Sprintf(`{"repo":"acme/widget","workflow_id":%q,"pull_request":%d}`, workflowID, pullRequest)
	if err := store.CreateJob(ctx, db.Job{
		ID:      fmt.Sprintf("job-%d", pullRequest),
		Agent:   "shell-worker",
		Type:    "ask",
		State:   "succeeded",
		Payload: payload,
	}); err != nil {
		t.Fatalf("CreateJob(%s): %v", workflowID, err)
	}
	if _, err := store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID: workflowID,
		Author:     "operator",
		Body:       "old human checkpoint",
	}); err != nil {
		t.Fatalf("InsertWorkflowNote(%s): %v", workflowID, err)
	}
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "acme/widget",
		Number:       int64(pullRequest),
		URL:          fmt.Sprintf("https://example.invalid/acme/widget/pull/%d", pullRequest),
		HeadBranch:   fmt.Sprintf("feature/%d", pullRequest),
		BaseBranch:   "main",
		State:        prState,
	}); err != nil {
		t.Fatalf("UpsertPullRequest(%d): %v", pullRequest, err)
	}
}
