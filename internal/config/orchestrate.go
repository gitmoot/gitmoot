package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// CockpitMode values for [orchestrate].cockpit_mode. "auto" lets the daemon
	// decide per job (gated on herdr reachability), "on" forces panes when herdr
	// is reachable, and "off" disables cockpit panes entirely regardless of the
	// per-job --cockpit flag.
	CockpitModeAuto = "auto"
	CockpitModeOn   = "on"
	CockpitModeOff  = "off"

	// CockpitPaneKey values for [orchestrate].cockpit_pane_key. "job" gives each
	// job its own pane (P0); "seat" reuses one pane per opaque seat key (P2).
	CockpitPaneKeyJob  = "job"
	CockpitPaneKeySeat = "seat"
)

// OrchestratePolicy is the host-level cockpit policy read from the
// [orchestrate] section of the gitmoot config. The daemon combines it with the
// per-job JobPayload.Cockpit flag to decide whether to wrap a job's delivery in
// a herdr pane (see issue #357). It never affects engine/DAG behavior.
type OrchestratePolicy struct {
	CockpitMode     string
	CockpitSession  string
	CockpitMaxPanes int
	CockpitPaneKey  string
	// InlineArtifactBodies opts the coordinator continuation prompt into inlining
	// each finished child's artifact_body as a fenced block (see issue #368). It is
	// off by default because inlined briefs can be large.
	InlineArtifactBodies bool
	// InlineArtifactMaxBytes is the per-body byte cap applied when
	// InlineArtifactBodies is set; 0 means the engine's built-in default.
	InlineArtifactMaxBytes int
	// InjectUpstreamDepContext opts a ready dependent delegation leg into running
	// with its succeeded direct deps' results injected into its prompt as a
	// byte-budgeted "Upstream dependency results" block (deps[] as real dataflow,
	// see issue #419). It is off by default — flag-off the enqueued prompt is
	// byte-identical — and reuses the same artifact-body byte budget as
	// InlineArtifactBodies (no new knob). The daemon wires this into
	// Engine.InjectUpstreamDepContext at startup.
	InjectUpstreamDepContext bool
	// MaxDelegationTokenBudget is the cumulative per-root token budget (input +
	// output, summed across a delegation tree) that bounds a tree by cost in
	// addition to depth/width/total-jobs/wall-clock (#338 Part B). 0 (the default)
	// means unlimited/off, so default behavior is unchanged. The daemon wires this
	// into Engine.MaxDelegationTokenBudget at startup.
	MaxDelegationTokenBudget int
	// MaxDelegationCostUSD is the cumulative per-root dollar-cost budget that bounds
	// a delegation tree by its measured spend (token usage × a per-model price
	// table), layered on top of the token budget (#380). 0 (the default) means
	// unlimited/off, so default behavior is unchanged. The daemon wires this into
	// Engine.MaxDelegationCostUSD at startup.
	MaxDelegationCostUSD float64
	// EscalationHandle is the GitHub login the escalate_human notifier @-tags when
	// a tree pauses awaiting a human (#340). Empty (the default) falls back to the
	// PR author, then the repo owner, so a notification always names someone.
	EscalationHandle string
	// EscalationTTL is how long a tree may sit paused awaiting a human before the
	// daemon's background scan auto-finalizes it (#340), as a Go duration string.
	// Empty (the default) uses DefaultEscalationTTL (24h); the daemon parses it.
	EscalationTTL string
	// MaxDelegationNonProgressStreak is the per-root threshold for the result-aware
	// non-progress loop detector (#339): how many consecutive continuation
	// generations a delegation tree may produce with no new durable side effect
	// before the loop ladder trips. 0 (the default) means use the engine's built-in
	// default (2), so default behavior is unchanged. The daemon wires this into
	// Engine.MaxDelegationNonProgressStreak at startup.
	MaxDelegationNonProgressStreak int
	// MaxVerifyReplanAttempts is the per-root cap on the engine-level verify→replan
	// corrective loop (#439): how many bounded replan continuations the engine issues
	// on a FAILED verify verdict before routing to the #305 graceful finalize
	// continuation. 0 (the default) means use the engine's built-in default (2), so
	// default behavior is unchanged. The daemon wires this into
	// Engine.MaxVerifyReplanAttempts at startup.
	MaxVerifyReplanAttempts int
}

// DefaultEscalationTTL is the fallback time a paused-for-human tree may sit
// before the daemon auto-finalizes it gracefully (#340).
const DefaultEscalationTTL = "24h"

func DefaultOrchestratePolicy() OrchestratePolicy {
	return OrchestratePolicy{
		CockpitMode:                    CockpitModeAuto,
		CockpitSession:                 "",
		CockpitMaxPanes:                4,
		CockpitPaneKey:                 CockpitPaneKeyJob,
		InlineArtifactBodies:           false,
		InlineArtifactMaxBytes:         0,
		InjectUpstreamDepContext:       false,
		MaxDelegationTokenBudget:       0,
		MaxDelegationCostUSD:           0,
		EscalationHandle:               "",
		EscalationTTL:                  "",
		MaxDelegationNonProgressStreak: 0,
		MaxVerifyReplanAttempts:        0,
	}
}

func LoadOrchestratePolicy(paths Paths) (OrchestratePolicy, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return OrchestratePolicy{}, err
	}
	policy := DefaultOrchestratePolicy()
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			current = strings.TrimSpace(section) == "orchestrate"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if err := applyOrchestratePolicyField(&policy, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return OrchestratePolicy{}, fmt.Errorf("parse [orchestrate].%s: %w", strings.TrimSpace(key), err)
		}
	}
	if err := validateOrchestratePolicy(policy); err != nil {
		return OrchestratePolicy{}, err
	}
	return policy, nil
}

func applyOrchestratePolicyField(policy *OrchestratePolicy, key string, value string) error {
	switch key {
	case "cockpit_mode":
		parsed, err := parseConfigString(value)
		policy.CockpitMode = strings.TrimSpace(parsed)
		return err
	case "cockpit_session":
		parsed, err := parseConfigString(value)
		policy.CockpitSession = strings.TrimSpace(parsed)
		return err
	case "cockpit_max_panes":
		parsed, err := strconv.Atoi(value)
		policy.CockpitMaxPanes = parsed
		return err
	case "cockpit_pane_key":
		parsed, err := parseConfigString(value)
		policy.CockpitPaneKey = strings.TrimSpace(parsed)
		return err
	case "inline_artifact_bodies":
		parsed, err := strconv.ParseBool(value)
		policy.InlineArtifactBodies = parsed
		return err
	case "inline_artifact_max_bytes":
		parsed, err := strconv.Atoi(value)
		policy.InlineArtifactMaxBytes = parsed
		return err
	case "inject_upstream_dep_context":
		parsed, err := strconv.ParseBool(value)
		policy.InjectUpstreamDepContext = parsed
		return err
	case "max_delegation_token_budget":
		parsed, err := strconv.Atoi(value)
		policy.MaxDelegationTokenBudget = parsed
		return err
	case "max_delegation_cost_usd":
		parsed, err := strconv.ParseFloat(value, 64)
		policy.MaxDelegationCostUSD = parsed
		return err
	case "escalation_handle":
		parsed, err := parseConfigString(value)
		policy.EscalationHandle = strings.TrimPrefix(strings.TrimSpace(parsed), "@")
		return err
	case "escalation_ttl":
		parsed, err := parseConfigString(value)
		policy.EscalationTTL = strings.TrimSpace(parsed)
		return err
	case "max_delegation_non_progress_streak":
		parsed, err := strconv.Atoi(value)
		policy.MaxDelegationNonProgressStreak = parsed
		return err
	case "max_verify_replan_attempts":
		parsed, err := strconv.Atoi(value)
		policy.MaxVerifyReplanAttempts = parsed
		return err
	default:
		return nil
	}
}

func validateOrchestratePolicy(policy OrchestratePolicy) error {
	switch policy.CockpitMode {
	case CockpitModeAuto, CockpitModeOn, CockpitModeOff:
	default:
		return fmt.Errorf("unsupported orchestrate.cockpit_mode %q; use on, off, or auto", policy.CockpitMode)
	}
	if policy.CockpitMaxPanes < 1 {
		return fmt.Errorf("orchestrate.cockpit_max_panes must be positive")
	}
	switch policy.CockpitPaneKey {
	case CockpitPaneKeyJob, CockpitPaneKeySeat:
	default:
		return fmt.Errorf("unsupported orchestrate.cockpit_pane_key %q; use job or seat", policy.CockpitPaneKey)
	}
	if policy.MaxDelegationTokenBudget < 0 {
		return fmt.Errorf("orchestrate.max_delegation_token_budget must be 0 (unlimited) or positive")
	}
	if policy.MaxDelegationCostUSD < 0 {
		return fmt.Errorf("orchestrate.max_delegation_cost_usd must be 0 (unlimited) or positive")
	}
	if ttl := strings.TrimSpace(policy.EscalationTTL); ttl != "" {
		parsed, err := time.ParseDuration(ttl)
		if err != nil {
			return fmt.Errorf("orchestrate.escalation_ttl %q is invalid: %w", ttl, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("orchestrate.escalation_ttl must be positive")
		}
	}
	if policy.MaxDelegationNonProgressStreak < 0 {
		return fmt.Errorf("orchestrate.max_delegation_non_progress_streak must be 0 (engine default) or positive")
	}
	if policy.MaxVerifyReplanAttempts < 0 {
		return fmt.Errorf("orchestrate.max_verify_replan_attempts must be 0 (engine default) or positive")
	}
	return nil
}

// EventsPolicy is the host-level outbound-event-stream policy read from the
// [events] section of the gitmoot config (#446). It is a distinct concern from
// [orchestrate] (cockpit/delegation budgets): when WebhookURL is empty (the
// default) NO sink is constructed and behavior is byte-identical (off by
// default). The daemon uses it to build the best-effort webhook Sink wired into
// the workflow engine's terminal-transition path.
type EventsPolicy struct {
	// WebhookURL is the single https/http endpoint each terminal/needs_attention
	// event is POSTed to as application/json. Empty (the default) means the event
	// stream is OFF: no sink, no goroutine, no emits.
	WebhookURL string
	// Timeout bounds a single outbound POST so a hung consumer never stalls the
	// drain goroutine, as a Go duration string. Empty (the default) uses
	// DefaultEventsTimeout (2s); the daemon parses it.
	Timeout string
	// SocketPath is RESERVED for the graduate Unix-socket transport (#446
	// open question). It is parsed and validated but UNUSED by the pilot
	// (webhook-only); listing it keeps the config surface forward-compatible.
	SocketPath string
}

// DefaultEventsTimeout is the fallback per-POST timeout when [events].timeout is
// unset. It matches events.DefaultWebhookTimeout (kept as a string here so the
// config package does not import internal/events).
const DefaultEventsTimeout = "2s"

func DefaultEventsPolicy() EventsPolicy {
	return EventsPolicy{
		WebhookURL: "",
		Timeout:    "",
		SocketPath: "",
	}
}

// Enabled reports whether the event stream is configured on. With no [events]
// config (the default) it is OFF and no sink should be constructed.
func (p EventsPolicy) Enabled() bool {
	return strings.TrimSpace(p.WebhookURL) != ""
}

// ResolvedTimeout returns the parsed per-POST timeout, falling back to
// DefaultEventsTimeout when unset. validateEventsPolicy guarantees a non-empty
// value parses, so this never errors for a validated policy.
func (p EventsPolicy) ResolvedTimeout() time.Duration {
	raw := strings.TrimSpace(p.Timeout)
	if raw == "" {
		raw = DefaultEventsTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		d, _ = time.ParseDuration(DefaultEventsTimeout)
	}
	return d
}

func LoadEventsPolicy(paths Paths) (EventsPolicy, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return EventsPolicy{}, err
	}
	policy := DefaultEventsPolicy()
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			current = strings.TrimSpace(section) == "events"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if err := applyEventsPolicyField(&policy, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return EventsPolicy{}, fmt.Errorf("parse [events].%s: %w", strings.TrimSpace(key), err)
		}
	}
	if err := validateEventsPolicy(policy); err != nil {
		return EventsPolicy{}, err
	}
	return policy, nil
}

func applyEventsPolicyField(policy *EventsPolicy, key string, value string) error {
	switch key {
	case "webhook_url":
		parsed, err := parseConfigString(value)
		policy.WebhookURL = strings.TrimSpace(parsed)
		return err
	case "timeout":
		parsed, err := parseConfigString(value)
		policy.Timeout = strings.TrimSpace(parsed)
		return err
	case "socket_path":
		parsed, err := parseConfigString(value)
		policy.SocketPath = strings.TrimSpace(parsed)
		return err
	default:
		return nil
	}
}

func validateEventsPolicy(policy EventsPolicy) error {
	if url := strings.TrimSpace(policy.WebhookURL); url != "" {
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return fmt.Errorf("events.webhook_url %q must be an http:// or https:// URL", url)
		}
	}
	if raw := strings.TrimSpace(policy.Timeout); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("events.timeout %q is invalid: %w", raw, err)
		}
		if parsed <= 0 {
			return fmt.Errorf("events.timeout must be positive")
		}
	}
	return nil
}

// SkillOptPolicy is the host-level template-learning policy read from the
// [skillopt] section of the gitmoot config (#465, Mode A). It gates the
// off-by-default automatic trace-harvester: when AutoTraceEnabled is false (the
// default) NO OutcomeHarvester is constructed and behavior is byte-identical
// (no synthetic feedback rows are ever written). It mirrors EventsPolicy's
// off-by-default admission knob; the daemon uses it to decide whether to wire a
// best-effort harvester into the workflow engine. Promotion stays 100% manual
// regardless of this knob — the harvester only writes eval/feedback rows.
type SkillOptPolicy struct {
	// AutoTraceEnabled opts the daemon into Mode A automatic trace-harvested
	// feedback (#465): when true, an implement job's verifiable terminal outcome
	// (merge merged/blocked, review decision, revert) is projected into a synthetic
	// FeedbackEvent in a dedicated auto-trace eval_run. Empty/false (the default)
	// means OFF: no harvester is constructed and the daemon writes nothing extra.
	AutoTraceEnabled bool

	// CrossFamilyReviewEnabled opts the daemon into the Mode A cross-family
	// review-agent SOFT signal (#469): when true (AND AutoTraceEnabled is also true),
	// a merged implement job additionally runs a read-only CROSS-FAMILY review leg
	// whose subjective-quality + scope-fidelity rubric is projected into a SECOND,
	// judge-tagged, down-weighted FeedbackEvent in the SAME auto-trace run. Empty/
	// false (the default) means OFF: NO review leg runs and NO review row is written
	// — byte-identical to the verifiable-floor-only behavior. It additionally
	// requires AutoTraceEnabled (a review row only makes sense inside the auto-trace
	// run); ReviewEnabled() encodes that dependency. A live cross-family LLM review
	// per merge is a real cost surface, so it must be opt-in.
	CrossFamilyReviewEnabled bool

	// AutoPromote opts into the off-by-default auto-promote policy (#471): when
	// false (the default) a newly-pending candidate is ONLY notified
	// (candidate.awaiting_promotion) and NEVER auto-promoted — promotion stays
	// 100% manual, byte-identical to today. When true, a candidate is auto-promoted
	// (the existing PromoteAgentTemplateVersion + a candidate.auto_promoted event)
	// ONLY when every configured guardrail below holds; ANY uncertainty fails safe
	// to notify-don't-promote.
	AutoPromote bool

	// AutoPromoteMinSamples is the minimum count of feedback_events in the
	// candidate's eval_run required to auto-promote. nil (unset) is a HARD
	// "do not promote" — flipping AutoPromote on without a threshold never promotes.
	AutoPromoteMinSamples *int
	// AutoPromoteMinScore is the minimum candidate Summary.Score required to
	// auto-promote. nil (unset) is a HARD "do not promote"; a nil candidate score
	// also fails safe.
	AutoPromoteMinScore *float64
	// AutoPromoteRequireExternalCI requires at least one feedback event in the
	// candidate's eval_run to be a real-CI positive (a merge that passed genuine
	// external CI, not the no-CI near-neutral band). When true and no such event is
	// present, the candidate is not promoted.
	AutoPromoteRequireExternalCI bool

	// AutoPromoteRequireMeasuredJudge is PARSED but DEFERRED (#471 / gated on #344):
	// there is no judge<->human calibration source yet, so when true it FAILS SAFE
	// to notify-don't-promote. Documented so a user can set it forward-compatibly.
	AutoPromoteRequireMeasuredJudge bool
	// AutoPromoteCanary is PARSED but DEFERRED (#471 canary follow-on): the sampled
	// traffic + regression-window infrastructure does not exist yet, so when true it
	// FAILS SAFE to notify-don't-promote. Documented as deferred.
	AutoPromoteCanary bool
}

func DefaultSkillOptPolicy() SkillOptPolicy {
	return SkillOptPolicy{
		AutoTraceEnabled:                false,
		CrossFamilyReviewEnabled:        false,
		AutoPromote:                     false,
		AutoPromoteMinSamples:           nil,
		AutoPromoteMinScore:             nil,
		AutoPromoteRequireExternalCI:    false,
		AutoPromoteRequireMeasuredJudge: false,
		AutoPromoteCanary:               false,
	}
}

// Enabled reports whether the automatic trace-harvester is configured on. With
// no [skillopt] config (the default) it is OFF and no harvester is constructed.
func (p SkillOptPolicy) Enabled() bool {
	return p.AutoTraceEnabled
}

// ReviewEnabled reports whether the cross-family review-agent soft signal (#469)
// is configured on. It requires BOTH cross_family_review_enabled AND
// auto_trace_enabled (the review row only makes sense inside the auto-trace run),
// so enabling the review knob alone — without the auto-trace harvester — is OFF.
func (p SkillOptPolicy) ReviewEnabled() bool {
	return p.AutoTraceEnabled && p.CrossFamilyReviewEnabled
}

func LoadSkillOptPolicy(paths Paths) (SkillOptPolicy, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return SkillOptPolicy{}, err
	}
	policy := DefaultSkillOptPolicy()
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			current = strings.TrimSpace(section) == "skillopt"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if err := applySkillOptPolicyField(&policy, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return SkillOptPolicy{}, fmt.Errorf("parse [skillopt].%s: %w", strings.TrimSpace(key), err)
		}
	}
	return policy, nil
}

func applySkillOptPolicyField(policy *SkillOptPolicy, key string, value string) error {
	switch key {
	case "auto_trace_enabled":
		parsed, err := strconv.ParseBool(value)
		policy.AutoTraceEnabled = parsed
		return err
	case "cross_family_review_enabled":
		parsed, err := strconv.ParseBool(value)
		policy.CrossFamilyReviewEnabled = parsed
		return err
	case "auto_promote":
		parsed, err := strconv.ParseBool(value)
		policy.AutoPromote = parsed
		return err
	case "auto_promote_min_samples":
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		policy.AutoPromoteMinSamples = &parsed
		return nil
	case "auto_promote_min_score":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return err
		}
		policy.AutoPromoteMinScore = &parsed
		return nil
	case "auto_promote_require_external_ci":
		parsed, err := strconv.ParseBool(value)
		policy.AutoPromoteRequireExternalCI = parsed
		return err
	case "auto_promote_require_measured_judge":
		parsed, err := strconv.ParseBool(value)
		policy.AutoPromoteRequireMeasuredJudge = parsed
		return err
	case "auto_promote_canary":
		parsed, err := strconv.ParseBool(value)
		policy.AutoPromoteCanary = parsed
		return err
	default:
		return nil
	}
}
