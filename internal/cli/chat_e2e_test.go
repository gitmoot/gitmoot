package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// This file is the no-LLM, no-network end-to-end proof for the native agent chat
// layer (#534, V1 local-only). Both tests drive the REAL code paths on an
// isolated t.TempDir home with a REAL shell-runtime agent (a subprocess whose
// stdout is a gitmoot_result envelope) and the REAL daemon dispatch worker
// (runQueuedJobsForRepo -> jobWorker.run -> engine.RunJob -> ShellAdapter):
//
//   - TestChatPromotionLoopE2E: create -> send with @mention -> inbox unread ->
//     `chat task` promotion -> the promoted job runs to a terminal decision ->
//     its result is back-linked into the thread as a kind='job_result' message,
//     exactly once (idempotent under a re-advance).
//   - TestChatAnswerKeystoneE2E: a shell result carrying human_questions[] pauses
//     the tree at awaiting_human; the ask-gate auto-links a thread with the
//     questions as a kind='system' message; `chat answer` routes the human answer
//     onto the resume path; the answer-driven continuation runs to completion.
//
// These exercise the two V1 keystones (promotion + the #445 ask-gate answer
// channel) as integrations, not units.

// chatE2EApproved is a valid gitmoot_result envelope with a healthy terminal
// decision and no questions — the shell agent's "work done" reply.
const chatE2EApproved = `{"gitmoot_result":{"decision":"approved","summary":"inspected the adapter; looks good","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

// chatE2EQuestions is a valid, HEALTHY gitmoot_result that also carries
// human_questions[] — the ask-gate trigger (#445). A healthy decision is required
// (a blocked/failed result never asks).
const chatE2EQuestions = `{"gitmoot_result":{"decision":"approved","summary":"need a decision before proceeding","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[],"human_questions":[{"id":"q1","prompt":"Which port should the adapter bind?"}]}}`

// chatE2EGitCheckout builds a real git repo whose origin is the given owner/repo,
// so the promotion dispatch's resolveLocalAgentRepo -> repoRecordForCheckout
// (which shells out to git for root/remote/branch) resolves it as a tracked repo.
func chatE2EGitCheckout(t *testing.T, fullName string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "branch", "-m", "main")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/"+fullName+".git")
	return dir
}

// chatE2ELatestKind returns the newest thread message of the given kind (nil if
// none), reading through the REAL store ordering key.
func chatE2ELatestKind(t *testing.T, store *db.Store, threadID, kind string) *db.ChatMessage {
	t.Helper()
	msgs, err := store.ListChatMessages(context.Background(), threadID, 0)
	if err != nil {
		t.Fatalf("ListChatMessages returned error: %v", err)
	}
	var found *db.ChatMessage
	for i := range msgs {
		if msgs[i].Kind == kind {
			found = &msgs[i]
		}
	}
	return found
}

func chatE2ECountKind(t *testing.T, store *db.Store, threadID, kind string) int {
	t.Helper()
	msgs, err := store.ListChatMessages(context.Background(), threadID, 0)
	if err != nil {
		t.Fatalf("ListChatMessages returned error: %v", err)
	}
	n := 0
	for _, m := range msgs {
		if m.Kind == kind {
			n++
		}
	}
	return n
}

// chatE2EDriveUntilTerminal runs the REAL dispatch worker until jobID reaches a
// terminal state (or fails the test on timeout). It returns the terminal job.
func chatE2EDriveUntilTerminal(t *testing.T, ctx context.Context, worker jobWorker, store *db.Store, jobID string) db.Job {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
			t.Fatalf("runQueuedJobsForRepo returned error: %v", err)
		}
		job, err := store.GetJob(ctx, jobID)
		if err != nil {
			t.Fatalf("GetJob(%s) returned error: %v", jobID, err)
		}
		switch job.State {
		case string(workflow.JobSucceeded), string(workflow.JobFailed), string(workflow.JobBlocked), string(workflow.JobCancelled):
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never reached a terminal state; last state=%q", jobID, job.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestChatPromotionLoopE2E proves the full promotion loop: a human creates a
// thread, sends a message tagging a registered agent (which lands in the agent's
// inbox as unread), then explicitly promotes a task; the promoted job runs on the
// real shell runtime to a terminal decision and its result is back-linked into
// the originating thread as a non-promotable job_result message — exactly once.
//
// MUTATION PROOF: gate postChatThreadResult on the wrong field (or drop the
// terminal call site) and the job_result assertion flips RED; drop the
// promoted_job_id back-link write in `chat task` and the linkage assertion flips.
func TestChatPromotionLoopE2E(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := chatE2EGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	// A real shell agent: its script echoes a terminal approved gitmoot_result.
	seedDaemonWorkerAgent(t, store, "codex-b", runtime.ShellRuntime,
		fmt.Sprintf("printf '%%s' '%s'", chatE2EApproved), []string{"ask"}, "owner/repo")

	// --- create ---------------------------------------------------------------
	if code := Run([]string{"chat", "create", "release-room", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("chat create failed")
	}

	// --- send with @mention ---------------------------------------------------
	var sendErr bytes.Buffer
	if code := Run([]string{"chat", "send", "release-room", "@codex-b can you inspect the runtime adapter?", "--home", home}, &bytes.Buffer{}, &sendErr); code != 0 {
		t.Fatalf("chat send exit != 0: %s", sendErr.String())
	}

	// --- inbox shows unread ---------------------------------------------------
	var inboxOut bytes.Buffer
	if code := Run([]string{"chat", "inbox", "codex-b", "--unread", "--json", "--home", home}, &inboxOut, &bytes.Buffer{}); code != 0 {
		t.Fatalf("chat inbox exit != 0")
	}
	var inbox []struct {
		ThreadSlug string `json:"thread_slug"`
		Unread     bool   `json:"unread"`
	}
	if err := json.Unmarshal(inboxOut.Bytes(), &inbox); err != nil {
		t.Fatalf("decode inbox JSON: %v (%s)", err, inboxOut.String())
	}
	if len(inbox) != 1 || inbox[0].ThreadSlug != "release-room" || !inbox[0].Unread {
		t.Fatalf("inbox = %+v, want one unread mention in release-room", inbox)
	}

	// --- task (promotion) -----------------------------------------------------
	var taskOut, taskErr bytes.Buffer
	if code := Run([]string{"chat", "task", "release-room", "@codex-b implement the adapter manifest", "--action", "ask", "--json", "--home", home}, &taskOut, &taskErr); code != 0 {
		t.Fatalf("chat task exit != 0: %s", taskErr.String())
	}
	var promoted localAgentJobOutput
	if err := json.Unmarshal(taskOut.Bytes(), &promoted); err != nil {
		t.Fatalf("decode chat task JSON: %v (%s)", err, taskOut.String())
	}
	if strings.TrimSpace(promoted.JobID) == "" {
		t.Fatalf("chat task produced no job id: %s", taskOut.String())
	}

	thread, err := store.GetChatThreadBySlug(ctx, "owner/repo", "release-room")
	if err != nil {
		t.Fatalf("GetChatThreadBySlug: %v", err)
	}
	// The promotion_request message was recorded and back-linked to the job.
	promo := chatE2ELatestKind(t, store, thread.ID, db.ChatKindPromotionRequest)
	if promo == nil {
		t.Fatal("no promotion_request message recorded")
	}
	if promo.PromotedJobID != promoted.JobID {
		t.Fatalf("promotion_request promoted_job_id = %q, want %q", promo.PromotedJobID, promoted.JobID)
	}

	// --- run the promoted job to a terminal decision --------------------------
	worker := blockerE2EWorker(store, home, checkout)
	job := chatE2EDriveUntilTerminal(t, ctx, worker, store, promoted.JobID)
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("promoted job state = %q, want succeeded", job.State)
	}

	// --- the result is back-linked into the thread as a job_result -----------
	result := chatE2ELatestKind(t, store, thread.ID, db.ChatKindJobResult)
	if result == nil {
		t.Fatal("no job_result message back-linked into the thread")
	}
	if result.AuthorKind != db.ChatAuthorKindAgent || result.AuthorName != "codex-b" {
		t.Fatalf("job_result authored wrong: kind=%q name=%q", result.AuthorKind, result.AuthorName)
	}
	if result.ReplyTo != promo.ID {
		t.Fatalf("job_result reply_to = %q, want the promoting message %q", result.ReplyTo, promo.ID)
	}
	// It carries an origin-qualified job ref back to the promoted job.
	hasJobRef := false
	for _, r := range result.Refs {
		if r.Kind == "job" && r.ID == promoted.JobID {
			hasJobRef = true
		}
	}
	if !hasJobRef {
		t.Fatalf("job_result refs = %+v, want a {kind:job,id:%s} back-ref", result.Refs, promoted.JobID)
	}
	if got := chatE2ECountKind(t, store, thread.ID, db.ChatKindJobResult); got != 1 {
		t.Fatalf("job_result count = %d, want exactly 1 after terminal advance", got)
	}

	// --- idempotency: re-run the terminal back-link, no second job_result -----
	w2 := jobWorker{Store: store}
	readvanceJob, readvancePayload, err := daemonWorkerJobPayload(ctx, store, promoted.JobID)
	if err != nil {
		t.Fatalf("daemonWorkerJobPayload (re-advance): %v", err)
	}
	if err := w2.postChatThreadResult(ctx, readvanceJob, readvancePayload, runtime.Agent{Name: "codex-b", Runtime: runtime.ShellRuntime}, nil); err != nil {
		t.Fatalf("postChatThreadResult (re-advance) returned error: %v", err)
	}
	if got := chatE2ECountKind(t, store, thread.ID, db.ChatKindJobResult); got != 1 {
		t.Fatalf("job_result count after re-advance = %d, want still exactly 1 (idempotent)", got)
	}
}

// TestChatAnswerKeystoneE2E proves the V1 keystone — the local answer channel for
// the #445 ask-gate. A real shell agent returns a healthy result carrying
// human_questions[]; the tree pauses at awaiting_human and the ask-gate
// auto-links a chat thread whose kind='system' message carries the question; the
// human runs `gitmoot chat answer`, which routes the answer onto the resume path;
// the answer-driven continuation then runs on the shell runtime to completion.
//
// MUTATION PROOF: drop the linkAskGateChatThread call in pauseAwaitingHumanAnswer
// and the auto-linked thread + system message assertions flip RED; break the
// ThreadID stamp and the continuation would not back-link.
func TestChatAnswerKeystoneE2E(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := chatE2EGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	// The task the asking job belongs to (the ask-gate pauses the task).
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-1", RepoFullName: "owner/repo", GoalID: "goal-1",
		Title: "Add the adapter", State: string(workflow.TaskPlanned), Branch: "main",
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}

	// A shell agent that ASKS on its first delivery, then APPROVES on the second
	// (the answer-driven continuation). A marker file flips the reply.
	marker := filepath.Join(t.TempDir(), "asked")
	script := fmt.Sprintf(`if [ ! -f %q ]; then
  : > %q
  printf '%%s' '%s'
else
  printf '%%s' '%s'
fi`, marker, marker, chatE2EQuestions, chatE2EApproved)
	seedDaemonWorkerAgent(t, store, "planner", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")

	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "coord-ask", Agent: "planner", Action: "ask", Repo: "owner/repo",
		Branch: "main", TaskID: "task-1", TaskTitle: "Add the adapter", GoalID: "goal-1",
	})

	worker := blockerE2EWorker(store, home, checkout)

	// --- first tick: shell delivers human_questions -> the tree pauses --------
	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("first dispatch returned error: %v", err)
	}
	// The task is paused at awaiting_human (the ask-gate opened a round).
	task, err := store.GetTask(ctx, "task-1")
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.State != string(workflow.TaskAwaitingHuman) {
		t.Fatalf("task state after ask = %q, want %q", task.State, workflow.TaskAwaitingHuman)
	}

	// --- the ask-gate auto-linked a thread with the question as a system msg --
	threads, err := store.ListChatThreads(ctx, "owner/repo", "")
	if err != nil {
		t.Fatalf("ListChatThreads returned error: %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("auto-linked threads = %d, want exactly 1", len(threads))
	}
	linked := threads[0]
	sysMsg := chatE2ELatestKind(t, store, linked.ID, db.ChatKindSystem)
	if sysMsg == nil {
		t.Fatal("no kind='system' message carrying the ask-gate question")
	}
	if !strings.Contains(sysMsg.Body, "Which port") {
		t.Fatalf("system message body = %q, want the question text", sysMsg.Body)
	}
	// It carries an origin-qualified job ref naming the paused (coordinator) job.
	hasJobRef := false
	for _, r := range sysMsg.Refs {
		if r.Kind == "job" && r.ID == "coord-ask" {
			hasJobRef = true
		}
	}
	if !hasJobRef {
		t.Fatalf("system message refs = %+v, want a {kind:job,id:coord-ask} ref", sysMsg.Refs)
	}

	// --- chat answer: route the human answer onto the resume path -------------
	var answerErr bytes.Buffer
	if code := Run([]string{"chat", "answer", linked.Slug, "q1: use port 8080", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &answerErr); code != 0 {
		t.Fatalf("chat answer exit != 0: %s", answerErr.String())
	}

	// The resume enqueued the coordinator continuation (the tree resumes). The
	// continuation id is the deterministic "<parent>/continuation".
	contID := "coord-ask/continuation"
	cont, err := store.GetJob(ctx, contID)
	if err != nil {
		t.Fatalf("the answer did not enqueue the continuation %s: %v", contID, err)
	}
	// The continuation inherits the thread linkage and carries the human answer.
	contPayload, err := workflow.ParseJobPayload(cont.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(continuation) returned error: %v", err)
	}
	if contPayload.ThreadID != linked.ID {
		t.Fatalf("continuation ThreadID = %q, want the auto-linked thread %q", contPayload.ThreadID, linked.ID)
	}
	if !strings.Contains(contPayload.HumanAnswer, "8080") {
		t.Fatalf("continuation HumanAnswer = %q, want the answer", contPayload.HumanAnswer)
	}

	// --- run the continuation to completion -----------------------------------
	done := chatE2EDriveUntilTerminal(t, ctx, worker, store, contID)
	if done.State != string(workflow.JobSucceeded) {
		t.Fatalf("continuation state = %q, want succeeded", done.State)
	}
	// The completed continuation back-links its result into the same thread.
	if got := chatE2ECountKind(t, store, linked.ID, db.ChatKindJobResult); got < 1 {
		t.Fatalf("job_result messages after completion = %d, want >= 1 (continuation result back-linked)", got)
	}
}
