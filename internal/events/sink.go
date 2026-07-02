// Package events defines the off-by-default, best-effort outbound event seam
// (#446): a typed, versioned JobEvent contract plus a Sink interface the
// workflow engine and daemon call from the terminal-transition path to fan a
// redacted event out to one configured transport (the pilot ships a webhook).
//
// Design invariants:
//   - OFF by default: a nil Sink is a no-op everywhere, so with no [events]
//     config the engine/daemon behave byte-identically (no goroutine, no emit).
//   - BEST-EFFORT: a slow/hung/erroring/full consumer must NEVER block or fail a
//     job. Emit is fire-and-forget with a bounded transport timeout and
//     drop-on-full, mirroring the EscalationNotifier contract.
//   - NO IMPORT CYCLE: this package must NOT import internal/workflow. The
//     redaction function is injected into NewEvent by the caller (the engine and
//     daemon pass workflow.RedactCommentText) so redaction happens at event
//     construction without an events->workflow dependency.
//
// #445 (the ask-gate) emits its job.needs_attention through this same seam.
package events

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// SchemaVersion is the contract version stamped on every emitted Event. Consumers
// pin to it; a breaking field change bumps it. Reserved (parsed-but-unused for
// the pilot) event types let consumers be forward-compatible without churn.
const SchemaVersion = 1

// EventType is the lifecycle/terminal enum carried on an Event. The pilot emits
// the terminal set plus needs_attention; the remaining values are reserved so
// the contract is stable as the graduate step adds them.
type EventType string

const (
	// EventJobFinished is emitted once when a job reaches a SUCCEEDED terminal
	// state (the engine's success/advance path).
	EventJobFinished EventType = "job.finished"
	// EventJobFailed is emitted once when a job reaches a FAILED terminal state.
	EventJobFailed EventType = "job.failed"
	// EventJobBlocked is emitted once when a job reaches a BLOCKED terminal state.
	EventJobBlocked EventType = "job.blocked"
	// EventJobNeedsAttention is emitted once when a job/tree pauses awaiting a
	// human (the escalate_human pause today; the #445 ask-gate next). detail
	// carries the redacted question.
	EventJobNeedsAttention EventType = "job.needs_attention"
	// EventJobDeferred is emitted when the daemon re-queues a job that just
	// emitted job.failed because the failure classified as a retryable
	// OPERATIONAL blocker (#532: runtime auth rejected, provider rate
	// limit/quota) — the daemon will automatically re-dispatch it after the
	// hold. Detail carries the blocker class, the attempt budget, and the
	// earliest retry-at. CONSUMER CONTRACT: a job.failed immediately followed by
	// a job.deferred for the same job id is NOT terminal — suppress terminal
	// handling (recovery, alerting, cleanup) for that job until a later
	// job.failed arrives WITHOUT a following job.deferred.
	EventJobDeferred EventType = "job.deferred"

	// EventCandidateAwaitingPromotion is emitted once when a SkillOpt template
	// candidate becomes PENDING (the post-import notify, #471): a new pending
	// agent_template_version is awaiting a human (or auto-promote) decision. JobID
	// is the pending version id, RootID the template id, Status "awaiting_promotion",
	// Detail a redacted score/samples/CI reason. Always emitted (when [events] is
	// configured) independent of the auto-promote policy.
	EventCandidateAwaitingPromotion EventType = "candidate.awaiting_promotion"
	// EventCandidateAutoPromoted is emitted once when the off-by-default
	// [skillopt].auto_promote policy auto-promotes a pending candidate to current
	// (#471), AFTER the existing PromoteAgentTemplateVersion write. JobID is the
	// promoted version id, RootID the template id, Status "auto_promoted", Detail a
	// redacted reason naming the guardrails that passed, so a human can review or
	// roll back even in full-auto.
	EventCandidateAutoPromoted EventType = "candidate.auto_promoted"
	// EventCandidateCanaryStarted is emitted once when the off-by-default
	// [skillopt].auto_promote_canary path promotes a pending candidate to the
	// `canary` state (#484) behind the live champion, AFTER the
	// CanaryPromoteAgentTemplateVersion write. JobID is the canary version id,
	// RootID the template id, Status "canary_started", Detail a redacted reason
	// naming the guardrails that passed and the sample fraction — so a human sees a
	// canary went live and can watch the regression window.
	EventCandidateCanaryStarted EventType = "candidate.canary_started"
	// EventCandidateRolledBack is emitted once when the #484 daemon regression
	// window AUTO-ROLLS-BACK a canary on a material regression vs the prior
	// champion: the champion stays the live current version and the canary is
	// rejected. JobID is the rolled-back canary version id, RootID the template id,
	// Status "rolled_back", Detail a redacted reason naming the score comparison, so
	// a human sees the auto-rollback happened.
	EventCandidateRolledBack EventType = "candidate.rolled_back"

	// Reserved for the graduate step (parsed/enumerated but NOT emitted by the
	// pilot). Listed so downstream consumers can switch over them forward-
	// compatibly without a schema bump when they start arriving.
	EventJobStarted            EventType = "job.started"
	EventDelegationEscalation  EventType = "delegation.escalation"
	EventDelegationFinalized   EventType = "delegation.finalized"
	EventOrchestrationFinished EventType = "orchestration.finished"
)

// Event is the stable, versioned, redacted JSON object emitted outbound. Every
// string field is redacted at construction (see NewEvent); ids are opaque, ts is
// RFC3339, status is the terminal/lifecycle enum string. It is intentionally
// small — a tight allowlist, not the AddJobEvent firehose — so the contract is
// easy to consume and stable.
type Event struct {
	// SchemaVersion is the contract version (currently SchemaVersion=1).
	SchemaVersion int `json:"schema_version"`
	// Type is the event_type enum (job.finished / job.failed / job.blocked /
	// job.needs_attention).
	Type EventType `json:"event_type"`
	// JobID is the opaque job id this event is about.
	JobID string `json:"job_id"`
	// RootID is the coordination tree's root id (payload.RootJobID, else the
	// job's own id) so a consumer can aggregate a run client-side. No synthetic
	// orchestration.finished is emitted in the pilot.
	RootID string `json:"root_id,omitempty"`
	// Repo is owner/repo only (never an absolute checkout path).
	Repo string `json:"repo,omitempty"`
	// Status is the terminal/lifecycle state string (succeeded/failed/blocked/
	// awaiting_human).
	Status string `json:"status,omitempty"`
	// Timestamp is the RFC3339 emit time.
	Timestamp string `json:"ts"`
	// Detail is a short redacted human-facing string (failure summary, the
	// escalation question, …). Never raw runtime output or secrets.
	Detail string `json:"detail,omitempty"`
}

// Sink is the injected, best-effort outbound seam the engine and daemon call
// from the terminal-transition path. Implementations MUST be non-blocking and
// MUST NOT return an error path that can fail a job: Emit is fire-and-forget.
// A nil Sink is a no-op (callers guard with EmitEvent / a nil check), so the
// default (no [events] config) path is byte-identical.
type Sink interface {
	// Emit dispatches an event best-effort. It must not block beyond a bounded
	// transport timeout and must never panic or fail the caller. ctx is honored
	// for cancellation only as a courtesy; a cancelled ctx never fails a job.
	Emit(ctx context.Context, event Event)
}

// RedactFunc redacts a single outbound string (secrets, tokens, …). The engine
// and daemon pass workflow.RedactCommentText so this package does not import
// internal/workflow (avoiding an import cycle).
type RedactFunc func(string) string

// NewEvent builds a redacted, versioned Event. Every free-text string field is
// run through redact (when non-nil); repo is reduced to owner/repo only so an
// absolute checkout path can never leak; ts defaults to time.Now() when zero.
// The constructor — not the transport — owns redaction, so a Sink just
// serializes and ships.
//
// detail is additionally scrubbed of absolute filesystem paths AFTER the
// injected secret redaction (scrubAbsolutePaths): the injected redact func
// (workflow.RedactCommentText) only strips secrets/tokens, but pre-flight
// causes routinely embed absolute checkout/worktree paths (CheckoutValidator
// errors, `git worktree add /root/.gitmoot/...`). Collapsing them to <path>
// here — the single chokepoint both the engine and daemon pass through —
// upholds the "no absolute paths/secrets/raw runtime output leave the box"
// acceptance criterion for every emit site at once (#446).
func NewEvent(eventType EventType, jobID, rootID, repo, status, detail string, ts time.Time, redact RedactFunc) Event {
	if ts.IsZero() {
		ts = time.Now()
	}
	return Event{
		SchemaVersion: SchemaVersion,
		Type:          eventType,
		JobID:         strings.TrimSpace(jobID),
		RootID:        strings.TrimSpace(rootID),
		Repo:          ownerRepoOnly(repo),
		Status:        strings.TrimSpace(status),
		Timestamp:     ts.UTC().Format(time.RFC3339),
		Detail:        scrubAbsolutePaths(redactString(strings.TrimSpace(detail), redact)),
	}
}

// EmitEvent is a nil-safe convenience: a nil sink is a no-op (the off-by-default
// guarantee), otherwise it forwards to sink.Emit. Callers in the engine/daemon
// use it so every emit site is uniformly nil-guarded.
func EmitEvent(ctx context.Context, sink Sink, event Event) {
	if sink == nil {
		return
	}
	sink.Emit(ctx, event)
}

// Flusher is the OPTIONAL drain-and-wait extension a Sink may implement when its
// Emit is asynchronous (the webhook sink hands events to a background goroutine).
// Flush blocks — bounded — until the already-enqueued events are delivered, so a
// SHORT-LIVED caller (a CLI command) does not exit and destroy them before the
// goroutine runs. The long-lived engine/daemon never need it; only per-invocation
// sinks do. A synchronous Sink need not implement it.
type Flusher interface {
	Flush(ctx context.Context)
}

// FlushSink drains a sink that implements Flusher and is a no-op for a nil sink or
// a synchronous one (so callers can defer it unconditionally over a sink that may
// be nil when [events] is OFF). It is the seam a short-lived CLI command uses to
// guarantee a candidate.* webhook POST lands before the process exits. The daemon,
// which shares one long-lived cached sink for the whole process, must NOT call it
// per-invocation.
func FlushSink(ctx context.Context, sink Sink) {
	if sink == nil {
		return
	}
	if f, ok := sink.(Flusher); ok {
		f.Flush(ctx)
	}
}

func redactString(value string, redact RedactFunc) string {
	if redact == nil {
		return value
	}
	return redact(value)
}

// absolutePathPattern matches an absolute Unix filesystem path: a `/` that is
// not preceded by another path/URL character (so the `//` of `https://host` is
// left intact — the leading `https:` is a non-`/` char before the first slash,
// and the second slash is excluded by requiring a name segment after the
// matched slash) followed by one or more `name/` segments and a trailing name.
// It collapses host home layout (`/root/.gitmoot/...`), usernames, and worktree
// paths embedded in failure detail to a single `<path>` placeholder. It runs
// AFTER secret redaction so a `[REDACTED]` token already substituted for a
// secret is never re-scanned as a path. Path-like fragments WITHOUT a separator
// after the leading slash (a bare `/`) are left untouched.
var absolutePathPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_./:-])(/[^\s/<>]+(?:/[^\s/<>]+)+/?)`)

// scrubAbsolutePaths collapses absolute filesystem paths in a redacted detail
// string to `<path>`, so host-layout/checkout/worktree paths never leave the
// box. It preserves the single non-path leading character the pattern captures
// (whitespace/punctuation before the path) so surrounding text stays readable.
func scrubAbsolutePaths(value string) string {
	return absolutePathPattern.ReplaceAllString(value, "${1}<path>")
}

// ownerRepoOnly trims a repo reference to its trailing owner/repo, dropping any
// host or absolute-path prefix so an absolute checkout path never leaks. It
// tolerates "owner/repo", "github.com/owner/repo", and a bare path; an empty or
// path-only value collapses to "".
func ownerRepoOnly(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return ""
	}
	// An absolute filesystem path is not a repo reference; never emit it.
	if strings.HasPrefix(repo, "/") {
		return ""
	}
	parts := strings.Split(strings.Trim(repo, "/"), "/")
	if len(parts) < 2 {
		return repo
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}
