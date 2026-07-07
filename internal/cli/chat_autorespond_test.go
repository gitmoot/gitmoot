package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

// chatAutoRespondFixture writes DefaultConfig + body, initializes it, and opens a
// store for a runChatAutoRespondScanOnce test.
func chatAutoRespondFixture(t *testing.T, body string) (config.Paths, *db.Store) {
	t.Helper()
	paths := config.PathsForHome(t.TempDir())
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return paths, store
}

// seedChatThread creates an open thread.
func seedChatThread(t *testing.T, store *db.Store, slug, repo string) db.ChatThread {
	t.Helper()
	thread, err := store.CreateChatThread(context.Background(), db.ChatThread{Slug: slug, Repo: repo, CreatedBy: db.ChatAuthorKindHuman})
	if err != nil {
		t.Fatalf("CreateChatThread: %v", err)
	}
	return thread
}

// seedChatMention appends a message of the given kind and, when agent != "", an
// unread resolved mention of that agent on it — the trigger shape the sweep reads.
func seedChatMention(t *testing.T, store *db.Store, thread db.ChatThread, kind, authorKind, authorName, body, agent string) db.ChatMessage {
	t.Helper()
	msg, err := store.AddChatMessage(context.Background(), db.ChatMessage{
		ThreadID:   thread.ID,
		AuthorKind: authorKind,
		AuthorName: authorName,
		Kind:       kind,
		Body:       body,
	})
	if err != nil {
		t.Fatalf("AddChatMessage: %v", err)
	}
	if strings.TrimSpace(agent) != "" {
		if err := store.AddChatMentions(context.Background(), []db.ChatMention{{
			MessageID: msg.ID, ThreadID: thread.ID, Agent: agent, Resolved: true, Unread: true,
		}}); err != nil {
			t.Fatalf("AddChatMentions: %v", err)
		}
	}
	return msg
}

// recordingChatDispatcher captures every dispatch and returns a synthetic output.
func recordingChatDispatcher() (chatAutoRespondDispatcher, *[]localAgentDispatchRequest) {
	var seen []localAgentDispatchRequest
	d := func(_ context.Context, _ *db.Store, request localAgentDispatchRequest) (localAgentJobOutput, error) {
		seen = append(seen, request)
		return localAgentJobOutput{JobID: "job-" + request.Agent, State: "queued"}, nil
	}
	return d, &seen
}

func unreadInThread(t *testing.T, store *db.Store, threadID string) int {
	t.Helper()
	n, err := store.CountUnreadMentionsForThread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("CountUnreadMentionsForThread: %v", err)
	}
	return n
}

const enrolledResponderBody = `
[chat]
auto_respond = true

[agents.responder]
runtime = "codex"
role = "responder"
chat_autorespond = true
`

// TestChatAutoRespondOffByDefault is the "zero queries on the tick hot path"
// invariant: with the global switch off, the sweep returns BEFORE any chat-table
// query (the candidate seam is never called), never dispatches, and leaves the
// triggering mention unread.
func TestChatAutoRespondOffByDefault(t *testing.T) {
	// Agent enrolled, but [chat].auto_respond is NOT set (off).
	paths, store := chatAutoRespondFixture(t, `
[agents.responder]
runtime = "codex"
role = "responder"
chat_autorespond = true
`)
	thread := seedChatThread(t, store, "room", "owner/repo")
	seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindHuman, "human", "@responder ping", "responder")

	restore := chatAutoRespondCandidates
	chatAutoRespondCandidates = func(context.Context, *db.Store) ([]db.ChatAutoRespondCandidate, error) {
		t.Fatal("candidate query must not run when auto_respond is off")
		return nil, nil
	}
	t.Cleanup(func() { chatAutoRespondCandidates = restore })

	dispatch, seen := recordingChatDispatcher()
	if err := runChatAutoRespondScanOnce(context.Background(), paths, paths.Home, store, dispatch, time.Now().UTC()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("off path dispatched %d jobs", len(*seen))
	}
	if unreadInThread(t, store, thread.ID) != 1 {
		t.Fatalf("off path consumed the mention")
	}
}

// TestChatAutoRespondNobodyEnrolled proves that even with the switch ON, if no agent
// opts in the sweep returns before any chat-table query.
func TestChatAutoRespondNobodyEnrolled(t *testing.T) {
	paths, store := chatAutoRespondFixture(t, `
[chat]
auto_respond = true

[agents.responder]
runtime = "codex"
role = "responder"
`)
	thread := seedChatThread(t, store, "room", "owner/repo")
	seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindHuman, "human", "@responder ping", "responder")

	restore := chatAutoRespondCandidates
	chatAutoRespondCandidates = func(context.Context, *db.Store) ([]db.ChatAutoRespondCandidate, error) {
		t.Fatal("candidate query must not run when nobody is enrolled")
		return nil, nil
	}
	t.Cleanup(func() { chatAutoRespondCandidates = restore })

	dispatch, seen := recordingChatDispatcher()
	if err := runChatAutoRespondScanOnce(context.Background(), paths, paths.Home, store, dispatch, time.Now().UTC()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("unenrolled path dispatched %d jobs", len(*seen))
	}
}

// TestChatAutoRespondEnqueuesAndIsIdempotent proves a chat @mention of an enrolled
// agent enqueues exactly ONE read-only ask (shaped like chat task: ask, background,
// ThreadID + ChatMessageID set), marks the mention read, and a re-scan does NOT
// re-fire (structural idempotency: the same mention can never double-dispatch).
func TestChatAutoRespondEnqueuesAndIsIdempotent(t *testing.T) {
	paths, store := chatAutoRespondFixture(t, enrolledResponderBody)
	thread := seedChatThread(t, store, "room", "owner/repo")
	msg := seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindHuman, "human", "@responder ping", "responder")

	dispatch, seen := recordingChatDispatcher()
	if err := runChatAutoRespondScanOnce(context.Background(), paths, paths.Home, store, dispatch, time.Now().UTC()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(*seen))
	}
	req := (*seen)[0]
	if req.Agent != "responder" || req.Action != "ask" || !req.Background ||
		req.ThreadID != thread.ID || req.ChatMessageID != msg.ID || req.RepoFlag != "owner/repo" {
		t.Fatalf("unexpected dispatch shape: %+v", req)
	}
	if !strings.Contains(req.Instructions, "Reply conversationally") {
		t.Fatalf("instructions missing conversational framing: %q", req.Instructions)
	}
	if unreadInThread(t, store, thread.ID) != 0 {
		t.Fatalf("trigger mention was not marked read after enqueue")
	}
	// Re-scan: the mention is read, so no second dispatch.
	if err := runChatAutoRespondScanOnce(context.Background(), paths, paths.Home, store, dispatch, time.Now().UTC()); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("re-scan double-fired: %d dispatches", len(*seen))
	}
}

// TestChatAutoRespondKindFiltering proves job_result / system / promotion_request
// messages NEVER produce a candidate — only kind='chat' does (the structural
// anti-ping-pong guarantee, enforced in SQL not prose).
func TestChatAutoRespondKindFiltering(t *testing.T) {
	_, store := chatAutoRespondFixture(t, enrolledResponderBody)
	thread := seedChatThread(t, store, "room", "owner/repo")
	seedChatMention(t, store, thread, db.ChatKindJobResult, db.ChatAuthorKindAgent, "responder", "prior result", "responder")
	seedChatMention(t, store, thread, db.ChatKindSystem, db.ChatAuthorKindSystem, "system", "escalation", "responder")
	seedChatMention(t, store, thread, db.ChatKindPromotionRequest, db.ChatAuthorKindHuman, "human", "@responder do it", "responder")

	candidates, err := store.ListChatAutoRespondCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListChatAutoRespondCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("non-chat kinds produced %d candidates, want 0: %+v", len(candidates), candidates)
	}

	// Now add a real chat mention: exactly one candidate, on the chat message.
	msg := seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindHuman, "human", "@responder ping", "responder")
	candidates, err = store.ListChatAutoRespondCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListChatAutoRespondCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].LastMessageID != msg.ID || candidates[0].Agent != "responder" {
		t.Fatalf("chat mention produced wrong candidate set: %+v", candidates)
	}
}

// TestChatAutoRespondOptInGating proves an @mention of a NON-enrolled agent (switch
// on) is skipped without dispatch and without consuming the mention.
func TestChatAutoRespondOptInGating(t *testing.T) {
	paths, store := chatAutoRespondFixture(t, `
[chat]
auto_respond = true

[agents.responder]
runtime = "codex"
role = "responder"
chat_autorespond = true

[agents.bystander]
runtime = "codex"
role = "bystander"
`)
	thread := seedChatThread(t, store, "room", "owner/repo")
	seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindHuman, "human", "@bystander ping", "bystander")

	dispatch, seen := recordingChatDispatcher()
	if err := runChatAutoRespondScanOnce(context.Background(), paths, paths.Home, store, dispatch, time.Now().UTC()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("non-enrolled agent dispatched %d jobs", len(*seen))
	}
	if unreadInThread(t, store, thread.ID) != 1 {
		t.Fatalf("non-enrolled path consumed the mention")
	}
}

// TestChatAutoRespondCapHitAndDedupe proves the cap HARD-STOPS: at the cap no job is
// dispatched, ONE visible "needs a human" system message is posted, the trigger is
// parked (marked read), and a later re-trigger does NOT duplicate the system message.
func TestChatAutoRespondCapHitAndDedupe(t *testing.T) {
	paths, store := chatAutoRespondFixture(t, `
[chat]
auto_respond = true
auto_respond_cap = 1

[agents.responder]
runtime = "codex"
role = "responder"
chat_autorespond = true
`)
	thread := seedChatThread(t, store, "room", "owner/repo")
	// The agent already has 1 auto-respond reply (a job_result) → count == cap.
	seedChatMention(t, store, thread, db.ChatKindJobResult, db.ChatAuthorKindAgent, "responder", "prior reply", "")
	seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindHuman, "human", "@responder again", "responder")

	dispatch, seen := recordingChatDispatcher()
	if err := runChatAutoRespondScanOnce(context.Background(), paths, paths.Home, store, dispatch, time.Now().UTC()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("cap-hit dispatched %d jobs, want 0", len(*seen))
	}
	wantBody := chatAutoRespondCapMessage("responder")
	if got := countChatKindBody(t, store, thread.ID, db.ChatKindSystem, wantBody); got != 1 {
		t.Fatalf("cap system message count = %d, want 1", got)
	}
	if unreadInThread(t, store, thread.ID) != 0 {
		t.Fatalf("cap-hit did not park (mark read) the trigger")
	}

	// A NEW chat trigger arrives; a re-scan must re-park WITHOUT duplicating the
	// system message.
	seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindHuman, "human", "@responder still there?", "responder")
	if err := runChatAutoRespondScanOnce(context.Background(), paths, paths.Home, store, dispatch, time.Now().UTC()); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("cap-hit rescan dispatched %d jobs, want 0", len(*seen))
	}
	if got := countChatKindBody(t, store, thread.ID, db.ChatKindSystem, wantBody); got != 1 {
		t.Fatalf("cap system message duplicated: count = %d, want 1", got)
	}
}

// TestChatAutoRespondCooldown proves a trigger inside the cooldown window is deferred
// (no dispatch, mention left UNREAD to re-fire), and fires once the window passes.
func TestChatAutoRespondCooldown(t *testing.T) {
	paths, store := chatAutoRespondFixture(t, `
[chat]
auto_respond = true
auto_respond_cooldown = "2m"

[agents.responder]
runtime = "codex"
role = "responder"
chat_autorespond = true
`)
	thread := seedChatThread(t, store, "room", "owner/repo")
	// A recent auto-respond reply sets the cooldown clock (its ts_ms ~ now).
	seedChatMention(t, store, thread, db.ChatKindJobResult, db.ChatAuthorKindAgent, "responder", "just replied", "")
	seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindHuman, "human", "@responder more", "responder")

	dispatch, seen := recordingChatDispatcher()
	if err := runChatAutoRespondScanOnce(context.Background(), paths, paths.Home, store, dispatch, time.Now().UTC()); err != nil {
		t.Fatalf("scan (within cooldown): %v", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("within-cooldown dispatched %d jobs, want 0", len(*seen))
	}
	if unreadInThread(t, store, thread.ID) != 1 {
		t.Fatalf("within-cooldown must leave the mention unread to re-fire")
	}

	// Past the cooldown window: it fires.
	if err := runChatAutoRespondScanOnce(context.Background(), paths, paths.Home, store, dispatch, time.Now().UTC().Add(3*time.Minute)); err != nil {
		t.Fatalf("scan (after cooldown): %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("after-cooldown expected 1 dispatch, got %d", len(*seen))
	}
	if unreadInThread(t, store, thread.ID) != 0 {
		t.Fatalf("after-cooldown dispatch did not mark the mention read")
	}
}

// TestChatAutoRespondFailedEnqueueLeavesMentionUnread proves a dispatch error does
// not permanently eat the mention: it stays unread so the next tick retries.
func TestChatAutoRespondFailedEnqueueLeavesMentionUnread(t *testing.T) {
	paths, store := chatAutoRespondFixture(t, enrolledResponderBody)
	thread := seedChatThread(t, store, "room", "owner/repo")
	seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindHuman, "human", "@responder ping", "responder")

	failing := func(context.Context, *db.Store, localAgentDispatchRequest) (localAgentJobOutput, error) {
		return localAgentJobOutput{}, context.DeadlineExceeded
	}
	if err := runChatAutoRespondScanOnce(context.Background(), paths, paths.Home, store, failing, time.Now().UTC()); err == nil {
		t.Fatal("expected the dispatch error to surface")
	}
	if unreadInThread(t, store, thread.ID) != 1 {
		t.Fatalf("failed enqueue consumed the mention (must stay unread to retry)")
	}
}

// countChatKindBody counts messages of a kind with an exact body in a thread.
func countChatKindBody(t *testing.T, store *db.Store, threadID, kind, body string) int {
	t.Helper()
	msgs, err := store.ListChatMessages(context.Background(), threadID, 0)
	if err != nil {
		t.Fatalf("ListChatMessages: %v", err)
	}
	n := 0
	for _, m := range msgs {
		if m.Kind == kind && m.Body == body {
			n++
		}
	}
	return n
}
