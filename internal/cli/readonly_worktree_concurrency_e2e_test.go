package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// This file is the no-LLM end-to-end proof for the #739 fix. Background
// read-only jobs each receive a dedicated detached committed-tip worktree, so
// their worktree:<path> keys run concurrently instead of serializing on the
// shared repo key. The hard-sandbox ask proof uses loopback only for its
// rendezvous; moot seats retain their relay-backed chat path.
//
// The #739 condition is reproduced faithfully: the repo checkout is parked on a
// NON-MAIN / stale odd branch (the live symptom was on a repo sitting on
// feat/delegations-task-3051-schema). The dispatch-time allocator resolves the
// worktree ref to HEAD (a committed tip that is always resolvable), so the stale
// branch is a non-issue — matching the researchers' refuted-ref diagnostic.
//
// CONCURRENCY PROOF: each seat's shell script blocks on a 2-of-2 filesystem
// rendezvous — it emits its `approved` result ONLY after BOTH seats' start markers
// exist. Two seats can therefore both reach `approved` (JobSucceeded) ONLY if they
// were live SIMULTANEOUSLY. If the fix regresses (both seats share repo:<repo> and
// serialize), the first-dispatched seat waits out the rendezvous, emits a `failed`
// decision (→ JobFailed), and the both-succeeded assertions flip RED. A concurrent
// state sampler independently records both jobs observed in `running` at once.

// staleBranchGitCheckout builds a real git repo for owner/repo with one commit and
// then parks the working tree on a NON-MAIN stale/odd branch, reproducing the exact
// #739 condition (the live repo sat on a long-lived feature branch, not main).
func staleBranchGitCheckout(t *testing.T, fullName string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "branch", "-m", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "gitmoot test")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/"+fullName+".git")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")
	// Reproduce #739: leave the checkout on a stale/odd non-main branch.
	runGit(t, dir, "checkout", "-b", "feat/delegations-stale-odd-branch")
	return dir
}

// rendezvousResult builds a valid gitmoot_result envelope with the given terminal
// decision (approved → JobSucceeded, failed → JobFailed).
func rendezvousResult(decision, summary string) string {
	return fmt.Sprintf(`{"gitmoot_result":{"decision":%q,"summary":%q,"findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`, decision, summary)
}

// rendezvousSeatScript is a shell-runtime agent command that touches its own start
// marker in stateDir, then waits until `peers` start markers exist before emitting
// okResult. If the wait times out (the seats serialized rather than running
// concurrently) it emits a distinctive `failed` result so the both-succeeded
// assertion flips RED. prefix (may be empty) runs first — used by the moot test to
// post a real `chat send` conversation turn before the rendezvous.
func rendezvousSeatScript(stateDir, self string, peers int, okResult, prefix string) string {
	failResult := rendezvousResult("failed", "rendezvous timeout: seat "+self+" serialized (#739 regression)")
	// After the barrier clears, hold briefly so BOTH seats are demonstrably in
	// `running` at the same time for a window the concurrent state sampler can
	// reliably observe (the barrier alone proves concurrency, but clears in ms).
	body := fmt.Sprintf(`: > %q/started-%s
i=0
while [ "$(ls %q/started-* 2>/dev/null | wc -l)" -lt %d ]; do
  i=$((i+1))
  if [ "$i" -gt 200 ]; then printf '%%s' '%s'; exit 0; fi
  sleep 0.05
done
sleep 0.3
printf '%%s' '%s'`, stateDir, self, stateDir, peers, failResult, okResult)
	if strings.TrimSpace(prefix) != "" {
		return prefix + "\n" + body
	}
	return body
}
func loopbackRendezvous(t *testing.T, peers int) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		if tcp, ok := listener.(*net.TCPListener); ok {
			_ = tcp.SetDeadline(time.Now().Add(5 * time.Second))
		}
		connections := make([]net.Conn, 0, peers)
		defer func() {
			for _, connection := range connections {
				_ = connection.Close()
			}
		}()
		for len(connections) < peers {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections = append(connections, connection)
		}
		for _, connection := range connections {
			_, _ = connection.Write([]byte{1})
		}
	}()
	return listener.Addr().String()
}

func loopbackRendezvousSeatScript(address, self, okResult string) string {
	failResult := rendezvousResult("failed", "rendezvous timeout: seat "+self+" serialized (#739 regression)")
	probe := `import socket,sys; host,port=sys.argv[1].rsplit(":",1); s=socket.create_connection((host,int(port)),2); s.settimeout(2); s.sendall(sys.argv[2].encode()); ok=s.recv(1); s.close(); sys.exit(0 if ok else 1)`
	return fmt.Sprintf(
		"if python3 -c %s %s %s; then sleep 0.3; printf '%%s' %s; else printf '%%s' %s; fi",
		shellQuote(probe, "posix"),
		shellQuote(address, "posix"),
		shellQuote(self, "posix"),
		shellQuote(okResult, "posix"),
		shellQuote(failResult, "posix"),
	)
}

// readonlyPoolWorker builds a REAL pool-scheduler jobWorker on the isolated home:
// the default ShellAdapter subprocess runtime and the default checkout resolver
// (which honors a job's payload worktree path), so a dispatch-time #739 worktree is
// the actual cwd the seat runs in.
func readonlyPoolWorker(store *db.Store, home string) jobWorker {
	worker := defaultJobWorker(store, io.Discard, home)
	worker.UsePool = true
	return worker
}

// drivePoolConcurrently runs ONE pool tick (workers=2) that dispatches and drains
// the queued seats concurrently, guarded by a timeout, while sampling job states.
// It returns whether both jobIDs were ever observed in `running` simultaneously.
func drivePoolConcurrently(t *testing.T, ctx context.Context, worker jobWorker, store *db.Store, jobIDs []string) bool {
	t.Helper()
	bothRunning := false
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			running := 0
			for _, id := range jobIDs {
				if job, err := store.GetJob(ctx, id); err == nil && job.State == string(workflow.JobRunning) {
					running++
				}
			}
			if running == len(jobIDs) {
				bothRunning = true
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- runQueuedJobsForRepo(ctx, worker, 2, "", "") }()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("pool tick returned error: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("pool tick did not complete within 60s — seats likely serialized (rendezvous never cleared)")
	}
	close(stop)
	<-done
	return bothRunning
}

// terminalJobDecision returns a terminal job and its parsed gitmoot_result decision.
func terminalJobDecision(t *testing.T, ctx context.Context, store *db.Store, jobID string) (db.Job, string) {
	t.Helper()
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("payload(%s): %v", jobID, err)
	}
	decision := ""
	if payload.Result != nil {
		decision = payload.Result.Decision
	}
	return job, decision
}

// TestReadOnlyWorktreeConcurrentAsksE2E is the primary #739 proof: two BACKGROUND
// read-only asks dispatched onto the SAME stale-branch repo are each born with a
// distinct detached committed-tip worktree, run CONCURRENTLY on the pool worker
// (proven by the 2-of-2 rendezvous + a live state sampler), each carry a
// readonly_worktree_allocated event, and each worktree is DISPOSED on terminal.
//
// MUTATION PROOF: revert the dispatch-time allocation (so both asks key
// repo:owner/repo) and the first seat serializes behind the second — it waits out
// the rendezvous, emits `failed`, and the both-succeeded assertion flips RED.
func TestReadOnlyWorktreeConcurrentAsksE2E(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := staleBranchGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)

	rendezvous := loopbackRendezvous(t, 2)
	seedDaemonWorkerAgent(t, store, "alice", runtime.ShellRuntime,
		loopbackRendezvousSeatScript(rendezvous, "alice", rendezvousResult("approved", "alice ran beside bob")), []string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "bob", runtime.ShellRuntime,
		loopbackRendezvousSeatScript(rendezvous, "bob", rendezvousResult("approved", "bob ran beside alice")), []string{"ask"}, "owner/repo")

	// Dispatch two BACKGROUND read-only asks on the SAME stale-branch repo through
	// the REAL dispatch entry (the moot-seat / chat-task / autorespond shape).
	outA, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "alice", Action: "ask", Instructions: "audit module A", Background: true, Home: home})
	if err != nil {
		t.Fatalf("dispatch alice: %v", err)
	}
	outB, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "bob", Action: "ask", Instructions: "audit module B", Background: true, Home: home})
	if err != nil {
		t.Fatalf("dispatch bob: %v", err)
	}

	jobA, err := store.GetJob(ctx, outA.JobID)
	if err != nil {
		t.Fatalf("GetJob alice: %v", err)
	}
	jobB, err := store.GetJob(ctx, outB.JobID)
	if err != nil {
		t.Fatalf("GetJob bob: %v", err)
	}
	payloadA, err := daemonJobPayload(jobA)
	if err != nil {
		t.Fatalf("payload alice: %v", err)
	}
	payloadB, err := daemonJobPayload(jobB)
	if err != nil {
		t.Fatalf("payload bob: %v", err)
	}

	// Each is born with its OWN detached worktree carrying the disposal marker.
	for name, p := range map[string]workflow.JobPayload{"alice": payloadA, "bob": payloadB} {
		if strings.TrimSpace(p.WorktreePath) == "" {
			t.Fatalf("seat %s has no dispatch-time worktree (#739)", name)
		}
		if !p.ReadOnlyWorktree {
			t.Fatalf("seat %s ReadOnlyWorktree = false, want true (top-level disposal marker)", name)
		}
	}
	// DISTINCT worktree:<path> checkout keys — the whole point of #739.
	keyA := queuedJobCheckoutKey(ctx, store, jobA)
	keyB := queuedJobCheckoutKey(ctx, store, jobB)
	if !strings.HasPrefix(keyA, "worktree:") || !strings.HasPrefix(keyB, "worktree:") {
		t.Fatalf("checkout keys must both be worktree:<path>, got %q and %q", keyA, keyB)
	}
	if keyA == keyB {
		t.Fatalf("both seats share checkout key %q — they would serialize (want distinct)", keyA)
	}
	// Each carries an observable allocation event.
	for _, id := range []string{outA.JobID, outB.JobID} {
		if n := countCLIJobEvents(t, store, id, "readonly_worktree_allocated"); n != 1 {
			t.Fatalf("job %s readonly_worktree_allocated events = %d, want 1", id, n)
		}
	}

	// Drive the pool: both must be live simultaneously to clear the 2-of-2 rendezvous.
	bothRunning := drivePoolConcurrently(t, ctx, readonlyPoolWorker(store, home), store, []string{outA.JobID, outB.JobID})
	if !bothRunning {
		t.Fatal("state sampler never observed both seats in `running` at once (serialized)")
	}

	// Both reached `approved` (JobSucceeded) ⇒ both cleared the 2-of-2 rendezvous ⇒
	// they ran concurrently. A serialized first seat would be `failed` here.
	for _, id := range []string{outA.JobID, outB.JobID} {
		job, decision := terminalJobDecision(t, ctx, store, id)
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("seat %s state = %q, want succeeded (a serialized seat times out the rendezvous → failed)", id, job.State)
		}
		if decision != "approved" {
			t.Fatalf("seat %s decision = %q, want approved (rendezvous cleared)", id, decision)
		}
	}

	// Each detached worktree is DISPOSED on terminal (dir gone + a removal event).
	for name, id := range map[string]string{"alice": outA.JobID, "bob": outB.JobID} {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		p, err := daemonJobPayload(job)
		if err != nil {
			t.Fatalf("payload %s: %v", id, err)
		}
		if _, statErr := os.Stat(p.WorktreePath); !os.IsNotExist(statErr) {
			t.Fatalf("seat %s worktree %s not disposed on terminal (stat err=%v)", name, p.WorktreePath, statErr)
		}
		if n := countCLIJobEvents(t, store, id, "delegation_worktree_removed"); n != 1 {
			t.Fatalf("seat %s delegation_worktree_removed events = %d, want 1", name, n)
		}
	}
}

// TestMootConcurrentSeatsOnStaleBranchE2E convenes a real 2-seat moot on the
// stale-branch repo and proves both seats run CONCURRENTLY (2-of-2 rendezvous +
// state sampler) AND converse: each seat posts a real `chat send --as <self>`
// conversation turn, and both turns plus both back-linked job_result conclusions
// land in the thread.
func TestMootConcurrentSeatsOnStaleBranchE2E(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := staleBranchGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	bin := sharedGitmootTestBinary(t)

	stateDir := t.TempDir()
	// Each seat posts one real chat turn BEFORE the rendezvous, then clears the
	// 2-of-2 barrier and returns its conclusions.
	sendTurn := func(self, msg string) string {
		return fmt.Sprintf("%q chat send stale-review %q --as %s --repo owner/repo --home %q >/dev/null 2>&1",
			bin, msg, self, home)
	}
	seedDaemonWorkerAgent(t, store, "alice", runtime.ShellRuntime,
		rendezvousSeatScript(stateDir, "alice", 2, rendezvousResult("approved", "alice: I lean toward A"), sendTurn("alice", "alice: I lean toward option A")),
		[]string{"ask"}, "owner/repo")
	seedDaemonWorkerAgent(t, store, "bob", runtime.ShellRuntime,
		rendezvousSeatScript(stateDir, "bob", 2, rendezvousResult("approved", "bob: A has a cost"), sendTurn("bob", "bob: option A has a runtime cost")),
		[]string{"ask"}, "owner/repo")

	// Convene the moot through the REAL command (chatMootDispatch is NOT faked).
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"moot", "stale-review", "which option ships?", "--agents", "alice,bob", "--repo", "owner/repo", "--json", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("moot exit != 0: %s", stderr.String())
	}
	var out mootOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode moot JSON: %v (%s)", err, stdout.String())
	}
	if len(out.Seats) != 2 {
		t.Fatalf("seats = %d, want 2", len(out.Seats))
	}
	seatJobs := make([]string, 0, 2)
	for _, s := range out.Seats {
		if s.JobID == "" || s.Error != "" {
			t.Fatalf("seat %s did not dispatch: %+v", s.Agent, s)
		}
		seatJobs = append(seatJobs, s.JobID)
	}

	// Each seat is born keyed off its own detached worktree (distinct keys).
	keys := map[string]bool{}
	for _, id := range seatJobs {
		job, err := store.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob %s: %v", id, err)
		}
		key := queuedJobCheckoutKey(ctx, store, job)
		if !strings.HasPrefix(key, "worktree:") {
			t.Fatalf("moot seat %s checkout key = %q, want worktree:<path> (#739)", id, key)
		}
		keys[key] = true
	}
	if len(keys) != 2 {
		t.Fatalf("moot seats collapsed onto %d distinct worktree keys, want 2", len(keys))
	}

	// Drive both seats concurrently on the pool worker.
	if !drivePoolConcurrently(t, ctx, readonlyPoolWorker(store, home), store, seatJobs) {
		t.Fatal("state sampler never observed both moot seats in `running` at once (serialized)")
	}
	for _, id := range seatJobs {
		job, decision := terminalJobDecision(t, ctx, store, id)
		if job.State != string(workflow.JobSucceeded) {
			t.Fatalf("moot seat %s state = %q, want succeeded (serialized seat times out the rendezvous)", id, job.State)
		}
		if decision != "approved" {
			t.Fatalf("moot seat %s decision = %q, want approved (ran concurrently)", id, decision)
		}
	}

	// Both seats' CONVERSATION turns and back-linked conclusions landed in the thread.
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
		t.Fatalf("agent chat turns = %v, want both alice and bob (the seats' concurrent chat sends)", chatTurns)
	}
	if !conclusions["alice"] || !conclusions["bob"] {
		t.Fatalf("job_result conclusions = %v, want both alice and bob (back-linked)", conclusions)
	}
}

// TestLoneUncontendedBackgroundAskE2E proves the fix does not regress the simple
// case: a single, uncontended background ask on the stale-branch repo still gets a
// dispatch-time worktree, runs to a terminal decision, and disposes its worktree.
func TestLoneUncontendedBackgroundAskE2E(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := staleBranchGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "solo", runtime.ShellRuntime,
		fmt.Sprintf("printf '%%s' '%s'", rendezvousResult("approved", "solo ran")), []string{"ask"}, "owner/repo")

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "solo", Action: "ask", Instructions: "lone audit", Background: true, Home: home})
	if err != nil {
		t.Fatalf("dispatch solo: %v", err)
	}
	job, err := store.GetJob(ctx, out.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if strings.TrimSpace(payload.WorktreePath) == "" || !payload.ReadOnlyWorktree {
		t.Fatalf("lone ask missing dispatch-time worktree: worktree=%q marker=%v", payload.WorktreePath, payload.ReadOnlyWorktree)
	}

	worker := readonlyPoolWorker(store, home)
	terminal := chatE2EDriveUntilTerminal(t, ctx, worker, store, out.JobID)
	if terminal.State != string(workflow.JobSucceeded) {
		t.Fatalf("lone ask state = %q, want succeeded", terminal.State)
	}
	if _, statErr := os.Stat(payload.WorktreePath); !os.IsNotExist(statErr) {
		t.Fatalf("lone ask worktree %s not disposed (stat err=%v)", payload.WorktreePath, statErr)
	}
	if n := countCLIJobEvents(t, store, out.JobID, "delegation_worktree_removed"); n != 1 {
		t.Fatalf("lone ask delegation_worktree_removed events = %d, want 1", n)
	}
	terminalPayload, err := daemonJobPayload(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if terminalPayload.ReadOnlyWorktreeDiff != "" || terminalPayload.ReadOnlyWorktreeDiffTruncated || terminalPayload.ReadOnlyWorktreeDiffError != "" {
		t.Fatalf("unchanged lone ask has bogus diff metadata: %+v", terminalPayload)
	}
}

// TestBackgroundAskCapturesDiffBeforeWorktreeCleanupE2E reproduces #1152 through
// the real background dispatch and worker paths: an ask edits its isolated
// worktree, terminal cleanup removes that worktree synchronously, and the
// bounded diff remains readable from both job detail and list output afterward.
func TestBackgroundAskCapturesDiffBeforeWorktreeCleanupE2E(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := staleBranchGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	result := rendezvousResult("implemented", "edited the isolated checkout")
	script := fmt.Sprintf("printf 'changed by ask\\n' > README.md\nprintf '%%s' '%s'", result)
	seedDaemonWorkerAgentWithPolicy(t, store, "writer", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "writer", Action: "ask", Instructions: "make the requested edit", Background: true, Home: home,
	})
	if err != nil {
		t.Fatalf("dispatch writer: %v", err)
	}
	queued, err := store.GetJob(ctx, out.JobID)
	if err != nil {
		t.Fatal(err)
	}
	queuedPayload, err := daemonJobPayload(queued)
	if err != nil {
		t.Fatal(err)
	}
	worktree := queuedPayload.WorktreePath
	terminal := chatE2EDriveUntilTerminal(t, ctx, readonlyPoolWorker(store, home), store, out.JobID)
	if terminal.State != string(workflow.JobSucceeded) {
		t.Fatalf("writer state = %q, want succeeded", terminal.State)
	}
	if _, statErr := os.Stat(worktree); !os.IsNotExist(statErr) {
		t.Fatalf("isolated worktree still exists after terminal cleanup: %v", statErr)
	}
	persisted, err := daemonJobPayload(terminal)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## git status --short", " M README.md", "## git diff HEAD", "+changed by ask"} {
		if !strings.Contains(persisted.ReadOnlyWorktreeDiff, want) {
			t.Fatalf("captured diff missing %q:\n%s", want, persisted.ReadOnlyWorktreeDiff)
		}
	}
	if persisted.ReadOnlyWorktreeDiffTruncated || persisted.ReadOnlyWorktreeDiffError != "" {
		t.Fatalf("unexpected diff metadata: truncated=%v error=%q", persisted.ReadOnlyWorktreeDiffTruncated, persisted.ReadOnlyWorktreeDiffError)
	}
	if got := gitOutput(t, checkout, "status", "--porcelain"); got != "" {
		t.Fatalf("registered checkout was mutated: %q", got)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"job", "show", out.JobID, "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job show --json exit=%d stderr=%s", code, stderr.String())
	}
	var shown jobShowOutput
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.ReadOnlyWorktreeDiff != persisted.ReadOnlyWorktreeDiff || shown.Payload.ReadOnlyWorktreeDiff != persisted.ReadOnlyWorktreeDiff {
		t.Fatal("job show --json did not surface the persisted diff")
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list --json exit=%d stderr=%s", code, stderr.String())
	}
	var listed []jobListEntry
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ReadOnlyWorktreeDiff != persisted.ReadOnlyWorktreeDiff {
		t.Fatalf("job list --json diff = %+v, want captured diff", listed)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "list", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job list exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "DIFF: ") || !strings.Contains(stdout.String(), " bytes captured") {
		t.Fatalf("job list text did not surface diff summary:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"job", "show", out.JobID, "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("job show exit=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "read_only_worktree_diff: captured") {
		t.Fatalf("job show text did not surface captured diff summary:\n%s", stdout.String())
	}
}

// TestBackgroundAskDiffCaptureFailureDoesNotBlockCleanupE2E corrupts only the
// throwaway worktree's index, forcing capture to fail without invalidating the
// worktree registration used for removal. The engine must record the hook event
// and still remove the actual worktree.
func TestBackgroundAskDiffCaptureFailureDoesNotBlockCleanupE2E(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := staleBranchGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	result := rendezvousResult("approved", "capture failure fixture")
	script := fmt.Sprintf("index=$(git rev-parse --git-path index)\nprintf x > \"$index\"\nprintf '%%s' '%s'", result)
	seedDaemonWorkerAgent(t, store, "breaker", runtime.ShellRuntime, script, []string{"ask"}, "owner/repo")

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "breaker", Action: "ask", Instructions: "exercise cleanup failure handling", Background: true, Home: home,
	})
	if err != nil {
		t.Fatalf("dispatch breaker: %v", err)
	}
	queued, err := store.GetJob(ctx, out.JobID)
	if err != nil {
		t.Fatal(err)
	}
	queuedPayload, err := daemonJobPayload(queued)
	if err != nil {
		t.Fatal(err)
	}
	terminal := chatE2EDriveUntilTerminal(t, ctx, readonlyPoolWorker(store, home), store, out.JobID)
	if terminal.State != string(workflow.JobSucceeded) {
		t.Fatalf("breaker state = %q, want succeeded", terminal.State)
	}
	if _, statErr := os.Stat(queuedPayload.WorktreePath); !os.IsNotExist(statErr) {
		t.Fatalf("worktree cleanup was blocked by diff capture failure: %v", statErr)
	}
	persisted, err := daemonJobPayload(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ReadOnlyWorktreeDiffError == "" {
		t.Fatal("diff capture failure marker was not persisted")
	}
	if n := countCLIJobEvents(t, store, out.JobID, "readonly_worktree_precleanup_failed"); n != 1 {
		t.Fatalf("readonly_worktree_precleanup_failed events = %d, want 1", n)
	}
	if n := countCLIJobEvents(t, store, out.JobID, "delegation_worktree_removed"); n != 1 {
		t.Fatalf("delegation_worktree_removed events = %d, want 1", n)
	}
}

// TestReadOnlyWorktreeAllocFailIsFailClosedE2E proves allocation is part of the
// taskless background ask's security boundary. A real filesystem failure must
// refuse dispatch before enqueue rather than run the prompt on the shared checkout.
// The allocation is forced to fail by planting a file where the per-repo worktree
// directory must be created.
func TestReadOnlyWorktreeAllocFailIsFailClosedE2E(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := staleBranchGitCheckout(t, "owner/repo")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "solo", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")

	// Plant a file at <home>/.gitmoot/worktrees/owner--repo so os.MkdirAll of the
	// deterministic worktree parent (…/owner--repo/delegations/<jobID>) fails ENOTDIR.
	wtDir := filepath.Join(home, ".gitmoot", "worktrees")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("mkdir worktrees: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "owner--repo"), []byte("block"), 0o644); err != nil {
		t.Fatalf("plant blocking file: %v", err)
	}

	_, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "solo", Action: "ask", Instructions: "audit anyway", Background: true, Home: home})
	if err == nil || !strings.Contains(err.Error(), "allocate read-only ask worktree") {
		t.Fatalf("allocation error = %v, want fail-closed taskless ask refusal", err)
	}
	if jobs, listErr := store.ListJobs(ctx); listErr != nil || len(jobs) != 0 {
		t.Fatalf("jobs after allocation refusal=%+v err=%v, want none", jobs, listErr)
	}
}
