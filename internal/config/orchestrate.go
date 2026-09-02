package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/reviewseverity"
)

// OrchestratePolicy contains host-level orchestration settings read from the
// [orchestrate] section of the gitmoot config.
type OrchestratePolicy struct {
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
	// BlockedTTL is the opt-in maximum time a job may sit BLOCKED — paused awaiting a
	// human (an operator permission gate or an unrecoverable BlockedError) — before
	// the daemon's housekeeping sweep dismisses it via CancelJob (#631), as a Go
	// duration string. Empty or zero (the DEFAULT) means the sweep is DISABLED: a
	// blocked job is a human-awaiting decision and is NEVER auto-discarded unless the
	// operator opts in with a positive duration; a negative value is rejected. Unlike
	// EscalationTTL — which auto-finalizes a whole paused delegation TREE and is ON by
	// default — this dismisses a SINGLE blocked job and is OFF by default. The daemon
	// resolves it per tick via resolveBlockedTTL.
	BlockedTTL string
	// BlockedRoleWakeAfter is the opt-in age after which a continuously blocked
	// task or Herdr-backed organization role emits one synthesized blocked event.
	// Zero (the default) disables both evaluators; negative values are rejected.
	BlockedRoleWakeAfter time.Duration
	// MaxConsecutiveMissedWakes is the per-role threshold at which org chart and
	// status flag a role whose delivered prompts repeatedly stall. 0 (the default)
	// disables flagging; negative values are rejected.
	MaxConsecutiveMissedWakes int
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
	// Default*Timeout are optional child delegation job timeout defaults. Empty
	// strings mean unbounded. Explicit per-delegation timeout values win, then the
	// phase-specific default, then DefaultDelegationTimeout, then unbounded. These
	// are ordinary orchestrator defaults; they are not tied to an agent name.
	DefaultDelegationTimeout string
	DefaultPlanTimeout       string
	DefaultImplementTimeout  string
	DefaultReviewTimeout     string
	DefaultGateTimeout       string
	DefaultRepairTimeout     string
}

// DefaultEscalationTTL is the fallback time a paused-for-human tree may sit
// before the daemon auto-finalizes it gracefully (#340).
const DefaultEscalationTTL = "24h"

func DefaultOrchestratePolicy() OrchestratePolicy {
	return OrchestratePolicy{
		InlineArtifactBodies:           false,
		InlineArtifactMaxBytes:         0,
		InjectUpstreamDepContext:       false,
		MaxDelegationTokenBudget:       0,
		MaxDelegationCostUSD:           0,
		EscalationHandle:               "",
		EscalationTTL:                  "",
		BlockedTTL:                     "",
		BlockedRoleWakeAfter:           0,
		MaxConsecutiveMissedWakes:      0,
		MaxDelegationNonProgressStreak: 0,
		MaxVerifyReplanAttempts:        0,
		DefaultDelegationTimeout:       "",
		DefaultPlanTimeout:             "",
		DefaultImplementTimeout:        "",
		DefaultReviewTimeout:           "",
		DefaultGateTimeout:             "",
		DefaultRepairTimeout:           "",
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
		if section, ok := sectionHeader(line); ok {
			current = section == "orchestrate"
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
	case "blocked_ttl":
		parsed, err := parseConfigString(value)
		policy.BlockedTTL = strings.TrimSpace(parsed)
		return err
	case "blocked_role_wake_after":
		parsed, err := parseConfigString(value)
		if err != nil {
			return err
		}
		parsed = strings.TrimSpace(parsed)
		if parsed == "" {
			policy.BlockedRoleWakeAfter = 0
			return nil
		}
		policy.BlockedRoleWakeAfter, err = time.ParseDuration(parsed)
		return err
	case "max_consecutive_missed_wakes":
		parsed, err := strconv.Atoi(value)
		policy.MaxConsecutiveMissedWakes = parsed
		return err
	case "max_delegation_non_progress_streak":
		parsed, err := strconv.Atoi(value)
		policy.MaxDelegationNonProgressStreak = parsed
		return err
	case "max_verify_replan_attempts":
		parsed, err := strconv.Atoi(value)
		policy.MaxVerifyReplanAttempts = parsed
		return err
	case "default_delegation_timeout":
		parsed, err := parseConfigString(value)
		policy.DefaultDelegationTimeout = strings.TrimSpace(parsed)
		return err
	case "default_plan_timeout":
		parsed, err := parseConfigString(value)
		policy.DefaultPlanTimeout = strings.TrimSpace(parsed)
		return err
	case "default_implement_timeout":
		parsed, err := parseConfigString(value)
		policy.DefaultImplementTimeout = strings.TrimSpace(parsed)
		return err
	case "default_review_timeout":
		parsed, err := parseConfigString(value)
		policy.DefaultReviewTimeout = strings.TrimSpace(parsed)
		return err
	case "default_gate_timeout":
		parsed, err := parseConfigString(value)
		policy.DefaultGateTimeout = strings.TrimSpace(parsed)
		return err
	case "default_repair_timeout":
		parsed, err := parseConfigString(value)
		policy.DefaultRepairTimeout = strings.TrimSpace(parsed)
		return err
	default:
		return nil
	}
}

func validateOrchestratePolicy(policy OrchestratePolicy) error {
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
	if ttl := strings.TrimSpace(policy.BlockedTTL); ttl != "" {
		parsed, err := time.ParseDuration(ttl)
		if err != nil {
			return fmt.Errorf("orchestrate.blocked_ttl %q is invalid: %w", ttl, err)
		}
		// Zero is the explicit "disabled" form (like an empty value); only a
		// NEGATIVE duration is a misconfiguration to reject, since the blocked_ttl
		// sweep is off-by-default and a non-positive value simply keeps it off.
		if parsed < 0 {
			return fmt.Errorf("orchestrate.blocked_ttl must be zero (disabled) or a positive duration")
		}
	}
	if policy.BlockedRoleWakeAfter < 0 {
		return fmt.Errorf("orchestrate.blocked_role_wake_after must be zero (disabled) or a positive duration")
	}
	if policy.MaxConsecutiveMissedWakes < 0 {
		return fmt.Errorf("orchestrate.max_consecutive_missed_wakes must be 0 (disabled) or positive")
	}
	if policy.MaxDelegationNonProgressStreak < 0 {
		return fmt.Errorf("orchestrate.max_delegation_non_progress_streak must be 0 (engine default) or positive")
	}
	if policy.MaxVerifyReplanAttempts < 0 {
		return fmt.Errorf("orchestrate.max_verify_replan_attempts must be 0 (engine default) or positive")
	}
	for key, value := range map[string]string{
		"default_delegation_timeout": policy.DefaultDelegationTimeout,
		"default_plan_timeout":       policy.DefaultPlanTimeout,
		"default_implement_timeout":  policy.DefaultImplementTimeout,
		"default_review_timeout":     policy.DefaultReviewTimeout,
		"default_gate_timeout":       policy.DefaultGateTimeout,
		"default_repair_timeout":     policy.DefaultRepairTimeout,
	} {
		if err := validateOptionalDuration("orchestrate."+key, value); err != nil {
			return err
		}
	}
	return nil
}

func validateOptionalDuration(name string, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s %q is invalid: %w", name, value, err)
	}
	if parsed <= 0 {
		return fmt.Errorf("%s must be positive when set", name)
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
		if section, ok := sectionHeader(line); ok {
			current = section == "events"
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

// ReviewPolicy is the resolved review policy for one repository. Native review
// fanout is disabled by default: agents and coordinators request reviews
// explicitly, while operators may opt a repository back into native scheduling.
// Risk-tiered adaptive review remains independently opt-in.
type ReviewPolicy struct {
	// NativeFanoutEnabled permits HandlePullRequestOpened to schedule the
	// configured native reviewer roster. Default false = OFF.
	NativeFanoutEnabled bool
	// BlockingSeverity is the least severe changes-requested review that restarts
	// the fix loop. Default P3 preserves the historical block-all behavior.
	BlockingSeverity string
	// RiskTiersEnabled opts the engine into risk-tiered review. Default false = OFF.
	RiskTiersEnabled bool
	// HighRiskPaths is the changed-path glob list that resolves the `high` tier.
	// Empty falls back to the engine's built-in defaults.
	HighRiskPaths []string
	// RiskLabelHigh / RiskLabelRoutine are the PR label names that force a tier
	// (winning over path heuristics). Empty falls back to the engine defaults
	// (risk:high / risk:routine).
	RiskLabelHigh    string
	RiskLabelRoutine string
}

func DefaultReviewPolicy() ReviewPolicy {
	return ReviewPolicy{
		NativeFanoutEnabled: false,
		BlockingSeverity:    reviewseverity.DefaultBlocking,
		RiskTiersEnabled:    false,
	}
}

// ReviewConfig is the parsed global [review] policy plus repository-scoped
// native_fanout_enabled and blocking_severity overrides.
type ReviewConfig struct {
	Global ReviewPolicy
	repos  map[string]reviewPolicyOverride
}

type reviewConfigFieldError struct {
	section string
	field   string
	err     error
}

func (e *reviewConfigFieldError) Error() string {
	return fmt.Sprintf("parse %s.%s: %v", e.section, e.field, e.err)
}

func (e *reviewConfigFieldError) Unwrap() error {
	return e.err
}

// ReviewConfigErrorsOnlyBlockingSeverity reports whether every parse failure is
// a blocking_severity field. Those failures are safe to retain because the
// parser replaces that field with the fail-closed P3 default. Any other parse
// failure makes the applied review policy ambiguous and must reject the file.
func ReviewConfigErrorsOnlyBlockingSeverity(err error) bool {
	if err == nil {
		return false
	}
	seen := false
	onlyBlockingSeverity := true
	var visit func(error)
	visit = func(current error) {
		if current == nil {
			return
		}
		if fieldErr, ok := current.(*reviewConfigFieldError); ok {
			seen = true
			if fieldErr.field != "blocking_severity" {
				onlyBlockingSeverity = false
			}
			return
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				visit(child)
			}
			return
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			visit(wrapped.Unwrap())
			return
		}
		seen = true
		onlyBlockingSeverity = false
	}
	visit(err)
	return seen && onlyBlockingSeverity
}

type reviewPolicyOverride struct {
	nativeFanoutEnabled *bool
	blockingSeverity    *string
}

// For resolves the effective policy for repo. Risk-tier settings remain global;
// native fanout and blocking severity support repository overrides.
func (c ReviewConfig) For(repo string) ReviewPolicy {
	policy := c.Global
	policy.HighRiskPaths = append([]string(nil), policy.HighRiskPaths...)
	override, ok := c.repos[strings.TrimSpace(repo)]
	if ok && override.nativeFanoutEnabled != nil {
		policy.NativeFanoutEnabled = *override.nativeFanoutEnabled
	}
	if ok && override.blockingSeverity != nil {
		policy.BlockingSeverity = *override.blockingSeverity
	}
	return policy
}

// LoadReviewConfig parses [review] and [repos."owner/repo".review]. A missing
// config file or section yields blocking severity P3, with native fanout and
// risk tiers disabled. Invalid fields retain their safe defaults while valid
// fields continue to load; the joined parse error still lets strict callers
// reject the file.
func LoadReviewConfig(paths Paths) (ReviewConfig, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return ReviewConfig{Global: DefaultReviewPolicy(), repos: map[string]reviewPolicyOverride{}}, nil
		}
		return ReviewConfig{}, err
	}
	cfg := ReviewConfig{Global: DefaultReviewPolicy(), repos: map[string]reviewPolicyOverride{}}
	var repo string
	inSection := false
	var parseErrors []error
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			if section == "" && malformedHeaderTargets(line, "review") {
				return ReviewConfig{}, fmt.Errorf("parse review section: missing closing ]")
			}
			repo, inSection = parseReviewSection(section)
			if inSection && repo != "" {
				if _, ok := cfg.repos[repo]; !ok {
					cfg.repos[repo] = reviewPolicyOverride{}
				}
			}
			continue
		}
		if !inSection {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if repo == "" {
			if err := applyReviewPolicyField(&cfg.Global, key, value); err != nil {
				parseErrors = append(parseErrors, &reviewConfigFieldError{
					section: "[review]", field: key, err: err,
				})
			}
			continue
		}
		override := cfg.repos[repo]
		if err := applyReviewPolicyOverrideField(&override, key, value); err != nil {
			parseErrors = append(parseErrors, &reviewConfigFieldError{
				section: fmt.Sprintf("[repos.%q.review]", repo), field: key, err: err,
			})
		}
		cfg.repos[repo] = override
	}
	return cfg, errors.Join(parseErrors...)
}

func parseReviewSection(section string) (string, bool) {
	section = strings.TrimSpace(section)
	if section == "review" {
		return "", true
	}
	if !strings.HasPrefix(section, "repos.") || !strings.HasSuffix(section, ".review") {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(section, "repos."), ".review"))
	if rest == "" {
		return "", false
	}
	if strings.HasPrefix(rest, "\"") {
		unquoted, err := strconv.Unquote(rest)
		if err != nil || strings.TrimSpace(unquoted) == "" {
			return "", false
		}
		return strings.TrimSpace(unquoted), true
	}
	return rest, true
}

func applyReviewPolicyField(policy *ReviewPolicy, key string, value string) error {
	switch key {
	case "native_fanout_enabled":
		parsed, err := parseConfigBool(value)
		if err != nil {
			return err
		}
		policy.NativeFanoutEnabled = parsed
		return nil
	case "blocking_severity":
		parsed, err := parseReviewBlockingSeverity(value)
		if err != nil {
			policy.BlockingSeverity = reviewseverity.DefaultBlocking
			return err
		}
		policy.BlockingSeverity = parsed
		return nil
	case "risk_tiers_enabled":
		parsed, err := parseConfigBool(value)
		if err != nil {
			return err
		}
		policy.RiskTiersEnabled = parsed
		return nil
	case "high_risk_paths":
		parsed, err := parseConfigStringArray(value)
		if err != nil {
			return err
		}
		policy.HighRiskPaths = parsed
		return nil
	case "risk_label_high":
		parsed, err := parseConfigString(value)
		if err != nil {
			return err
		}
		policy.RiskLabelHigh = strings.TrimSpace(parsed)
		return nil
	case "risk_label_routine":
		parsed, err := parseConfigString(value)
		if err != nil {
			return err
		}
		policy.RiskLabelRoutine = strings.TrimSpace(parsed)
		return nil
	default:
		return nil
	}
}

func applyReviewPolicyOverrideField(override *reviewPolicyOverride, key string, value string) error {
	switch key {
	case "native_fanout_enabled":
		parsed, err := parseConfigBool(value)
		if err != nil {
			return err
		}
		override.nativeFanoutEnabled = &parsed
		return nil
	case "blocking_severity":
		parsed, err := parseReviewBlockingSeverity(value)
		if err != nil {
			safe := reviewseverity.DefaultBlocking
			override.blockingSeverity = &safe
			return err
		}
		override.blockingSeverity = &parsed
		return nil
	default:
		return nil
	}
}

func parseReviewBlockingSeverity(value string) (string, error) {
	parsed, err := parseConfigString(value)
	if err != nil {
		return "", err
	}
	severity := strings.ToUpper(strings.TrimSpace(parsed))
	if !reviewseverity.Valid(severity) {
		return "", fmt.Errorf("blocking severity %q must be one of %s", parsed, strings.Join(reviewseverity.Values, ", "))
	}
	return severity, nil
}
