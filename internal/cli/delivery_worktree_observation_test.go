package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestDeliveryWorktreeObservationHydratesOrdinaryImplementCheckout(t *testing.T) {
	ctx := context.Background()
	checkout := deliveryObservationRepo(t)
	if err := os.WriteFile(filepath.Join(checkout, "ordinary.go"), []byte("package ordinary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openCLIJobStore(t, t.TempDir())
	defer store.Close()

	mailbox := workflow.NewMailbox(store, deliveryWorktreeResolver(store, checkout))
	var importedInto string
	mailbox.CollectChangeSet = func(context.Context, execbackend.Backend, string) (*execbackend.ChangeSet, error) {
		return &execbackend.ChangeSet{}, nil
	}
	mailbox.ApplyChangeSet = func(_ context.Context, worktree string, _ execbackend.ChangeSet) error {
		importedInto = worktree
		return nil
	}
	if _, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "ordinary-observation", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	adapter := &cliWorkerFakeAdapter{output: deliveryObservationResult("updated ordinary.go")}
	if _, err := mailbox.Run(ctx, "ordinary-observation", runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime}, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}

	job, err := store.GetJob(ctx, "ordinary-observation")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	if payload.WorktreePath != "" {
		t.Fatalf("persisted WorktreePath = %q, want unchanged empty payload field", payload.WorktreePath)
	}
	if importedInto != checkout {
		t.Fatalf("change set imported into %q, want resolved checkout %q", importedInto, checkout)
	}
	observation := payload.ResultObservation
	if observation == nil {
		t.Fatal("persisted result_observation is nil; ordinary implement observation did not run")
	}
	if observation.Source != workflow.ResultObservationSourceWorktreeDiff {
		t.Fatalf("observation source = %q, want %q", observation.Source, workflow.ResultObservationSourceWorktreeDiff)
	}
	if got, want := fmt.Sprint(observation.TouchedFiles), "[ordinary.go]"; got != want {
		t.Fatalf("touched files = %s, want %s", got, want)
	}
	if observation.Divergent || observation.Error != "" {
		t.Fatalf("ordinary observation = %+v, want clean matching observation", observation)
	}
}

func TestDeliveryWorktreeObservationMarksWorktreeLessDelegationChild(t *testing.T) {
	ctx := context.Background()
	checkout := deliveryObservationRepo(t)
	store := openCLIJobStore(t, t.TempDir())
	defer store.Close()
	if err := store.UpsertTask(ctx, db.Task{ID: "shared-checkout-task", RepoFullName: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	mailbox := workflow.NewMailbox(store, deliveryWorktreeResolver(store, checkout))
	if _, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "worktree-less-child-observation", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot",
		TaskID: "shared-checkout-task", ParentJobID: "parent", DelegationID: "child",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	adapter := &cliWorkerFakeAdapter{output: deliveryObservationResult("updated imaginary.go")}
	if _, err := mailbox.Run(ctx, "worktree-less-child-observation", runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime}, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}

	job, err := store.GetJob(ctx, "worktree-less-child-observation")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	observation := payload.ResultObservation
	if observation == nil {
		t.Fatal("excluded worktree-less delegation child persisted a nil observation")
	}
	if observation.Source != workflow.ResultObservationSourceWorktreeLessDelegationChild {
		t.Fatalf("observation source = %q, want %q", observation.Source, workflow.ResultObservationSourceWorktreeLessDelegationChild)
	}
	if observation.Divergent || observation.Error != "" {
		t.Fatalf("excluded observation = %+v, want a typed non-error marker", observation)
	}
}

func TestDeliveryWorktreeObservationUsesTaskWorktreeForDelegationChild(t *testing.T) {
	ctx := context.Background()
	checkout := deliveryObservationRepo(t)
	if err := os.WriteFile(filepath.Join(checkout, "task.go"), []byte("package task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openCLIJobStore(t, t.TempDir())
	defer store.Close()
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-worktree", RepoFullName: "gitmoot/gitmoot", WorktreePath: checkout,
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	mailbox := workflow.NewMailbox(store, deliveryWorktreeResolver(store, checkout))
	if _, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "task-worktree-child-observation", Agent: "audit", Action: "implement", Repo: "gitmoot/gitmoot",
		TaskID: "task-worktree", ParentJobID: "parent", DelegationID: "child",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	adapter := &cliWorkerFakeAdapter{output: deliveryObservationResult("updated task.go")}
	if _, err := mailbox.Run(ctx, "task-worktree-child-observation", runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime}, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}

	job, err := store.GetJob(ctx, "task-worktree-child-observation")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatal(err)
	}
	if payload.WorktreePath != "" {
		t.Fatalf("persisted WorktreePath = %q, want empty task-backed delegation payload", payload.WorktreePath)
	}
	observation := payload.ResultObservation
	if observation == nil {
		t.Fatal("task-worktree delegation child persisted a nil observation")
	}
	if observation.Source != workflow.ResultObservationSourceWorktreeDiff {
		t.Fatalf("observation source = %q, want %q", observation.Source, workflow.ResultObservationSourceWorktreeDiff)
	}
	if got, want := fmt.Sprint(observation.TouchedFiles), "[task.go]"; got != want {
		t.Fatalf("touched files = %s, want %s", got, want)
	}
	if observation.Divergent || observation.Error != "" {
		t.Fatalf("task-worktree observation = %+v, want clean matching observation", observation)
	}
}

func deliveryObservationRepo(t *testing.T) string {
	t.Helper()
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "tests@gitmoot.local")
	runGit(t, checkout, "config", "user.name", "Gitmoot Tests")
	if err := os.WriteFile(filepath.Join(checkout, "ordinary.go"), []byte("package original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, checkout, "add", "ordinary.go")
	runGit(t, checkout, "commit", "-m", "base")
	return checkout
}

func deliveryObservationResult(change string) string {
	return fmt.Sprintf(`{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[%q],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`, change)
}
