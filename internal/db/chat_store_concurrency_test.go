package db

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestAddChatMessageConcurrentSeatsDoNotDropTurns is the moot-shaped regression for
// the #534 V1.5 concurrent-send hazard: moot seats run as SEPARATE processes (each
// its own single-conn Store), so several near-simultaneous `chat send`s race to
// append to one thread. The read->write upgrade under WAL returns SQLITE_BUSY /
// SQLITE_BUSY_SNAPSHOT immediately (busy_timeout cannot wait a deadlock out), which
// a no-backoff retry livelocks on. With the jittered backoff in AddChatMessage every
// send must eventually land — no turn is dropped. Pre-fix this failed ~15% of sends
// with "assign chat message seq after N attempts: database is locked".
func TestAddChatMessageConcurrentSeatsDoNotDropTurns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	owner, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer owner.Close()
	ctx := context.Background()
	thread, err := owner.CreateChatThread(ctx, ChatThread{Slug: "moot", Repo: "o/r", CreatedBy: ChatAuthorKindHuman})
	if err != nil {
		t.Fatalf("CreateChatThread: %v", err)
	}

	// N separate Stores = N separate single-conn connections = N seat processes.
	const seats = 8
	const rounds = 5
	stores := make([]*Store, seats)
	for i := range stores {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("Open seat %d: %v", i, err)
		}
		stores[i] = st
		defer st.Close()
	}

	var mu sync.Mutex
	var failures []error
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		for i := 0; i < seats; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if _, err := stores[i].AddChatMessage(ctx, ChatMessage{
					ThreadID: thread.ID, AuthorKind: ChatAuthorKindAgent,
					AuthorName: fmt.Sprintf("seat%d", i), Kind: ChatKindChat, Body: "turn",
				}); err != nil {
					mu.Lock()
					failures = append(failures, err)
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
	}
	if len(failures) != 0 {
		t.Fatalf("%d/%d concurrent seat sends dropped a turn; first: %v", len(failures), seats*rounds, failures[0])
	}
	// Every send landed as a distinct, gap-free seq.
	msgs, err := owner.ListChatMessages(ctx, thread.ID, 0)
	if err != nil {
		t.Fatalf("ListChatMessages: %v", err)
	}
	if len(msgs) != seats*rounds {
		t.Fatalf("landed %d messages, want %d", len(msgs), seats*rounds)
	}
	seen := make(map[int64]bool, len(msgs))
	for _, m := range msgs {
		if seen[m.Seq] {
			t.Fatalf("duplicate seq %d", m.Seq)
		}
		seen[m.Seq] = true
	}
}

// TestCountInFlightChatThreadJobs proves the real-time in-flight gate query counts
// only an agent's not-yet-terminal jobs whose payload links them to the thread.
func TestCountInFlightChatThreadJobs(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	const tid = "chat-thread-1"
	mk := func(id, agent, state, thread string) {
		payload := "{}"
		if thread != "" {
			payload = fmt.Sprintf(`{"thread_id":%q}`, thread)
		}
		if err := store.CreateJob(ctx, Job{ID: id, Agent: agent, Type: "ask", State: state, Payload: payload}); err != nil {
			t.Fatalf("CreateJob %s: %v", id, err)
		}
	}
	mk("j-queued", "responder", "queued", tid)   // counts
	mk("j-running", "responder", "running", tid)  // counts
	mk("j-done", "responder", "succeeded", tid)   // terminal: excluded
	mk("j-failed", "responder", "failed", tid)    // terminal: excluded
	mk("j-other", "responder", "running", "chat-thread-2") // other thread: excluded
	mk("j-agent", "someoneelse", "running", tid)  // other agent: excluded
	mk("j-nothread", "responder", "running", "")  // no thread link: excluded

	n, err := store.CountInFlightChatThreadJobs(ctx, tid, "responder")
	if err != nil {
		t.Fatalf("CountInFlightChatThreadJobs: %v", err)
	}
	if n != 2 {
		t.Fatalf("in-flight count = %d, want 2 (queued + running for this thread+agent)", n)
	}

	if z, _ := store.CountInFlightChatThreadJobs(ctx, "", "responder"); z != 0 {
		t.Fatalf("empty thread id must count 0, got %d", z)
	}
}

// TestListChatAutoRespondCandidatesExcludesMoot proves a moot thread is structurally
// excluded from the auto-respond candidate set: a seat's @mention of a peer must not
// double-drive that peer with an extra auto-respond ask on top of its seat job.
func TestListChatAutoRespondCandidatesExcludesMoot(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	seed := func(slug string) ChatThread {
		th, err := store.CreateChatThread(ctx, ChatThread{Slug: slug, Repo: "o/r", CreatedBy: ChatAuthorKindHuman})
		if err != nil {
			t.Fatalf("CreateChatThread %s: %v", slug, err)
		}
		msg, err := store.AddChatMessage(ctx, ChatMessage{ThreadID: th.ID, AuthorKind: ChatAuthorKindAgent, AuthorName: "alice", Kind: ChatKindChat, Body: "@bob thoughts?"})
		if err != nil {
			t.Fatalf("AddChatMessage %s: %v", slug, err)
		}
		if err := store.AddChatMentions(ctx, []ChatMention{{MessageID: msg.ID, ThreadID: th.ID, Agent: "bob", Resolved: true, Unread: true}}); err != nil {
			t.Fatalf("AddChatMentions %s: %v", slug, err)
		}
		return th
	}

	plain := seed("plain")
	moot := seed("moot")
	if err := store.MarkChatThreadMoot(ctx, moot.ID, 30); err != nil {
		t.Fatalf("MarkChatThreadMoot: %v", err)
	}

	cands, err := store.ListChatAutoRespondCandidates(ctx)
	if err != nil {
		t.Fatalf("ListChatAutoRespondCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].ThreadID != plain.ID {
		t.Fatalf("candidates = %+v, want exactly the non-moot thread %q", cands, plain.ID)
	}
}
