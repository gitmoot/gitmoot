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

func TestRuntimeOverrideForegroundStoredEffectiveRuntimeFailsClosed(t *testing.T) {
	tests := []struct {
		name                        string
		updateExpr                  string
		wantError                   string
		wantPresent                 bool
		wantStored                  string
		wantOverride                string
		bumpGeneration              bool
		rejectFailed                bool
		suppressTerminalTransitions bool
		wantSuppressed              bool
		wantState                   workflow.JobState
		wantErrorExtra              string
	}{
		{
			name:         "missing",
			updateExpr:   `json_remove(payload, '$.effective_runtime')`,
			wantError:    "stored job payload is missing effective_runtime",
			wantOverride: runtime.ShellRuntime,
			wantState:    workflow.JobFailed,
		},
		{
			name:         "empty",
			updateExpr:   `json_set(payload, '$.effective_runtime', '')`,
			wantError:    "stored job payload effective_runtime is empty",
			wantPresent:  true,
			wantOverride: runtime.ShellRuntime,
			wantState:    workflow.JobFailed,
		},
		{
			name:         "effective_runtime_mismatch",
			updateExpr:   `json_set(payload, '$.effective_runtime', 'codex')`,
			wantError:    `stored job payload effective_runtime "codex" does not match execution runtime "shell"`,
			wantPresent:  true,
			wantStored:   runtime.CodexRuntime,
			wantOverride: runtime.ShellRuntime,
			wantState:    workflow.JobFailed,
		},
		{
			name:         "runtime_override_mismatch",
			updateExpr:   `json_set(payload, '$.runtime_override', 'codex')`,
			wantError:    `stored job payload runtime_override "codex" does not match execution runtime "shell"`,
			wantPresent:  true,
			wantStored:   runtime.ShellRuntime,
			wantOverride: runtime.CodexRuntime,
			wantState:    workflow.JobFailed,
		},
		{
			name:           "lifecycle_generation_changed",
			updateExpr:     `json_remove(payload, '$.effective_runtime')`,
			wantError:      "stored job payload is missing effective_runtime",
			wantOverride:   runtime.ShellRuntime,
			bumpGeneration: true,
			wantState:      workflow.JobFailed,
		},
		{
			name:           "failed_transition_rejected",
			updateExpr:     `json_remove(payload, '$.effective_runtime')`,
			wantError:      "stored job payload is missing effective_runtime",
			wantOverride:   runtime.ShellRuntime,
			rejectFailed:   true,
			wantState:      workflow.JobBlocked,
			wantErrorExtra: "forced foreground failed settlement rejection",
		},
		{
			name:                        "terminal_transitions_do_not_apply",
			updateExpr:                  `json_remove(payload, '$.effective_runtime')`,
			wantError:                   "stored job payload is missing effective_runtime",
			wantOverride:                runtime.ShellRuntime,
			suppressTerminalTransitions: true,
			wantSuppressed:              true,
			wantState:                   workflow.JobQueued,
			wantErrorExtra:              "daemon dispatch suppressed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			home, store, _ := runtimeOverrideE2EHome(t)
			marker := filepath.Join(t.TempDir(), "shell-override-must-not-run")
			script := runtimeOverrideShellScript(marker)

			raw, err := sql.Open("sqlite", store.DatabasePath())
			if err != nil {
				t.Fatalf("open raw sqlite: %v", err)
			}
			defer raw.Close()
			generationUpdate := ""
			if tc.bumpGeneration {
				generationUpdate = ", lifecycle_generation = lifecycle_generation + 1"
			}
			trigger := fmt.Sprintf(`CREATE TRIGGER corrupt_foreground_effective_runtime
				AFTER INSERT ON jobs
				WHEN json_extract(NEW.payload, '$.runtime_override') = 'shell'
				BEGIN
					UPDATE jobs SET payload = %s%s WHERE id = NEW.id;
				END`, tc.updateExpr, generationUpdate)
			if _, err := raw.Exec(trigger); err != nil {
				t.Fatalf("create effective-runtime corruption trigger: %v", err)
			}
			if tc.rejectFailed {
				if _, err := raw.Exec(`CREATE TRIGGER reject_foreground_failed_settlement
					BEFORE UPDATE OF state ON jobs
					WHEN OLD.state = 'queued' AND NEW.state = 'failed'
					BEGIN
						SELECT RAISE(FAIL, 'forced foreground failed settlement rejection');
					END`); err != nil {
					t.Fatalf("create failed-settlement rejection trigger: %v", err)
				}
			}
			if tc.suppressTerminalTransitions {
				if _, err := raw.Exec(`CREATE TRIGGER suppress_foreground_terminal_transitions
					BEFORE UPDATE OF state ON jobs
					WHEN OLD.state = 'queued' AND NEW.state IN ('failed', 'blocked')
					BEGIN
						SELECT RAISE(IGNORE);
					END`); err != nil {
					t.Fatalf("create terminal-transition suppression trigger: %v", err)
				}
			}

			var out, errBuf bytes.Buffer
			code := Run([]string{
				"agent", "ask", "maintainer", "do not run with corrupted runtime evidence",
				"--home", home,
				"--repo", "owner/repo",
				"--runtime", "shell",
				"--session", script,
				"--json",
			}, &out, &errBuf)
			if code == 0 {
				t.Fatalf("foreground ask exit = 0 with %s runtime evidence, output=%s", tc.name, out.String())
			}
			if !strings.Contains(errBuf.String(), tc.wantError) {
				t.Fatalf("foreground error = %q, want %q", errBuf.String(), tc.wantError)
			}
			if tc.wantErrorExtra != "" && !strings.Contains(errBuf.String(), tc.wantErrorExtra) {
				t.Fatalf("foreground error = %q, want additional settlement diagnostic %q", errBuf.String(), tc.wantErrorExtra)
			}
			if tc.rejectFailed && !strings.Contains(errBuf.String(), "failed settlement unavailable:") {
				t.Fatalf("foreground error = %q, want failed-settlement fallback classification", errBuf.String())
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("shell override ran with %s runtime evidence (marker err=%v)", tc.name, err)
			}

			jobs, err := store.ListJobs(ctx)
			if err != nil {
				t.Fatalf("ListJobs: %v", err)
			}
			if len(jobs) != 1 {
				t.Fatalf("jobs = %d, want exactly the foreground job", len(jobs))
			}
			job := jobs[0]
			if job.State != string(tc.wantState) {
				t.Fatalf("job state = %q, want %s before shell execution", job.State, tc.wantState)
			}
			if tc.bumpGeneration && job.LifecycleGeneration < 1 {
				t.Fatalf("lifecycle generation = %d, want fixture-proven generation bump", job.LifecycleGeneration)
			}
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal([]byte(job.Payload), &envelope); err != nil {
				t.Fatalf("decode stored payload: %v", err)
			}
			rawRuntime, present := envelope["effective_runtime"]
			if present != tc.wantPresent {
				t.Fatalf("effective_runtime present = %v, want %v; payload=%s", present, tc.wantPresent, job.Payload)
			}
			if present {
				var stored string
				if err := json.Unmarshal(rawRuntime, &stored); err != nil {
					t.Fatalf("decode stored effective_runtime: %v", err)
				}
				if stored != tc.wantStored {
					t.Fatalf("stored effective_runtime = %q, want %q", stored, tc.wantStored)
				}
			}
			var storedOverride string
			if err := json.Unmarshal(envelope["runtime_override"], &storedOverride); err != nil {
				t.Fatalf("decode stored runtime_override: %v", err)
			}
			if storedOverride != tc.wantOverride {
				t.Fatalf("stored runtime_override = %q, want %q", storedOverride, tc.wantOverride)
			}
			events, err := store.ListJobEvents(ctx, job.ID)
			if err != nil {
				t.Fatalf("ListJobEvents: %v", err)
			}
			var terminalEvent string
			var suppressionEvent string
			for _, event := range events {
				if event.Kind == string(tc.wantState) {
					terminalEvent = event.Message
				}
				if event.Kind == runtimeOverrideEventKind {
					t.Fatalf("runtime override was journaled despite pre-execution refusal: %+v", event)
				}
				if event.Kind == foregroundRuntimeDispatchSuppressedEventKind {
					suppressionEvent = event.Message
				}
			}
			if tc.wantSuppressed {
				if !strings.Contains(suppressionEvent, tc.wantError) || !strings.Contains(suppressionEvent, tc.wantErrorExtra) {
					t.Fatalf("suppression event = %q, want validation %q and diagnostic %q", suppressionEvent, tc.wantError, tc.wantErrorExtra)
				}
			} else {
				if !strings.Contains(terminalEvent, tc.wantError) {
					t.Fatalf("terminal event = %q, want attributed error %q", terminalEvent, tc.wantError)
				}
				if tc.wantErrorExtra != "" && !strings.Contains(terminalEvent, tc.wantErrorExtra) {
					t.Fatalf("terminal event = %q, want settlement diagnostic %q", terminalEvent, tc.wantErrorExtra)
				}
			}

			if tc.rejectFailed || tc.wantSuppressed {
				queued, err := store.ListQueuedJobs(ctx)
				if err != nil {
					t.Fatalf("ListQueuedJobs: %v", err)
				}
				if len(queued) != 0 {
					t.Fatalf("failed-settlement fallback left daemon-claimable jobs: %+v", queued)
				}
				queuedCount, err := store.CountQueuedJobsForRepo(ctx, "owner/repo")
				if err != nil {
					t.Fatalf("CountQueuedJobsForRepo: %v", err)
				}
				if queuedCount != 0 {
					t.Fatalf("queued count = %d, want suppressed refusal excluded", queuedCount)
				}
				worker := defaultJobWorker(store, io.Discard, home)
				if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
					t.Fatalf("worker tick after blocked fallback: %v", err)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("daemon executed blocked foreground refusal (marker err=%v)", err)
				}
			}
		})
	}
}

func TestRuntimeOverrideForegroundRuntimeEvidenceRepairedBeforeSettlementContinues(t *testing.T) {
	ctx := context.Background()
	home, store, _ := runtimeOverrideE2EHome(t)
	marker := filepath.Join(t.TempDir(), "shell-override-ran-after-repair")
	script := runtimeOverrideShellScript(marker)

	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER strip_foreground_effective_runtime
		AFTER INSERT ON jobs
		WHEN json_extract(NEW.payload, '$.runtime_override') = 'shell'
		BEGIN
			UPDATE jobs SET payload = json_remove(payload, '$.effective_runtime') WHERE id = NEW.id;
		END;
		CREATE TRIGGER repair_foreground_effective_runtime_before_settlement
		BEFORE UPDATE OF state ON jobs
		WHEN OLD.state = 'queued' AND NEW.state = 'failed'
		BEGIN
			UPDATE jobs SET payload = json_set(payload, '$.effective_runtime', 'shell') WHERE id = OLD.id;
			SELECT RAISE(IGNORE);
		END`); err != nil {
		t.Fatalf("create runtime-evidence repair triggers: %v", err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{
		"agent", "ask", "maintainer", "run after stored runtime evidence is repaired",
		"--home", home,
		"--repo", "owner/repo",
		"--runtime", "shell",
		"--session", script,
		"--json",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("foreground ask exit = %d after evidence repair, stderr=%s", code, errBuf.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("shell override did not run after evidence repair: %v", err)
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != string(workflow.JobSucceeded) {
		t.Fatalf("jobs = %+v, want one succeeded foreground job", jobs)
	}
	payload, err := workflow.ParseJobPayload(jobs[0].Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if payload.EffectiveRuntime != runtime.ShellRuntime || payload.RuntimeOverride != runtime.ShellRuntime {
		t.Fatalf("runtime evidence = effective %q override %q, want shell/shell", payload.EffectiveRuntime, payload.RuntimeOverride)
	}
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
	if err := runEnabledRepoWorkerTicksTracked(ctx, store, worker, 1, "", io.Discard, time.Now().UTC(), nil, nil); err != nil {
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
