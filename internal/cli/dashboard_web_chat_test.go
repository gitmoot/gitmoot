package cli

import (
	"context"
	"fmt"
	"testing"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

// chatSeed captures the thread ids a seeded chat store produced so the assertions
// can look each thread up by id (ordering is wall-clock dependent, so the tests
// assert per-thread fields via a map plus the UpdatedAt-desc invariant, never a
// hard-coded 3-way order).
type chatSeed struct {
	busy     string // multi-author thread with a resolved+unread mention and refs
	archived string // single-message, archived thread
	empty    string // thread with no messages (updated_at fallback)
}

// seedChatStore builds three threads exercising the Chat bridge: a busy thread
// (three authors, an @codex-b resolved+unread mention and an @ghost unresolved one,
// a ref carrying an http url and a ref carrying a non-http url, and a long final
// message for the snippet cap); an archived single-message thread; and an
// empty (no-message) thread.
func seedChatStore(t *testing.T, home string) chatSeed {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	mk := func(slug, repo string) db.ChatThread {
		th, err := store.CreateChatThread(ctx, db.ChatThread{Slug: slug, Name: slug, Repo: repo, CreatedBy: "jerry"})
		if err != nil {
			t.Fatalf("CreateChatThread %s: %v", slug, err)
		}
		return th
	}
	send := func(threadID, kind, authorKind, author, body string, refs []db.ChatRef) db.ChatMessage {
		msg, err := store.AddChatMessage(ctx, db.ChatMessage{
			ThreadID: threadID, Kind: kind, AuthorKind: authorKind, AuthorName: author, Body: body, Refs: refs,
		})
		if err != nil {
			t.Fatalf("AddChatMessage %s/%s: %v", threadID, author, err)
		}
		return msg
	}

	// Empty thread first (oldest activity clock).
	empty := mk("triage-inbox", "jerryfane/noted")

	// Archived thread: one human message, then archive it.
	arch := mk("sqlite-migration", "acme/webapp")
	send(arch.ID, db.ChatKindChat, db.ChatAuthorKindHuman, "jerry", "Archiving the modernc migration thread, thanks.", nil)
	if err := store.SetChatThreadState(ctx, arch.ID, db.ChatThreadStateArchived); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Busy thread: jerry (@codex-b @ghost), researcher (refs), codex-b (long job_result).
	busy := mk("release-room", "jerryfane/gitmoot")
	m1 := send(busy.ID, db.ChatKindChat, db.ChatAuthorKindHuman, "jerry",
		"@codex-b can you inspect the runtime adapter seam? @ghost too", nil)
	if err := store.AddChatMentions(ctx, []db.ChatMention{
		{MessageID: m1.ID, ThreadID: busy.ID, Agent: "codex-b", Resolved: true, Unread: true},
		{MessageID: m1.ID, ThreadID: busy.ID, Agent: "ghost", Resolved: false, Unread: true},
	}); err != nil {
		t.Fatalf("AddChatMentions: %v", err)
	}
	send(busy.ID, db.ChatKindChat, db.ChatAuthorKindAgent, "researcher",
		"Compared the options; a fixed schema wins for V1.",
		[]db.ChatRef{
			{Kind: "pr", Repo: "jerryfane/gitmoot", ID: "742", URL: "https://github.com/jerryfane/gitmoot/pull/742"},
			{Kind: "job", Repo: "jerryfane/gitmoot", ID: "job-adapter-01"},
			{Kind: "artifact", URL: "file:///etc/passwd"}, // non-http url must be dropped
		})
	longBody := "decision: implemented\nsummary: added the adapter manifest (manifest.go + schema test) with a fixed schema of {kind, body, refs[]} and no runtime negotiation whatsoever."
	last := send(busy.ID, db.ChatKindJobResult, db.ChatAuthorKindAgent, "codex-b", longBody, nil)
	_ = last

	return chatSeed{busy: busy.ID, archived: arch.ID, empty: empty.ID}
}

func TestWebDataSourceChatThreads(t *testing.T) {
	home := dashboardTestHome(t)
	seed := seedChatStore(t, home)

	ds := &webDataSource{home: home}
	threads, err := ds.ChatThreads(context.Background())
	if err != nil {
		t.Fatalf("ChatThreads: %v", err)
	}
	if len(threads) != 3 {
		t.Fatalf("threads = %d, want 3: %+v", len(threads), threads)
	}

	byID := map[string]dashboard.ChatThreadSummary{}
	for _, th := range threads {
		byID[th.ID] = th
	}

	// UpdatedAt-desc invariant (most-recently-active first).
	for i := 1; i < len(threads); i++ {
		if threads[i-1].UpdatedAt < threads[i].UpdatedAt {
			t.Fatalf("threads not sorted UpdatedAt desc: [%d]=%d < [%d]=%d",
				i-1, threads[i-1].UpdatedAt, i, threads[i].UpdatedAt)
		}
	}

	// Busy thread: rollup, unread mention (only the resolved one), participants
	// (authors ∪ resolved mentions; @ghost excluded), snippet cap, last message.
	busy := byID[seed.busy]
	if busy.Repo != "jerryfane/gitmoot" || busy.State != "open" || busy.Slug != "release-room" {
		t.Fatalf("busy identity = %+v", busy)
	}
	if busy.MessageCount != 3 {
		t.Fatalf("busy MessageCount = %d, want 3", busy.MessageCount)
	}
	if busy.UnreadMentions != 1 {
		t.Fatalf("busy UnreadMentions = %d, want 1 (only the resolved @codex-b)", busy.UnreadMentions)
	}
	wantParts := []string{"codex-b", "jerry", "researcher"}
	if fmt.Sprint(busy.Participants) != fmt.Sprint(wantParts) {
		t.Fatalf("busy Participants = %v, want %v (sorted, @ghost unresolved excluded)", busy.Participants, wantParts)
	}
	if busy.LastAuthor != "codex-b" || busy.LastKind != db.ChatKindJobResult {
		t.Fatalf("busy last = author %q kind %q, want codex-b/job_result", busy.LastAuthor, busy.LastKind)
	}
	if r := []rune(busy.LastSnippet); r[len(r)-1] != '…' || len(r) != dashChatSnippetCap+1 {
		t.Fatalf("busy LastSnippet not capped: %q (len %d)", busy.LastSnippet, len(r))
	}

	// Archived thread.
	arch := byID[seed.archived]
	if arch.State != "archived" || arch.MessageCount != 1 || arch.UnreadMentions != 0 {
		t.Fatalf("archived = %+v, want archived/1/0", arch)
	}
	if fmt.Sprint(arch.Participants) != fmt.Sprint([]string{"jerry"}) {
		t.Fatalf("archived Participants = %v, want [jerry]", arch.Participants)
	}

	// Empty thread: no messages -> zero rollup, updated_at fallback, no snippet.
	empty := byID[seed.empty]
	if empty.MessageCount != 0 || empty.UnreadMentions != 0 {
		t.Fatalf("empty rollup = count %d unread %d, want 0/0", empty.MessageCount, empty.UnreadMentions)
	}
	if empty.UpdatedAt <= 0 {
		t.Fatalf("empty UpdatedAt = %d, want > 0 (thread row updated_at fallback)", empty.UpdatedAt)
	}
	if empty.LastSnippet != "" || len(empty.Participants) != 0 {
		t.Fatalf("empty should have no snippet/participants: snippet=%q parts=%v", empty.LastSnippet, empty.Participants)
	}

	// Determinism (the UI polls with a signature-skip).
	again, err := ds.ChatThreads(context.Background())
	if err != nil {
		t.Fatalf("ChatThreads again: %v", err)
	}
	if fmt.Sprintf("%+v", threads) != fmt.Sprintf("%+v", again) {
		t.Fatalf("ChatThreads not deterministic across calls")
	}
}

func TestWebDataSourceChatThreadDetail(t *testing.T) {
	home := dashboardTestHome(t)
	seed := seedChatStore(t, home)

	ds := &webDataSource{home: home}
	detail, err := ds.ChatThread(context.Background(), seed.busy)
	if err != nil {
		t.Fatalf("ChatThread: %v", err)
	}
	if detail == nil {
		t.Fatal("ChatThread returned nil detail")
	}

	// Summary agrees with the list projection.
	if detail.MessageCount != 3 || detail.UnreadMentions != 1 {
		t.Fatalf("detail rollup = count %d unread %d, want 3/1", detail.MessageCount, detail.UnreadMentions)
	}
	if fmt.Sprint(detail.Participants) != fmt.Sprint([]string{"codex-b", "jerry", "researcher"}) {
		t.Fatalf("detail Participants = %v", detail.Participants)
	}

	// Full history, ascending by Seq, never nil.
	if len(detail.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(detail.Messages))
	}
	for i := 1; i < len(detail.Messages); i++ {
		if detail.Messages[i-1].Seq >= detail.Messages[i].Seq {
			t.Fatalf("messages not ascending by Seq: %+v", detail.Messages)
		}
	}

	// Refs decoded: the researcher message (seq 2) carries an http ref (url kept),
	// a job ref (no url), and an artifact ref whose non-http url is dropped.
	var refMsg *dashboard.ChatMessage
	for i := range detail.Messages {
		if detail.Messages[i].AuthorName == "researcher" {
			refMsg = &detail.Messages[i]
		}
	}
	if refMsg == nil || len(refMsg.Refs) != 3 {
		t.Fatalf("researcher refs = %+v, want 3 refs", refMsg)
	}
	byKind := map[string]dashboard.ChatRef{}
	for _, r := range refMsg.Refs {
		byKind[r.Kind] = r
	}
	if byKind["pr"].URL != "https://github.com/jerryfane/gitmoot/pull/742" {
		t.Fatalf("pr ref url = %q, want the https link", byKind["pr"].URL)
	}
	if byKind["job"].URL != "" {
		t.Fatalf("job ref url = %q, want empty", byKind["job"].URL)
	}
	if byKind["artifact"].URL != "" {
		t.Fatalf("artifact non-http url must be dropped, got %q", byKind["artifact"].URL)
	}

	// Determinism.
	again, err := ds.ChatThread(context.Background(), seed.busy)
	if err != nil {
		t.Fatalf("ChatThread again: %v", err)
	}
	if fmt.Sprintf("%+v", detail) != fmt.Sprintf("%+v", again) {
		t.Fatalf("ChatThread not deterministic across calls")
	}
}

// TestWebDataSourceChatThreadNotFound pins that an unknown id maps to
// dashboard.ErrChatThreadNotFound (the API layer serves that as a 404), not an
// empty 200.
func TestWebDataSourceChatThreadNotFound(t *testing.T) {
	home := dashboardTestHome(t)
	ds := &webDataSource{home: home}

	detail, err := ds.ChatThread(context.Background(), "chat-does-not-exist")
	if err != dashboard.ErrChatThreadNotFound {
		t.Fatalf("ChatThread(unknown) err = %v, want dashboard.ErrChatThreadNotFound", err)
	}
	if detail != nil {
		t.Fatalf("ChatThread(unknown) detail = %+v, want nil", detail)
	}
}
