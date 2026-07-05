package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// TestSessionJobFullChainE2E is the full-chain, no-LLM, no-network proof for
// session jobs (#657). It drives the REAL code paths end to end on an isolated
// t.TempDir home — the actual CLI commands (`job open`/`job retry`/`job close`),
// the daemon's real dispatch tick, and the real stuck-running reaper — to prove
// the whole "clock in / clock out" contract holds as an integration, not just in
// units:
//
//	(a) `job open`  -> a RUNNING externally_driven row, a running/"started" event,
//	                   NO queued row, and NO runtime-session/checkout lock taken;
//	(b) a real dispatch tick delivers a normal queued job but NEVER the session job;
//	(c) the real stuck-running reaper, given a stale session row, does NOT reap it;
//	(d) `job retry` on the session job is refused;
//	(e) `job close --decision implemented` -> terminal succeeded + a "finished"
//	    (succeeded) event, reflected in ListJobs/GetJob exactly like an engine job.
//
// The fake adapter records every Deliver, so "never dispatched to a runtime" is
// asserted against the actual delivery seam, not a proxy.
func TestSessionJobFullChainE2E(t *testing.T) {
	ctx := context.Background()

	// Isolated home shared by the persistent assertion store and every CLI command
	// (both resolve config.PathsForHome(home), so they hit the same DB file).
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("db.Open returned error: %v", err)
	}
	defer store.Close()

	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	// lead has both ask + implement so it can own an implement session job and run a
	// normal ask job; the implement capability also gives it a write policy.
	seedDaemonWorkerAgent(t, store, "lead", runtime.ShellRuntime, "unused", []string{"ask", "implement"}, "owner/repo")

	homeArgs := []string{"--home", home}

	// --- (a) job open: clock in ---------------------------------------------
	var openOut bytes.Buffer
	if code := runJobOpen(append(append([]string{}, homeArgs...),
		"--agent", "lead", "--repo", "owner/repo", "--type", "implement", "--json"), &openOut, io.Discard); code != 0 {
		t.Fatalf("job open exit code = %d, want 0; out=%q", code, openOut.String())
	}
	sessionID := decodeSessionJobID(t, openOut.Bytes())

	opened, err := store.GetJob(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetJob(session) returned error: %v", err)
	}
	if opened.State != string(workflow.JobRunning) {
		t.Fatalf("opened session job state = %q, want running", opened.State)
	}
	if !opened.ExternallyDriven {
		t.Fatalf("opened session job ExternallyDriven = false, want true")
	}
	if evs, err := store.ListJobEvents(ctx, sessionID); err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	} else if !anyEventKind(evs, string(workflow.JobRunning)) {
		t.Fatalf("session job events = %+v, want a running/started event", evs)
	}
	if queued, err := store.ListQueuedJobs(ctx); err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	} else if len(queued) != 0 {
		t.Fatalf("queued jobs after open = %d, want 0 (a session job never queues)", len(queued))
	}
	if locks, err := store.ListResourceLocks(ctx); err != nil {
		t.Fatalf("ListResourceLocks returned error: %v", err)
	} else if len(locks) != 0 {
		t.Fatalf("resource locks after open = %+v, want none (no runtime-session/checkout lock for a session job)", locks)
	}

	// --- (b) a real dispatch tick never claims/Delivers the session job ------
	// A NORMAL queued job proves the tick DOES dispatch — the session job's absence
	// from the delivered set is then a real exemption, not a dead tick.
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{ID: "engine-ask-1", Agent: "lead", Action: "ask", Repo: "owner/repo", Branch: "main", PullRequest: 1})
	adapter := &cliWorkerFakeAdapter{output: poolSchedulerAskResult}
	worker := poolSchedulerWorker(t, store, adapter, false)
	if err := runDaemonWorkerTick(ctx, store, worker, 1, false, "owner/repo", "", io.Discard, time.Now().UTC()); err != nil {
		t.Fatalf("runDaemonWorkerTick(dispatch) returned error: %v", err)
	}
	delivered := adapter.deliveredIDs()
	if !containsString(delivered, "engine-ask-1") {
		t.Fatalf("delivered = %v, want the normal engine job to be dispatched (tick must be live)", delivered)
	}
	if containsString(delivered, sessionID) {
		t.Fatalf("delivered = %v; the session job %s was dispatched to a runtime (invariant breach)", delivered, sessionID)
	}
	if reloaded, err := store.GetJob(ctx, sessionID); err != nil {
		t.Fatalf("GetJob(session) returned error: %v", err)
	} else if reloaded.State != string(workflow.JobRunning) {
		t.Fatalf("session job state after dispatch tick = %q, want still running (never claimed)", reloaded.State)
	}

	// --- (c) the real stuck-running reaper does NOT reap the session job -----
	// Treat the session row as stale by giving the REAL reaper a `before` threshold
	// past its updated_at (the same way the repo's own reaper tests simulate
	// staleness). A normal running job below the threshold WOULD be requeued; the
	// session job must be exempt because the calling session holds it open.
	reapNow := time.Now().UTC()
	if err := recoverRunningJobsBeforeForRepo(ctx, store, io.Discard, reapNow, reapNow.Add(time.Hour), "owner/repo", ""); err != nil {
		t.Fatalf("recoverRunningJobsBeforeForRepo returned error: %v", err)
	}
	if reaped, err := store.GetJob(ctx, sessionID); err != nil {
		t.Fatalf("GetJob(session) returned error: %v", err)
	} else if reaped.State != string(workflow.JobRunning) {
		t.Fatalf("session job state after reaper = %q, want still running (session job must be exempt)", reaped.State)
	}

	// --- (d) job retry on the session job is refused -------------------------
	var retryErr bytes.Buffer
	if code := runJobRetry(append(append([]string{}, sessionID), homeArgs...), io.Discard, &retryErr); code == 0 {
		t.Fatalf("job retry on a session job exit code = 0, want non-zero (retry must be refused)")
	}
	if !strings.Contains(retryErr.String(), "session job") {
		t.Fatalf("job retry stderr = %q, want a session-job refusal", retryErr.String())
	}
	// The refusal must be pre-transition: the job is untouched (still running, still
	// never queued).
	if afterRetry, err := store.GetJob(ctx, sessionID); err != nil {
		t.Fatalf("GetJob(session) returned error: %v", err)
	} else if afterRetry.State != string(workflow.JobRunning) {
		t.Fatalf("session job state after refused retry = %q, want still running", afterRetry.State)
	}
	if queued, err := store.ListQueuedJobs(ctx); err != nil {
		t.Fatalf("ListQueuedJobs returned error: %v", err)
	} else if containsQueued(queued, sessionID) {
		t.Fatalf("session job appeared in the queue after a refused retry: %+v", queued)
	}

	// --- (e) job close: clock out, terminal succeeded ------------------------
	var closeOut bytes.Buffer
	if code := runJobClose(append(append([]string{sessionID}, homeArgs...),
		"--decision", "implemented", "--summary", "did the here-work"), &closeOut, io.Discard); code != 0 {
		t.Fatalf("job close exit code = %d, want 0; out=%q", code, closeOut.String())
	}
	closed, err := store.GetJob(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetJob(session) returned error: %v", err)
	}
	if closed.State != string(workflow.JobSucceeded) {
		t.Fatalf("closed session job state = %q, want succeeded", closed.State)
	}
	if evs, err := store.ListJobEvents(ctx, sessionID); err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	} else if !anyEventKind(evs, string(workflow.JobSucceeded)) {
		t.Fatalf("session job events after close = %+v, want a succeeded/finished event", evs)
	}

	// It reflects in ListJobs/GetJob exactly like an engine-run job: succeeded,
	// externally_driven set, and carrying its recorded implemented decision.
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	var listed *db.Job
	for i := range jobs {
		if jobs[i].ID == sessionID {
			listed = &jobs[i]
			break
		}
	}
	if listed == nil {
		t.Fatalf("session job %s not present in ListJobs", sessionID)
	}
	if listed.State != string(workflow.JobSucceeded) || !listed.ExternallyDriven {
		t.Fatalf("ListJobs session row = %+v, want succeeded + externally_driven", *listed)
	}
	payload, err := workflow.ParseJobPayload(closed.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.Result == nil || payload.Result.Decision != "implemented" {
		t.Fatalf("closed session job payload result = %+v, want decision implemented", payload.Result)
	}
}

func decodeSessionJobID(t *testing.T, jsonBytes []byte) string {
	t.Helper()
	var out jobSessionOutput
	if err := json.Unmarshal(jsonBytes, &out); err != nil {
		t.Fatalf("decode job open JSON %q: %v", string(jsonBytes), err)
	}
	if strings.TrimSpace(out.JobID) == "" {
		t.Fatalf("job open JSON carried no job_id: %q", string(jsonBytes))
	}
	return out.JobID
}

func (f *cliWorkerFakeAdapter) deliveredIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.delivered...)
}

func anyEventKind(events []db.JobEvent, kind string) bool {
	for _, ev := range events {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}

func containsQueued(jobs []db.Job, id string) bool {
	for _, j := range jobs {
		if j.ID == id {
			return true
		}
	}
	return false
}
