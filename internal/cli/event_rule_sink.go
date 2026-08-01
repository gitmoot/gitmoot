package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/workflow"
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

// eventRuleSink decorates the existing outbound sink. Durable event families
// persist their obligations before Emit returns; remaining rule work is detached
// and timeout-bounded so delivery failures cannot fail the emitting job.
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
	event = s.addressBlockedEvent(ctx, event)
	events.EmitEvent(ctx, s.inner, event)
	if s.store == nil {
		return
	}
	// Classify BEFORE spawning: the webhook's normal (non-classifiable) traffic
	// never touches the wake path, so it costs no goroutine or context.
	kinds := classifyEventRuleKinds(event)
	if len(kinds) == 0 {
		return
	}
	if wakeKind, durable := durableWakeKind(kinds); durable {
		rules, err := s.store.ListEventRules(ctx)
		if err != nil {
			slog.Warn("org event rules list failed", "job_id", event.JobID, "error", err)
			return
		}
		targetRoles, remainingRules := partitionDurableWakeRules(rules, kinds, wakeKind, event)
		if len(targetRoles) > 0 {
			payload, err := json.Marshal(event)
			if err != nil {
				slog.Warn("durable wake event encode failed", "job_id", event.JobID, "kind", wakeKind, "error", err)
				return
			}
			if err := s.store.InsertWakeOutbox(
				ctx, wakeKind, string(payload), wakeKind, targetRoles,
			); err != nil {
				slog.Warn("durable wake outbox insert failed", "job_id", event.JobID, "kind", wakeKind, "error", err)
				return
			}
		}
		if s.wake == nil || len(remainingRules) == 0 {
			return
		}
		base := context.WithoutCancel(ctx)
		go func() {
			if err := s.evaluateSafely(base, event, remainingRules); err != nil {
				slog.Warn("org event wake failed", "job_id", event.JobID, "error", err)
			}
		}()
		return
	}
	if s.wake == nil {
		return
	}
	// Detach from the emitting job's ctx (a cancelled job must never cancel a
	// wake) with NO deadline of its own: each herdr call below bounds itself, so a
	// slow earlier rule cannot starve a later rule's wake.
	base := context.WithoutCancel(ctx)
	go func() {
		if err := s.evaluateSafely(base, event, nil); err != nil {
			slog.Warn("org event wake failed", "job_id", event.JobID, "error", err)
		}
	}()
}

// durableWakeKind selects the outbox kind for an event, or reports that the
// event takes the live path.
//
// #1352: directive TTL nags are admitted. They were previously live-only, so
// every due directive produced one prompt per sweep with no coalescing — a noise
// factory at any directive volume, and the one wake class GUARANTEED to repeat
// on a timer rather than on an external trigger. Routing them through the outbox
// gives them the same batching every other recurring wake already has, and the
// same durable record when delivery fails.
func durableWakeKind(kinds []string) (string, bool) {
	for _, kind := range kinds {
		switch kind {
		case db.WakeOutboxKindBlocked, db.WakeOutboxKindEscalation, db.WakeOutboxKindDirective:
			return kind, true
		}
	}
	return "", false
}

func partitionDurableWakeRules(
	rules []db.EventRule,
	kinds []string,
	wakeKind string,
	event events.Event,
) (targetRoles []string, remaining []db.EventRule) {
	for _, rule := range rules {
		if !rule.Enabled || !containsEventRuleKind(kinds, rule.OnKind) || !eventRuleMatches(rule.MatchFilter, event) || !eventRuleMatchesAddressee(rule, event) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(rule.OnKind), wakeKind) {
			targetRoles = append(targetRoles, rule.WakeRole)
			continue
		}
		remaining = append(remaining, rule)
	}
	return targetRoles, remaining
}

// emitWakeOutbox evaluates one already-claimed durable wake synchronously. The
// daemon tick is the delivery owner after the short-lived producer exits, so it
// must not return while an untracked goroutine still owns the attempted row.
// Ordinary event emissions retain Emit's detached, best-effort behavior.
func (s *eventRuleSink) emitWakeOutbox(ctx context.Context, event events.Event, rules []db.EventRule) error {
	if s == nil {
		return fmt.Errorf("event-rule sink is nil")
	}
	events.EmitEvent(ctx, s.inner, event)
	if s.store == nil || s.wake == nil {
		return fmt.Errorf("event-rule sink is not configured for durable delivery")
	}
	return s.evaluateSafely(context.WithoutCancel(ctx), event, rules)
}

func (s *eventRuleSink) evaluateSafely(ctx context.Context, event events.Event, rules []db.EventRule) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Warn("org event wake panicked", "job_id", event.JobID, "error", recovered)
			panicErr := fmt.Errorf("event-rule wake panicked: %v", recovered)
			err = errors.Join(panicErr, s.finishWakeOutbox(ctx, event, db.WakeOutboxStateFailed, panicErr.Error()))
		}
	}()
	if rules == nil {
		return s.evaluate(ctx, event)
	}
	return s.evaluateRules(ctx, event, rules)
}

func (s *eventRuleSink) evaluate(ctx context.Context, event events.Event) error {
	rules, err := s.store.ListEventRules(ctx)
	if err != nil {
		slog.Warn("org event rules list failed", "job_id", event.JobID, "error", err)
		return s.completeWakeOutbox(ctx, event, db.WakeOutboxStateFailed, err.Error(), err)
	}
	return s.evaluateRules(ctx, event, rules)
}

func (s *eventRuleSink) evaluateRules(ctx context.Context, event events.Event, rules []db.EventRule) error {
	kinds := classifyEventRuleKinds(event)
	if len(kinds) == 0 {
		return s.completeWakeOutbox(ctx, event, db.WakeOutboxStateFailed, "event was not classifiable", errors.New("event was not classifiable"))
	}
	if !hasEnabledEventRule(rules) {
		return s.completeWakeOutbox(ctx, event, db.WakeOutboxStateFailed, "no enabled event rule", errors.New("no enabled event rule"))
	}
	// Any matching rule needs herdr; probe once under its own bounded context.
	probeCtx, probeCancel := context.WithTimeout(ctx, eventRuleProbeTimeout)
	available := s.wake.Available(probeCtx)
	probeCancel()
	if !available {
		return s.completeWakeOutbox(ctx, event, db.WakeOutboxStateFailed, "herdr unavailable", errors.New("herdr unavailable"))
	}
	// Load the org registry ONCE per event, not once per matching rule.
	cfg, ok := s.loadOrgConfig()
	if !ok {
		return s.completeWakeOutbox(ctx, event, db.WakeOutboxStateFailed, "organization registry unavailable", errors.New("organization registry unavailable"))
	}
	isAddressedNoteWake := event.Type == events.EventOrgReply || event.Type == events.EventOrgDirective
	replyHandled := false
	addressedReplyHandled := false
	for _, rule := range rules {
		if !rule.Enabled || !containsEventRuleKind(kinds, rule.OnKind) || !eventRuleMatches(rule.MatchFilter, event) || !eventRuleMatchesAddressee(rule, event) {
			continue
		}
		observer := rule.Scope == db.EventRuleScopeObserver
		// Preserve the existing one-attempt-per-target behavior for duplicate
		// addressed reply rules while allowing every observer rule to see the
		// directed event independently.
		if isAddressedNoteWake && !observer && addressedReplyHandled {
			continue
		}
		pane, ok := s.resolveRolePane(ctx, cfg, rule.WakeRole)
		if !ok {
			if err := recordUnresolvedRoleWake(ctx, s.store, rule.WakeRole); err != nil {
				slog.Warn("org event unresolved wake counter failed", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "error", err)
			}
			slog.Warn("org event wake skipped", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "reason", "role pane binding unresolved")
			if err := s.finishWakeOutbox(ctx, event, db.WakeOutboxStateStalled, "role pane binding unresolved"); err != nil {
				return err
			}
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
			if err := s.finishWakeOutbox(ctx, event, db.WakeOutboxStateDelivered, ""); err != nil {
				return err
			}
		case stalled:
			counterCtx, ccancel := context.WithTimeout(ctx, eventRuleProbeTimeout)
			if incrementErr := s.store.IncrementRoleMissedWake(counterCtx, rule.WakeRole, time.Now().UTC()); incrementErr != nil {
				slog.Warn("org event wake counter increment failed", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "error", incrementErr)
			}
			ccancel()
			slog.Info("org event wake stalled", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "delivered", false)
			if err := s.finishWakeOutbox(ctx, event, db.WakeOutboxStateStalled, "agent_prompt_stalled"); err != nil {
				return err
			}
		case err != nil:
			// A Herdr outage is infrastructure failure, not a role ignoring a wake;
			// it must not falsely increment every role's missed-wake counter.
			slog.Warn("org event wake failed", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "error", err)
			return s.completeWakeOutbox(ctx, event, db.WakeOutboxStateFailed, err.Error(), err)
		default:
			// An odd non-delivery is not proof that the role ignored a delivered
			// prompt, so leave the counter unchanged just as for infrastructure errors.
			slog.Info("org event wake not delivered", "rule_id", rule.ID, "role", rule.WakeRole, "job_id", event.JobID, "delivered", false)
			return s.completeWakeOutbox(ctx, event, db.WakeOutboxStateFailed, "agent prompt was not delivered", errors.New("agent prompt was not delivered"))
		}
		// A coalesced reply batch still gives its addressed target one attempt per
		// window. Observer rules are independent copies and do not consume that
		// target attempt.
		if isAddressedNoteWake {
			replyHandled = true
			if !observer {
				addressedReplyHandled = true
			}
		}
	}
	if isAddressedNoteWake && !replyHandled {
		return s.completeWakeOutbox(ctx, event, db.WakeOutboxStateFailed, "no matching addressed rule or role binding", errors.New("no matching addressed rule or role binding"))
	}
	return nil
}

// eventRuleMatchesAddressee is the single scope gate shared by durable enqueue
// and live evaluation. Addressed rules require positive target evidence;
// observer rules deliberately retain fleet-wide oversight for address-less and
// directed events alike.
func eventRuleMatchesAddressee(rule db.EventRule, event events.Event) bool {
	if rule.Scope == db.EventRuleScopeObserver {
		return true
	}
	target := strings.TrimSpace(event.WakeTargetRole)
	return target != "" && strings.EqualFold(strings.TrimSpace(rule.WakeRole), target)
}

// addressBlockedEvent enriches every blocked event at the event-sink source
// boundary before webhook fanout or durable routing. Persisted job attribution
// is the strongest ownership evidence; synthesized blocked-since events fall
// back to the most-specific org role scoped to the event repository.
func (s *eventRuleSink) addressBlockedEvent(ctx context.Context, event events.Event) events.Event {
	if event.Type != events.EventJobBlocked || strings.TrimSpace(event.WakeTargetRole) != "" {
		return event
	}
	targetRole := ""
	if s.store != nil && strings.TrimSpace(event.JobID) != "" {
		if job, err := s.store.GetJob(ctx, event.JobID); err == nil {
			if payload, err := daemonJobPayload(job); err == nil {
				if role := workflow.NormalizeActingOrgRole(payload.ActingOrgRole); role != "" {
					targetRole = role
				}
			}
		}
	}
	if targetRole == "" {
		if cfg, ok := s.loadOrgConfig(); ok {
			targetRole, _ = repoOrgOwner(cfg, event.Repo)
		}
	}
	if targetRole != "" {
		event.WakeTargetRole = targetRole
	}
	return event
}

func recordUnresolvedRoleWake(ctx context.Context, store *db.Store, role string) error {
	if store == nil {
		return nil
	}
	counterCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventRuleProbeTimeout)
	defer cancel()
	return store.IncrementRoleMissedWake(counterCtx, role, time.Now().UTC())
}

func (s *eventRuleSink) completeWakeOutbox(ctx context.Context, event events.Event, state, detail string, cause error) error {
	finishErr := s.finishWakeOutbox(ctx, event, state, detail)
	if len(event.WakeOutboxIDs) == 0 {
		return finishErr
	}
	return errors.Join(cause, finishErr)
}

func (s *eventRuleSink) finishWakeOutbox(ctx context.Context, event events.Event, state, detail string) error {
	if s == nil || s.store == nil || len(event.WakeOutboxIDs) == 0 {
		return nil
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventRuleProbeTimeout)
	defer cancel()
	if err := s.store.FinishWakeOutbox(finishCtx, event.WakeOutboxIDs, state, detail, time.Now().UTC()); err != nil {
		slog.Warn("wake outbox state update failed", "job_id", event.JobID, "state", state, "error", err)
		return fmt.Errorf("finish wake outbox as %s: %w", state, err)
	}
	return nil
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
	case events.EventOrgInputPending:
		return []string{"pane_input_pending"}
	case events.EventOrgReply:
		return []string{"reply"}
	case events.EventOrgDirective:
		return []string{"directive"}
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
	if strings.EqualFold(strings.TrimSpace(kind), db.WakeOutboxKindDirective) {
		directiveID := strings.TrimPrefix(event.RootID, db.WakeOutboxSourceWorkflowNote+":")
		return fmt.Sprintf(
			"gitmoot directive %s for %s; acknowledge receipt with: gitmoot org directive ack %s --by %s",
			directiveID, event.WakeTargetRole, directiveID, event.WakeTargetRole,
		)
	}
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
