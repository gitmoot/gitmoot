package cli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// authWordRe matches "auth" only as a whole word, so an authoritative auth
// signal ("auth token invalid") is caught while an unrelated substring such as
// "author" in "invalid author email" is not.
var authWordRe = regexp.MustCompile(`\bauth\b`)

// stuckReasonEventKinds are the job_event kinds that carry an authoritative
// reason a queued or blocked job is not progressing (issue #552). Surfacing why a
// stuck job waits keys on the LATEST event of one of these kinds; benign
// lifecycle events (queued, route_selected, delegation_continuation_enqueued) are
// deliberately excluded so a healthy queued job stays silent and its `job list`
// row keeps its existing shape.
var stuckReasonEventKinds = []string{
	blockerDeferredEventKind,
	"runtime_lock_wait",
	"advance_blocked",
	"advance_awaiting_human",
	"permission_blocked",
	"deterministic_checkers_failed",
	"advance_retry",
	"retry_queued",
	"repair_retry",
	"session_refresh_retry",
	"delegation_retry",
	"delegation_escalation_retry",
	"delegation_loop_detected",
	"delegation_width_exceeded",
	"delegation_depth_exceeded",
	"delegation_walltime_exceeded",
	"delegation_cost_exceeded",
	"delegation_cost_usd_exceeded",
	"delegation_preflight_failed",
}

// stuckReason is a concise, human-readable explanation of why a queued/blocked
// job is not progressing, derived from existing signals. The zero value means
// "not stuck / no derivable reason" — callers must render nothing so healthy
// output is byte-stable.
type stuckReason struct {
	Reason      string // e.g. "waiting on runtime session lock ...", "blocked: awaiting human", "auth failing: ..."
	NextRetryAt string // an RFC3339 lease expiry when one applies (a runtime-session lock), else ""
	// SuggestedAction is a concrete human-facing remedy for a deferral that usually
	// needs manual intervention (a dirty/wrong-head checkout, #532 slice C). Empty
	// for self-healing holds; callers render it only when non-empty.
	SuggestedAction string
}

func (r stuckReason) empty() bool { return strings.TrimSpace(r.Reason) == "" }

// deriveStuckReason returns why a queued/blocked job is not progressing, from the
// most authoritative existing signal: the latest reason-bearing job_event and,
// for a runtime-session lock wait, the owning lock's lease. It is a pure function
// over already-queried state so it is trivially testable. Healthy (non-queued/
// blocked) jobs and jobs with no derivable signal return the zero value.
func deriveStuckReason(job db.Job, reasonEvent db.JobEvent, hasReasonEvent bool, locks []db.ResourceLock) stuckReason {
	state := strings.TrimSpace(job.State)
	queued := state == string(workflow.JobQueued)
	blocked := state == string(workflow.JobBlocked)
	if !queued && !blocked {
		return stuckReason{}
	}
	if !hasReasonEvent {
		// A blocked job with no reason-bearing event is still stuck by definition;
		// surface the bare state so it is never silently unexplained. A plain queued
		// job with only lifecycle events is not yet "stuck" — leave it silent.
		if blocked {
			return stuckReason{Reason: "blocked"}
		}
		return stuckReason{}
	}
	msg := firstLineTrimmed(reasonEvent.Message)
	switch reasonEvent.Kind {
	case blockerDeferredEventKind:
		// Operational-blocker deferral (#532): the job is held awaiting an
		// external condition (auth fixed, quota reset), then auto re-dispatched.
		// The event message already names the class + attempt budget; the payload
		// carries the authoritative earliest-retry-at the queue gate honors.
		next := ""
		action := ""
		if payload, err := daemonJobPayload(job); err == nil {
			next = strings.TrimSpace(payload.BlockerRetryAt)
			action = strings.TrimSpace(payload.BlockerSuggestedAction)
		} else if job.Payload == "" {
			next = strings.TrimSpace(job.BlockerRetryAt)
			action = strings.TrimSpace(job.BlockerSuggestedAction)
		}
		return stuckReason{Reason: withDetail("blocked-operational", msg), NextRetryAt: next, SuggestedAction: action}
	case "runtime_lock_wait":
		reason := "waiting on runtime session lock"
		next := ""
		if key := extractRuntimeLockKey(reasonEvent.Message); key != "" {
			reason += " " + key
			if lock, ok := findResourceLock(locks, key); ok {
				if owner := strings.TrimSpace(lock.OwnerJobID); owner != "" && owner != job.ID {
					reason += fmt.Sprintf(" (held by job %s)", owner)
				}
				next = strings.TrimSpace(lock.ExpiresAt)
			}
		}
		return stuckReason{Reason: reason, NextRetryAt: next}
	case "advance_awaiting_human":
		return stuckReason{Reason: withDetail("blocked: awaiting human", msg)}
	case "permission_blocked":
		return stuckReason{Reason: withDetail("permission blocked", msg)}
	case "advance_retry", "retry_queued", "repair_retry", "session_refresh_retry",
		"delegation_retry", "delegation_escalation_retry":
		return stuckReason{Reason: withDetail("retrying", msg)}
	case "delegation_preflight_failed":
		return stuckReason{Reason: withDetail("delegation preflight failed", msg)}
	default:
		// advance_blocked, delegation_*_exceeded, delegation_loop_detected,
		// deterministic_checkers_failed, ... — classify auth/quota so the operator
		// sees an actionable label instead of a raw error line.
		label := "blocked"
		if queued {
			label = "waiting"
		}
		if cls := classifyAuthQuota(msg); cls != "" {
			label = cls
		}
		return stuckReason{Reason: withDetail(label, msg)}
	}
}

// classifyAuthQuota labels a stuck-reason message as an auth or throttling
// problem when its text matches the well-known runtime/gh signatures (issue #552
// points 3 and 4). It returns "" when the message is not auth/quota related, so
// the caller keeps its generic label.
func classifyAuthQuota(msg string) string {
	l := strings.ToLower(msg)
	// A bare 401/429 digit run is ambiguous (a PR number, a token count), so only
	// treat it as an HTTP status when the message actually frames it as one.
	httpCtx := strings.Contains(l, "status") || strings.Contains(l, "http")
	switch {
	case strings.Contains(l, "usage limit"), strings.Contains(l, "rate limit"),
		strings.Contains(l, "quota"), strings.Contains(l, "limit resets"),
		httpCtx && strings.Contains(l, "429"):
		return "throttled"
	case strings.Contains(l, "authentication"), strings.Contains(l, "unauthorized"),
		httpCtx && strings.Contains(l, "401"),
		authWordRe.MatchString(l) && strings.Contains(l, "invalid"):
		return "auth failing"
	}
	return ""
}

// extractRuntimeLockKey pulls the runtime:<rt>:<ref> resource key out of a
// runtime_lock_wait event message ("runtime session runtime:codex:foo is busy;
// ..."). It returns "" when the message does not carry a recognizable key.
func extractRuntimeLockKey(message string) string {
	const marker = "runtime session "
	idx := strings.Index(message, marker)
	if idx < 0 {
		return ""
	}
	rest := message[idx+len(marker):]
	if end := strings.Index(rest, " "); end >= 0 {
		rest = rest[:end]
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "runtime:") {
		return ""
	}
	return rest
}

// firstLineTrimmed returns the first non-empty line of value, trimmed. Unlike the
// display-oriented firstLine helper it returns "" (not "-") for an empty value so
// withDetail can drop an absent detail cleanly.
func firstLineTrimmed(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func findResourceLock(locks []db.ResourceLock, key string) (db.ResourceLock, bool) {
	for _, lock := range locks {
		if lock.ResourceKey == key {
			return lock, true
		}
	}
	return db.ResourceLock{}, false
}

// withDetail appends a concise detail to a label, guarding against an empty or
// duplicate detail so the surface stays tidy.
func withDetail(label, detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return label
	}
	return label + ": " + detail
}
