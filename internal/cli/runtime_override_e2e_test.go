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

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// Per-job --runtime override E2Es (#531): deterministic, NO-LLM, offline.
//
// Setup in both tests: a registered agent whose DEFAULT runtime is codex (a
// runtime that is NEVER invoked — its session ref is a non-existent named
// session, so any accidental codex dispatch fails fast) is dispatched with
// `--runtime shell --session <script>`, where the script is a shell-runtime
// fixture (the heartbeat/canary E2E pattern) that writes a marker file and
// emits a valid approved gitmoot_result.
//
// Proven invariants:
//   - the job SUCCEEDS via the SHELL adapter (terminal succeeded + the
//     script's marker file exists). MUTATION: ignoring the override at the
//     adapter-selection seam re-selects the codex adapter, whose delivery
//     fails (no such codex session) — this assertion goes red;
//   - the runtime-session lock key names the OVERRIDE runtime
//     ("runtime:shell:<hash>", exposed by the runtime_override job event) and
//     never the default runtime's session;
//   - the agent's registered default runtime is untouched: `agent show`
//     still reports codex with the original session ref and model.
//
// The foreground test drives the CLI dispatch path; the daemon test drives
// enqueue-with-override -> the REAL worker tick, proving background jobs
// honor the override identically.

const runtimeOverrideCodexRef = "codex-session-never-invoked"

func runtimeOverrideE2EHome(t *testing.T) (string, *db.Store, string) {
	t.Helper()
	home, _, store := heartbeatLoopE2EHome(t)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	// Default runtime codex with a stored model: the override must use neither.
	if err := store.UpsertAgent(context.Background(), db.Agent{
		Name:           "maintainer",
		Role:           "worker",
		Runtime:        runtime.CodexRuntime,
		RuntimeRef:     runtimeOverrideCodexRef,
		RepoScope:      "owner/repo",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto,
		Model:          "gpt-5.5-codex",
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	return home, store, checkout
}

// runtimeOverrideShellScript is the shell-runtime session body run as
// `sh -c <script> gitmoot <prompt>`: it records that the SHELL adapter really
// executed (the marker file) and emits a valid approved gitmoot_result so the
// job runs to terminal succeeded with no LLM and no network.
func runtimeOverrideShellScript(marker string) string {
	return fmt.Sprintf(`touch %q; printf '%%s' '{"gitmoot_result":{"decision":"approved","summary":"ran on shell override","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`, marker)
}

// assertRuntimeOverrideInvariants holds the shared post-run assertions for
// both the foreground and daemon paths.
func assertRuntimeOverrideInvariants(t *testing.T, store *db.Store, home string, jobID string, marker string) {
	t.Helper()
	ctx := context.Background()

	// The SHELL adapter executed the fixture (mutation-sensitive: a codex
	// dispatch never runs the script).
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("shell fixture did not run (marker missing): %v", err)
	}

	// Terminal succeeded with the script's result persisted.
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
	if payload.Result == nil || payload.Result.Decision != "approved" || payload.Result.Summary != "ran on shell override" {
		t.Fatalf("job result = %+v, want the shell fixture's approved result", payload.Result)
	}
	// History exposes the effective runtime.
	if payload.RuntimeOverride != runtime.ShellRuntime {
		t.Fatalf("payload runtime_override = %q, want shell", payload.RuntimeOverride)
	}

	// The runtime-session lock key named the OVERRIDE runtime, never codex.
	events, err := store.ListJobEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var overrideEvent string
	for _, event := range events {
		if event.Kind == "runtime_override" {
			overrideEvent = event.Message
		}
	}
	if overrideEvent == "" {
		t.Fatalf("expected a runtime_override job event, got %+v", events)
	}
	if !strings.Contains(overrideEvent, "job runs on runtime shell (agent default codex)") {
		t.Fatalf("runtime_override event %q must expose effective + default runtime", overrideEvent)
	}
	if !strings.Contains(overrideEvent, "session lock runtime:shell:") {
		t.Fatalf("runtime_override event %q must name a runtime:shell: session lock", overrideEvent)
	}
	if strings.Contains(overrideEvent, "runtime:codex") {
		t.Fatalf("override job must not touch the default-runtime session lock: %q", overrideEvent)
	}
	// The default-runtime session lock was never taken (and the override lock
	// was released on the terminal path).
	if _, err := store.GetResourceLock(ctx, "runtime:codex:"+runtimeOverrideCodexRef); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("default-runtime session lock exists (err=%v); an override job must never take it", err)
	}

	// The agent's registered default runtime is untouched — assert via the
	// REAL `agent show` surface, not just the store.
	var out, errBuf bytes.Buffer
	if code := Run([]string{"agent", "show", "maintainer", "--home", home}, &out, &errBuf); code != 0 {
		t.Fatalf("agent show exit = %d, stderr=%s", code, errBuf.String())
	}
	show := out.String()
	for _, want := range []string{"runtime: codex", "runtime_ref: " + runtimeOverrideCodexRef} {
		if !strings.Contains(show, want) {
			t.Fatalf("agent show output %q must still report %q", show, want)
		}
	}
	stored, err := store.GetAgent(ctx, "maintainer")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if stored.Runtime != runtime.CodexRuntime || stored.RuntimeRef != runtimeOverrideCodexRef || stored.Model != "gpt-5.5-codex" {
		t.Fatalf("override persisted onto the agent config: runtime=%q ref=%q model=%q", stored.Runtime, stored.RuntimeRef, stored.Model)
	}
}

// TestRuntimeOverrideForegroundShellE2E drives the real CLI foreground path:
// `agent ask --runtime shell --session <fixture>`.
func TestRuntimeOverrideForegroundShellE2E(t *testing.T) {
	home, store, _ := runtimeOverrideE2EHome(t)
	marker := filepath.Join(t.TempDir(), "shell-override-ran")
	script := runtimeOverrideShellScript(marker)

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "maintainer", "what is the state of the repo?",
		"--home", home,
		"--repo", "owner/repo",
		"--runtime", "shell",
		"--session", script,
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
	if output.Result == nil || output.Result.Summary != "ran on shell override" {
		t.Fatalf("foreground ask result = %+v, want the shell fixture's result", output.Result)
	}
	assertRuntimeOverrideInvariants(t, store, home, output.JobID, marker)
}

// TestRuntimeOverrideDaemonBackgroundShellE2E drives the DAEMON path: the CLI
// enqueues a background job whose payload carries the override, and the REAL
// worker tick claims + runs it through the shell adapter.
func TestRuntimeOverrideDaemonBackgroundShellE2E(t *testing.T) {
	ctx := context.Background()
	home, store, _ := runtimeOverrideE2EHome(t)
	marker := filepath.Join(t.TempDir(), "shell-override-ran-daemon")
	script := runtimeOverrideShellScript(marker)

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "maintainer", "what is the state of the repo?",
		"--home", home,
		"--repo", "owner/repo",
		"--runtime", "shell",
		"--session", script,
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

	// The REAL worker tick honors the payload's override.
	worker := defaultJobWorker(store, io.Discard, home)
	if err := runEnabledRepoWorkerTicks(ctx, store, worker, 1, io.Discard, time.Now().UTC()); err != nil {
		t.Fatalf("worker tick: %v", err)
	}
	assertRuntimeOverrideInvariants(t, store, home, output.JobID, marker)
}

// TestRuntimeOverrideValidationBeforeEnqueue: an unknown --runtime (or a shell
// override without --session, or --session without --runtime) fails with a
// clear error BEFORE any job is enqueued.
func TestRuntimeOverrideValidationBeforeEnqueue(t *testing.T) {
	ctx := context.Background()
	home, store, _ := runtimeOverrideE2EHome(t)

	for name, args := range map[string][]string{
		"unknown runtime":         {"agent", "ask", "maintainer", "hi", "--home", home, "--repo", "owner/repo", "--runtime", "bogus"},
		"shell without session":   {"agent", "ask", "maintainer", "hi", "--home", home, "--repo", "owner/repo", "--runtime", "shell"},
		"session without runtime": {"agent", "ask", "maintainer", "hi", "--home", home, "--repo", "owner/repo", "--session", "printf ok"},
		// "last" names no concrete session: the delivery would resume whichever
		// session is most recent (possibly another agent's default-runtime
		// session, mid-flight) under a "runtime:<rt>:last" lock that can never
		// serialize with the concrete session's lock.
		"last session": {"agent", "ask", "maintainer", "hi", "--home", home, "--repo", "owner/repo", "--runtime", "claude", "--session", "last"},
	} {
		var out, errBuf bytes.Buffer
		if code := Run(args, &out, &errBuf); code == 0 {
			t.Fatalf("%s: expected a non-zero exit, stdout=%s", name, out.String())
		}
		jobs, err := store.ListJobs(ctx)
		if err != nil {
			t.Fatalf("%s: ListJobs: %v", name, err)
		}
		if len(jobs) != 0 {
			t.Fatalf("%s: invalid override must fail before enqueue, found jobs %+v", name, jobs)
		}
	}

	// The unknown-runtime error enumerates the valid registry values.
	var out, errBuf bytes.Buffer
	if code := Run([]string{"agent", "ask", "maintainer", "hi", "--home", home, "--repo", "owner/repo", "--runtime", "bogus"}, &out, &errBuf); code == 0 {
		t.Fatal("unknown runtime accepted")
	}
	for _, supported := range runtime.SupportedRuntimes() {
		if !strings.Contains(errBuf.String(), supported) {
			t.Fatalf("error %q must enumerate supported runtime %q", errBuf.String(), supported)
		}
	}
}

// TestRuntimeOverridePermissionBlockedJobKeepsOverride: an implement dispatch
// on a non-write-policy agent routes to the permission-blocked enqueue path,
// whose persisted payload must keep the resolved --runtime/--session override
// AND the per-job --model. `gitmoot job retry` re-runs the stored payload
// as-is, so dropping them here would silently retry the job on the agent's
// DEFAULT runtime — taking the default runtime-session lock and resuming the
// exact session the user's --runtime asked it to stay off.
func TestRuntimeOverridePermissionBlockedJobKeepsOverride(t *testing.T) {
	ctx := context.Background()
	home, store, _ := runtimeOverrideE2EHome(t)
	// Implement capability + read-only policy: dispatch reaches
	// readOnlyImplementationBlocked and enqueues the blocked job.
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "ro-implementer",
		Role:           "worker",
		Runtime:        runtime.CodexRuntime,
		RuntimeRef:     runtimeOverrideCodexRef,
		RepoScope:      "owner/repo",
		Capabilities:   []string{"implement"},
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
		HealthStatus:   "ok",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "implement", "ro-implementer", "add a feature",
		"--home", home,
		"--repo", "owner/repo",
		"--runtime", "shell",
		"--session", "printf ok",
		"--model", "override-model",
		"--json",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("agent implement exit = %d, stderr=%s", code, errBuf.String())
	}
	var output localAgentJobOutput
	if err := json.Unmarshal(out.Bytes(), &output); err != nil {
		t.Fatalf("parse implement output %q: %v", out.String(), err)
	}
	if output.State != string(workflow.JobBlocked) {
		t.Fatalf("implement state = %q, want blocked", output.State)
	}
	job, err := store.GetJob(ctx, output.JobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", output.JobID, err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if payload.RuntimeOverride != runtime.ShellRuntime || payload.RuntimeOverrideRef != "printf ok" {
		t.Fatalf("blocked payload override = %q/%q, want shell/\"printf ok\" (a retry must honor the user's --runtime)", payload.RuntimeOverride, payload.RuntimeOverrideRef)
	}
	if payload.Model != "override-model" {
		t.Fatalf("blocked payload Model = %q, want %q (a retry must honor the per-job --model)", payload.Model, "override-model")
	}
}
