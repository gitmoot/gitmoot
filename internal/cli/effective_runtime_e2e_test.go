package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Effective-runtime recording E2Es (#1528): a job dispatched with NO --runtime
// override must STILL record the runtime it ran on — structurally on the job
// payload (payload.effective_runtime, the field the review-loop family
// resolver reads) and in the journal (the effective_runtime job event). Before
// #1528 the event was emitted only for overridden jobs, so a default-runtime
// job's family survived only in the posted PR comment — a record no
// engine-side check can read. Deterministic, NO-LLM, offline: the agent's
// DEFAULT runtime is shell, so no override flag is involved at all.

// effectiveRuntimeE2EHome registers "shell-asker" whose DEFAULT runtime is
// shell; RuntimeRef carries the fixture script, so a plain `agent ask` runs
// the script with no --runtime/--session flags.
func effectiveRuntimeE2EHome(t *testing.T, script string) (string, *db.Store) {
	t.Helper()
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           "shell-asker",
		Role:           "worker",
		Runtime:        runtime.ShellRuntime,
		RuntimeRef:     script,
		RepoScope:      "owner/repo",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto,
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	return home, store
}

// assertEffectiveRuntimeRecordedWithoutOverride holds the shared post-run
// assertions for both the foreground and daemon paths.
func assertEffectiveRuntimeRecordedWithoutOverride(t *testing.T, store *db.Store, jobID string, marker string) {
	t.Helper()
	ctx := context.Background()

	// The shell fixture really ran and the job reached terminal succeeded.
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("shell fixture did not run (marker missing): %v", err)
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}

	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	// PIN the fixture: this job carried NO override — the distinguishing
	// property of this test. A fixture that silently grew an override would
	// pass while testing nothing.
	if payload.RuntimeOverride != "" {
		t.Fatalf("fixture weakened: payload runtime_override = %q, want empty (this test is the NO-override case)", payload.RuntimeOverride)
	}
	// Structural record: the effective runtime survives into the TERMINAL
	// payload — the exact record SucceededReviewVerdicts decodes.
	if payload.EffectiveRuntime != runtime.ShellRuntime {
		t.Fatalf("payload effective_runtime = %q, want shell recorded for a default-runtime job (#1528)", payload.EffectiveRuntime)
	}

	// Journal record: a default-runtime selection uses effective_runtime, not
	// runtime_override (which truthfully remains reserved for actual overrides).
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var runtimeEvent string
	for _, event := range events {
		if event.Kind == effectiveRuntimeEventKind {
			runtimeEvent = event.Message
		}
		if event.Kind == runtimeOverrideEventKind {
			t.Fatalf("default-runtime job emitted a false runtime_override event: %+v", event)
		}
	}
	if runtimeEvent == "" {
		t.Fatalf("expected an effective_runtime job event for a NO-override job, got %+v", events)
	}
	if !strings.Contains(runtimeEvent, "job runs on runtime shell (agent default shell)") {
		t.Fatalf("effective_runtime event %q must expose the selected runtime for a default-runtime job", runtimeEvent)
	}
}

func TestPersistJobEffectiveRuntimePreservesUnknownPayloadFields(t *testing.T) {
	ctx := context.Background()
	_, _, store := heartbeatLoopE2EHome(t)
	const jobID = "effective-runtime-preserves-envelope"
	const payload = `{"repo":"owner/repo","future_evidence":{"score":7,"source":"next-version"},"legacy_flag":true}`
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: jobID, Agent: "shell-asker", Type: "ask", State: string(workflow.JobQueued), Payload: payload,
	}, db.JobEvent{JobID: jobID, Kind: string(workflow.JobQueued), Message: "queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}

	// PIN the fixture's distinguishing property: the seed payload must NOT
	// already carry effective_runtime. A fixture that pre-contains the value
	// turns persistJobEffectiveRuntime into a no-op, and this test would pass
	// green without the envelope update ever executing — testing nothing.
	// Read the STORED row (not the literal above) so a seed-time mutation is
	// caught too.
	seeded, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(seeded): %v", err)
	}
	var seededEnvelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(seeded.Payload), &seededEnvelope); err != nil {
		t.Fatalf("decode seeded envelope: %v", err)
	}
	if _, present := seededEnvelope["effective_runtime"]; present {
		t.Fatalf("fixture weakened: seed payload already carries effective_runtime=%s; persistence would be a no-op", seededEnvelope["effective_runtime"])
	}

	if err := persistJobEffectiveRuntime(ctx, store, jobID, runtime.ShellRuntime); err != nil {
		t.Fatalf("persistJobEffectiveRuntime: %v", err)
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(job.Payload), &envelope); err != nil {
		t.Fatalf("decode persisted envelope: %v", err)
	}
	if got := string(envelope["future_evidence"]); got != `{"score":7,"source":"next-version"}` {
		t.Fatalf("future_evidence = %s, want unknown object preserved", got)
	}
	if got := string(envelope["legacy_flag"]); got != "true" {
		t.Fatalf("legacy_flag = %s, want unknown scalar preserved", got)
	}
	parsed, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if parsed.EffectiveRuntime != runtime.ShellRuntime {
		t.Fatalf("effective_runtime = %q, want shell", parsed.EffectiveRuntime)
	}
}

// TestEffectiveRuntimeRecordedWithoutOverrideForegroundE2E drives the real CLI
// foreground path: `agent ask` with NO --runtime on a shell-default agent.
func TestEffectiveRuntimeRecordedWithoutOverrideForegroundE2E(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "shell-default-ran")
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "shell-asker", "what is the state of the repo?",
		"--home", home,
		"--repo", "owner/repo",
		"--json",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("agent ask exit = %d, stderr=%s", code, errBuf.String())
	}
	var output localAgentJobOutput
	if err := json.Unmarshal(out.Bytes(), &output); err != nil {
		t.Fatalf("parse ask output %q: %v", out.String(), err)
	}
	if output.State != string(workflow.JobSucceeded) {
		t.Fatalf("foreground ask state = %q, want succeeded", output.State)
	}
	assertEffectiveRuntimeRecordedWithoutOverride(t, store, output.JobID, marker)
}

// TestEffectiveRuntimeRecordedWithoutOverrideDaemonE2E drives the DAEMON path:
// a background job with no override, claimed and run by the REAL worker tick.
func TestEffectiveRuntimeRecordedWithoutOverrideDaemonE2E(t *testing.T) {
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "shell-default-ran-daemon")
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "shell-asker", "what is the state of the repo?",
		"--home", home,
		"--repo", "owner/repo",
		"--background",
		"--json",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("agent ask --background exit = %d, stderr=%s", code, errBuf.String())
	}
	var output localAgentJobOutput
	if err := json.Unmarshal(out.Bytes(), &output); err != nil {
		t.Fatalf("parse ask output %q: %v", out.String(), err)
	}
	if output.State != string(workflow.JobQueued) {
		t.Fatalf("background ask state = %q, want queued", output.State)
	}
	// Nothing ran at enqueue time: the fixture must not have executed yet.
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shell fixture ran at enqueue time (err=%v)", err)
	}

	worker := defaultJobWorker(store, io.Discard, home)
	if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
		t.Fatalf("worker tick: %v", err)
	}
	assertEffectiveRuntimeRecordedWithoutOverride(t, store, output.JobID, marker)
}

func TestEffectiveRuntimePersistenceFailureBlocksDaemonExecution(t *testing.T) {
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "must-not-run")
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "shell-asker", "do not run after a persistence failure",
		"--home", home,
		"--repo", "owner/repo",
		"--background",
		"--json",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("agent ask --background exit = %d, stderr=%s", code, errBuf.String())
	}
	var output localAgentJobOutput
	if err := json.Unmarshal(out.Bytes(), &output); err != nil {
		t.Fatalf("parse ask output %q: %v", out.String(), err)
	}

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_effective_runtime_update
		BEFORE UPDATE OF payload ON jobs
		WHEN NEW.id = '` + output.JobID + `' AND instr(NEW.payload, '"effective_runtime"') > 0
		BEGIN
			SELECT RAISE(FAIL, 'forced effective runtime write failure');
		END`); err != nil {
		t.Fatalf("create rejection trigger: %v", err)
	}

	worker := defaultJobWorker(store, io.Discard, home)
	if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
		t.Fatalf("worker tick: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("adapter ran after effective-runtime persistence failed (marker err=%v)", err)
	}
	job, err := store.GetJob(ctx, output.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed before adapter execution", job.State)
	}
	parsed, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if parsed.EffectiveRuntime != "" {
		t.Fatalf("effective_runtime = %q after rejected write, want empty", parsed.EffectiveRuntime)
	}
}

// TestEffectiveRuntimePersistenceFailureBlocksForegroundExecutionDurably is the
// FOREGROUND half of the fail-stop (#1528 review): `agent ask` with the
// effective-runtime write forced to fail must not run the adapter AND must
// leave the job ineligible for later daemon execution — a queued job a tick
// can pick up is relabelled, not stopped. The trigger is installed BEFORE the
// dispatch and matches any payload write carrying effective_runtime (the
// fixture's payload starts without it, so the persist write is the only
// possible fire), because the job ID does not exist until Run enqueues it.
func TestEffectiveRuntimePersistenceFailureBlocksForegroundExecutionDurably(t *testing.T) {
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "must-not-run-foreground")
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_effective_runtime_update_foreground
		BEFORE UPDATE OF payload ON jobs
		WHEN instr(NEW.payload, '"effective_runtime"') > 0
		BEGIN
			SELECT RAISE(FAIL, 'forced effective runtime write failure');
		END`); err != nil {
		t.Fatalf("create rejection trigger: %v", err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "shell-asker", "do not run after a persistence failure",
		"--home", home,
		"--repo", "owner/repo",
		"--json",
	}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("foreground ask exit = 0 with the write forced to fail, stderr=%s", errBuf.String())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("adapter ran after effective-runtime persistence failed (marker err=%v)", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want exactly the one dispatched job", len(jobs))
	}
	job := jobs[0]
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed — a queued job is relabelled, not stopped", job.State)
	}
	// Anchor: the write was genuinely ATTEMPTED and rejected — the settle
	// event carries the trigger's own RAISE message. Without this, an earlier
	// unrelated failure would pass the state check for the wrong reason.
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var settleEvent string
	for _, event := range events {
		if event.Kind == string(workflow.JobFailed) {
			settleEvent = event.Message
		}
	}
	if !strings.Contains(settleEvent, "forced effective runtime write failure") {
		t.Fatalf("settle event %q must carry the forced write failure; the persist write may never have run", settleEvent)
	}

	// The durable half: clear the failure and run a REAL daemon tick. The job
	// must stay failed and the adapter must still not run.
	if _, err := raw.Exec(`DROP TRIGGER reject_effective_runtime_update_foreground`); err != nil {
		t.Fatalf("drop rejection trigger: %v", err)
	}
	worker := defaultJobWorker(store, io.Discard, home)
	if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
		t.Fatalf("worker tick: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon tick executed a job whose persistence had failed (marker err=%v)", err)
	}
	after, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob(after tick): %v", err)
	}
	if after.State != string(workflow.JobFailed) {
		t.Fatalf("job state after tick = %q, want failed", after.State)
	}
}
