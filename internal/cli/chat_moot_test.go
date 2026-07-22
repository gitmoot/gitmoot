package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// mootFixtureHome initializes DefaultConfig + body on a RAW throwaway home and
// opens a store on it, returning (store, rawHome). The raw home is what the CLI
// commands take via --home (PathsForHome resolves it once); passing the already-
// resolved home instead would double-resolve (the #446/#459 bug class) and
// the CLI would open a different DB than the seeded one.
func mootFixtureHome(t *testing.T, body string) (*db.Store, string) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if strings.TrimSpace(body) != "" {
		if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, home
}

// recordingMootDispatcher captures every seat dispatch and returns a synthetic
// queued job so runMoot's back-linking/printing runs without a runtime.
func recordingMootDispatcher() (func(context.Context, *db.Store, localAgentDispatchRequest) (localAgentJobOutput, error), *[]localAgentDispatchRequest) {
	var seen []localAgentDispatchRequest
	d := func(_ context.Context, _ *db.Store, request localAgentDispatchRequest) (localAgentJobOutput, error) {
		seen = append(seen, request)
		return localAgentJobOutput{JobID: "seat-" + request.Agent, State: "queued"}, nil
	}
	return d, &seen
}

// withRecordingMootDispatch installs a recording dispatcher for the test and
// restores the real one on cleanup.
func withRecordingMootDispatch(t *testing.T) *[]localAgentDispatchRequest {
	t.Helper()
	d, seen := recordingMootDispatcher()
	restore := chatMootDispatch
	chatMootDispatch = d
	t.Cleanup(func() { chatMootDispatch = restore })
	return seen
}

// TestMootSeatLimitRejected proves a moot convening more agents than
// [chat].moot_max_seats is rejected BEFORE any seat is dispatched and before the
// thread is created.
func TestMootSeatLimitRejected(t *testing.T) {
	store, home := mootFixtureHome(t, "\n[chat]\nmoot_max_seats = 2\n")
	for _, a := range []string{"a", "b", "c"} {
		seedDaemonWorkerAgent(t, store, a, runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	}
	seen := withRecordingMootDispatch(t)

	var stderr bytes.Buffer
	code := Run([]string{"moot", "room", "brainstorm", "--agents", "a,b,c", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &stderr)
	if code != 2 {
		t.Fatalf("seat-limit exit = %d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "seat limit is 2") {
		t.Fatalf("stderr = %q, want seat-limit message", stderr.String())
	}
	if len(*seen) != 0 {
		t.Fatalf("seat-limit dispatched %d seats", len(*seen))
	}
	if _, err := store.GetChatThreadBySlug(context.Background(), "owner/repo", "room"); err == nil {
		t.Fatal("seat-limit created a thread")
	}
}

// TestMootUnknownAgentRejected proves an unregistered seat fails validation up
// front (no dispatch, no thread).
func TestMootUnknownAgentRejected(t *testing.T) {
	store, home := mootFixtureHome(t, "")
	seedDaemonWorkerAgent(t, store, "known", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	seen := withRecordingMootDispatch(t)

	var stderr bytes.Buffer
	code := Run([]string{"moot", "room", "topic", "--agents", "known,ghost", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Fatalf("unknown-agent exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `agent "ghost" is not registered`) {
		t.Fatalf("stderr = %q, want unknown-agent message", stderr.String())
	}
	if len(*seen) != 0 {
		t.Fatalf("unknown-agent dispatched %d seats", len(*seen))
	}
}

// TestMootRepoScopeRejected proves a seat registered but not scoped to the moot's
// repo is rejected.
func TestMootRepoScopeRejected(t *testing.T) {
	store, home := mootFixtureHome(t, "")
	// Scoped to a DIFFERENT repo.
	seedDaemonWorkerAgent(t, store, "scoped", runtime.ShellRuntime, "", []string{"ask"}, "other/repo")
	seen := withRecordingMootDispatch(t)

	var stderr bytes.Buffer
	code := Run([]string{"moot", "room", "topic", "--agents", "scoped", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Fatalf("repo-scope exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `is not allowed on "owner/repo"`) {
		t.Fatalf("stderr = %q, want repo-scope message", stderr.String())
	}
	if len(*seen) != 0 {
		t.Fatalf("repo-scope dispatched %d seats", len(*seen))
	}
}

// TestMootLacksAskCapability proves a seat without ask capability is rejected.
func TestMootLacksAskCapability(t *testing.T) {
	store, home := mootFixtureHome(t, "")
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "", []string{"review"}, "owner/repo")
	seen := withRecordingMootDispatch(t)

	var stderr bytes.Buffer
	code := Run([]string{"moot", "room", "topic", "--agents", "reviewer", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Fatalf("capability exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "lacks ask capability") {
		t.Fatalf("stderr = %q, want capability message", stderr.String())
	}
	if len(*seen) != 0 {
		t.Fatalf("capability dispatched %d seats", len(*seen))
	}
}

// TestMootConvenesSeats is the happy path: a valid roster creates+marks the moot
// thread, posts the announcement, and dispatches one read-only ask seat per agent
// with the correct request shape (ThreadID set, background ask, home+repo+slug
// embedded in the prompt, starting since-seq = the announcement seq).
func TestMootConvenesSeats(t *testing.T) {
	store, home := mootFixtureHome(t, "\n[chat]\nmoot_message_cap = 12\n")
	for _, a := range []string{"alice", "bob"} {
		seedDaemonWorkerAgent(t, store, a, runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	}
	seen := withRecordingMootDispatch(t)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"moot", "release-room", "how should we ship?", "--agents", "alice,bob", "--repo", "owner/repo", "--json", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("moot exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	var out mootOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode moot JSON: %v (%s)", err, stdout.String())
	}
	if out.MessageCap != 12 {
		t.Fatalf("message cap = %d, want 12 (from config)", out.MessageCap)
	}
	if len(out.Seats) != 2 {
		t.Fatalf("seats = %d, want 2", len(out.Seats))
	}

	ctx := context.Background()
	thread, err := store.GetChatThreadBySlug(ctx, "owner/repo", "release-room")
	if err != nil {
		t.Fatalf("GetChatThreadBySlug: %v", err)
	}
	// Thread was marked a moot with the resolved cap.
	isMoot, messageCap, err := store.ChatThreadMoot(ctx, thread.ID)
	if err != nil {
		t.Fatalf("ChatThreadMoot: %v", err)
	}
	if !isMoot || messageCap != 12 {
		t.Fatalf("moot meta = (%v, %d), want (true, 12)", isMoot, messageCap)
	}
	// The announcement is a system message naming the participants + cap.
	announce := chatE2ELatestKind(t, store, thread.ID, db.ChatKindSystem)
	if announce == nil || !strings.Contains(announce.Body, "MOOT convened") || !strings.Contains(announce.Body, "@alice") || !strings.Contains(announce.Body, "@bob") {
		t.Fatalf("announcement = %+v, want a MOOT-convened system message naming both seats", announce)
	}

	// Two seats dispatched, each a background read-only ask carrying ThreadID and a
	// prompt embedding home+repo+slug.
	if len(*seen) != 2 {
		t.Fatalf("dispatched %d seats, want 2", len(*seen))
	}
	for _, req := range *seen {
		if req.Action != "ask" || !req.Background {
			t.Fatalf("seat %s: action=%q background=%v, want ask/true", req.Agent, req.Action, req.Background)
		}
		if req.ThreadID != thread.ID {
			t.Fatalf("seat %s: ThreadID=%q, want %q", req.Agent, req.ThreadID, thread.ID)
		}
		if req.RepoFlag != "owner/repo" {
			t.Fatalf("seat %s: repo=%q", req.Agent, req.RepoFlag)
		}
		if !strings.Contains(req.Instructions, "--home "+home) {
			t.Fatalf("seat %s: prompt missing absolute home path %q:\n%s", req.Agent, home, req.Instructions)
		}
		if !strings.Contains(req.Instructions, "release-room") || !strings.Contains(req.Instructions, "owner/repo") {
			t.Fatalf("seat %s: prompt missing slug/repo:\n%s", req.Agent, req.Instructions)
		}
	}
}

// TestChatSendMootCapRejection proves `chat send --as` is refused once a moot
// thread reaches its cap, and that the overrun system message is posted exactly
// ONCE (idempotent) across repeated over-cap attempts. Human sends stay allowed.
func TestChatSendMootCapRejection(t *testing.T) {
	store, home := mootFixtureHome(t, "")
	ctx := context.Background()
	seedDaemonWorkerAgent(t, store, "seat", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	thread := seedChatThread(t, store, "capped", "owner/repo")
	if err := store.MarkChatThreadMoot(ctx, thread.ID, 2); err != nil {
		t.Fatalf("MarkChatThreadMoot: %v", err)
	}
	// Fill the cap with two agent turns (author_kind=agent, kind=chat).
	for i := 0; i < 2; i++ {
		seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindAgent, "seat", "turn", "")
	}

	// A third agent send is refused with the distinctive cap error.
	var stderr bytes.Buffer
	code := Run([]string{"chat", "send", "capped", "one more", "--as", "seat", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Fatalf("over-cap send exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "moot cap reached") {
		t.Fatalf("stderr = %q, want moot-cap error", stderr.String())
	}
	// A SECOND over-cap attempt is still refused but does not duplicate the overrun.
	if code := Run([]string{"chat", "send", "capped", "again", "--as", "seat", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Fatalf("second over-cap send exit = %d, want 1", code)
	}

	msgs, err := store.ListChatMessages(ctx, thread.ID, 0)
	if err != nil {
		t.Fatalf("ListChatMessages: %v", err)
	}
	overrun, agentTurns := 0, 0
	for _, m := range msgs {
		if m.Kind == db.ChatKindSystem && strings.Contains(m.Body, "MOOT CAP REACHED") {
			overrun++
		}
		if m.Kind == db.ChatKindChat && m.AuthorKind == db.ChatAuthorKindAgent {
			agentTurns++
		}
	}
	if overrun != 1 {
		t.Fatalf("overrun system messages = %d, want exactly 1 (idempotent)", overrun)
	}
	if agentTurns != 2 {
		t.Fatalf("agent turns = %d, want 2 (no over-cap turn inserted)", agentTurns)
	}

	// A HUMAN send is never blocked by the cap.
	if code := Run([]string{"chat", "send", "capped", "human nudge", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("human send was blocked by the moot cap")
	}
}

// TestChatSendBelowCapAllowed proves an agent send on a moot thread below its cap
// succeeds normally (the gate is a hard stop, not a blanket block).
func TestChatSendBelowCapAllowed(t *testing.T) {
	store, home := mootFixtureHome(t, "")
	ctx := context.Background()
	seedDaemonWorkerAgent(t, store, "seat", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	thread := seedChatThread(t, store, "roomy", "owner/repo")
	if err := store.MarkChatThreadMoot(ctx, thread.ID, 5); err != nil {
		t.Fatalf("MarkChatThreadMoot: %v", err)
	}
	if code := Run([]string{"chat", "send", "roomy", "hello", "--as", "seat", "--repo", "owner/repo", "--home", home}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("below-cap agent send was rejected")
	}
	if n, _ := store.CountChatMootMessages(ctx, thread.ID); n != 1 {
		t.Fatalf("moot message count = %d, want 1", n)
	}
}

// TestChatWaitSinceSeq proves `chat wait` returns only messages with seq greater
// than --since-seq and reports the true tail via the last-seq line.
func TestChatWaitSinceSeq(t *testing.T) {
	store, home := mootFixtureHome(t, "")
	thread := seedChatThread(t, store, "convo", "owner/repo")
	for i := 0; i < 3; i++ {
		seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindHuman, "human", "msg", "")
	}

	var stdout bytes.Buffer
	code := Run([]string{"chat", "wait", "convo", "--since-seq", "1", "--timeout", "1s", "--repo", "owner/repo", "--json", "--home", home}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("wait exit = %d, want 0", code)
	}
	var got struct {
		Messages   []chatMessageOutput `json:"messages"`
		LastSeq    int64               `json:"last_seq"`
		CapReached bool                `json:"cap_reached"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode wait JSON: %v (%s)", err, stdout.String())
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (seq 2 and 3)", len(got.Messages))
	}
	if got.Messages[0].Seq != 2 || got.Messages[1].Seq != 3 {
		t.Fatalf("message seqs = %d,%d, want 2,3", got.Messages[0].Seq, got.Messages[1].Seq)
	}
	if got.LastSeq != 3 {
		t.Fatalf("last_seq = %d, want 3", got.LastSeq)
	}
	if got.CapReached {
		t.Fatal("cap_reached true on a non-moot thread")
	}
}

// TestChatWaitTimeout proves `chat wait` returns cleanly on timeout when no new
// message arrives, still reporting the tail last-seq (a seat can then decide to
// speak or stop).
func TestChatWaitTimeout(t *testing.T) {
	t.Parallel()
	store, home := mootFixtureHome(t, "")
	thread := seedChatThread(t, store, "quiet", "owner/repo")
	seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindHuman, "human", "only", "")

	var stdout bytes.Buffer
	start := time.Now()
	code := runChatWaitWithPollInterval([]string{"quiet", "--since-seq", "1", "--timeout", "60ms", "--repo", "owner/repo", "--home", home}, &stdout, &bytes.Buffer{}, 10*time.Millisecond)
	if code != 0 {
		t.Fatalf("wait exit = %d, want 0", code)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("wait returned in %s, want it to block ~the timeout", elapsed)
	}
	if !strings.Contains(stdout.String(), "(no new messages)") || !strings.Contains(stdout.String(), "last-seq: 1") {
		t.Fatalf("stdout = %q, want no-new-messages + last-seq: 1", stdout.String())
	}
}

// TestChatWaitCapSignal proves `chat wait` on a capped moot prints the exact
// wrap-up line and returns immediately (no timeout spin), even with no new message.
func TestChatWaitCapSignal(t *testing.T) {
	t.Parallel()
	store, home := mootFixtureHome(t, "")
	ctx := context.Background()
	thread := seedChatThread(t, store, "done", "owner/repo")
	if err := store.MarkChatThreadMoot(ctx, thread.ID, 1); err != nil {
		t.Fatalf("MarkChatThreadMoot: %v", err)
	}
	seedChatMention(t, store, thread, db.ChatKindChat, db.ChatAuthorKindAgent, "seat", "final turn", "")

	var stdout bytes.Buffer
	start := time.Now()
	// A large --timeout: if the cap short-circuit works, wait returns immediately.
	code := runChatWaitWithPollInterval([]string{"done", "--since-seq", "99", "--timeout", "30s", "--repo", "owner/repo", "--home", home}, &stdout, &bytes.Buffer{}, 10*time.Millisecond)
	if code != 0 {
		t.Fatalf("wait exit = %d, want 0", code)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("capped wait blocked %s, expected an immediate return", elapsed)
	}
	if !strings.Contains(stdout.String(), chatMootCapWaitLine) {
		t.Fatalf("stdout = %q, want %q", stdout.String(), chatMootCapWaitLine)
	}
}

// TestMootSeatE2E is the no-LLM integration proof: a moot convened with two REAL
// shell-runtime seats dispatches one background read-only ask job per seat through
// the REAL dispatch gate; both jobs run on the REAL daemon worker to a terminal
// decision, and each seat's conclusion back-links into the thread as a
// non-promotable job_result — the "conclusions arrive via the existing back-link
// path, NOT blocked by the cap" contract. It also confirms both seats are
// top-level read-only ask jobs (so they parallelize under the shipped pool
// auto-isolation).
func TestMootSeatE2E(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := chatE2EGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	aliceResult := `{"gitmoot_result":{"decision":"approved","summary":"alice: know X, unsure Y, would ask Z","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
	bobResult := `{"gitmoot_result":{"decision":"approved","summary":"bob: know P, unsure Q, would ask R","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
	seedDaemonWorkerAgent(t, store, "alice", runtime.ShellRuntime, fmt.Sprintf("printf '%%s' '%s'", aliceResult), []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "bob", runtime.ShellRuntime, fmt.Sprintf("printf '%%s' '%s'", bobResult), []string{"ask"}, "owner/repo")

	// Convene the moot through the REAL command (chatMootDispatch is NOT faked).
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"moot", "paper-review", "compare protocol options", "--agents", "alice,bob", "--repo", "owner/repo", "--json", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("moot exit != 0: %s", stderr.String())
	}
	var out mootOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode moot JSON: %v (%s)", err, stdout.String())
	}
	if len(out.Seats) != 2 {
		t.Fatalf("seats = %d, want 2", len(out.Seats))
	}

	// Each seat job is a top-level read-only ask (the concurrency precondition).
	for _, s := range out.Seats {
		if s.JobID == "" || s.Error != "" {
			t.Fatalf("seat %s did not dispatch: %+v", s.Agent, s)
		}
		job, err := store.GetJob(ctx, s.JobID)
		if err != nil {
			t.Fatalf("GetJob(%s): %v", s.JobID, err)
		}
		if job.Type != "ask" {
			t.Fatalf("seat %s job type = %q, want ask", s.Agent, job.Type)
		}
	}

	// Drive both seat jobs to terminal on the REAL worker.
	worker := blockerE2EWorker(store, home, checkout)
	for _, s := range out.Seats {
		job := chatE2EDriveUntilTerminal(t, ctx, worker, store, s.JobID)
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("seat %s job state = %q, want succeeded", s.Agent, job.State)
		}
	}

	// Both conclusions back-linked into the thread as job_result messages (the cap
	// never blocks these), attributed to each seat.
	msgs, err := store.ListChatMessages(ctx, out.ThreadID, 0)
	if err != nil {
		t.Fatalf("ListChatMessages: %v", err)
	}
	got := map[string]bool{}
	for _, m := range msgs {
		if m.Kind == db.ChatKindJobResult && m.AuthorKind == db.ChatAuthorKindAgent {
			got[m.AuthorName] = true
		}
	}
	if !got["alice"] || !got["bob"] {
		t.Fatalf("job_result back-links = %v, want both alice and bob", got)
	}
}

// mootSerialWarnRun convenes a 2-seat moot on home with a recording dispatcher and
// returns (stderr, dispatchedSeatCount) so the serialization-preflight tests can
// assert BOTH the warning text and that the moot still dispatched.
func mootSerialWarnRun(t *testing.T, body string) (string, int) {
	t.Helper()
	store, home := mootFixtureHome(t, body)
	for _, a := range []string{"alice", "bob"} {
		seedDaemonWorkerAgent(t, store, a, runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	}
	seen := withRecordingMootDispatch(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"moot", "room", "topic", "--agents", "alice,bob", "--repo", "owner/repo", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("moot exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	return stderr.String(), len(*seen)
}

// TestMootSerializationWarningDefaultDaemon proves the default daemon config
// (single worker, barrier scheduler — no [daemon] section) makes moot WARN that a
// 2-seat moot serializes, yet STILL dispatches both seats (advisory, non-blocking).
func TestMootSerializationWarningDefaultDaemon(t *testing.T) {
	stderr, dispatched := mootSerialWarnRun(t, "")
	if !strings.Contains(stderr, "runs a single worker") || !strings.Contains(stderr, "workers=1") {
		t.Fatalf("stderr = %q, want a single-worker serialization warning naming workers=1", stderr)
	}
	if dispatched != 2 {
		t.Fatalf("default-daemon moot dispatched %d seats, want 2 (warning must not block)", dispatched)
	}
}

// TestMootSerializationWarningPoolConcurrent proves a pool scheduler with >=2
// workers ([daemon] parallel = 2) SUPPRESSES the warning (seats get distinct
// worktrees and converse concurrently) and still dispatches both seats.
func TestMootSerializationWarningPoolConcurrent(t *testing.T) {
	stderr, dispatched := mootSerialWarnRun(t, "\n[daemon]\nparallel = 2\n")
	if strings.Contains(stderr, "runs seats sequentially") {
		t.Fatalf("stderr = %q, want NO serialization warning under pool+2 workers", stderr)
	}
	if dispatched != 2 {
		t.Fatalf("pool moot dispatched %d seats, want 2", dispatched)
	}
}

// TestMootSerializationWarningPerRepoOverride proves a per-repo [repos."owner/repo"]
// override (#576) is honored: raising max_parallel with a pool scheduler for THIS
// repo suppresses the warning even though the global config would serialize.
func TestMootSerializationWarningPerRepoOverride(t *testing.T) {
	body := "\n[repos.\"owner/repo\"]\nmax_parallel = 2\nscheduler = \"pool\"\n"
	stderr, dispatched := mootSerialWarnRun(t, body)
	if strings.Contains(stderr, "runs seats sequentially") {
		t.Fatalf("stderr = %q, want NO warning under a per-repo pool/max_parallel override", stderr)
	}
	if dispatched != 2 {
		t.Fatalf("per-repo-override moot dispatched %d seats, want 2", dispatched)
	}
}

// TestMootSerializationWarningUnit exercises the predicate directly: a solo seat
// never warns (no conversation needed), barrier/1-worker warns, pool/>=2 suppresses.
func TestMootSerializationWarningUnit(t *testing.T) {
	_, home := mootFixtureHome(t, "")
	paths := config.PathsForHome(home)
	if w := mootSerializationWarning(paths, "owner/repo", 1); w != "" {
		t.Fatalf("solo seat warned: %q", w)
	}
	if w := mootSerializationWarning(paths, "owner/repo", 3); w == "" {
		t.Fatal("default daemon (1/barrier) did not warn a 3-seat moot")
	}
	_, poolHome := mootFixtureHome(t, "\n[daemon]\nparallel = 2\n")
	poolPaths := config.PathsForHome(poolHome)
	if w := mootSerializationWarning(poolPaths, "owner/repo", 3); w != "" {
		t.Fatalf("pool+2 workers warned: %q", w)
	}
}

// writeMootDaemonRunMeta records a running/registered daemon's start argv in the
// home's daemon.json (as a systemd `daemon run` self-registers, #505) so the
// serialization preflight sees a flag-configured daemon rather than only config.toml.
func writeMootDaemonRunMeta(t *testing.T, home string, args []string) {
	t.Helper()
	state := daemonProcessState(config.PathsForHome(home))
	b, err := json.Marshal(daemonMeta{PID: os.Getpid(), Args: args})
	if err != nil {
		t.Fatalf("marshal daemon meta: %v", err)
	}
	if err := os.WriteFile(state.MetaFile, b, 0o600); err != nil {
		t.Fatalf("write daemon.json: %v", err)
	}
}

// TestMootSerializationWarningRunningPoolDaemon reproduces the LIVE deployment shape:
// a daemon configured entirely by flags (systemd `daemon run --workers 6 --scheduler
// pool`, self-registered in daemon.json) with NO [daemon] section in config.toml.
// Reading only config.toml would FALSE-WARN; the preflight must instead read the
// recorded daemon args and stay SILENT (that daemon runs seats concurrently).
func TestMootSerializationWarningRunningPoolDaemon(t *testing.T) {
	_, home := mootFixtureHome(t, "")
	writeMootDaemonRunMeta(t, home, []string{"daemon", "run", "--poll", "30s", "--workers", "6", "--scheduler", "pool", "--watch-issues"})
	if w := mootSerializationWarning(config.PathsForHome(home), "owner/repo", 3); w != "" {
		t.Fatalf("pool/6 daemon (recorded in daemon.json) warned: %q", w)
	}
	// `--workers N` without an explicit --scheduler autoSelects pool, exactly as the
	// daemon resolves it at start — so it must also suppress the warning.
	_, autoHome := mootFixtureHome(t, "")
	writeMootDaemonRunMeta(t, autoHome, []string{"daemon", "run", "--workers", "4"})
	if w := mootSerializationWarning(config.PathsForHome(autoHome), "owner/repo", 3); w != "" {
		t.Fatalf("--workers 4 (autoSelect pool) warned: %q", w)
	}
	// A daemon genuinely started at 1/barrier (recorded) still warns.
	_, serialHome := mootFixtureHome(t, "")
	writeMootDaemonRunMeta(t, serialHome, []string{"daemon", "run", "--workers", "1", "--scheduler", "barrier"})
	if w := mootSerializationWarning(config.PathsForHome(serialHome), "owner/repo", 3); w == "" {
		t.Fatal("recorded 1/barrier daemon did not warn a 3-seat moot")
	}
}

// TestMootSerializationWarningBarrierMultiWorkerSuppressed pins the #739 behavior
// change: the verdict is the WORKER COUNT alone, not the scheduler mode. A barrier
// scheduler with >=2 workers no longer warns — since background read-only seats get
// a dedicated detached worktree at dispatch, the barrier scheduler runs same-repo
// seats with distinct checkout keys concurrently in one per-tick batch. It also
// proves the warm-reload still resolves the effective worker count: a config.toml
// [daemon] scheduler=barrier warm-reloads OVER a pool/6 start (SIGHUP semantics),
// and workers=6 must SUPPRESS. A genuinely single-worker barrier daemon still warns.
func TestMootSerializationWarningBarrierMultiWorkerSuppressed(t *testing.T) {
	_, home := mootFixtureHome(t, "\n[daemon]\nscheduler = \"barrier\"\n")
	writeMootDaemonRunMeta(t, home, []string{"daemon", "run", "--workers", "6", "--scheduler", "pool"})
	if w := mootSerializationWarning(config.PathsForHome(home), "owner/repo", 3); w != "" {
		t.Fatalf("barrier/6-worker daemon warned: %q, want NO warning under #739 (worktrees parallelize seats under any scheduler)", w)
	}
	// A single-worker barrier daemon is the only remaining serializer and still warns.
	_, serialHome := mootFixtureHome(t, "\n[daemon]\nscheduler = \"barrier\"\nparallel = 1\n")
	if w := mootSerializationWarning(config.PathsForHome(serialHome), "owner/repo", 3); w == "" {
		t.Fatal("barrier/1-worker daemon did not warn a 3-seat moot")
	}
}

// TestLoadChatSettingsMootDefaults proves the [chat] moot knobs resolve to their
// documented defaults and reject out-of-range values.
func TestLoadChatSettingsMootDefaults(t *testing.T) {
	store, home := mootFixtureHome(t, "")
	_ = store
	paths := config.PathsForHome(home)
	settings, err := config.LoadChatSettings(paths)
	if err != nil {
		t.Fatalf("LoadChatSettings: %v", err)
	}
	if settings.MootMaxSeats != config.DefaultChatMootMaxSeats || settings.MootMessageCap != config.DefaultChatMootMessageCap {
		t.Fatalf("defaults = (%d, %d), want (%d, %d)", settings.MootMaxSeats, settings.MootMessageCap,
			config.DefaultChatMootMaxSeats, config.DefaultChatMootMessageCap)
	}
}
