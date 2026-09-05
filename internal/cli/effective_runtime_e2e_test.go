//go:build e2e

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

	// A marker is only available on foreground paths. Detached read-only seats
	// prove delivery through the persisted shell result below.
	if marker != "" {
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("shell fixture did not run (marker missing): %v", err)
		}
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
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(""))

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
	assertEffectiveRuntimeRecordedWithoutOverride(t, store, output.JobID, "")
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

func TestEffectiveRuntimeNullPayloadBlocksDaemonExecution(t *testing.T) {
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "must-not-run-null-payload")
	home, store := effectiveRuntimeE2EHome(t, runtimeOverrideShellScript(marker))

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "shell-asker", "do not run with a null payload envelope",
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
	admitted, err := store.GetJob(ctx, output.JobID)
	if err != nil {
		t.Fatalf("GetJob(admitted): %v", err)
	}
	if admitted.Payload == "null" {
		t.Fatal("fixture admitted an already-null payload; worker input must retain the valid dispatch envelope")
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE jobs SET payload = 'null' WHERE id = ?`, output.JobID); err != nil {
		t.Fatalf("corrupt stored payload: %v", err)
	}
	seeded, err := store.GetJob(ctx, output.JobID)
	if err != nil {
		t.Fatalf("GetJob(before worker run): %v", err)
	}
	if seeded.Payload != "null" {
		t.Fatalf("fixture payload = %q, want JSON null to reach the nil-envelope guard", seeded.Payload)
	}

	worker := defaultJobWorker(store, io.Discard, home)
	if err := worker.run(ctx, admitted); err != nil {
		t.Fatalf("worker run: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("adapter ran after null-payload persistence failed (marker err=%v)", err)
	}
	job, err := store.GetJob(ctx, output.JobID)
	if err != nil {
		t.Fatalf("GetJob(after tick): %v", err)
	}
	if job.State != string(workflow.JobFailed) {
		t.Fatalf("job state = %q, want failed before adapter execution", job.State)
	}
	events, err := store.ListJobEvents(ctx, output.JobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var failedEvent string
	for _, event := range events {
		if event.Kind == string(workflow.JobFailed) {
			failedEvent = event.Message
		}
		if event.Kind == effectiveRuntimeEventKind {
			t.Fatalf("effective-runtime event emitted despite null-payload fail-stop: %+v", event)
		}
	}
	if !strings.Contains(failedEvent, "persist effective runtime before execution: job payload must be a JSON object") {
		t.Fatalf("failed event = %q, want null-envelope persistence cause", failedEvent)
	}
}

// TestForegroundEffectiveRuntimePersistsAtomicallyWithEnqueue closes the last
// foreground fail-stop gap (#1528): even when a later effective-runtime
// backfill is unavailable, the job never depends on it because its already-
// resolved runtime was in the initial row.
func TestForegroundEffectiveRuntimePersistsAtomicallyWithEnqueue(t *testing.T) {
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "foreground-runs")
	script := fmt.Sprintf(`printf 'run\n' >> %q; printf '%%s' '{"gitmoot_result":{"decision":"approved","summary":"ran with atomic runtime evidence","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`, marker)
	home, store := effectiveRuntimeE2EHome(t, script)

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_effective_runtime_update_foreground
		BEFORE UPDATE OF payload ON jobs
		WHEN instr(OLD.payload, '"effective_runtime"') = 0
		  AND instr(NEW.payload, '"effective_runtime"') > 0
		BEGIN
			SELECT RAISE(FAIL, 'forced effective runtime write failure');
		END`); err != nil {
		t.Fatalf("create rejection trigger: %v", err)
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "shell-asker", "run with enqueue-time runtime evidence",
		"--home", home,
		"--repo", "owner/repo",
		"--json",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("foreground ask exit = %d, stderr=%s", code, errBuf.String())
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "run\n" {
		t.Fatalf("foreground adapter evidence = %q, err=%v; want exactly one run", got, err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want exactly the one dispatched job", len(jobs))
	}
	job := jobs[0]
	if job.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state = %q, want succeeded", job.State)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if payload.EffectiveRuntime != runtime.ShellRuntime {
		t.Fatalf("terminal effective_runtime = %q, want shell", payload.EffectiveRuntime)
	}

	// Prove the backfill rejection is genuinely armed. A separate queued row
	// without effective_runtime cannot add it, so this test cannot pass because
	// the trigger silently failed to install.
	const probeID = "foreground-backfill-rejection-probe"
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: probeID, Agent: "shell-asker", Type: "ask", State: string(workflow.JobQueued), Payload: `{"repo":"owner/repo"}`,
	}, db.JobEvent{JobID: probeID, Kind: string(workflow.JobQueued), Message: "queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent(probe): %v", err)
	}
	if err := persistJobEffectiveRuntime(ctx, store, probeID, runtime.ShellRuntime); err == nil || !strings.Contains(err.Error(), "forced effective runtime write failure") {
		t.Fatalf("persistence trigger error = %v, want forced failure", err)
	}
	if _, err := raw.Exec(`DROP TRIGGER reject_effective_runtime_update_foreground`); err != nil {
		t.Fatalf("drop rejection trigger: %v", err)
	}
	if transitioned, err := store.TransitionJobStateWithEvent(ctx, probeID, string(workflow.JobQueued), string(workflow.JobCancelled), db.JobEvent{
		JobID: probeID, Kind: string(workflow.JobCancelled), Message: "fixture cleanup",
	}); err != nil || !transitioned {
		t.Fatalf("cancel probe: transitioned=%v err=%v", transitioned, err)
	}

	// A real daemon tick cannot run the already-succeeded foreground row.
	worker := defaultJobWorker(store, io.Discard, home)
	if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
		t.Fatalf("worker tick: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "run\n" {
		t.Fatalf("adapter evidence after daemon tick = %q, err=%v; foreground row ran again", got, err)
	}
	after, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob(after tick): %v", err)
	}
	if after.State != string(workflow.JobSucceeded) {
		t.Fatalf("job state after tick = %q, want succeeded", after.State)
	}
}
