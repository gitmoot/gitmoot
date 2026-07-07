package db

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func openChatTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestHomeIDStableAndReserved proves HomeID get-or-creates a stable per-DB id
// and that it is a generated hex value, NOT the literal "self".
func TestHomeIDStableAndReserved(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)

	first, err := store.HomeID(ctx)
	if err != nil {
		t.Fatalf("HomeID returned error: %v", err)
	}
	if first == "" || first == "self" {
		t.Fatalf("HomeID = %q, want a generated non-empty id that is not the literal \"self\"", first)
	}
	second, err := store.HomeID(ctx)
	if err != nil {
		t.Fatalf("HomeID (2nd) returned error: %v", err)
	}
	if first != second {
		t.Fatalf("HomeID not stable: %q then %q", first, second)
	}
}

// TestCreateChatThreadStampsOriginAndSlug proves a created thread carries the
// home_id origin (not "self"), defaults name to slug, and defaults state to open.
func TestCreateChatThreadStampsOrigin(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)
	home, _ := store.HomeID(ctx)

	thread, err := store.CreateChatThread(ctx, ChatThread{Slug: "release-room", Repo: "o/r"})
	if err != nil {
		t.Fatalf("CreateChatThread returned error: %v", err)
	}
	if thread.ID == "" {
		t.Fatal("CreateChatThread did not assign an id")
	}
	if thread.Origin != home {
		t.Fatalf("thread origin = %q, want home_id %q (no code may assume origin==self)", thread.Origin, home)
	}
	if thread.Name != "release-room" {
		t.Fatalf("thread name = %q, want it to default to the slug", thread.Name)
	}
	if thread.State != ChatThreadStateOpen {
		t.Fatalf("thread state = %q, want open", thread.State)
	}

	byID, err := store.GetChatThreadByID(ctx, thread.ID)
	if err != nil {
		t.Fatalf("GetChatThreadByID returned error: %v", err)
	}
	bySlug, err := store.GetChatThreadBySlug(ctx, "o/r", "release-room")
	if err != nil {
		t.Fatalf("GetChatThreadBySlug returned error: %v", err)
	}
	if byID.ID != thread.ID || bySlug.ID != thread.ID {
		t.Fatalf("id/slug lookups disagree: %+v %+v", byID, bySlug)
	}
}

// TestCreateChatThreadUniquePerRepo proves the UNIQUE(repo, slug) constraint:
// the same slug is allowed in a different repo but rejected in the same one.
func TestCreateChatThreadUniquePerRepo(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)

	if _, err := store.CreateChatThread(ctx, ChatThread{Slug: "room", Repo: "o/a"}); err != nil {
		t.Fatalf("first create returned error: %v", err)
	}
	if _, err := store.CreateChatThread(ctx, ChatThread{Slug: "room", Repo: "o/b"}); err != nil {
		t.Fatalf("same slug in a different repo should be allowed: %v", err)
	}
	if _, err := store.CreateChatThread(ctx, ChatThread{Slug: "room", Repo: "o/a"}); err == nil {
		t.Fatal("duplicate (repo, slug) should be rejected")
	}
}

// TestAddChatMessageSeqAndEnvelope proves per-thread seq assignment, unix-millis
// ts_ms, home_id origin stamping, and a deterministic canonical envelope.
func TestAddChatMessageSeqAndEnvelope(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)
	home, _ := store.HomeID(ctx)
	thread, _ := store.CreateChatThread(ctx, ChatThread{Slug: "room", Repo: "o/r"})

	m1, err := store.AddChatMessage(ctx, ChatMessage{
		ThreadID: thread.ID, AuthorName: "human", Body: "hello @codex-b",
		Mentions: []string{"codex-b"},
		Refs:     []ChatRef{{Kind: "job", ID: "job-1"}},
	})
	if err != nil {
		t.Fatalf("AddChatMessage returned error: %v", err)
	}
	m2, err := store.AddChatMessage(ctx, ChatMessage{ThreadID: thread.ID, AuthorName: "codex-b", AuthorKind: ChatAuthorKindAgent, Body: "on it"})
	if err != nil {
		t.Fatalf("AddChatMessage (2) returned error: %v", err)
	}
	if m1.Seq != 1 || m2.Seq != 2 {
		t.Fatalf("seq assignment = %d,%d, want 1,2", m1.Seq, m2.Seq)
	}
	if m1.TsMs <= 0 {
		t.Fatalf("ts_ms = %d, want unix-millis > 0", m1.TsMs)
	}
	if m1.Origin != home || m1.AuthorOrigin != home {
		t.Fatalf("origin/author_origin = %q/%q, want home_id %q", m1.Origin, m1.AuthorOrigin, home)
	}
	// Ref origin defaulted to home_id.
	if len(m1.Refs) != 1 || m1.Refs[0].Origin != home {
		t.Fatalf("ref origin = %+v, want home_id-qualified", m1.Refs)
	}

	// Envelope is the versioned canonical unit with deterministic key order.
	var env struct {
		SchemaVersion int       `json:"schema_version"`
		Kind          string    `json:"kind"`
		Body          string    `json:"body"`
		Mentions      []string  `json:"mentions"`
		Refs          []ChatRef `json:"refs"`
		ReplyTo       string    `json:"reply_to"`
	}
	if err := json.Unmarshal([]byte(m1.EnvelopeJSON), &env); err != nil {
		t.Fatalf("envelope is not valid json: %v (%s)", err, m1.EnvelopeJSON)
	}
	if env.SchemaVersion != 1 || env.Kind != ChatKindChat || env.Body != "hello @codex-b" {
		t.Fatalf("envelope mismatch: %+v", env)
	}
	if len(env.Mentions) != 1 || env.Mentions[0] != "codex-b" {
		t.Fatalf("envelope mentions = %v", env.Mentions)
	}
	// Deterministic key order (schema_version first, reply_to last).
	wantPrefix := `{"schema_version":1,`
	if got := m1.EnvelopeJSON[:len(wantPrefix)]; got != wantPrefix {
		t.Fatalf("envelope key order not deterministic, got prefix %q", got)
	}
}

// TestAddChatMessageRejectsBadKind proves the fixed kind vocabulary is enforced.
func TestAddChatMessageRejectsBadKind(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)
	thread, _ := store.CreateChatThread(ctx, ChatThread{Slug: "room", Repo: "o/r"})
	if _, err := store.AddChatMessage(ctx, ChatMessage{ThreadID: thread.ID, AuthorName: "x", Kind: "shout"}); err == nil {
		t.Fatal("AddChatMessage accepted an unsupported kind")
	}
}

// TestListChatMessagesOrdering proves ListChatMessages orders by (ts_ms, seq):
// same-millisecond messages render in INSERTION order via the gapless per-thread
// seq, NOT by the random message id (which would scramble same-ms same-author
// messages). It seeds several rows sharing one ts_ms with deliberately
// out-of-order random ids and asserts the seq (insertion) order is what renders.
func TestListChatMessagesOrdering(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)
	thread, _ := store.CreateChatThread(ctx, ChatThread{Slug: "room", Repo: "o/r"})
	for i := 0; i < 5; i++ {
		if _, err := store.AddChatMessage(ctx, ChatMessage{ThreadID: thread.ID, AuthorName: "human", Body: "m"}); err != nil {
			t.Fatalf("AddChatMessage returned error: %v", err)
		}
	}
	all, err := store.ListChatMessages(ctx, thread.ID, 0)
	if err != nil {
		t.Fatalf("ListChatMessages returned error: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("ListChatMessages returned %d, want 5", len(all))
	}
	// Adjacent pairs are non-decreasing under (ts_ms, seq) AND seq is the insertion
	// order (strictly increasing, gapless), so the transcript never scrambles.
	for i := 1; i < len(all); i++ {
		prev, cur := all[i-1], all[i]
		if cur.TsMs < prev.TsMs || (cur.TsMs == prev.TsMs && cur.Seq < prev.Seq) {
			t.Fatalf("messages not ordered by (ts_ms, seq): #%d(seq=%d) then #%d(seq=%d)", i-1, prev.Seq, i, cur.Seq)
		}
		if cur.Seq != prev.Seq+1 {
			t.Fatalf("seq not gapless insertion order: %d then %d", prev.Seq, cur.Seq)
		}
	}

	// The hard case the fix targets: rows sharing ONE ts_ms with random ids inserted
	// out of id-order must still render by seq (insertion), not by id.
	same, _ := store.CreateChatThread(ctx, ChatThread{Slug: "same-ms", Repo: "o/r"})
	seedSameTs := func(id string, seq int64) {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO chat_messages(
				id, origin, thread_id, seq, ts_ms, author_kind, author_name, author_origin,
				kind, body, envelope_json, refs_json, reply_to, promoted_job_id, created_at
			) VALUES (?, '', ?, ?, 1000, 'human', 'human', '', 'chat', 'b', '', '', '', '', CURRENT_TIMESTAMP)`,
			id, same.ID, seq); err != nil {
			t.Fatalf("seed same-ms row: %v", err)
		}
	}
	seedSameTs("msg-zzz", 1)
	seedSameTs("msg-aaa", 2)
	seedSameTs("msg-mmm", 3)
	got, err := store.ListChatMessages(ctx, same.ID, 0)
	if err != nil {
		t.Fatalf("ListChatMessages(same-ms) returned error: %v", err)
	}
	wantOrder := []string{"msg-zzz", "msg-aaa", "msg-mmm"} // seq 1,2,3 — NOT id order
	if len(got) != 3 {
		t.Fatalf("same-ms list len = %d, want 3", len(got))
	}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Fatalf("same-ms order[%d] = %s, want %s (must be by seq, not id)", i, got[i].ID, w)
		}
	}

	// A positive limit returns the LAST N of the full ordered list (still ascending).
	last2, err := store.ListChatMessages(ctx, thread.ID, 2)
	if err != nil {
		t.Fatalf("ListChatMessages(limit) returned error: %v", err)
	}
	if len(last2) != 2 {
		t.Fatalf("limited list len = %d, want 2", len(last2))
	}
	if last2[0].ID != all[3].ID || last2[1].ID != all[4].ID {
		t.Fatalf("limited list = [%s %s], want the last two [%s %s]", last2[0].ID, last2[1].ID, all[3].ID, all[4].ID)
	}
}

// TestMentionsInboxAndRead proves the mention -> inbox index and MarkThreadRead.
func TestMentionsInboxAndRead(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)
	thread, _ := store.CreateChatThread(ctx, ChatThread{Slug: "room", Repo: "o/r"})
	msg, _ := store.AddChatMessage(ctx, ChatMessage{ThreadID: thread.ID, AuthorName: "human", Body: "@codex-b @ghost"})

	if err := store.AddChatMentions(ctx, []ChatMention{
		{MessageID: msg.ID, ThreadID: thread.ID, Agent: "codex-b", Resolved: true, Unread: true},
		{MessageID: msg.ID, ThreadID: thread.ID, Agent: "ghost", Resolved: false, Unread: true},
	}); err != nil {
		t.Fatalf("AddChatMentions returned error: %v", err)
	}

	// Resolved agent sees the inbox entry; unresolved "ghost" does not.
	inbox, err := store.InboxForAgent(ctx, "codex-b", false)
	if err != nil {
		t.Fatalf("InboxForAgent returned error: %v", err)
	}
	if len(inbox) != 1 || inbox[0].ThreadSlug != "room" || !inbox[0].Unread {
		t.Fatalf("inbox = %+v, want one unread entry for room", inbox)
	}
	ghost, _ := store.InboxForAgent(ctx, "ghost", false)
	if len(ghost) != 0 {
		t.Fatalf("unresolved mention should not appear in an inbox, got %+v", ghost)
	}

	// Mark read clears the unread flag.
	n, err := store.MarkThreadRead(ctx, "codex-b", thread.ID)
	if err != nil {
		t.Fatalf("MarkThreadRead returned error: %v", err)
	}
	if n != 1 {
		t.Fatalf("MarkThreadRead cleared %d, want 1", n)
	}
	unread, _ := store.InboxForAgent(ctx, "codex-b", true)
	if len(unread) != 0 {
		t.Fatalf("after MarkThreadRead there should be no unread, got %+v", unread)
	}
	// The mention still exists (audit) when unreadOnly=false.
	if seen, _ := store.InboxForAgent(ctx, "codex-b", false); len(seen) != 1 {
		t.Fatalf("read mention should still be visible, got %+v", seen)
	}
}

// TestChatThreadLifecycle proves rename (display name only, slug immutable),
// close/reopen state transitions, and list filtering by state.
func TestChatThreadLifecycle(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)
	thread, _ := store.CreateChatThread(ctx, ChatThread{Slug: "room", Repo: "o/r"})

	if err := store.RenameChatThread(ctx, thread.ID, "Release Room"); err != nil {
		t.Fatalf("RenameChatThread returned error: %v", err)
	}
	renamed, _ := store.GetChatThreadByID(ctx, thread.ID)
	if renamed.Name != "Release Room" {
		t.Fatalf("rename name = %q, want \"Release Room\"", renamed.Name)
	}
	if renamed.Slug != "room" {
		t.Fatalf("rename changed the slug to %q; the slug must be immutable", renamed.Slug)
	}

	if err := store.SetChatThreadState(ctx, thread.ID, ChatThreadStateArchived); err != nil {
		t.Fatalf("SetChatThreadState(archived) returned error: %v", err)
	}
	open, _ := store.ListChatThreads(ctx, "o/r", ChatThreadStateOpen)
	if len(open) != 0 {
		t.Fatalf("archived thread should not appear in open list, got %+v", open)
	}
	all, _ := store.ListChatThreads(ctx, "o/r", "")
	if len(all) != 1 {
		t.Fatalf("archived thread should still be listable, got %+v", all)
	}
	if err := store.SetChatThreadState(ctx, thread.ID, ChatThreadStateOpen); err != nil {
		t.Fatalf("SetChatThreadState(open) returned error: %v", err)
	}
	reopened, _ := store.ListChatThreads(ctx, "o/r", ChatThreadStateOpen)
	if len(reopened) != 1 {
		t.Fatalf("reopened thread should appear in open list, got %+v", reopened)
	}
}

// TestSetChatMessagePromotedJob proves the promotion → job back-reference write
// (#534) and that a missing message id is a clear error.
func TestSetChatMessagePromotedJob(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)
	thread, err := store.CreateChatThread(ctx, ChatThread{Slug: "room", Repo: "o/r"})
	if err != nil {
		t.Fatalf("CreateChatThread: %v", err)
	}
	msg, err := store.AddChatMessage(ctx, ChatMessage{
		ThreadID: thread.ID, AuthorName: "human", Kind: ChatKindPromotionRequest, Body: "@a go",
	})
	if err != nil {
		t.Fatalf("AddChatMessage: %v", err)
	}
	if err := store.SetChatMessagePromotedJob(ctx, msg.ID, "job-123"); err != nil {
		t.Fatalf("SetChatMessagePromotedJob: %v", err)
	}
	got, err := store.ListChatMessages(ctx, thread.ID, 0)
	if err != nil {
		t.Fatalf("ListChatMessages: %v", err)
	}
	if len(got) != 1 || got[0].PromotedJobID != "job-123" {
		t.Fatalf("promoted_job_id not persisted: %+v", got)
	}
	if err := store.SetChatMessagePromotedJob(ctx, "nope", "job-x"); err == nil {
		t.Fatal("SetChatMessagePromotedJob on a missing message should error")
	}
}

// TestRecentPromotionRequestExists proves the fingerprint dedupe: an identical
// (thread, body) promotion_request that ACTUALLY produced a job is detected within
// the window and not across a different body / thread / message kind, and — the
// review fix — a promotion whose dispatch FAILED (no promoted_job_id) does NOT
// dedupe so a legitimate retry is never blocked (#534 anti-ping-pong).
func TestRecentPromotionRequestExists(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)
	thread, _ := store.CreateChatThread(ctx, ChatThread{Slug: "room", Repo: "o/r"})
	other, _ := store.CreateChatThread(ctx, ChatThread{Slug: "room2", Repo: "o/r"})

	// A promotion that produced a job: record the row then back-link it (the real
	// `chat task` order — record before dispatch, set promoted_job_id after).
	promo, err := store.AddChatMessage(ctx, ChatMessage{
		ThreadID: thread.ID, AuthorName: "human", Kind: ChatKindPromotionRequest, Body: "@a ship it",
	})
	if err != nil {
		t.Fatalf("AddChatMessage: %v", err)
	}
	if err := store.SetChatMessagePromotedJob(ctx, promo.ID, "job-1"); err != nil {
		t.Fatalf("SetChatMessagePromotedJob: %v", err)
	}
	// A plain chat message with the same body must NOT count as a promotion.
	if _, err := store.AddChatMessage(ctx, ChatMessage{
		ThreadID: thread.ID, AuthorName: "human", Kind: ChatKindChat, Body: "@a ship it different",
	}); err != nil {
		t.Fatalf("AddChatMessage: %v", err)
	}

	dup, err := store.RecentPromotionRequestExists(ctx, thread.ID, "@a ship it", chatMinuteMs)
	if err != nil {
		t.Fatalf("RecentPromotionRequestExists: %v", err)
	}
	if !dup {
		t.Fatal("identical promotion should be detected as a duplicate")
	}
	if none, _ := store.RecentPromotionRequestExists(ctx, thread.ID, "@a other body", chatMinuteMs); none {
		t.Fatal("a different body must not dedupe")
	}
	if none, _ := store.RecentPromotionRequestExists(ctx, other.ID, "@a ship it", chatMinuteMs); none {
		t.Fatal("a different thread must not dedupe")
	}
	// windowMs <= 0 disables the check.
	if any, _ := store.RecentPromotionRequestExists(ctx, thread.ID, "@a ship it", 0); any {
		t.Fatal("a zero window must disable dedupe")
	}

	// A FAILED dispatch leaves an orphan promotion_request (no promoted_job_id); an
	// identical retry must NOT be refused.
	orphanThread, _ := store.CreateChatThread(ctx, ChatThread{Slug: "orphan", Repo: "o/r"})
	if _, err := store.AddChatMessage(ctx, ChatMessage{
		ThreadID: orphanThread.ID, AuthorName: "human", Kind: ChatKindPromotionRequest, Body: "@a retry me",
	}); err != nil {
		t.Fatalf("AddChatMessage (orphan): %v", err)
	}
	if any, _ := store.RecentPromotionRequestExists(ctx, orphanThread.ID, "@a retry me", chatMinuteMs); any {
		t.Fatal("a promotion whose dispatch failed (no promoted_job_id) must NOT poison the dedupe")
	}
}

const chatMinuteMs = 60_000

// TestChatThreadRollupHelpers proves the three dashboard-facing aggregate helpers
// (ListChatThreadMessageStats, CountUnreadMentionsByThread,
// ListChatThreadParticipants) return correct per-thread rollups in bounded queries:
// message counts + the last message (greatest (ts_ms, seq)), resolved+unread
// mention counts (origin-scoped), and the author ∪ resolved-mention participant set.
func TestChatThreadRollupHelpers(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)

	busy, _ := store.CreateChatThread(ctx, ChatThread{Slug: "busy", Repo: "o/r"})
	empty, _ := store.CreateChatThread(ctx, ChatThread{Slug: "empty", Repo: "o/r"})

	m1, err := store.AddChatMessage(ctx, ChatMessage{ThreadID: busy.ID, AuthorKind: ChatAuthorKindHuman, AuthorName: "jerry", Body: "@codex-b @ghost"})
	if err != nil {
		t.Fatalf("AddChatMessage m1: %v", err)
	}
	if err := store.AddChatMentions(ctx, []ChatMention{
		{MessageID: m1.ID, ThreadID: busy.ID, Agent: "codex-b", Resolved: true, Unread: true},
		{MessageID: m1.ID, ThreadID: busy.ID, Agent: "ghost", Resolved: false, Unread: true},
	}); err != nil {
		t.Fatalf("AddChatMentions: %v", err)
	}
	if _, err := store.AddChatMessage(ctx, ChatMessage{ThreadID: busy.ID, AuthorKind: ChatAuthorKindAgent, AuthorName: "codex-b", Kind: ChatKindJobResult, Body: "done"}); err != nil {
		t.Fatalf("AddChatMessage m2: %v", err)
	}

	// Message stats: busy has 2 messages, last is codex-b's job_result; empty absent.
	stats, err := store.ListChatThreadMessageStats(ctx)
	if err != nil {
		t.Fatalf("ListChatThreadMessageStats: %v", err)
	}
	byThread := map[string]ChatThreadMessageStat{}
	for _, s := range stats {
		byThread[s.ThreadID] = s
	}
	if _, ok := byThread[empty.ID]; ok {
		t.Fatal("empty thread must be absent from message stats")
	}
	bs := byThread[busy.ID]
	if bs.MessageCount != 2 || bs.LastAuthorName != "codex-b" || bs.LastKind != ChatKindJobResult || bs.LastBody != "done" {
		t.Fatalf("busy stats = %+v, want count 2 / last codex-b job_result 'done'", bs)
	}

	// Unread mentions: only the resolved @codex-b counts.
	counts, err := store.CountUnreadMentionsByThread(ctx)
	if err != nil {
		t.Fatalf("CountUnreadMentionsByThread: %v", err)
	}
	unread := map[string]int{}
	for _, c := range counts {
		unread[c.ThreadID] = c.Count
	}
	if unread[busy.ID] != 1 {
		t.Fatalf("busy unread = %d, want 1 (resolved only)", unread[busy.ID])
	}

	// Marking the thread read clears the unread badge.
	if _, err := store.MarkThreadRead(ctx, "codex-b", busy.ID); err != nil {
		t.Fatalf("MarkThreadRead: %v", err)
	}
	counts, _ = store.CountUnreadMentionsByThread(ctx)
	for _, c := range counts {
		if c.ThreadID == busy.ID && c.Count != 0 {
			t.Fatalf("busy unread after read = %d, want 0", c.Count)
		}
	}

	// Participants: authors (jerry, codex-b) ∪ resolved mentions (codex-b); @ghost
	// excluded. Sorted (thread_id, name).
	parts, err := store.ListChatThreadParticipants(ctx)
	if err != nil {
		t.Fatalf("ListChatThreadParticipants: %v", err)
	}
	var busyParts []string
	for _, p := range parts {
		if p.ThreadID == busy.ID {
			busyParts = append(busyParts, p.Name)
		}
	}
	if len(busyParts) != 2 || busyParts[0] != "codex-b" || busyParts[1] != "jerry" {
		t.Fatalf("busy participants = %v, want [codex-b jerry]", busyParts)
	}
}

// TestChatParticipantsExcludeSystem proves a `system` message (the authorless
// ask-gate post, stamped author_name "system" by chat_link.go) never leaks into a
// thread's participant set — neither the corpus-wide ListChatThreadParticipants nor
// the thread-scoped ListChatThreadParticipantsForThread returns a bogus "system"
// chip — while a real human ("jerry") and a real agent ("reviewer") do.
func TestChatParticipantsExcludeSystem(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)
	thread, _ := store.CreateChatThread(ctx, ChatThread{Slug: "adapter-review", Repo: "o/r"})

	if _, err := store.AddChatMessage(ctx, ChatMessage{ThreadID: thread.ID, AuthorKind: ChatAuthorKindAgent, AuthorName: "reviewer", Body: "looking now"}); err != nil {
		t.Fatalf("AddChatMessage reviewer: %v", err)
	}
	// The ask-gate system post: author_kind=system, author_name="system" (exactly what
	// workflow/chat_link.go writes). This must NOT become a participant.
	if _, err := store.AddChatMessage(ctx, ChatMessage{ThreadID: thread.ID, AuthorKind: ChatAuthorKindSystem, AuthorName: "system", Kind: ChatKindSystem, Body: "paused: awaiting an answer"}); err != nil {
		t.Fatalf("AddChatMessage system: %v", err)
	}
	if _, err := store.AddChatMessage(ctx, ChatMessage{ThreadID: thread.ID, AuthorKind: ChatAuthorKindHuman, AuthorName: "jerry", Body: "go ahead"}); err != nil {
		t.Fatalf("AddChatMessage jerry: %v", err)
	}

	// Corpus-wide list path.
	parts, err := store.ListChatThreadParticipants(ctx)
	if err != nil {
		t.Fatalf("ListChatThreadParticipants: %v", err)
	}
	var got []string
	for _, p := range parts {
		if p.ThreadID == thread.ID {
			got = append(got, p.Name)
		}
	}
	if len(got) != 2 || got[0] != "jerry" || got[1] != "reviewer" {
		t.Fatalf("list participants = %v, want [jerry reviewer] (no \"system\")", got)
	}

	// Thread-scoped detail path must return the identical set.
	scoped, err := store.ListChatThreadParticipantsForThread(ctx, thread.ID)
	if err != nil {
		t.Fatalf("ListChatThreadParticipantsForThread: %v", err)
	}
	if len(scoped) != 2 || scoped[0] != "jerry" || scoped[1] != "reviewer" {
		t.Fatalf("scoped participants = %v, want [jerry reviewer] (no \"system\")", scoped)
	}
}

// TestChatThreadScopedRollups proves the single-thread detail helpers
// (CountUnreadMentionsForThread, ListChatThreadParticipantsForThread) return the
// same values the corpus-wide list aggregates do, per thread, and that the mention
// side is origin-scoped (a mention stamped for another home never counts).
func TestChatThreadScopedRollups(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)

	busy, _ := store.CreateChatThread(ctx, ChatThread{Slug: "busy", Repo: "o/r"})
	quiet, _ := store.CreateChatThread(ctx, ChatThread{Slug: "quiet", Repo: "o/r"})

	m1, _ := store.AddChatMessage(ctx, ChatMessage{ThreadID: busy.ID, AuthorKind: ChatAuthorKindHuman, AuthorName: "jerry", Body: "@codex-b @ghost"})
	if err := store.AddChatMentions(ctx, []ChatMention{
		{MessageID: m1.ID, ThreadID: busy.ID, Agent: "codex-b", Resolved: true, Unread: true},
		{MessageID: m1.ID, ThreadID: busy.ID, Agent: "ghost", Resolved: false, Unread: true},
	}); err != nil {
		t.Fatalf("AddChatMentions: %v", err)
	}

	// Scoped unread count equals the corpus-wide GROUP BY for this thread (only the
	// resolved @codex-b counts).
	got, err := store.CountUnreadMentionsForThread(ctx, busy.ID)
	if err != nil {
		t.Fatalf("CountUnreadMentionsForThread(busy): %v", err)
	}
	if got != 1 {
		t.Fatalf("busy scoped unread = %d, want 1", got)
	}
	if zero, _ := store.CountUnreadMentionsForThread(ctx, quiet.ID); zero != 0 {
		t.Fatalf("quiet scoped unread = %d, want 0", zero)
	}

	// A mention stamped for a DIFFERENT home must NOT count locally (origin-scope).
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO chat_mentions(message_id, thread_id, agent, agent_origin, resolved, unread)
			VALUES (?, ?, 'codex-b', 'other-home', 1, 1)`, m1.ID, busy.ID); err != nil {
		t.Fatalf("seed foreign-origin mention: %v", err)
	}
	if still, _ := store.CountUnreadMentionsForThread(ctx, busy.ID); still != 1 {
		t.Fatalf("busy scoped unread after foreign mention = %d, want 1 (origin-scoped)", still)
	}
	// The corpus-wide aggregate and the scoped variant must still agree.
	counts, _ := store.CountUnreadMentionsByThread(ctx)
	byThread := map[string]int{}
	for _, c := range counts {
		byThread[c.ThreadID] = c.Count
	}
	if byThread[busy.ID] != 1 {
		t.Fatalf("corpus unread = %d, want 1 (must agree with scoped, origin-scoped)", byThread[busy.ID])
	}
	// The foreign-origin agent is likewise excluded from the participant set.
	scoped, _ := store.ListChatThreadParticipantsForThread(ctx, busy.ID)
	if len(scoped) != 2 || scoped[0] != "codex-b" || scoped[1] != "jerry" {
		t.Fatalf("scoped participants = %v, want [codex-b jerry]", scoped)
	}
}

// TestChatThreadMootMetadata proves the moot metadata round-trips (MarkChatThreadMoot
// -> ChatThreadMoot), a non-moot thread reads (false, 0), and CountChatMootMessages
// counts ONLY agent-authored chat turns — excluding system, job_result, and human
// messages (the cap metric bounds seat turns only).
func TestChatThreadMootMetadata(t *testing.T) {
	ctx := context.Background()
	store := openChatTestStore(t)

	moot, err := store.CreateChatThread(ctx, ChatThread{Slug: "moot", Repo: "owner/repo", CreatedBy: ChatAuthorKindHuman})
	if err != nil {
		t.Fatalf("CreateChatThread: %v", err)
	}
	plain, err := store.CreateChatThread(ctx, ChatThread{Slug: "plain", Repo: "owner/repo", CreatedBy: ChatAuthorKindHuman})
	if err != nil {
		t.Fatalf("CreateChatThread: %v", err)
	}

	// A fresh thread is not a moot.
	if isMoot, _, err := store.ChatThreadMoot(ctx, plain.ID); err != nil || isMoot {
		t.Fatalf("plain ChatThreadMoot = (%v, err=%v), want (false, nil)", isMoot, err)
	}
	// A rejected cap.
	if err := store.MarkChatThreadMoot(ctx, moot.ID, 0); err == nil {
		t.Fatal("MarkChatThreadMoot cap=0 was accepted, want an error")
	}
	// Mark + read back.
	if err := store.MarkChatThreadMoot(ctx, moot.ID, 7); err != nil {
		t.Fatalf("MarkChatThreadMoot: %v", err)
	}
	isMoot, messageCap, err := store.ChatThreadMoot(ctx, moot.ID)
	if err != nil || !isMoot || messageCap != 7 {
		t.Fatalf("ChatThreadMoot = (%v, %d, err=%v), want (true, 7, nil)", isMoot, messageCap, err)
	}
	// Re-mark overwrites the cap (idempotent re-convene).
	if err := store.MarkChatThreadMoot(ctx, moot.ID, 3); err != nil {
		t.Fatalf("MarkChatThreadMoot re-mark: %v", err)
	}
	if _, messageCap, _ := store.ChatThreadMoot(ctx, moot.ID); messageCap != 3 {
		t.Fatalf("re-marked cap = %d, want 3", messageCap)
	}

	// Counting: only agent-authored kind='chat' messages count.
	add := func(kind, authorKind, name string) {
		if _, err := store.AddChatMessage(ctx, ChatMessage{ThreadID: moot.ID, Kind: kind, AuthorKind: authorKind, AuthorName: name, Body: "x"}); err != nil {
			t.Fatalf("AddChatMessage(%s/%s): %v", kind, authorKind, err)
		}
	}
	add(ChatKindChat, ChatAuthorKindAgent, "alice")  // counts
	add(ChatKindChat, ChatAuthorKindAgent, "bob")     // counts
	add(ChatKindChat, ChatAuthorKindHuman, "human")   // excluded (human)
	add(ChatKindSystem, ChatAuthorKindSystem, "system") // excluded (system)
	add(ChatKindJobResult, ChatAuthorKindAgent, "alice") // excluded (job_result)

	n, err := store.CountChatMootMessages(ctx, moot.ID)
	if err != nil {
		t.Fatalf("CountChatMootMessages: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountChatMootMessages = %d, want 2 (agent chat turns only)", n)
	}
}
