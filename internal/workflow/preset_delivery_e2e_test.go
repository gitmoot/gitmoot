package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// presetJobPayload builds a stored job payload that snapshots a preset, mirroring
// what Enqueue writes. The snapshot (id/commit/content) is what auditability and
// retry rely on.
func presetJobPayload(t *testing.T) string {
	t.Helper()
	payload, err := marshalPayload(JobPayload{
		Repo:                   "gitmoot/gitmoot",
		TemplateID:             "thermo",
		TemplateResolvedCommit: "abc123",
		TemplateContent:        "Review deeply.",
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	return payload
}

// TestPresetDeliveryAlwaysInlinesFullPreset is the surviving contract after #1756
// removed the #33 referenced/auto delivery modes: EVERY delivery inlines the whole
// resolved preset body, with no per-agent mode and no session-state lookup that
// could shorten it.
//
// It is deliberately an END-TO-END test through Mailbox.Run rather than a check on
// the renderer: prompts.RenderJob is already pinned directly by prompts_test, and a
// mutant that reintroduced a shortening branch anywhere between the stored payload
// and the adapter would leave that renderer test green. This test enters through the
// same path a real job takes, so it observes the prompt the adapter actually
// receives.
//
// The negative assertion is the load-bearing half: it names the exact reference
// wording the deleted modes emitted, so resurrecting that behaviour fails here
// rather than passing quietly.
func TestPresetDeliveryAlwaysInlinesFullPreset(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}

	// Two deliveries on the SAME session: the deleted modes shortened the second
	// one once the first had "loaded" the preset, so repeating the delivery is what
	// exercises the removal rather than a single call would.
	agent := runtime.Agent{
		Name:       "audit",
		Runtime:    runtime.ShellRuntime,
		RuntimeRef: "printf ok",
		RepoScope:  "gitmoot/gitmoot",
		Role:       "reviewer",
	}
	for _, id := range []string{"job-preset-1", "job-preset-2"} {
		adapter := &fakeDelivery{outputs: []string{okDeliveryResult}}
		if err := store.CreateJob(ctx, db.Job{ID: id, Agent: "audit", Type: "review", State: string(JobQueued), Payload: presetJobPayload(t)}); err != nil {
			t.Fatalf("CreateJob %s: %v", id, err)
		}
		if _, err := mailbox.Run(ctx, id, agent, adapter); err != nil {
			t.Fatalf("Run %s: %v", id, err)
		}
		if !strings.Contains(adapter.prompts[0], "Template instructions:\nReview deeply.") {
			t.Fatalf("%s must inline the full preset:\n%s", id, adapter.prompts[0])
		}
		if strings.Contains(adapter.prompts[0], "Use your installed thermo preset") {
			t.Fatalf("%s emitted the removed short-reference form:\n%s", id, adapter.prompts[0])
		}
	}
}
