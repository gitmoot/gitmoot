package cli

import (
	"errors"
	"fmt"
	"strings"

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

// jobRuntimeOverrideEventMessage renders the runtime_override job event that
// exposes the effective runtime (and the session lock it ran under) in job
// history.
func jobRuntimeOverrideEventMessage(defaultRuntime string, effective runtime.Agent, lockKey string) string {
	message := fmt.Sprintf("job runs on runtime %s (agent default %s)", effective.Runtime, defaultRuntime)
	if strings.TrimSpace(lockKey) != "" {
		message += "; session lock " + lockKey
	}
	return message
}
