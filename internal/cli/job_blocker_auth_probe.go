package cli

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// Issue #532 slice B: gate re-dispatch of a runtime_auth deferral on a
// doctor-style LIVE credential probe (the ClaudeClassifyProbe SET-vs-VALID
// pattern from #486/#487) instead of blindly re-dispatching on the coarse 5m
// cadence. A runtime_auth blocker clears only when a human rotates/re-logs the
// token — an event with no machine-readable ETA — so re-dispatching purely on a
// timer either wastes a retry attempt into a still-broken token or holds a
// now-fixed token longer than necessary. The probe closes that gap: the coarse
// hold (authBlockerRetryDelay) governs WHEN to probe, and the probe governs
// whether to actually re-dispatch.
//
// Interaction with the 3-attempt budget (maxOperationalBlockerRetries): a probe
// FAILURE (credential still Invalid) extends the hold WITHOUT burning an attempt,
// so a long human outage never exhausts the budget on probes alone; an attempt is
// spent only when the job is actually re-dispatched and the delivery fails again.
// A probe that cannot run (Unknown — a non-claude runtime with no wired probe, or
// a transient network blip) falls back to the coarse cadence so a broken probe can
// never permanently strand a job.

// authProbeVerdict is the tri-state result of a live credential probe, mirroring
// runtime.ClaudeTokenStatus: Valid (re-dispatch), Invalid (extend the hold), or
// Unknown (fall back to the coarse cadence).
type authProbeVerdict int

const (
	authProbeUnknown authProbeVerdict = iota
	authProbeValid
	authProbeInvalid
)

// authProbeTimeout bounds a single live probe run so it can never block a
// dispatch tick indefinitely. runtime.ClaudeLiveCheck applies its own internal
// bound too; this is the belt-and-braces ceiling honored via context.
const authProbeTimeout = 25 * time.Second

// authProbeCache dedupes the live credential probe WITHIN a single
// listPendingQueuedJobs pass. The production probe (defaultAuthProbe) reads the
// AMBIENT daemon token, so its verdict is identical for every auth-held job that
// resolves to the same effective runtime; caching by that runtime key collapses N
// auth-held jobs to ONE live `claude -p` subprocess per pass instead of N. A nil
// cache disables dedup (each call probes) so direct/legacy callers are unaffected.
type authProbeCache map[string]authProbeVerdict

// authProbeAllowsRedispatch reports whether a queued job whose operational-blocker
// hold has ALREADY elapsed may be re-dispatched now. It only gates runtime_auth
// deferrals (every other class defers on a time-based reset the hold already
// encodes); everything else — no probe wired, a non-auth class, an unparseable
// payload — returns true so the coarse cadence alone governs (slice A behavior).
//
// The verdict is deduped across the pass via `cache` (see authProbeCache), and a
// probe-cadence marker is written on EVERY verdict — not just Invalid — so a job
// that probed this cadence but was NOT actually dispatched (its runtime session is
// busy, its checkout key is contended, or the host admission budget is full) is not
// re-probed on the very next dispatch pass. Without the marker a stuck-but-valid
// auth-held job would spawn a fresh live probe on every worker reap.
func authProbeAllowsRedispatch(ctx context.Context, worker jobWorker, job db.Job, now time.Time, cache authProbeCache) bool {
	if worker.AuthProbe == nil {
		return true
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		return true
	}
	if payload.BlockerClass != string(blockerClassRuntimeAuth) {
		return true
	}
	verdict := worker.probeAuthVerdict(ctx, job, payload, cache)
	// Mark the probe cadence on every verdict: re-arm the coarse hold so this job is
	// not re-probed until authBlockerRetryDelay elapses again. Done AFTER the verdict
	// so the return value below still governs THIS pass (the queuedJobBlockerHeld gate
	// already cleared for the in-memory job, so a Valid job is still dispatchable now).
	// BlockerAttempts is left untouched, so a long outage never eats the retry budget.
	worker.extendAuthBlockerHold(ctx, job, payload, now)
	switch verdict {
	case authProbeValid:
		return true
	case authProbeInvalid:
		// Credential still bad: the re-armed hold means the daemon re-probes next
		// cadence instead of re-dispatching into a broken token.
		return false
	default:
		// Unknown/transient: fall back to the coarse cadence. Releasing now (bounded by
		// the 3-attempt budget) is safer than stranding a job behind an inconclusive
		// probe forever.
		return true
	}
}

// probeAuthVerdict returns the live probe verdict for a runtime_auth-held job,
// consulting/populating the per-pass dedup cache when one is supplied. The cache
// key is the effective runtime the re-dispatch would use (honoring a per-job
// runtime override): the ambient-token probe result is identical for all jobs of
// that runtime, so they share one subprocess. A nil cache probes directly.
func (w jobWorker) probeAuthVerdict(ctx context.Context, job db.Job, payload workflow.JobPayload, cache authProbeCache) authProbeVerdict {
	if cache == nil {
		return w.AuthProbe(ctx, job, payload)
	}
	key := w.authProbeDedupKey(ctx, job, payload)
	if verdict, ok := cache[key]; ok {
		return verdict
	}
	verdict := w.AuthProbe(ctx, job, payload)
	cache[key] = verdict
	return verdict
}

// authProbeDedupKey identifies the credential domain a probe result applies to:
// the effective runtime (agent runtime + any per-job override). All auth-held jobs
// that share this key share one ambient-token probe. If the agent can't be read,
// key by agent name so a lookup failure never merges distinct agents' verdicts.
func (w jobWorker) authProbeDedupKey(ctx context.Context, job db.Job, payload workflow.JobPayload) string {
	record, err := w.Store.GetAgent(ctx, job.Agent)
	if err != nil {
		return "agent:" + job.Agent
	}
	agent := applyJobRuntimeOverride(runtimeAgent(record), payload)
	return "runtime:" + strings.TrimSpace(agent.Runtime)
}

// extendAuthBlockerHold pushes a runtime_auth deferral's earliest-retry-at forward
// by the coarse cadence, leaving BlockerAttempts untouched. It is the probe-cadence
// marker (#532 slice B): written after every probe verdict so a job is not re-probed
// until the next cadence, whether the probe held it (Invalid) or released it
// (Valid/Unknown) but it could not actually be dispatched this pass. Best-effort: a
// write error just means the next tick re-probes, which is safe.
func (w jobWorker) extendAuthBlockerHold(ctx context.Context, job db.Job, payload workflow.JobPayload, now time.Time) {
	payload.BlockerRetryAt = now.Add(authBlockerRetryDelay).Format(time.RFC3339Nano)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = w.Store.UpdateJobPayload(ctx, job.ID, string(encoded))
}

// defaultAuthProbe is the daemon's live credential probe. It probes the EFFECTIVE
// runtime the re-dispatch would use (the agent's runtime, honoring a per-job
// runtime override): claude gets a bounded live runtime.ClaudeLiveCheck classified
// SET-vs-VALID; every other runtime has no wired live probe, so it returns Unknown
// and the coarse cadence stays in charge.
func (w jobWorker) defaultAuthProbe(ctx context.Context, job db.Job, payload workflow.JobPayload) authProbeVerdict {
	record, err := w.Store.GetAgent(ctx, job.Agent)
	if err != nil {
		return authProbeUnknown
	}
	agent := applyJobRuntimeOverride(runtimeAgent(record), payload)
	if strings.TrimSpace(agent.Runtime) != runtime.ClaudeRuntime {
		return authProbeUnknown
	}
	probeCtx, cancel := context.WithTimeout(ctx, authProbeTimeout)
	defer cancel()
	return classifyClaudeAuthProbe(runtime.ClaudeLiveCheck(probeCtx, nil, ""))
}

// classifyClaudeAuthProbe maps a runtime.ClaudeLiveCheck result to an
// authProbeVerdict via the shared ClaudeClassifyProbe tri-state, so an INVALID
// verdict (and only an invalid one) extends the hold, while a missing binary /
// network blip stays Unknown and never mis-holds a job.
func classifyClaudeAuthProbe(err error) authProbeVerdict {
	switch runtime.ClaudeClassifyProbe(err) {
	case runtime.ClaudeTokenValid:
		return authProbeValid
	case runtime.ClaudeTokenInvalid:
		return authProbeInvalid
	default:
		return authProbeUnknown
	}
}
