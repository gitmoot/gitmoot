package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// startTestChatRelay boots a relay server on a short temp socket dir (unix socket
// paths are length-limited, so t.TempDir() is too long) bound to a cancelable
// context, and returns the server + its socket path.
func startTestChatRelay(t *testing.T, store *db.Store) (*chatRelayServer, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "gmrelay")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	server := newChatRelayServer(store, dir, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := server.Start(ctx); err != nil {
		t.Fatalf("relay Start: %v", err)
	}
	return server, server.SocketPath()
}

func seedRelayThread(t *testing.T, store *db.Store, repo, slug string) db.ChatThread {
	t.Helper()
	thread, err := store.CreateChatThread(context.Background(), db.ChatThread{Slug: slug, Name: slug, Repo: repo})
	if err != nil {
		t.Fatalf("CreateChatThread: %v", err)
	}
	return thread
}

// TestBuildSeatAwareAdapterElevation pins the #732 seat-classification fix: the
// daemon elevates a codex agent to ChatSeat (workspace-write+network) — and injects
// the relay env — ONLY for a real `gitmoot moot` seat (payload.MootSeat) that also
// gets a working relay, NOT for any ThreadID-carrying job (chat-task promotions /
// continuations) and NOT when no relay is running. Elevation is coupled to real
// relay injection so a seat never carries the extra privilege without a relay.
func TestBuildSeatAwareAdapterElevation(t *testing.T) {
	store := openCLIJobStore(t, t.TempDir())
	defer store.Close()
	thread := seedRelayThread(t, store, "owner/repo", "room")
	server, _ := startTestChatRelay(t, store)

	// A factory whose adapter is unused here — the point is the agent-elevation +
	// token side effects, which we assert directly.
	factory := func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return runtime.ShellAdapter{}, nil
	}

	cases := []struct {
		name         string
		relay        *chatRelayServer
		payload      workflow.JobPayload
		wantElevated bool
		wantToken    bool
	}{
		{
			name:         "moot seat with relay elevates and injects",
			relay:        server,
			payload:      workflow.JobPayload{MootSeat: true, ThreadID: thread.ID},
			wantElevated: true,
			wantToken:    true,
		},
		{
			name:         "threadid-only job is not a seat",
			relay:        server,
			payload:      workflow.JobPayload{ThreadID: thread.ID},
			wantElevated: false,
			wantToken:    false,
		},
		{
			name:         "moot seat without relay is not elevated",
			relay:        nil,
			payload:      workflow.JobPayload{MootSeat: true, ThreadID: thread.ID},
			wantElevated: false,
			wantToken:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := jobWorker{Store: store, RelayServer: tc.relay, AdapterFactory: factory, Stdout: io.Discard}
			agent := runtime.Agent{Name: "alice", Runtime: runtime.CodexRuntime}
			_, token, err := w.buildSeatAwareAdapter(&agent, "", tc.payload)
			if err != nil {
				t.Fatalf("buildSeatAwareAdapter: %v", err)
			}
			if token != "" {
				t.Cleanup(func() { tc.relay.ReleaseSeat(token) })
			}
			if agent.ChatSeat != tc.wantElevated {
				t.Fatalf("agent.ChatSeat = %v, want %v", agent.ChatSeat, tc.wantElevated)
			}
			if (token != "") != tc.wantToken {
				t.Fatalf("token present = %v (%q), want %v", token != "", token, tc.wantToken)
			}
		})
	}
}

// TestChatRelayRoundTrip proves a relay send lands in the daemon's store (authored
// as the token's bound agent) and a relay wait returns it.
func TestChatRelayRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := openCLIJobStore(t, t.TempDir())
	defer store.Close()
	seedDaemonWorkerAgent(t, store, "alice", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	thread := seedRelayThread(t, store, "owner/repo", "room")

	// Register the seat token the daemon would mint for alice on this thread.
	relaySrv, sock := startTestChatRelay(t, store)
	token, err := relaySrv.RegisterSeat("alice", thread.ID)
	if err != nil {
		t.Fatalf("RegisterSeat: %v", err)
	}

	msg, warnings, err := chatRelaySendClient(sock, token, chatSendParams{
		Ref: "room", Repo: "owner/repo", As: "alice", Body: "hello @ghost",
	})
	if err != nil {
		t.Fatalf("relay send: %v", err)
	}
	if msg.AuthorName != "alice" || msg.AuthorKind != db.ChatAuthorKindAgent {
		t.Fatalf("message authored wrong: %+v", msg)
	}
	if msg.Seq == 0 {
		t.Fatalf("message seq not assigned: %+v", msg)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "ghost") {
		t.Fatalf("expected an unknown-mention warning, got %v", warnings)
	}

	// The write actually landed in the daemon's store.
	msgs, err := store.ListChatMessages(ctx, thread.ID, 0)
	if err != nil {
		t.Fatalf("ListChatMessages: %v", err)
	}
	var found bool
	for _, m := range msgs {
		if m.AuthorName == "alice" && m.Body == "hello @ghost" {
			found = true
		}
	}
	if !found {
		t.Fatalf("relay send did not land in store: %+v", msgs)
	}

	// A wait snapshot since 0 returns the new message + last seq.
	snap, err := chatRelayWaitClient(sock, token, "room", "owner/repo", 0)
	if err != nil {
		t.Fatalf("relay wait: %v", err)
	}
	if len(snap.Messages) == 0 || snap.LastSeq == 0 {
		t.Fatalf("relay wait returned nothing: %+v", snap)
	}
	// A wait since the last seq returns no new messages.
	snap2, err := chatRelayWaitClient(sock, token, "room", "owner/repo", snap.LastSeq)
	if err != nil {
		t.Fatalf("relay wait 2: %v", err)
	}
	if len(snap2.Messages) != 0 {
		t.Fatalf("relay wait since last seq returned messages: %+v", snap2)
	}
}

// TestChatRelayTokenRejected proves a bad, empty, or released token is refused.
func TestChatRelayTokenRejected(t *testing.T) {
	store := openCLIJobStore(t, t.TempDir())
	defer store.Close()
	seedDaemonWorkerAgent(t, store, "alice", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	thread := seedRelayThread(t, store, "owner/repo", "room")
	server, sock := startTestChatRelay(t, store)

	for _, tok := range []string{"", "bogus-token"} {
		if _, _, err := chatRelaySendClient(sock, tok, chatSendParams{Ref: "room", Repo: "owner/repo", As: "alice", Body: "x"}); err == nil {
			t.Fatalf("token %q accepted", tok)
		} else if !strings.Contains(err.Error(), "token") {
			t.Fatalf("token %q error = %v, want a token rejection", tok, err)
		}
	}

	// A released token can no longer be replayed.
	token, err := server.RegisterSeat("alice", thread.ID)
	if err != nil {
		t.Fatalf("RegisterSeat: %v", err)
	}
	server.ReleaseSeat(token)
	if _, _, err := chatRelaySendClient(sock, token, chatSendParams{Ref: "room", Repo: "owner/repo", As: "alice", Body: "x"}); err == nil {
		t.Fatal("released token accepted")
	}
}

// TestChatRelayGateRejection proves the relay reuses the same access gate: an
// agent without repo access is refused, and a seat cannot impersonate a sibling.
func TestChatRelayGateRejection(t *testing.T) {
	store := openCLIJobStore(t, t.TempDir())
	defer store.Close()
	// carol is scoped to a DIFFERENT repo than the thread.
	seedDaemonWorkerAgent(t, store, "carol", runtime.ShellRuntime, "", []string{"ask"}, "other/repo")
	seedDaemonWorkerAgent(t, store, "alice", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "bob", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	thread := seedRelayThread(t, store, "owner/repo", "room")
	server, sock := startTestChatRelay(t, store)

	// Repo-scope rejection: carol's token is bound to an owner/repo thread she
	// cannot access.
	carolTok, _ := server.RegisterSeat("carol", thread.ID)
	if _, _, err := chatRelaySendClient(sock, carolTok, chatSendParams{Ref: "room", Repo: "owner/repo", As: "carol", Body: "x"}); err == nil {
		t.Fatal("carol allowed on a repo she is not scoped to")
	} else if !strings.Contains(err.Error(), "not allowed on") {
		t.Fatalf("carol error = %v, want a repo-scope rejection", err)
	}

	// Impersonation rejection: alice's token cannot author as bob.
	aliceTok, _ := server.RegisterSeat("alice", thread.ID)
	if _, _, err := chatRelaySendClient(sock, aliceTok, chatSendParams{Ref: "room", Repo: "owner/repo", As: "bob", Body: "x"}); err == nil {
		t.Fatal("alice allowed to send as bob")
	} else if !strings.Contains(err.Error(), "cannot send as") {
		t.Fatalf("impersonation error = %v, want an impersonation rejection", err)
	}

	// Wrong-thread rejection: a token bound to this thread cannot touch another.
	other := seedRelayThread(t, store, "owner/repo", "other-room")
	if _, _, err := chatRelaySendClient(sock, aliceTok, chatSendParams{Ref: "other-room", Repo: "owner/repo", As: "alice", Body: "x"}); err == nil {
		t.Fatal("token accepted for the wrong thread")
	} else if !strings.Contains(err.Error(), "thread") {
		t.Fatalf("wrong-thread error = %v", err)
	}
	_ = other
}

// TestChatRelaySocketPermissions proves the socket is 0600, its dir 0700, and it
// is removed on shutdown.
func TestChatRelaySocketPermissions(t *testing.T) {
	store := openCLIJobStore(t, t.TempDir())
	defer store.Close()
	dir, err := os.MkdirTemp("", "gmrelayperm")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(dir)
	server := newChatRelayServer(store, dir, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	sock := server.SocketPath()
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket perm = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir perm = %o, want 700", perm)
	}

	cancel()
	// The socket is unlinked on ctx.Done (async); poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(sock); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket not removed after shutdown")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestChatSendDirectPathUnchanged proves that WITHOUT GITMOOT_CHAT_RELAY the
// `chat send` command writes directly to the store, byte-identically (no socket).
func TestChatSendDirectPathUnchanged(t *testing.T) {
	// Ensure no stray relay env leaks from the harness.
	t.Setenv(chatRelayEnvSocket, "")
	t.Setenv(chatRelayEnvToken, "")
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedDaemonWorkerAgent(t, store, "alice", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	seedRelayThread(t, store, "owner/repo", "room")
	store.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"chat", "send", "room", "hi there", "--as", "alice", "--repo", "owner/repo", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("chat send exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "sent #") {
		t.Fatalf("stdout = %q, want a sent confirmation", stdout.String())
	}
	// The message is in the store.
	store2 := openCLIJobStore(t, home)
	defer store2.Close()
	threads, _ := store2.ListChatThreads(context.Background(), "owner/repo", "")
	if len(threads) == 0 {
		t.Fatal("no thread")
	}
	msgs, _ := store2.ListChatMessages(context.Background(), threads[0].ID, 0)
	var found bool
	for _, m := range msgs {
		if m.AuthorName == "alice" && m.Body == "hi there" {
			found = true
		}
	}
	if !found {
		t.Fatalf("direct send not in store: %+v", msgs)
	}
}

// TestChatSendRelayEnvBranch proves that WITH GITMOOT_CHAT_RELAY set, `chat send`
// routes over the socket to the daemon (never opening the store itself), and the
// daemon performs the write — the sandboxed-seat path.
func TestChatSendRelayEnvBranch(t *testing.T) {
	store := openCLIJobStore(t, t.TempDir())
	defer store.Close()
	seedDaemonWorkerAgent(t, store, "alice", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	thread := seedRelayThread(t, store, "owner/repo", "room")
	server, sock := startTestChatRelay(t, store)
	token, _ := server.RegisterSeat("alice", thread.ID)

	t.Setenv(chatRelayEnvSocket, sock)
	t.Setenv(chatRelayEnvToken, token)

	// Point --home at a NON-EXISTENT dir the seat could not write; the relay path
	// must not touch it (it dials the socket instead), proving the store open is
	// bypassed entirely.
	var stdout, stderr bytes.Buffer
	code := Run([]string{"chat", "send", "room", "relayed body", "--as", "alice", "--repo", "owner/repo", "--home", "/nonexistent/relay-home"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("relayed chat send exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "sent #") {
		t.Fatalf("stdout = %q, want a sent confirmation", stdout.String())
	}
	msgs, _ := store.ListChatMessages(context.Background(), thread.ID, 0)
	var found bool
	for _, m := range msgs {
		if m.AuthorName == "alice" && m.Body == "relayed body" {
			found = true
		}
	}
	if !found {
		t.Fatalf("relayed send did not land via daemon store: %+v", msgs)
	}
}

// TestChatSendRelayDialFailureNoFallback proves that when the relay env is set but
// the socket is unreachable, `chat send` errors instead of silently falling back
// to a direct (sandbox-broken) write.
func TestChatSendRelayDialFailureNoFallback(t *testing.T) {
	t.Setenv(chatRelayEnvSocket, "/nonexistent/relay.sock")
	t.Setenv(chatRelayEnvToken, "tok")
	home := t.TempDir()
	store := openCLIJobStore(t, home)
	seedDaemonWorkerAgent(t, store, "alice", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	seedRelayThread(t, store, "owner/repo", "room")
	store.Close()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"chat", "send", "room", "x", "--as", "alice", "--repo", "owner/repo", "--home", home}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected failure on unreachable relay, got 0; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "chat send") {
		t.Fatalf("stderr = %q, want a chat send error", stderr.String())
	}
}
