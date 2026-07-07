package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// This file is the no-LLM, no-network end-to-end proof for the #534 V1.5 MOOT
// primitive's conversation loop and its --max-messages hard cap. Where
// chat_moot_test.go's TestMootSeatE2E proves seats DISPATCH and back-link
// conclusions, TestMootConversationLoopE2E proves seats actually CONVERSE: each
// seat job runs a REAL `gitmoot chat send --as <self>` subprocess against the same
// home before returning its conclusions, and both the chat turns AND the
// job_result conclusions land in the thread.

// buildGitmootBinaryForTest compiles the gitmoot CLI once into the test's temp dir
// so a shell-runtime seat can invoke `gitmoot chat send` as a real subprocess (the
// ShellAdapter runs `sh -c <script> gitmoot <prompt>`, so the script needs a real
// binary on disk). It uses the module import path, so the package cwd is enough for
// `go` to resolve the module.
func buildGitmootBinaryForTest(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gitmoot")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "github.com/jerryfane/gitmoot/cmd/gitmoot")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build gitmoot binary: %v\n%s", err, out)
	}
	return bin
}

// TestMootConversationLoopE2E convenes a two-seat moot through the REAL command and
// runs both seats on the REAL daemon worker. Each seat's script posts one chat turn
// via a real `gitmoot chat send --as <self>` subprocess (embedding the home/repo/
// slug the moot dispatched onto), then returns its partial conclusions as a
// gitmoot_result. Assertions: the announce system message, BOTH seats' kind='chat'
// turns present in the thread, BOTH seats' kind='job_result' conclusions back-linked
// (the cap never blocks these), and both seat jobs succeeded.
//
// MUTATION PROOF: break renderMootSeatInstructions' thread wiring and the seat's
// chat turn would target the wrong thread (the chat-turn assertion flips RED); gate
// postChatThreadResult on the wrong field and the job_result assertion flips.
func TestMootConversationLoopE2E(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := chatE2EGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	bin := buildGitmootBinaryForTest(t)

	aliceResult := `{"gitmoot_result":{"decision":"approved","summary":"alice: know A, unsure B, would ask C","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
	bobResult := `{"gitmoot_result":{"decision":"approved","summary":"bob: know P, unsure Q, would ask R","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

	// Each seat script does one real `chat send --as <self>` (a conversation turn)
	// then prints its conclusions. The slug/repo/home are the ones the moot below
	// convenes onto — a real agent would parse these from its prompt ($1); a no-LLM
	// shell seat hardcodes them, which is an equivalent proof that a seat's chat-send
	// subprocess loop lands turns in the thread.
	seatScript := func(self, msg, result string) string {
		send := fmt.Sprintf("%q chat send paper-review %q --as %s --repo owner/repo --home %q >/dev/null 2>&1",
			bin, msg, self, home)
		return fmt.Sprintf("%s; printf '%%s' '%s'", send, result)
	}
	seedDaemonWorkerAgent(t, store, "alice", runtime.ShellRuntime,
		seatScript("alice", "alice: I lean toward option A", aliceResult), []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "bob", runtime.ShellRuntime,
		seatScript("bob", "bob: option A has a runtime cost", bobResult), []string{"ask"}, "owner/repo")

	// Convene the moot through the REAL command (chatMootDispatch is NOT faked).
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"moot", "paper-review", "which option ships?", "--agents", "alice,bob", "--repo", "owner/repo", "--json", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("moot exit != 0: %s", stderr.String())
	}
	var out mootOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode moot JSON: %v (%s)", err, stdout.String())
	}
	if len(out.Seats) != 2 {
		t.Fatalf("seats = %d, want 2", len(out.Seats))
	}

	// The announcement is a visible system message naming both seats.
	announce := chatE2ELatestKind(t, store, out.ThreadID, db.ChatKindSystem)
	if announce == nil || !strings.Contains(announce.Body, "MOOT convened") {
		t.Fatalf("announcement = %+v, want a MOOT-convened system message", announce)
	}

	// Drive both seat jobs to terminal on the REAL worker (sequentially — no shared
	// SQLite write contention).
	worker := blockerE2EWorker(store, home, checkout)
	for _, s := range out.Seats {
		if s.JobID == "" || s.Error != "" {
			t.Fatalf("seat %s did not dispatch: %+v", s.Agent, s)
		}
		job := chatE2EDriveUntilTerminal(t, ctx, worker, store, s.JobID)
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("seat %s job state = %q, want succeeded", s.Agent, job.State)
		}
	}

	// Both seats' CONVERSATION turns (kind='chat', authored by the agent via the real
	// `chat send --as` subprocess) landed in the thread.
	msgs, err := store.ListChatMessages(ctx, out.ThreadID, 0)
	if err != nil {
		t.Fatalf("ListChatMessages: %v", err)
	}
	chatTurns := map[string]bool{}
	conclusions := map[string]bool{}
	for _, m := range msgs {
		if m.AuthorKind != db.ChatAuthorKindAgent {
			continue
		}
		switch m.Kind {
		case db.ChatKindChat:
			chatTurns[m.AuthorName] = true
		case db.ChatKindJobResult:
			conclusions[m.AuthorName] = true
		}
	}
	if !chatTurns["alice"] || !chatTurns["bob"] {
		t.Fatalf("agent chat turns = %v, want both alice and bob (the seats' chat sends)", chatTurns)
	}
	if !conclusions["alice"] || !conclusions["bob"] {
		t.Fatalf("job_result conclusions = %v, want both alice and bob (back-linked, cap never blocks)", conclusions)
	}
}

// TestMootMaxMessagesCapE2E convenes a real moot with --max-messages 2 (a recording
// dispatcher stands in for the seat runtimes so the cap wiring is tested in
// isolation) and proves the flag wires straight to the enforced hard cap: two agent
// turns are accepted, the third `chat send --as` is structurally REFUSED with the
// distinctive error, exactly ONE visible overrun system message is posted (idempotent
// across repeated over-cap attempts), and a human send is never blocked.
//
// MUTATION PROOF: drop the *maxMessages override in runMoot and the cap resolves to
// the default 30 — the third send succeeds and the rejection assertion flips RED.
func TestMootMaxMessagesCapE2E(t *testing.T) {
	ctx := context.Background()
	store, home := mootFixtureHome(t, "")
	seedDaemonWorkerAgent(t, store, "seat", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	_ = withRecordingMootDispatch(t)

	// Convene with the per-moot override --max-messages 2.
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"moot", "caproom", "tight loop", "--agents", "seat", "--max-messages", "2", "--repo", "owner/repo", "--json", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("moot exit != 0: %s", stderr.String())
	}
	var out mootOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode moot JSON: %v (%s)", err, stdout.String())
	}
	if out.MessageCap != 2 {
		t.Fatalf("message cap = %d, want 2 (from --max-messages)", out.MessageCap)
	}

	// Two agent turns fill the cap (both below-cap, so both accepted).
	for i := 0; i < 2; i++ {
		if code := Run([]string{"chat", "send", "caproom", fmt.Sprintf("turn %d", i), "--as", "seat", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
			t.Fatalf("turn %d agent send was rejected below cap", i)
		}
	}

	// The third agent send is structurally REFUSED with the distinctive cap error.
	var capErr bytes.Buffer
	if code := Run([]string{"chat", "send", "caproom", "one too many", "--as", "seat", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &capErr); code != 1 {
		t.Fatalf("over-cap send exit = %d, want 1", code)
	}
	if !strings.Contains(capErr.String(), "moot cap reached") {
		t.Fatalf("over-cap stderr = %q, want moot-cap error", capErr.String())
	}
	// A repeated over-cap attempt is still refused but does not duplicate the overrun.
	if code := Run([]string{"chat", "send", "caproom", "again", "--as", "seat", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Fatal("second over-cap send exit != 1")
	}

	thread, err := store.GetChatThreadBySlug(ctx, "owner/repo", "caproom")
	if err != nil {
		t.Fatalf("GetChatThreadBySlug: %v", err)
	}
	if got := countChatKindBody(t, store, thread.ID, db.ChatKindSystem, chatMootOverrunMessage(2)); got != 1 {
		t.Fatalf("overrun system messages = %d, want exactly 1 (idempotent)", got)
	}
	if n, _ := store.CountChatMootMessages(ctx, thread.ID); n != 2 {
		t.Fatalf("agent turn count = %d, want 2 (no over-cap turn inserted)", n)
	}
	// A HUMAN send is never blocked by the cap.
	if code := Run([]string{"chat", "send", "caproom", "human nudge", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("human send was blocked by the moot cap")
	}
}
