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

// runtimeOverrideShellScript is the shell-runtime session body run as
// `sh -c <script> gitmoot <prompt>`: it records that the SHELL adapter really
// executed (the marker file) and emits a valid approved gitmoot_result so the
// job runs to terminal succeeded with no LLM and no network.
func runtimeOverrideShellScript(marker string) string {
	result := `printf '%s' '{"gitmoot_result":{"decision":"approved","summary":"ran on shell override","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`
	if strings.TrimSpace(marker) == "" {
		return result
	}
	return fmt.Sprintf("touch %q; %s", marker, result)
}

// assertRuntimeOverrideInvariants holds the shared post-run assertions for
// both the foreground and daemon paths.
func assertRuntimeOverrideInvariants(t *testing.T, store *db.Store, home string, jobID string, marker string) {
	t.Helper()
	ctx := context.Background()

	// A marker is only available on foreground paths. Detached read-only seats
	// cannot write to arbitrary host paths; their persisted result proves delivery.
	if marker != "" {
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("shell fixture did not run (marker missing): %v", err)
		}
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
	// ... and records it STRUCTURALLY for engine-side consumers (#1528).
	if payload.EffectiveRuntime != runtime.ShellRuntime {
		t.Fatalf("payload effective_runtime = %q, want shell (#1528)", payload.EffectiveRuntime)
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

func TestRuntimeOverrideDaemonBackgroundShellE2E(t *testing.T) {
	ctx := context.Background()
	home, store, _ := runtimeOverrideE2EHome(t)
	marker := filepath.Join(t.TempDir(), "shell-override-ran-daemon")
	script := runtimeOverrideShellScript("")

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
	if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
		t.Fatalf("worker tick: %v", err)
	}
	assertRuntimeOverrideInvariants(t, store, home, output.JobID, "")
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

// #2203 retired TestRuntimeOverridePermissionBlockedJobKeepsOverride. It drove
// `gitmoot agent implement` so dispatch reached readOnlyImplementationBlocked, then
// asserted the permission-blocked payload kept --runtime/--session/--model, so that
// `gitmoot job retry` could not silently resume the agent's DEFAULT runtime session.
//
// The `implement` CLI verb is gone and readOnlyImplementationBlocked returns false for
// every surviving job type (agent_permissions.go:22), so that entry point cannot be
// reached. Stating the coverage loss plainly rather than implying it moved: blocked-path
// payload preservation is no longer exercised by any test, because no surviving verb can
// produce a permission-blocked job. The override-preservation contract for verbs that DO
// dispatch stays covered by TestRuntimeOverrideForegroundShellE2E and
// TestRuntimeOverrideDaemonBackgroundShellE2E above.
//
// The worker-side guards (daemon_worker.go:434, :3270) are deliberately KEPT: a legacy
// `implement` job row can still be retried and must still fail closed. The dispatch-side
// call at agent_dispatch.go:445 is now unreachable; tracked with the other #2203 residue.
