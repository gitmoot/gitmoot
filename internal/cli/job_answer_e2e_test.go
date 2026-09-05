//go:build e2e

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// askGateQuestionCmd is the SHELL-runtime session body for an agent that returns
// human_questions[] instead of a plain decision, so the ask gate parks its task
// at awaiting_human. No LLM and no network participate.
func askGateQuestionCmd() string {
	return `printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"need a decision before proceeding",` +
		`"findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[],` +
		`"human_questions":[{"id":"q1","prompt":"Target v2 or v3 API?","choices":["v2","v3"]}]}}'`
}

// TestJobAnswerResumesAwaitingHumanE2E is the end-to-end proof for the #1754
// retained answer path. `gitmoot chat answer` is gone; `gitmoot job answer` is
// the only local (non-PR) way to answer a job paused at awaiting_human, and this
// drives the REAL command against a job that is GENUINELY parked — not a helper.
//
// The chain is real end to end: a registered shell agent returns
// human_questions[] through the real dispatch and worker paths, the engine's ask
// gate opens an escalation round and flips the task to awaiting_human, and then
// `Run([]string{"job", "answer", ...})` resumes it. The assertions are on
// engine-owned state the command never writes itself: the escalation round is
// resolved, and the answer-carrying continuation is enqueued.
//
// MUTATION PROOF: drop the EscalationPending guard and the "already answered"
// refusal below stops erroring; drop the ResolveEscalation call and both the
// resolved-round and continuation assertions flip RED.
func TestJobAnswerResumesAwaitingHumanE2E(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := readonlyWorktreeGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "asker", runtime.ShellRuntime, askGateQuestionCmd(), []string{"ask"}, "owner/repo")

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "asker", Action: "ask",
		Instructions: "decide the API target", Background: true, Home: home,
	})
	if err != nil {
		t.Fatalf("dispatch asker: %v", err)
	}

	worker := readonlyPoolWorker(store, home)
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("runQueuedJobsForRepo: %v", err)
	}

	// The job is GENUINELY parked: the engine opened an ask round on it.
	engine := workflow.Engine{Store: store}
	pending, err := engine.EscalationPending(ctx, out.JobID)
	if err != nil {
		t.Fatalf("EscalationPending: %v", err)
	}
	if !pending {
		events, _ := store.ListJobEvents(ctx, out.JobID)
		t.Fatalf("job %s is not parked at awaiting_human; events=%+v", out.JobID, events)
	}

	queuedBefore := queuedJobIDsForAnswerTest(t, store)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "answer", out.JobID, "q1: v3", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job answer exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "answered job: "+out.JobID) {
		t.Fatalf("job answer stdout=%q", stdout.String())
	}

	// The round is resolved, so the pause is closed rather than merely reported.
	stillPending, err := engine.EscalationPending(ctx, out.JobID)
	if err != nil {
		t.Fatalf("EscalationPending after answer: %v", err)
	}
	if stillPending {
		t.Fatal("escalation round still open after job answer")
	}

	// The answer reached the engine: a continuation was enqueued carrying it.
	continuation, ok := newlyQueuedJobForAnswerTest(t, store, queuedBefore)
	if !ok {
		t.Fatal("job answer enqueued no continuation")
	}
	job, err := store.GetJob(ctx, continuation)
	if err != nil {
		t.Fatalf("GetJob(continuation): %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("continuation payload: %v", err)
	}
	if !strings.Contains(payload.HumanAnswer, "v3") {
		t.Fatalf("continuation human_answer=%q, want the submitted answer", payload.HumanAnswer)
	}

	// Answering an already-resolved pause is a clear refusal, never a false success.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "answer", out.JobID, "q1: v2", "--home", home}, &stdout, &stderr); code == 0 ||
		!strings.Contains(stderr.String(), "no pending question to answer") {
		t.Fatalf("second answer exit=%d stderr=%s, want a refusal", code, stderr.String())
	}
}

// TestJobAnswerRefusesMissingAnswer pins the argument contract of the retained
// command. The two positionals are read BEFORE flag parsing, so a DEFINED FLAG
// sitting in the answer slot must be REFUSED: accepting it stored the literal
// "--json" as the human's answer and resolved the escalation, after which the real
// answer could no longer be submitted — a silent, unrecoverable wrong answer on
// the one path this reduction keeps.
//
// The guard is scoped to the command's OWN flags because #1754 specifies a
// FREE-FORM body and the engine's single-question convenience accepts bare text,
// so answers like "-1", "- use v3", "-> go with the second option" and
// "--force is fine" must all reach parseHumanAnswers. Both halves are pinned.
//
// Refusals are asserted WITHOUT a parked job on purpose: an argument-shape refusal
// must happen before any store or engine work, so a passing case here cannot be
// explained by a missing escalation.
func TestJobAnswerRefusesMissingAnswer(t *testing.T) {
	home := t.TempDir()
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"job", "answer", "job-1", "--json", "--home", home}, want: "is missing its answer"},
		{args: []string{"job", "answer", "job-1", "--home", home}, want: "is missing its answer"},
		{args: []string{"job", "answer", "job-1", "--home=" + home}, want: "is missing its answer"},
		{args: []string{"job", "answer", "job-1", "---json"}, want: "is missing its answer"},
		{args: []string{"job", "answer", "job-1", "-h"}, want: "is missing its answer"},
		{args: []string{"job", "answer", "job-1", "--help"}, want: "is missing its answer"},
		{args: []string{"job", "answer", "job-1"}, want: "requires a <job-id> and a"},
		{args: []string{"job", "answer"}, want: "requires a <job-id> and a"},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(test.args, &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), test.want) {
			t.Fatalf("Run(%v) exit=%d stdout=%q stderr=%q, want exit 2 naming %q", test.args, code, stdout.String(), stderr.String(), test.want)
		}
	}

	// Controls: every free-form body — including the dash-leading shapes the issue
	// requires — must get past the argument gate and fail on the absent job
	// instead, proving the refusals above are about DEFINED FLAGS and not a
	// reject-everything matcher.
	for _, answer := range []string{
		"q1: v3", "-1", "- use v3", "-> go with the second option", "--force is fine",
		"--json is not what I meant, use v3",
		// A dash-leading PROSE answer whose first "="-delimited segment collides
		// with a defined flag name ("home"). Whitespace makes it prose, so the
		// "="-split must not reach it — the regression this control pins.
		"-home=us-east is the region",
	} {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"job", "answer", "job-1", answer, "--home", home}, &stdout, &stderr)
		if code != 1 || strings.Contains(stderr.String(), "is missing its answer") || strings.Contains(stderr.String(), "requires a <job-id> and a") {
			t.Fatalf("control answer %q: exit=%d stderr=%q, want a store-level failure rather than a usage refusal", answer, code, stderr.String())
		}
	}
}

func queuedJobIDsForAnswerTest(t *testing.T, store *db.Store) map[string]struct{} {
	t.Helper()
	jobs, err := store.ListQueuedJobs(context.Background())
	if err != nil {
		t.Fatalf("ListQueuedJobs: %v", err)
	}
	ids := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		ids[job.ID] = struct{}{}
	}
	return ids
}

func newlyQueuedJobForAnswerTest(t *testing.T, store *db.Store, before map[string]struct{}) (string, bool) {
	t.Helper()
	for id := range queuedJobIDsForAnswerTest(t, store) {
		if _, existed := before[id]; !existed {
			return id, true
		}
	}
	return "", false
}
