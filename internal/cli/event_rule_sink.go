package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const eventRuleWakeTimeout = 12 * time.Second

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
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Warn("org event wake panicked", "job_id", event.JobID, "error", recovered)
			}
		}()
		wakeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventRuleWakeTimeout)
		defer cancel()
		s.evaluate(wakeCtx, event)
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
	if !hasEnabledEventRule(rules) || !s.wake.Available(ctx) {
		return
	}
	for _, rule := range rules {
		if !rule.Enabled || !containsEventRuleKind(kinds, rule.OnKind) || !eventRuleMatches(rule.MatchFilter, event) {
			continue
		}
		pane, ok := s.resolveRolePane(ctx, rule.WakeRole)
		if !ok {
			continue
		}
		prompt := eventRuleWakePrompt(rule.OnKind, event)
		// until="" uses herdr's default settled set; a wake only needs delivery
		// confirmation, so agentPrompt's bounded --timeout returns as soon as the
		// prompt is confirmed landed and never blocks on the woken agent settling.
		delivered, stalled, err := s.wake.AgentPrompt(ctx, pane, prompt, "")
		switch {
		case err != nil:
			slog.Warn("org event wake failed", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "error", err)
		case stalled:
			slog.Info("org event wake stalled", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "delivered", false)
		case delivered:
			slog.Info("org event wake delivered", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "delivered", true)
		default:
			slog.Info("org event wake not delivered", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "delivered", false)
		}
	}
}

// resolveRolePane is the v1 config-backed role→pane binding seam. Keeping it in
// one small method lets a later live registry replace config without changing
// classification, matching, or wake delivery. The configured value is either a
// herdr pane id (contains ':', the wX:pY shape) used directly, or a pane LABEL
// resolved to the current pane id at wake time so a recycled pane (org phase 3)
// is still reached by its stable label.
func (s *eventRuleSink) resolveRolePane(ctx context.Context, role string) (pane string, ok bool) {
	configFile := resolveConfigFile(s.home)
	if configFile == "" {
		return "", false
	}
	cfg, err := config.LoadOrg(config.Paths{ConfigFile: configFile})
	if err != nil {
		return "", false
	}
	orgRole, ok := cfg.Role(role)
	if !ok {
		return "", false
	}
	binding := strings.TrimSpace(orgRole.Pane)
	if binding == "" {
		return "", false
	}
	if strings.Contains(binding, ":") {
		return binding, true
	}
	return s.wake.ResolvePaneByLabel(ctx, binding)
}

func classifyEventRuleKinds(event events.Event) []string {
	switch event.Type {
	case events.EventJobFinished, events.EventJobFailed:
		return []string{"job-terminal"}
	case events.EventJobBlocked:
		switch event.Cause {
		case "merge_guard", "permission_guard":
			return []string{"guard"}
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
	detail := strings.TrimSpace(workflow.RedactCommentText(event.Detail))
	if len(detail) > 320 {
		detail = detail[:320] + "…"
	}
	prompt := fmt.Sprintf("gitmoot %s event for job %s", kind, event.JobID)
	if detail != "" {
		prompt += ": " + detail
	}
	return prompt
}
