package workflow

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestMailboxRunDoesNotRecordContentFreeNeeds is the #1809 review's N1: the
// gate-RECORDING path kept a raw len() after the judging gates were fixed, so
// `needs: [{}]` was admitted and written to job_gates verbatim. A human then
// saw a gate row reading "{}" through `gitmoot job gates` and the dashboard's
// "Needs a human" view.
//
// Driven through the real mailbox, not the helper: this is the consumer
// literally named gates, and the principle the branch already stated is that
// two gates disagreeing about one input is the defect.
func TestMailboxRunDoesNotRecordContentFreeNeeds(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"blocked","summary":"blocked","findings":[],"changes_made":[],"tests_run":[],"needs":[{},{"name":"","detail":""}],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}

	gates, err := store.ListJobGates(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobGates: %v", err)
	}
	if len(gates) != 0 {
		t.Fatalf("gates = %+v, want none: a content-free need must not become a durable row a human reads", gates)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	if hasEvent(events, "gates_recorded") {
		t.Fatalf("events = %+v, want NO gates_recorded event for content-free needs", events)
	}
}

// TestMailboxRunStillRecordsMixedNeeds keeps the fix from becoming a bigger
// defect than the one it closes: a real need alongside a content-free one is
// still recorded, and only the empty one is dropped.
func TestMailboxRunStillRecordsMixedNeeds(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	adapter := &fakeDelivery{outputs: []string{
		`{"gitmoot_result":{"decision":"blocked","summary":"blocked","findings":[],"changes_made":[],"tests_run":[],"needs":[{},"API key"],"delegations":[]}}`,
	}}

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err != nil {
		t.Fatalf("Run: %v", err)
	}
	gates, err := store.ListJobGates(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobGates: %v", err)
	}
	if len(gates) != 1 || gates[0].Need != "API key" {
		t.Fatalf("gates = %+v, want exactly the real need", gates)
	}
	events, err := store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	if !hasEvent(events, "gates_recorded") {
		t.Fatalf("events = %+v, want a gates_recorded event for the real need", events)
	}
}
