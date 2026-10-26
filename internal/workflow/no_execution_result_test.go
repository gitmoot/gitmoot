package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestNoExecutionReviewResultChecks pins what a no-execution review (#2364)
// may record: a static-only verdict passes; any claim that something ran, or
// delegated work that would run outside the restriction, is refused.
func TestNoExecutionReviewResultChecks(t *testing.T) {
	cases := []struct {
		name   string
		result AgentResult
		failed []string
	}{
		{name: "static only approval", result: AgentResult{Decision: "approved", Evidence: EvidenceStaticOnly, Summary: "read the diff and CI logs"}},
		{name: "static only with a placeholder tests_run entry", result: AgentResult{Decision: "approved", Evidence: EvidenceStaticOnly, TestsRun: []string{"  "}}},
		{name: "executed claim", result: AgentResult{Decision: "approved", Evidence: EvidenceExecuted, TestsRun: []string{"go test ./..."}}, failed: []string{"review-no-execution-claims-nothing-ran"}},
		{name: "tests listed under static_only", result: AgentResult{Decision: "approved", Evidence: EvidenceStaticOnly, TestsRun: []string{"go test ./internal/x"}}, failed: []string{"review-no-execution-claims-nothing-ran"}},
		{name: "delegation", result: AgentResult{Decision: "approved", Evidence: EvidenceStaticOnly, Delegations: []Delegation{{Agent: "x", Action: "ask"}}}, failed: []string{"review-no-execution-delegates-nothing"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NoExecutionReviewResultChecks(tc.result)
			if len(got) != len(tc.failed) {
				t.Fatalf("failed checks = %+v, want %v", got, tc.failed)
			}
			for i, id := range tc.failed {
				if got[i].ID != id || got[i].Pass {
					t.Fatalf("check %d = %+v, want failed %s", i, got[i], id)
				}
			}
		})
	}
}

// TestMailboxRefusesExecutedClaimFromNoExecutionReviewInEveryMode: the
// no-execution result contract is enforced with result_checks off, for a job
// requested as no-execution and for one made no-execution by repository
// policy (which the worker marks on the agent), and a truthful static-only
// verdict still succeeds and is stored as a no-execution review.
func TestMailboxRefusesExecutedClaimFromNoExecutionReviewInEveryMode(t *testing.T) {
	const executed = `{"gitmoot_result":{"decision":"approved","summary":"ran the suite and it passed","evidence":"executed","findings":[],"changes_made":[],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`
	const static = `{"gitmoot_result":{"decision":"approved","summary":"read the diff and the CI log; the failing test was fixed","evidence":"static_only","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
	reviewer := func(noExecution bool) runtime.Agent {
		agent := shellAgent()
		agent.Role = "reviewer"
		agent.NoExecution = noExecution
		return agent
	}
	cases := []struct {
		name             string
		requested        bool
		agentNoExecution bool
		output           string
		wantState        JobState
	}{
		{name: "requested, executed claim", requested: true, output: executed, wantState: JobFailed},
		{name: "repo policy, executed claim", agentNoExecution: true, output: executed, wantState: JobFailed},
		{name: "requested, static only", requested: true, output: static, wantState: JobSucceeded},
		{name: "ordinary review, executed claim", output: executed, wantState: JobSucceeded},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree"), resultCheckMode: ResultChecksOff}
			id := fmt.Sprintf("job-noexec-%d", i)
			if _, err := mailbox.Enqueue(ctx, JobRequest{ID: id, Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot", ReviewNoExecution: tc.requested}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			_, runErr := mailbox.Run(ctx, id, reviewer(tc.agentNoExecution), &fakeDelivery{outputs: []string{tc.output}})
			job, err := store.GetJob(ctx, id)
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			if job.State != string(tc.wantState) {
				t.Fatalf("state = %q (run err %v), want %q", job.State, runErr, tc.wantState)
			}
			payload, err := unmarshalPayload(job.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if want := tc.requested || tc.agentNoExecution; payload.ReviewNoExecution != want {
				t.Fatalf("stored review_no_execution = %v, want %v", payload.ReviewNoExecution, want)
			}
			if tc.wantState == JobFailed {
				var checksErr *ResultChecksError
				if !errors.As(runErr, &checksErr) || len(payload.ResultChecks) == 0 || payload.ResultChecks[0].ID != "review-no-execution-claims-nothing-ran" {
					t.Fatalf("run err %v, stored checks %+v; want the no-execution claim refused", runErr, payload.ResultChecks)
				}
			}
		})
	}
}
