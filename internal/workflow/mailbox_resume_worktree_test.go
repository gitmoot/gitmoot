package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/prompts"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

func TestMailboxRunPrependsResumeNotice(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	const output = `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

	run := func(t *testing.T, id string, resumed bool) (string, string) {
		t.Helper()
		if _, err := mailbox.Enqueue(ctx, JobRequest{
			ID: id, Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main", HeadSHA: "base",
			TaskTitle: "Inspect recovery", Instructions: "Inspect the worktree.",
		}); err != nil {
			t.Fatalf("Enqueue returned error: %v", err)
		}
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob returned error: %v", err)
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			t.Fatalf("unmarshalPayload returned error: %v", err)
		}
		payload.BlockerClass = "checkout_contention"
		payload.BlockerAttempts = 1
		payload.BlockerPreDelivery = true
		payload.ResumedSelfDirtyWorktree = resumed
		encoded, err := marshalPayload(payload)
		if err != nil {
			t.Fatalf("marshalPayload returned error: %v", err)
		}
		if err := store.UpdateJobPayload(ctx, id, encoded); err != nil {
			t.Fatalf("UpdateJobPayload returned error: %v", err)
		}
		adapter := &fakeDelivery{outputs: []string{output}}
		if _, err := mailbox.Run(ctx, id, runtime.Agent{Name: "audit"}, adapter); err != nil {
			t.Fatalf("Mailbox.Run returned error: %v", err)
		}
		if len(adapter.prompts) != 1 {
			t.Fatalf("delivered prompts = %d, want 1", len(adapter.prompts))
		}
		return adapter.prompts[0], prompts.RenderJob(payload.prompt("ask"))
	}

	resumedPrompt, resumedBase := run(t, "resume-notice", true)
	wantPrefix := resumedSelfDirtyWorktreeNotice() + "\n\n"
	if !strings.HasPrefix(resumedPrompt, wantPrefix) {
		t.Fatalf("resumed prompt missing exact notice prefix:\n%s", resumedPrompt)
	}
	if strings.TrimPrefix(resumedPrompt, wantPrefix) != resumedBase {
		t.Fatalf("resume notice changed the base prompt:\n%s", resumedPrompt)
	}
	if strings.Contains(resumedPrompt, "NOTE (operational retry") || strings.Contains(resumedPrompt, "pushed branches") {
		t.Fatalf("resumed prompt carried generic blocker reconciliation:\n%s", resumedPrompt)
	}

	controlPrompt, controlBase := run(t, "resume-notice-control", false)
	if controlPrompt != controlBase {
		t.Fatalf("unset resumed flag changed the existing pre-delivery prompt:\ngot:  %q\nwant: %q", controlPrompt, controlBase)
	}
}
