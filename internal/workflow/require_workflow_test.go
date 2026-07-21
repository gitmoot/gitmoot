package workflow

import (
	"context"
	"strings"
	"testing"
)

func TestMailboxRequireWorkflow(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mb := Mailbox{Store: store, RequireWorkflowPolicy: func(string) RequireWorkflowPolicy { return RequireWorkflowPolicy{Enabled: true, Mode: "auto"} }}
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
	mb := Mailbox{Store: store, RequireWorkflowPolicy: func(string) RequireWorkflowPolicy { return RequireWorkflowPolicy{Enabled: true, Mode: "strict"} }}
	_, err := mb.Enqueue(ctx, JobRequest{ID: "strict", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "local"})
	if err == nil || !strings.Contains(err.Error(), "pass --workflow") {
		t.Fatalf("err=%v", err)
	}
	if _, err := store.GetJob(ctx, "strict"); err == nil {
		t.Fatal("strict rejection created a job")
	}
}

func TestMailboxRequireWorkflowExcludesInternalProducers(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mb := Mailbox{Store: store, RequireWorkflowPolicy: func(string) RequireWorkflowPolicy { return RequireWorkflowPolicy{Enabled: true, Mode: "strict"} }}
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
