package workflow

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
)

func TestMailboxRequireWorkflow(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mb := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree"), RequireWorkflowPolicy: func(string) RequireWorkflowPolicy { return RequireWorkflowPolicy{Enabled: true, Mode: "auto"} }}
	job, err := mb.Enqueue(ctx, JobRequest{ID: "auto", Agent: "A_bad name", Action: "ask", Repo: "owner/repo", Sender: "local"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkflowID(p.WorkflowID); err != nil || !strings.HasPrefix(p.WorkflowID, "adhoc/") {
		t.Fatalf("workflow=%q err=%v", p.WorkflowID, err)
	}
	if _, err := mb.Enqueue(ctx, JobRequest{ID: "explicit", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "local", WorkflowID: "team/campaign"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mb.Enqueue(ctx, JobRequest{ID: "pipeline", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: PipelineJobSender}); err != nil {
		t.Fatal(err)
	}
}

func TestMailboxRequireWorkflowStrictRejectsBeforeCreation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mb := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree"), RequireWorkflowPolicy: func(string) RequireWorkflowPolicy { return RequireWorkflowPolicy{Enabled: true, Mode: "strict"} }}
	_, err := mb.Enqueue(ctx, JobRequest{ID: "strict", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "local"})
	if err == nil || !strings.Contains(err.Error(), "pass --workflow") {
		t.Fatalf("err=%v", err)
	}
	if _, err := store.GetJob(ctx, "strict"); err == nil {
		t.Fatal("strict rejection created a job")
	}
}

func TestMailboxRequireWorkflowConfigCompatibility(t *testing.T) {
	for _, test := range []struct {
		name       string
		body       string
		wantStrict bool
	}{
		{name: "mode only auto files", body: "[workflow]\nrequire_workflow_mode = \"strict\"\n"},
		{name: "explicit enable rejects", body: "[workflow]\nrequire_workflow = true\nrequire_workflow_mode = \"strict\"\n", wantStrict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := config.PathsForHome(t.TempDir())
			if err := config.Initialize(paths); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte(test.body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadRequireWorkflow(paths)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			store := openTestStore(t)
			mb := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree"), RequireWorkflowPolicy: func(repo string) RequireWorkflowPolicy {
				policy := cfg.For(repo)
				return RequireWorkflowPolicy{Enabled: policy.Enabled, Mode: policy.Mode}
			}}
			job, err := mb.Enqueue(ctx, JobRequest{ID: "compat", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "local"})
			if test.wantStrict {
				if err == nil || !strings.Contains(err.Error(), "pass --workflow") {
					t.Fatalf("Enqueue() job=%+v err=%v, want strict rejection", job, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Enqueue() error = %v, want auto-file", err)
			}
			payload, err := ParseJobPayload(job.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(payload.WorkflowID, "adhoc/") {
				t.Fatalf("workflow_id = %q, want adhoc auto-file", payload.WorkflowID)
			}
		})
	}
}

func TestMailboxRequireWorkflowExcludesInternalProducers(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mb := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree"), RequireWorkflowPolicy: func(string) RequireWorkflowPolicy { return RequireWorkflowPolicy{Enabled: true, Mode: "strict"} }}
	for _, request := range []JobRequest{
		{ID: "pipeline", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: PipelineJobSender},
		{ID: "heartbeat", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "heartbeat"},
		{ID: "child", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "a", ParentJobID: "parent"},
		{ID: "merge", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "temp", DelegationReason: "temp_worker_merge_back"},
	} {
		if _, err := mb.Enqueue(ctx, request); err != nil {
			t.Fatalf("%s: %v", request.ID, err)
		}
	}
}

func TestMailboxRequireWorkflowPolicyExemptions(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mb := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree"), RequireWorkflowPolicy: func(string) RequireWorkflowPolicy { return RequireWorkflowPolicy{Enabled: true, Mode: "strict"} }}
	for _, request := range []JobRequest{
		{ID: "engine", Agent: "a", Action: "review", Repo: "owner/repo", Sender: "lead", PolicyExempt: "exempt"},
		{ID: "comment", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "operator", PolicyExempt: "auto-only"},
	} {
		job, err := mb.Enqueue(ctx, request)
		if err != nil {
			t.Fatalf("%s: Enqueue: %v", request.ID, err)
		}
		payload, err := ParseJobPayload(job.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if request.PolicyExempt == "exempt" && payload.WorkflowID != "" {
			t.Fatalf("engine workflow=%q, want inherited empty label", payload.WorkflowID)
		}
		if request.PolicyExempt == "auto-only" && !strings.HasPrefix(payload.WorkflowID, "adhoc/") {
			t.Fatalf("comment workflow=%q, want auto label", payload.WorkflowID)
		}
	}
}
