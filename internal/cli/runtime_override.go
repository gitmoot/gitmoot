package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Per-job runtime override (#531).
//
// An agent keeps ONE registered default runtime + session; `--runtime <rt>`
// on agent run/ask/review/implement (and orchestrate) runs a single job
// through another runtime without touching the agent's stored config. The
// invariants, enforced here and asserted by the E2Es:
//
//   - the override applies to THIS job only — `agent show` still reports the
//     registered default runtime afterwards;
//   - SESSION SAFETY: an overridden job neither resumes nor writes the
//     agent's default-runtime session. It runs on its own ref — an explicit
//     `--session` on the override runtime, or a minted fresh ref that every
//     adapter treats as "start a brand-new session" — and the runtime-session
//     lock key names the OVERRIDE runtime, so it can never collide with the
//     default-runtime session lock;
//   - MODEL COMPAT: the agent's configured default model belongs to its
//     default runtime and is never leaked onto the override runtime. An
//     override without --model runs on the override runtime's own default
//     model; --model with --runtime is interpreted for the override runtime.

// resolveJobRuntimeOverride validates a requested per-job runtime override
// BEFORE enqueue and resolves the session ref the overridden job will run on.
// It returns ("", "", nil) when no override was requested. Valid runtimes are
// enumerated from the actual adapter registry (runtime.SupportedRuntimes),
// never hard-coded.
func resolveJobRuntimeOverride(overrideRuntime string, session string) (string, string, error) {
	rt := strings.TrimSpace(overrideRuntime)
	session = strings.TrimSpace(session)
	if rt == "" {
		if session != "" {
			return "", "", errors.New("--session requires --runtime (it names a session on the override runtime)")
		}
		return "", "", nil
	}
	if _, err := (runtime.Factory{}).Adapter(rt); err != nil {
		return "", "", err
	}
	if session != "" {
		// SESSION SAFETY: "last" names no concrete session — the delivery would
		// resume whichever session in the checkout is most recent (possibly an
		// agent's default-runtime session, mid-flight), while the lock key would
		// be the literal "runtime:<rt>:last" and so could never serialize with
		// that concrete session's lock. Require an explicit id; shell refs are
		// commands, not resumable sessions, so they are exempt.
		if session == runtime.LastRef && rt != runtime.ShellRuntime {
			return "", "", errors.New("--session last is not allowed with --runtime; pass an explicit session id")
		}
		return rt, session, nil
	}
	if rt == runtime.ShellRuntime {
		return "", "", errors.New("--runtime shell requires --session <command> (shell sessions are commands)")
	}
	ref, err := runtime.NewFreshRef()
	if err != nil {
		return "", "", err
	}
	return rt, ref, nil
}

// applyJobRuntimeOverride returns the EFFECTIVE runtime.Agent an overridden
// job runs as: the override runtime + the job's own session ref, with the
// agent's configured default model and effort cleared (they belong to the
// default runtime and may be invalid on the override runtime; per-job --model
// and --effort still flow through the job payload). A payload with no override
// returns the agent unchanged. The stored agent row is never modified.
func applyJobRuntimeOverride(agent runtime.Agent, payload workflow.JobPayload) runtime.Agent {
	return runtime.ApplyJobRuntimeOverride(agent, payload.RuntimeOverride, payload.RuntimeOverrideRef)
}

// scopeRegisteredFreshRefForJob rewrites a stored fresh:<seat> ref to a
// deterministic fresh:<job> ref for the actual execution. This keeps fresh
// registered agents isolated per job and gives their runtime-session lock a
// job-scoped key. Runtime overrides already mint a unique fresh ref per job at
// enqueue time, so callers use this only for non-overridden jobs.
func scopeRegisteredFreshRefForJob(agent runtime.Agent, jobID string) runtime.Agent {
	if runtime.IsFreshRef(agent.RuntimeRef) {
		agent.RuntimeRef = runtime.FreshRefForJob(jobID)
	}
	return agent
}

// overrideRuntimeSessionResourceKey computes the runtime-session lock key for
// a job running under a runtime override. Resumable runtimes keep the normal
// "runtime:<rt>:<ref>" key (an explicit session serializes with other users
// of that same session; a fresh ref is unique per job). A non-resumable
// runtime (shell) — which normally takes no session lock — still gets an
// override-scoped key here so the lock provably names the OVERRIDE runtime
// and can never collide with the agent's default-runtime session lock; shell
// refs are whole commands, so they are hashed into a bounded key.
func overrideRuntimeSessionResourceKey(agent runtime.Agent) (string, bool) {
	runtimeName := strings.TrimSpace(agent.Runtime)
	runtimeRef := strings.TrimSpace(agent.RuntimeRef)
	if runtimeName == "" || runtimeRef == "" {
		return "", false
	}
	if key, ok := runtimeSessionResourceKey(agent); ok {
		return key, true
	}
	return "runtime:" + runtimeName + ":" + shortHash(runtimeRef), true
}

// isolatedShellStageRuntimeSessionKey returns a JOB-SCOPED runtime-session lock
// key for a shell-override stage that runs in its OWN detached worktree, or
// ok=false for any other job (leaving the normal key derivation in force).
//
// A pipeline shell stage runs as a runtime override, so it takes the bounded
// override key runtime:shell:<hash(cmd)> (overrideRuntimeSessionResourceKey).
// That key exists (#531) ONLY so the lock provably names the OVERRIDE runtime
// and can never collide with the agent's default-runtime session lock —
// serializing identical commands is an incidental side effect of hashing the
// command, not a guarantee the key was created to provide. For an ISOLATED stage
// that side effect is spurious: each such stage runs in its own detached
// read-only worktree (#1016) and shares no checkout or session state, yet two
// isolated forks of the identical command still hash to the same key and
// serialize (#1034). Keying by job id removes that false dependency while
// preserving the #531 invariant — the key keeps the runtime:shell: prefix (still
// names the override runtime, still cannot collide with a resumable runtime's
// runtime:<rt>:<ref> key) but is unique per job, so distinct isolated forks never
// wait on one another. Non-isolated shell stages (shared checkout, no
// WorktreePath) keep the command-hash key and stay serialized — their shared
// checkout genuinely needs it (and the checkout lock covers it besides) — and
// resumable runtimes are untouched.
//
// The signal is read from the PERSISTED payload: ReadOnlyWorktree + WorktreePath
// are set synchronously at enqueue, before the job is visible to the daemon
// selector, so this resolves identically at scheduling time
// (queuedJobRuntimeResourceKey) and at lock acquisition — the two MUST agree or
// the gate and the lock diverge.
func isolatedShellStageRuntimeSessionKey(payload workflow.JobPayload, jobID string) (string, bool) {
	if strings.TrimSpace(payload.RuntimeOverride) != runtime.ShellRuntime {
		return "", false
	}
	if !payload.ReadOnlyWorktree || strings.TrimSpace(payload.WorktreePath) == "" {
		return "", false
	}
	id := strings.TrimSpace(jobID)
	if id == "" {
		return "", false
	}
	return "runtime:" + runtime.ShellRuntime + ":job:" + shortHash(id), true
}

// Runtime selection is journalled for every job so the family a job ran on
// survives in engine-readable history. A real override retains the established
// runtime_override kind; default selection uses effective_runtime.
const (
	effectiveRuntimeEventKind = "effective_runtime"
	runtimeOverrideEventKind  = "runtime_override"
)

func jobRuntimeEventKind(overridden bool) string {
	if overridden {
		return runtimeOverrideEventKind
	}
	return effectiveRuntimeEventKind
}

func jobRuntimeOverrideEventMessage(defaultRuntime string, effective runtime.Agent, lockKey string) string {
	message := fmt.Sprintf("job runs on runtime %s (agent default %s)", effective.Runtime, defaultRuntime)
	if strings.TrimSpace(lockKey) != "" {
		message += "; session lock " + lockKey
	}
	return message
}

func storedJobPayloadEnvelope(ctx context.Context, store *db.Store, jobID string) (map[string]json.RawMessage, error) {
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(job.Payload), &envelope); err != nil {
		return nil, err
	}
	if envelope == nil {
		return nil, errors.New("job payload must be a JSON object")
	}
	return envelope, nil
}

func runtimeFieldFromEnvelope(envelope map[string]json.RawMessage, field string) (string, bool, error) {
	raw, present := envelope[field]
	if !present {
		return "", false, nil
	}
	var recorded string
	if err := json.Unmarshal(raw, &recorded); err != nil {
		return "", true, fmt.Errorf("decode %s: %w", field, err)
	}
	return strings.TrimSpace(recorded), true, nil
}

func effectiveRuntimeFromEnvelope(envelope map[string]json.RawMessage) (string, bool, error) {
	return runtimeFieldFromEnvelope(envelope, "effective_runtime")
}

func validateStoredJobEffectiveRuntime(ctx context.Context, store *db.Store, jobID string, effectiveRuntime string) error {
	effective := strings.TrimSpace(effectiveRuntime)
	if effective == "" {
		return errors.New("execution runtime is empty")
	}
	envelope, err := storedJobPayloadEnvelope(ctx, store, jobID)
	if err != nil {
		return err
	}
	recorded, present, err := effectiveRuntimeFromEnvelope(envelope)
	if err != nil {
		return err
	}
	if !present {
		return errors.New("stored job payload is missing effective_runtime")
	}
	if recorded == "" {
		return errors.New("stored job payload effective_runtime is empty")
	}
	if recorded != effective {
		return fmt.Errorf("stored job payload effective_runtime %q does not match execution runtime %q", recorded, effective)
	}
	override, overridePresent, err := runtimeFieldFromEnvelope(envelope, "runtime_override")
	if err != nil {
		return err
	}
	if overridePresent && override != "" && override != effective {
		return fmt.Errorf("stored job payload runtime_override %q does not match execution runtime %q", override, effective)
	}
	return nil
}

const foregroundRuntimeSettlementAttempts = 3

// settleForegroundRuntimeValidationFailure makes a pre-execution refusal
// durable. A lifecycle CAS loss is retried only after the newer queued row is
// re-read and independently fails the same validation. If SQLite rejects the
// failed transition itself, blocked is the truthful terminal fallback: the job
// did not run, remains manually retryable, and cannot be claimed by the daemon.
func settleForegroundRuntimeValidationFailure(ctx context.Context, store *db.Store, home string, admitted db.Job, effectiveRuntime string, validationErr error) error {
	settler := jobWorker{Store: store, Stdout: io.Discard, ConfigHome: home, ConfigHomeExplicit: true}
	current := admitted
	cause := validationErr

	for attempt := 0; attempt < foregroundRuntimeSettlementAttempts; attempt++ {
		if settleErr := settler.finishQueuedJob(ctx, current, workflow.JobFailed, cause); settleErr != nil {
			blockedCause := fmt.Errorf("%w (failed settlement unavailable: %v)", cause, settleErr)
			if blockedErr := settler.finishQueuedJob(ctx, current, workflow.JobBlocked, blockedCause); blockedErr != nil {
				return fmt.Errorf("%w (additionally failed to settle the foreground job as blocked: %v)", blockedCause, blockedErr)
			}
			blocked, readErr := store.GetJob(ctx, current.ID)
			if readErr != nil {
				return fmt.Errorf("%w (additionally could not verify blocked foreground settlement: %v)", blockedCause, readErr)
			}
			if blocked.State != string(workflow.JobBlocked) {
				return fmt.Errorf("%w (blocked foreground settlement did not apply; current job state is %q)", blockedCause, blocked.State)
			}
			return blockedCause
		}

		settled, readErr := store.GetJob(ctx, current.ID)
		if readErr != nil {
			return fmt.Errorf("%w (additionally could not verify foreground settlement: %v)", cause, readErr)
		}
		if settled.State == string(workflow.JobFailed) {
			return cause
		}
		if settled.State != string(workflow.JobQueued) {
			return fmt.Errorf("%w (foreground settlement lost to concurrent state %q)", cause, settled.State)
		}
		freshValidationErr := validateStoredJobEffectiveRuntime(ctx, store, settled.ID, effectiveRuntime)
		if freshValidationErr == nil {
			return fmt.Errorf("%w (stored runtime evidence changed before settlement; current lifecycle remains queued)", cause)
		}
		cause = fmt.Errorf("validate effective runtime before foreground execution: %w", freshValidationErr)
		current = settled
	}

	blockedCause := fmt.Errorf("%w (failed settlement lost %d lifecycle comparisons)", cause, foregroundRuntimeSettlementAttempts)
	if blockedErr := settler.finishQueuedJob(ctx, current, workflow.JobBlocked, blockedCause); blockedErr != nil {
		return fmt.Errorf("%w (additionally failed to settle the foreground job as blocked: %v)", blockedCause, blockedErr)
	}
	blocked, readErr := store.GetJob(ctx, current.ID)
	if readErr != nil {
		return fmt.Errorf("%w (additionally could not verify blocked foreground settlement: %v)", blockedCause, readErr)
	}
	if blocked.State != string(workflow.JobBlocked) {
		return fmt.Errorf("%w (blocked foreground settlement did not apply; current job state is %q)", blockedCause, blocked.State)
	}
	return blockedCause
}

// persistJobEffectiveRuntime records the runtime a job is about to run on
// STRUCTURALLY on the job payload (#1528), so engine-side consumers — the
// review-loop family resolver now, the merge gate in the #1531 round — read a
// field instead of parsing the runtime_override event sentence. The payload is
// RE-READ from the store rather than trusting the caller's in-memory copy:
// earlier run-phase steps (e.g. the checkout head re-sync) persist their own
// payload updates, and encoding a stale copy would clobber them. No-op when
// the stored payload already carries the same value.
func persistJobEffectiveRuntime(ctx context.Context, store *db.Store, jobID string, effectiveRuntime string) error {
	effective := strings.TrimSpace(effectiveRuntime)
	if effective == "" {
		return nil
	}
	envelope, err := storedJobPayloadEnvelope(ctx, store, jobID)
	if err != nil {
		return err
	}
	recorded, _, err := effectiveRuntimeFromEnvelope(envelope)
	if err != nil {
		return err
	}
	if recorded == effective {
		return nil
	}
	encodedRuntime, err := json.Marshal(effective)
	if err != nil {
		return err
	}
	envelope["effective_runtime"] = encodedRuntime
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return store.UpdateJobPayload(ctx, jobID, string(encoded))
}
