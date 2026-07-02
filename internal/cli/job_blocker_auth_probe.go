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

// authProbeAllowsRedispatch reports whether a queued job whose operational-blocker
// hold has ALREADY elapsed may be re-dispatched now. It only gates runtime_auth
// deferrals (every other class defers on a time-based reset the hold already
// encodes); everything else — no probe wired, a non-auth class, an unparseable
// payload — returns true so the coarse cadence alone governs (slice A behavior).
func authProbeAllowsRedispatch(ctx context.Context, worker jobWorker, job db.Job, now time.Time) bool {
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
	switch worker.AuthProbe(ctx, job, payload) {
	case authProbeValid:
		return true
	case authProbeInvalid:
		// Credential still bad: re-arm the coarse hold so the daemon re-probes next
		// cadence instead of re-dispatching into a broken token — and do NOT increment
		// BlockerAttempts, so the outage does not eat the retry budget.
		worker.extendAuthBlockerHold(ctx, job, payload, now)
		return false
	default:
		// Unknown/transient: fall back to the coarse cadence. Releasing now (bounded by
		// the 3-attempt budget) is safer than stranding a job behind an inconclusive
		// probe forever.
		return true
	}
}

// extendAuthBlockerHold pushes a runtime_auth deferral's earliest-retry-at forward
// by the coarse cadence, leaving BlockerAttempts untouched, so a failed probe
// re-holds the job for another cadence without spending a retry. Best-effort: a
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
