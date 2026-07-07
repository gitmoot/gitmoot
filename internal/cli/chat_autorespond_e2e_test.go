package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// This file is the no-LLM, no-network end-to-end proof for the #534 V1.5
// auto-respond sweep. Unlike chat_autorespond_test.go (which injects a recording
// dispatcher to assert the sweep's request SHAPE), this test wires the sweep to
// the REAL dispatch path (dispatchLocalAgentJob) and runs the enqueued job on the
// REAL daemon worker with a REAL shell-runtime agent, proving the whole loop:
//
//   human @mention -> runChatAutoRespondScanOnce -> one bounded read-only ask job
//   -> jobWorker.run -> ShellAdapter -> gitmoot_result -> postChatThreadResult
//   back-links a kind='job_result' reply into the thread; the trigger mention is
//   marked read (no re-fire); the cap hard-stops at 4 with ONE visible system
//   message and no further dispatch.

// appendChatConfig appends a [chat]/[agents.*] fragment to an already-initialized
// home's config file (blockerE2EHome runs config.Initialize first).
func appendChatConfig(t *testing.T, home, fragment string) {
	t.Helper()
	cfgFile := config.PathsForHome(home).ConfigFile
	existing, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(cfgFile, append(existing, []byte(fragment)...), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestChatAutoRespondSweepE2E drives the sweep + real worker end to end: an
// enrolled shell agent auto-replies to a human @mention, the reply back-links as a
// non-triggering job_result, the mention is consumed exactly once, and the cap
// hard-stops the fifth trigger with one visible system message.
//
// MUTATION PROOF: drop the MarkChatChatMentionsRead in runOneChatAutoRespond and
// the re-scan double-fires (RED); gate postChatThreadResult on the wrong field and
// the job_result back-link count stays 0 (RED); remove the cap park and the fifth
// trigger dispatches a job (RED).
func TestChatAutoRespondSweepE2E(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := chatE2EGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	// The DB agent (shell runtime) is what dispatch RUNS; the config
	// [agents.responder].chat_autorespond = true is what the sweep reads for
	// enrollment — BOTH are required. Its script echoes a terminal approved result.
	seedDaemonWorkerAgent(t, store, "responder", runtime.ShellRuntime,
		fmt.Sprintf("printf '%%s' '%s'", chatE2EApproved), []string{"ask"}, "owner/repo")
	// cooldown = 0 so the four cap-driving cycles are not spacing-throttled; the cap
	// stays at its documented default (4). auto_respond ON + one enrolled agent.
	appendChatConfig(t, home, `
[chat]
auto_respond = true
auto_respond_cooldown = "0"

[agents.responder]
runtime = "codex"
role = "responder"
chat_autorespond = true
`)
	paths := config.PathsForHome(home)

	if code := Run([]string{"chat", "create", "release-room", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("chat create failed")
	}
	thread, err := store.GetChatThreadBySlug(ctx, "owner/repo", "release-room")
	if err != nil {
		t.Fatalf("GetChatThreadBySlug: %v", err)
	}
	worker := blockerE2EWorker(store, home, checkout)

	// sweepOnce runs the REAL sweep and returns the queued ask jobs it produced.
	sweepOnce := func() []db.Job {
		t.Helper()
		if err := runChatAutoRespondScanOnce(ctx, paths, home, store, dispatchLocalAgentJob, time.Now().UTC()); err != nil {
			t.Fatalf("auto-respond sweep: %v", err)
		}
		queued, err := store.ListQueuedJobs(ctx)
		if err != nil {
			t.Fatalf("ListQueuedJobs: %v", err)
		}
		return queued
	}

	// --- drive to the cap (4): each cycle is one full auto-response ------------
	for i := 1; i <= 4; i++ {
		if code := Run([]string{"chat", "send", "release-room", fmt.Sprintf("@responder ping %d", i), "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("cycle %d: human chat send failed", i)
		}

		queued := sweepOnce()
		if len(queued) != 1 {
			t.Fatalf("cycle %d: sweep enqueued %d jobs, want exactly 1", i, len(queued))
		}
		job := queued[0]
		if job.Type != "ask" || job.Agent != "responder" {
			t.Fatalf("cycle %d: enqueued job type=%q agent=%q, want ask/responder", i, job.Type, job.Agent)
		}
		// The trigger mention was consumed (marked read) on dispatch.
		if unread := unreadInThread(t, store, thread.ID); unread != 0 {
			t.Fatalf("cycle %d: trigger mention still unread (%d) after dispatch", i, unread)
		}

		if i == 1 {
			// No re-trigger: a second sweep with the mention already read and the job
			// still queued enqueues nothing new.
			if again := sweepOnce(); len(again) != 1 {
				t.Fatalf("re-sweep changed the queue to %d jobs (must not re-fire a read mention)", len(again))
			}
		}

		done := chatE2EDriveUntilTerminal(t, ctx, worker, store, job.ID)
		if done.State != string(workflow.JobSucceeded) {
			t.Fatalf("cycle %d: auto-respond job state=%q, want succeeded", i, done.State)
		}
		// The reply back-linked as a job_result authored by the responder — the count
		// grows by exactly one per cycle (it is what CountChatAgentAutoResponses reads).
		if got := chatE2ECountKind(t, store, thread.ID, db.ChatKindJobResult); got != i {
			t.Fatalf("cycle %d: job_result back-links = %d, want %d", i, got, i)
		}
		reply := chatE2ELatestKind(t, store, thread.ID, db.ChatKindJobResult)
		if reply == nil || reply.AuthorKind != db.ChatAuthorKindAgent || reply.AuthorName != "responder" {
			t.Fatalf("cycle %d: latest job_result authored wrong: %+v", i, reply)
		}
	}

	// --- the fifth trigger hits the cap: NO dispatch, ONE visible system msg ---
	if code := Run([]string{"chat", "send", "release-room", "@responder one more?", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("fifth human chat send failed")
	}
	if queued := sweepOnce(); len(queued) != 0 {
		t.Fatalf("cap-hit sweep enqueued %d jobs, want 0", len(queued))
	}
	capBody := chatAutoRespondCapMessage("responder")
	if got := countChatKindBody(t, store, thread.ID, db.ChatKindSystem, capBody); got != 1 {
		t.Fatalf("cap system message count = %d, want exactly 1", got)
	}
	// The parked trigger is read; a further trigger re-parks without duplicating.
	if unread := unreadInThread(t, store, thread.ID); unread != 0 {
		t.Fatalf("cap-hit did not park (mark read) the trigger; unread=%d", unread)
	}
	if code := Run([]string{"chat", "send", "release-room", "@responder still stuck?", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("sixth human chat send failed")
	}
	if queued := sweepOnce(); len(queued) != 0 {
		t.Fatalf("post-cap sweep enqueued %d jobs, want 0", len(queued))
	}
	if got := countChatKindBody(t, store, thread.ID, db.ChatKindSystem, capBody); got != 1 {
		t.Fatalf("cap system message duplicated: count = %d, want 1", got)
	}
	// Exactly 4 auto-responses were produced — the cap held.
	if got := chatE2ECountKind(t, store, thread.ID, db.ChatKindJobResult); got != 4 {
		t.Fatalf("total job_result back-links = %d, want 4 (cap)", got)
	}
}
