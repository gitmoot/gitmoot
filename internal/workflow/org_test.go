package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func testOrgPolicy(enforce string) func(string) OrgEnforcement {
	return func(string) OrgEnforcement {
		return OrgEnforcement{Enabled: true, Enforce: enforce,
			Role: func(name string) (OrgRole, bool) {
				if name == "owner" {
					return OrgRole{Name: name, Scope: []string{"owner/*"}}, true
				}
				return OrgRole{}, false
			},
			ScopeMatches: func(scopes []string, repo string) bool {
				return len(scopes) == 1 && scopes[0] == "owner/*" && strings.HasPrefix(repo, "owner/")
			},
		}
	}
}

func TestMailboxOrgScopeGate(t *testing.T) {
	ctx, store := context.Background(), openTestStore(t)
	mb := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree"), OrgPolicy: testOrgPolicy("block"), RequireWorkflowPolicy: func(string) RequireWorkflowPolicy { return RequireWorkflowPolicy{} }}
	job, err := mb.Enqueue(ctx, JobRequest{ID: "ok", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "local", OperatorOrigin: true, ActingOrgRole: "OWNER"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseJobPayload(job.Payload)
	if err != nil || p.ActingOrgRole != "owner" {
		t.Fatalf("payload=%+v err=%v", p, err)
	}
	for _, request := range []JobRequest{
		{ID: "missing", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "local", OperatorOrigin: true},
		{ID: "unknown", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "local", OperatorOrigin: true, ActingOrgRole: "nope"},
		{ID: "scope", Agent: "a", Action: "ask", Repo: "other/repo", Sender: "local", OperatorOrigin: true, ActingOrgRole: "owner"},
		{ID: "task-run", Agent: "a", Action: "implement", Repo: "owner/repo", Sender: "task run", OperatorOrigin: true},
	} {
		if _, err := mb.Enqueue(ctx, request); err == nil {
			t.Fatalf("%s unexpectedly accepted", request.ID)
		}
	}
	for _, request := range []JobRequest{
		{ID: "automated-local", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "local"},
		{ID: "comment", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "commenter"},
		{ID: "heartbeat", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "heartbeat"}, {ID: "pipeline", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: PipelineJobSender},
		{ID: "child", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "worker", ParentJobID: "parent"}, {ID: "exempt", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "local", PolicyExempt: "exempt"},
	} {
		if _, err := mb.Enqueue(ctx, request); err != nil {
			t.Fatalf("%s: %v", request.ID, err)
		}
	}
}

func TestMailboxOrgWarnAndMalformed(t *testing.T) {
	ctx, store := context.Background(), openTestStore(t)
	warn := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree"), OrgPolicy: testOrgPolicy("warn")}
	job, err := warn.Enqueue(ctx, JobRequest{ID: "warn", Agent: "a", Action: "ask", Repo: "other/repo", Sender: "local", OperatorOrigin: true, ActingOrgRole: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Kind != "org_scope_violation" {
		t.Fatalf("events=%+v", events)
	}
	bad := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree"), OrgPolicy: func(string) OrgEnforcement { return OrgEnforcement{LoadErr: errors.New("bad org toml")} }}
	if _, err := bad.Enqueue(ctx, JobRequest{ID: "bad-automated", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "local"}); err != nil {
		t.Fatalf("automated malformed config: %v", err)
	}
	if _, err := bad.Enqueue(ctx, JobRequest{ID: "bad", Agent: "a", Action: "ask", Repo: "owner/repo", Sender: "local", OperatorOrigin: true}); err == nil || !strings.Contains(err.Error(), "could not be loaded") {
		t.Fatalf("err=%v", err)
	}
}

func TestOrgScopeDecisionEnqueueParity(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name          string
		enforce       string
		role          string
		repo          string
		wantViolation bool
		wantErr       bool
	}{
		{name: "block missing role", enforce: "block", repo: "owner/repo", wantErr: true},
		{name: "block unknown role", enforce: "block", role: "unknown", repo: "owner/repo", wantErr: true},
		{name: "block out of scope", enforce: "block", role: "owner", repo: "other/repo", wantErr: true},
		{name: "block in scope", enforce: "block", role: "owner", repo: "owner/repo"},
		{name: "warn missing role", enforce: "warn", repo: "owner/repo", wantViolation: true},
		{name: "warn unknown role", enforce: "warn", role: "unknown", repo: "owner/repo", wantViolation: true},
		{name: "warn out of scope", enforce: "warn", role: "owner", repo: "other/repo", wantViolation: true},
		{name: "warn in scope", enforce: "warn", role: "owner", repo: "owner/repo"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			policy := testOrgPolicy(tt.enforce)(tt.repo)
			violation, decisionErr := OrgScopeDecision(policy, tt.role, tt.repo)
			if (decisionErr != nil) != tt.wantErr || (violation != "") != tt.wantViolation {
				t.Fatalf("decision violation=%q err=%v", violation, decisionErr)
			}
			store := openTestStore(t)
			job, enqueueErr := (Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree"), OrgPolicy: fixedOrgPolicyForTest(policy)}).Enqueue(ctx, JobRequest{ID: "parity", Agent: "a", Action: "ask", Repo: tt.repo, OperatorOrigin: true, ActingOrgRole: tt.role})
			if (enqueueErr != nil) != tt.wantErr {
				t.Fatalf("enqueue err=%v, decision err=%v", enqueueErr, decisionErr)
			}
			if enqueueErr != nil {
				return
			}
			events, err := store.ListJobEvents(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			gotViolation := false
			for _, event := range events {
				gotViolation = gotViolation || event.Kind == "org_scope_violation"
			}
			if gotViolation != tt.wantViolation {
				t.Fatalf("warning event=%v, want %v; events=%+v", gotViolation, tt.wantViolation, events)
			}
		})
	}
}

func fixedOrgPolicyForTest(policy OrgEnforcement) func(string) OrgEnforcement {
	return func(string) OrgEnforcement { return policy }
}
