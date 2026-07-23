package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
)

const (
	// eventRuleWakeTimeout bounds a SINGLE herdr agent-prompt on the Go side. It
	// MUST exceed herdr's own --timeout (herdrWakeTimeoutMS = 8s) so herdr returns
	// a JSON outcome before the context kills the process. Each matching rule gets
	// a FRESH context with this budget (rules are processed sequentially), so a
	// slow earlier wake can never SIGKILL a later rule's wake.
	eventRuleWakeTimeout = 12 * time.Second
	// eventRuleProbeTimeout bounds the availability probe and each pane-label
	// resolution (a herdr `pane list`) so neither can hang the wake path.
	eventRuleProbeTimeout = 5 * time.Second
)

type eventWakeClient interface {
	Available(context.Context) bool
	AgentPrompt(context.Context, string, string, string) (bool, bool, error)
	ResolvePaneByLabel(context.Context, string) (string, bool)
}

// eventRuleSink decorates the existing outbound sink. Its rule work is detached
// and timeout-bounded so a DB, config, or Herdr failure can never affect the job
// emitting the event.
type eventRuleSink struct {
	inner events.Sink
	store *db.Store
	home  string
	wake  eventWakeClient
}

func (s *eventRuleSink) Emit(ctx context.Context, event events.Event) {
	if s == nil {
		return
	}
	events.EmitEvent(ctx, s.inner, event)
	if s.store == nil || s.wake == nil {
		return
	}
	// Classify BEFORE spawning: the webhook's normal (non-classifiable) traffic
	// never touches the wake path, so it costs no goroutine or context.
	if len(classifyEventRuleKinds(event)) == 0 {
		return
	}
	// Detach from the emitting job's ctx (a cancelled job must never cancel a
	// wake) with NO deadline of its own: each herdr call below bounds itself, so a
	// slow earlier rule cannot starve a later rule's wake.
	base := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Warn("org event wake panicked", "job_id", event.JobID, "error", recovered)
			}
		}()
		s.evaluate(base, event)
	}()
}

func (s *eventRuleSink) evaluate(ctx context.Context, event events.Event) {
	kinds := classifyEventRuleKinds(event)
	if len(kinds) == 0 {
		return
	}
	rules, err := s.store.ListEventRules(ctx)
	if err != nil {
		slog.Warn("org event rules list failed", "job_id", event.JobID, "error", err)
		return
	}
	if !hasEnabledEventRule(rules) {
		return
	}
	// Any matching rule needs herdr; probe once under its own bounded context.
	probeCtx, probeCancel := context.WithTimeout(ctx, eventRuleProbeTimeout)
	available := s.wake.Available(probeCtx)
	probeCancel()
	if !available {
		return
	}
	// Load the org registry ONCE per event, not once per matching rule.
	cfg, ok := s.loadOrgConfig()
	if !ok {
		return
	}
	for _, rule := range rules {
		if !rule.Enabled || !containsEventRuleKind(kinds, rule.OnKind) || !eventRuleMatches(rule.MatchFilter, event) {
			continue
		}
		pane, ok := s.resolveRolePane(ctx, cfg, rule.WakeRole)
		if !ok {
			continue
		}
		prompt := eventRuleWakePrompt(rule.OnKind, event)
		// A FRESH per-rule budget (> herdr's 8s --timeout) so a slow wake for one
		// rule cannot consume a shared budget and leave a later rule's wake to be
		// SIGKILLed mid-call. until="" uses herdr's default settled set; a wake only
		// needs delivery confirmation, so agentPrompt's bounded --timeout returns as
		// soon as the prompt is confirmed landed and never blocks on the agent settling.
		callCtx, cancel := context.WithTimeout(ctx, eventRuleWakeTimeout)
		delivered, stalled, err := s.wake.AgentPrompt(callCtx, pane, prompt, "")
		cancel()
		switch {
		case delivered:
			// Bound the best-effort counter write like every other op on this path,
			// so a SQLite-busy stall (shared-DB write contention) is cut at the probe
			// timeout instead of blocking the next rule's wake for the 15s busy-timeout.
			counterCtx, ccancel := context.WithTimeout(ctx, eventRuleProbeTimeout)
			if resetErr := s.store.ResetRoleMissedWake(counterCtx, rule.WakeRole); resetErr != nil {
				slog.Warn("org event wake counter reset failed", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "error", resetErr)
			}
			ccancel()
			slog.Info("org event wake delivered", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "delivered", true)
		case stalled:
			counterCtx, ccancel := context.WithTimeout(ctx, eventRuleProbeTimeout)
			if incrementErr := s.store.IncrementRoleMissedWake(counterCtx, rule.WakeRole, time.Now().UTC()); incrementErr != nil {
				slog.Warn("org event wake counter increment failed", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "error", incrementErr)
			}
			ccancel()
			slog.Info("org event wake stalled", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "delivered", false)
		case err != nil:
			// A Herdr outage is infrastructure failure, not a role ignoring a wake;
			// it must not falsely increment every role's missed-wake counter.
			slog.Warn("org event wake failed", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "error", err)
		default:
			// An odd non-delivery is not proof that the role ignored a delivered
			// prompt, so leave the counter unchanged just as for infrastructure errors.
			slog.Info("org event wake not delivered", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "delivered", false)
		}
	}
}

// loadOrgConfig reads the org registry once per event. Best-effort: a missing or
// unreadable config disables all wakes for this event (no role bindings resolve).
func (s *eventRuleSink) loadOrgConfig() (config.OrgConfig, bool) {
	configFile := resolveConfigFile(s.home)
	if configFile == "" {
		return config.OrgConfig{}, false
	}
	cfg, err := config.LoadOrg(config.Paths{ConfigFile: configFile})
	if err != nil {
		return config.OrgConfig{}, false
	}
	return cfg, true
}

// resolveRolePane is the v1 config-backed role→pane binding seam. Keeping it in
// one small method lets a later live registry replace config without changing
// classification, matching, or wake delivery.
func (s *eventRuleSink) resolveRolePane(ctx context.Context, cfg config.OrgConfig, role string) (pane string, ok bool) {
	orgRole, ok := cfg.Role(role)
	if !ok {
		return "", false
	}
	return config.ResolveRolePaneBinding(ctx, orgRole.Pane, func(ctx context.Context, label string) (string, bool) {
		// Bound the `pane list` resolution so it cannot hang the wake path.
		resolveCtx, cancel := context.WithTimeout(ctx, eventRuleProbeTimeout)
		defer cancel()
		return s.wake.ResolvePaneByLabel(resolveCtx, label)
	})
}

func classifyEventRuleKinds(event events.Event) []string {
	switch event.Type {
	case events.EventJobFinished, events.EventJobFailed:
		return []string{"job-terminal"}
	case events.EventJobBlocked:
		switch event.Cause {
		case "merge_guard", "permission_guard":
			return []string{"guard"}
		case "blocked_since":
			return []string{"blocked"}
		case "":
			// A plain blocked transition is both a terminal outcome and the
			// narrower blocked rule kind.
			return []string{"job-terminal", "blocked"}
		}
	case events.EventJobNeedsAttention:
		switch event.Cause {
		case "escalation":
			return []string{"escalation"}
		case "ask_gate":
			return []string{"attention"}
		}
	case events.EventOrgRecycleOverdue:
		return []string{"recycle-overdue"}
	}
	return nil
}

func hasEnabledEventRule(rules []db.EventRule) bool {
	for _, rule := range rules {
		if rule.Enabled {
			return true
		}
	}
	return false
}

func containsEventRuleKind(kinds []string, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

func eventRuleMatches(filter string, event events.Event) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	return strings.Contains(strings.ToLower(event.Repo), filter) ||
		strings.Contains(strings.ToLower(event.JobID), filter)
}

func eventRuleWakePrompt(kind string, event events.Event) string {
	// event.Detail is already redacted + absolute-path-scrubbed by events.NewEvent,
	// so it is used as-is here (only trimmed and rune-safe truncated for the arg).
	detail := truncateForWake(strings.TrimSpace(event.Detail), 320)
	prompt := fmt.Sprintf("gitmoot %s event for job %s", kind, event.JobID)
	if detail != "" {
		prompt += ": " + detail
	}
	return prompt
}

// truncateForWake caps s to at most max BYTES without splitting a multibyte UTF-8
// rune (a byte slice mid-rune would emit invalid UTF-8), appending an ellipsis
// when it actually truncates. It backs the cut up over continuation bytes at the
// cut point ONLY — validating the whole prefix instead would let a stray invalid
// byte anywhere before max collapse the detail down to just the ellipsis.
func truncateForWake(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
