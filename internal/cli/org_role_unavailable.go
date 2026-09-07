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
	return cockpit.New(cockpit.Options{HerdrBin: "herdr"})
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
// successful attributed job on that same runtime is the definitive signal that
// clears it.
func (h quotaRoleUnavailableHooks) recordRuntimeOutcome(ctx context.Context, job db.Job, payload workflow.JobPayload, agent runtime.Agent, runErr error, now time.Time) error {
	if runErr == nil {
		return h.clearOnSuccess(ctx, payload.ActingOrgRole, agent.Runtime)
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
	if err := h.store.UpsertOrgRoleUnavailableForRuntime(ctx, role, agent.Runtime, orgRoleUnavailableReasonQuota, until, now); err != nil {
		return fmt.Errorf("record org role %q unavailable: %w", role, err)
	}
	claimed, err := h.store.MarkOrgRoleUnavailableEscalatedForRuntime(ctx, role, agent.Runtime, now)
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

func (h quotaRoleUnavailableHooks) clearOnSuccess(ctx context.Context, role, runtimeName string) error {
	role = strings.TrimSpace(role)
	runtimeName = strings.TrimSpace(runtimeName)
	if role == "" || runtimeName == "" || h.store == nil {
		return nil
	}
	return h.store.ClearOrgRoleUnavailableForRuntime(ctx, role, runtimeName)
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
		if err := recordUnresolvedRoleWake(ctx, h.store, targetName); err != nil {
			writeLine(h.stdout, "org role %s quota escalation unresolved wake counter failed for %s: %v", incident.Role, targetName, err)
		}
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

// knownRuntimeName reports whether name resolves to a real runtime adapter.
// This is the same test the per-job --runtime override validation uses
// (resolveJobRuntimeOverride), so an unrecognized stored or selected runtime is
// classified identically at both ends.
func knownRuntimeName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	_, err := (runtime.Factory{}).Adapter(strings.ToLower(strings.TrimSpace(name)))
	return err == nil
}

// orgRoleUnavailableRefusesRuntime decides whether an ACTIVE incident row bars a
// dispatch whose selected runtime is selectedRuntime (#1641).
//
// The row is written per-runtime (UpsertOrgRoleUnavailableForRuntime) because a
// provider wall is a property of one provider, not of the role: a claude quota
// wall says nothing about codex or kimi. Enforcement must therefore compare the
// row's runtime against the runtime this dispatch will ACTUALLY use.
//
// FAIL CLOSED, in both directions:
//   - An empty stored runtime keeps its pre-#1641 whole-role meaning. It is not a
//     corrupt value: UpsertOrgRoleUnavailable still writes "", and the runtime
//     column was added by ALTER TABLE ... DEFAULT ” (#1490), so every row
//     predating runtime attribution is legitimately unattributed.
//   - An unrecognized stored runtime is corrupt, and corruption is not permission
//     to dispatch.
//   - An empty or unrecognized SELECTED runtime means the caller could not say
//     what this job will run as, so it cannot claim to be a different runtime.
func orgRoleUnavailableRefusesRuntime(incident db.OrgRoleUnavailable, selectedRuntime string) bool {
	stored := strings.ToLower(strings.TrimSpace(incident.Runtime))
	if !knownRuntimeName(stored) {
		return true
	}
	selected := strings.ToLower(strings.TrimSpace(selectedRuntime))
	if !knownRuntimeName(selected) {
		return true
	}
	return stored == selected
}

// refuseUnavailableOrgRole refuses a dispatch only when the role's active
// incident belongs to the runtime this dispatch actually selected. Callers must
// pass the runtime from their own final resolution — the value the job will run
// as, overrides included — never the registered agent's default when an override
// is in play.
//
// The store read is unconditional so its eager expiry of a stale row (and
// #1490's fail-closed error on a malformed until) still happens for every
// dispatch, whatever the runtime decision turns out to be.
func refuseUnavailableOrgRole(ctx context.Context, store *db.Store, role, selectedRuntime string, now time.Time) error {
	role = strings.TrimSpace(role)
	if role == "" || store == nil {
		return nil
	}
	incident, found, err := store.GetActiveOrgRoleUnavailable(ctx, role, now)
	if err != nil {
		return fmt.Errorf("read org role %q unavailability: %w", role, err)
	}
	if found && orgRoleUnavailableRefusesRuntime(incident, selectedRuntime) {
		return unavailableRoleDispatchError(incident)
	}
	return nil
}

// selectedJobDispatchRuntime reports the runtime a QUEUED job will actually run
// as, resolved in the same order the claiming worker resolves it.
//
// THE ORDER IS LOAD-BEARING AND IS NOT THE OBVIOUS ONE (#1641, #1952 review):
//
//  1. An ephemeral job's spec wins. daemon_worker.go materializes the throwaway
//     worker UNCONDITIONALLY when payload.Ephemeral is set, building the agent
//     from spec.Runtime and UpsertAgent-ing it under the job's agent name — so it
//     OVERWRITES any pre-existing row of that name. Consulting the agents table
//     first therefore checks the wall against a runtime that will never execute
//     whenever a same-name row happens to exist.
//  2. Otherwise the registered agent's runtime.
//  3. A per-job runtime override wins over both, because the worker applies it
//     after materialization.
//
// Returning "" means UNKNOWN, not "some other runtime": refuseUnavailableOrgRole
// fails closed on an empty value, so an ephemeral spec with an empty or
// unrecognized runtime refuses rather than dispatches.
//
// Deliberately NOT resolveTranscriptRuntime, and for a sharper reason than the
// first version of this comment gave: that helper answers "which transcript do I
// read" and treats the ephemeral spec as a LAST fallback, the same shape
// dashboard_web.go uses for a rendered column. Read/display paths may rank the
// spec last; write/execute paths must not — mailbox.go assigns the spec's runtime
// onto the agent at enqueue for exactly this reason. A dispatch refusal is an
// execute-path decision.
func selectedJobDispatchRuntime(ctx context.Context, store *db.Store, job db.Job, payload workflow.JobPayload) string {
	agent, ok := selectedJobRuntimeAgent(ctx, store, job, payload)
	if !ok {
		return ""
	}
	return agent.Runtime
}

// selectedJobRuntimeAgent resolves the runtime.Agent a QUEUED job will actually
// run as, and is the ONE place the precedence override > ephemeral spec >
// registered row is expressed. Every execute-path decision about a queued job —
// refuse, hold, probe, reserve, serialize — must resolve through here, because
// the whole #1641/#1952 defect class is a consumer that derived a runtime from
// the agents table while the job ran on something else.
//
// For an ephemeral job the spec is reconstructed with the same fields
// startEphemeralWorker materializes, so a caller that needs more than the
// runtime name (an auth probe needs the autonomy policy and template; a resource
// key needs the runtime) sees the agent the worker will build rather than a
// runtime string in a hollow struct.
//
// ok=false means UNRESOLVABLE, never "some other runtime": callers must fail
// closed on it, and each one keeps the conservative fallback it already had.
func selectedJobRuntimeAgent(ctx context.Context, store *db.Store, job db.Job, payload workflow.JobPayload) (runtime.Agent, bool) {
	if spec := payload.Ephemeral; spec != nil {
		return applyJobRuntimeOverride(runtime.Agent{
			Name:           job.Agent,
			Role:           firstNonEmpty(strings.TrimSpace(spec.Role), strings.TrimSpace(job.Type), "worker"),
			Runtime:        spec.Runtime,
			Model:          spec.Model,
			Effort:         spec.Effort,
			TemplateID:     spec.Template,
			Capabilities:   spec.Capabilities,
			AutonomyPolicy: spec.AutonomyPolicy,
			RepoScope:      payload.Repo,
		}, payload), true
	}
	if store == nil {
		return runtime.Agent{}, false
	}
	agent, err := store.GetAgent(ctx, job.Agent)
	if err != nil {
		return runtime.Agent{}, false
	}
	return applyJobRuntimeOverride(runtimeAgent(agent), payload), true
}
