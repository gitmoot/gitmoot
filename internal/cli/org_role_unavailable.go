package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const orgRoleUnavailableReasonQuota = "quota"

var newQuotaRoleUnavailableWakeClient = func() eventWakeClient {
	return cockpit.New(cockpit.Options{HerdrBin: "herdr"}, nil)
}

type quotaRoleUnavailableHooks struct {
	store  *db.Store
	home   string
	stdout io.Writer
	wake   eventWakeClient
}

func newQuotaRoleUnavailableHooks(store *db.Store, home string, stdout io.Writer) quotaRoleUnavailableHooks {
	if stdout == nil {
		stdout = io.Discard
	}
	return quotaRoleUnavailableHooks{
		store:  store,
		home:   home,
		stdout: stdout,
		wake:   newQuotaRoleUnavailableWakeClient(),
	}
}

func (w jobWorker) quotaRoleUnavailableHooks() quotaRoleUnavailableHooks {
	stdout := w.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	return quotaRoleUnavailableHooks{store: w.Store, home: w.ConfigHome, stdout: stdout, wake: w.QuotaWake}
}

// classifyRuntimeRoleUnavailable is the provider-specific edge around the
// shared operational-blocker classifier. Phase 1 recognizes Claude only; the
// switch deliberately leaves an obvious extension point for Codex/Kimi without
// duplicating quota parsing or durable incident handling.
func classifyRuntimeRoleUnavailable(runtimeName string, cause error, now time.Time) (blockerClassification, bool) {
	switch strings.TrimSpace(runtimeName) {
	case runtime.ClaudeRuntime:
		classification, ok := classifyOperationalBlocker(cause, now)
		return classification, ok && classification.Class == blockerClassRuntimeQuota
	default:
		return blockerClassification{}, false
	}
}

// recordRuntimeOutcome applies the same role-availability transition at every
// delivery seam: a Claude quota failure records/escalates the incident, while a
// successful attributed job is the definitive signal that clears it.
func (h quotaRoleUnavailableHooks) recordRuntimeOutcome(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent, runErr error, now time.Time) error {
	if runErr == nil {
		return h.clearOnSuccess(ctx, payload.ActingOrgRole)
	}
	return h.captureFailure(ctx, job, payload, agent, runErr, now)
}

// captureFailure records a Claude quota wall for the canonical job-attributed
// organization role, then atomically claims and attempts the incident's single
// escalation. Failures are returned for caller logging but never replace the
// job's original outcome.
func (h quotaRoleUnavailableHooks) captureFailure(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent, cause error, now time.Time) error {
	role := strings.ToLower(strings.TrimSpace(payload.ActingOrgRole))
	if role == "" || h.store == nil {
		return nil
	}
	classification, ok := classifyRuntimeRoleUnavailable(agent.Runtime, cause, now)
	if !ok {
		return nil
	}
	until := classification.QuotaResetAt
	if until.IsZero() {
		until = now.UTC().Add(quotaBlockerFallbackDelay)
	}
	if err := h.store.UpsertOrgRoleUnavailable(ctx, role, orgRoleUnavailableReasonQuota, until, now); err != nil {
		return fmt.Errorf("record org role %q unavailable: %w", role, err)
	}
	claimed, err := h.store.MarkOrgRoleUnavailableEscalated(ctx, role, now)
	if err != nil {
		return fmt.Errorf("claim org role %q quota escalation: %w", role, err)
	}
	if !claimed {
		return nil
	}
	incident, found, err := h.store.GetActiveOrgRoleUnavailable(ctx, role, now)
	if err != nil {
		return fmt.Errorf("reload org role %q unavailability: %w", role, err)
	}
	if found {
		h.wakeParent(ctx, job, payload, incident, classification.QuotaResetParsed, classification.QuotaResetMentioned)
	}
	return nil
}

func (h quotaRoleUnavailableHooks) clearOnSuccess(ctx context.Context, role string) error {
	role = strings.TrimSpace(role)
	if role == "" || h.store == nil {
		return nil
	}
	return h.store.ClearOrgRoleUnavailable(ctx, role)
}

func (w jobWorker) captureQuotaRoleUnavailable(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent, cause error, now time.Time) error {
	return w.quotaRoleUnavailableHooks().captureFailure(ctx, job, payload, agent, cause, now)
}

func (w jobWorker) clearQuotaRoleUnavailableOnSuccess(ctx context.Context, role string) error {
	return w.quotaRoleUnavailableHooks().clearOnSuccess(ctx, role)
}

// wakeQuotaRoleUnavailable directly wakes the unavailable role's configured
// parent. The durable escalation claim is marked before this best-effort call,
// matching the codebase's mark-before-emit discipline and preventing storms.
func (h quotaRoleUnavailableHooks) wakeParent(ctx context.Context, job db.Job, payload workflow.JobPayload, incident db.OrgRoleUnavailable, resetParsed, resetMentioned bool) {
	if h.wake == nil {
		return
	}
	paths, err := pathsFromFlag(h.home)
	if err != nil {
		writeLine(h.stdout, "org role %s quota escalation skipped: resolve config: %v", incident.Role, err)
		return
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		writeLine(h.stdout, "org role %s quota escalation skipped: load org registry: %v", incident.Role, err)
		return
	}
	source, ok := cfg.Role(incident.Role)
	if !ok {
		writeLine(h.stdout, "org role %s quota escalation skipped: role is not configured", incident.Role)
		return
	}
	targetName := strings.TrimSpace(source.Parent)
	if targetName == "" {
		writeLine(h.stdout, "org role %s quota escalation skipped: role has no parent", incident.Role)
		return
	}
	target, ok := cfg.Role(targetName)
	if !ok {
		writeLine(h.stdout, "org role %s quota escalation skipped: parent role %s is not configured", incident.Role, targetName)
		return
	}
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventRuleProbeTimeout)
	available := h.wake.Available(probeCtx)
	cancel()
	if !available {
		writeLine(h.stdout, "org role %s quota escalation not delivered: Herdr unavailable", incident.Role)
		return
	}
	pane, ok := config.ResolveRolePaneBinding(context.WithoutCancel(ctx), target.Pane, func(resolveCtx context.Context, label string) (string, bool) {
		bounded, cancel := context.WithTimeout(resolveCtx, eventRuleProbeTimeout)
		defer cancel()
		return h.wake.ResolvePaneByLabel(bounded, label)
	})
	if !ok {
		writeLine(h.stdout, "org role %s quota escalation skipped: parent role %s has no pane", incident.Role, targetName)
		return
	}
	resetSource := "provider reset"
	if !resetParsed && resetMentioned {
		resetSource = "bounded fallback; provider reset hint was not parseable"
	} else if !resetParsed {
		resetSource = "bounded fallback; provider supplied no reset hint"
	}
	prompt := fmt.Sprintf(
		"Gitmoot quota escalation: org role %s is UNAVAILABLE (reason=quota) until %s (%s) after job %s for %s.",
		incident.Role, formatOrgRoleUnavailableUntil(incident.Until), resetSource, job.ID, payload.Repo,
	)
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventRuleWakeTimeout)
	delivered, stalled, err := h.wake.AgentPrompt(callCtx, pane, prompt, "")
	cancel()
	if err != nil || stalled || !delivered {
		writeLine(h.stdout, "org role %s quota escalation not delivered to %s: %v", incident.Role, targetName, err)
	}
}

func unavailableRoleDispatchError(incident db.OrgRoleUnavailable) error {
	return fmt.Errorf("org role %q is unavailable (reason=%s) until %s; dispatch refused",
		incident.Role, incident.Reason, formatOrgRoleUnavailableUntil(incident.Until))
}

func refuseUnavailableOrgRole(ctx context.Context, store *db.Store, role string, now time.Time) error {
	role = strings.TrimSpace(role)
	if role == "" || store == nil {
		return nil
	}
	incident, found, err := store.GetActiveOrgRoleUnavailable(ctx, role, now)
	if err != nil {
		return fmt.Errorf("read org role %q unavailability: %w", role, err)
	}
	if found {
		return unavailableRoleDispatchError(incident)
	}
	return nil
}
