package cli

import (
	"context"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// daemonOutcomeHarvesterWithCanary returns the daemon's Mode-A trace-harvester,
// wrapped with the off-by-default #484 canary regression evaluator when canary
// promotion is configured. When the base harvester is nil
// ([skillopt].auto_trace_enabled OFF — the default), it returns nil unchanged so
// the engine harvests nothing and behavior is byte-identical. When auto_trace is
// on but canary is off, it returns the bare base harvester, so the only path that
// adds the post-harvest regression check is the fully-configured canary one. The
// wrapper is gated on CanaryEnabled() (auto_promote_canary AND a valid sample), so
// a malformed config or an unset sample never starts the evaluator. Because the
// evaluator only acts when an active `canary` row exists — which is only ever
// written when auto_promote fires in canary mode — it is a no-op until a canary is
// live, even when constructed.
func daemonOutcomeHarvesterWithCanary(store *db.Store, gh github.Client, home string) workflow.OutcomeHarvester {
	base := daemonOutcomeHarvester(store, gh, home)
	if base == nil {
		return nil
	}
	policy, err := loadSkillOptPolicy(home)
	if err != nil || !policy.CanaryEnabled() {
		return base
	}
	return &canaryRegressionHarvester{
		inner:      base,
		store:      store,
		sink:       daemonEventSink(store, home),
		minSamples: policy.AutoPromoteMinSamples,
	}
}

// canaryRegressionHarvester decorates the Mode-A OutcomeHarvester (#465) with the
// #484 bounded regression window: after the inner harvester writes a verifiable
// outcome (which is what attributes a canary-routed job's outcome to the canary
// version's auto-trace run), it loads the active canary + the prior champion
// auto-trace runs and runs the pure skillopt.EvaluateCanaryRegression comparator,
// acting on the verdict via the EXISTING store transactions. It is best-effort:
// the regression evaluation NEVER changes the inner harvester's error (which the
// engine records as auto_trace_harvest_failed) and every store/read error in the
// evaluation is swallowed (fail-safe: leave the champion live, keep the canary).
type canaryRegressionHarvester struct {
	inner      workflow.OutcomeHarvester
	store      *db.Store
	sink       events.Sink
	minSamples *int
}

var _ workflow.OutcomeHarvester = (*canaryRegressionHarvester)(nil)

func (h *canaryRegressionHarvester) Harvest(ctx context.Context, job db.Job, payload workflow.JobPayload, outcome workflow.Outcome) error {
	err := h.inner.Harvest(ctx, job, payload, outcome)
	// Run the regression window AFTER the inner harvest so the just-written outcome
	// is counted. Best-effort and independent of the inner result.
	h.evaluate(ctx, payload)
	return err
}

// evaluate runs the bounded regression window for the job's template, if a canary
// is active, and graduates / rolls back / holds per the pure comparator. It is
// fail-safe throughout: any read or store error returns early (leaving the
// champion live and the canary sampling), and it NEVER leaves the template without
// a current version — on rollback the prior champion is already the live current
// version (the canary never displaced it), so the rollback retires the canary via
// the EXISTING RejectAgentTemplateVersion; the EXISTING RevertAgentTemplateVersion
// is reused only defensively, to restore the champion if it had somehow been
// superseded. Graduate reuses PromoteAgentTemplateVersion.
func (h *canaryRegressionHarvester) evaluate(ctx context.Context, payload workflow.JobPayload) {
	if h == nil || h.store == nil {
		return
	}
	templateID := payload.TemplateID
	if templateID == "" {
		return
	}
	canary, found, err := h.store.GetActiveCanaryVersion(ctx, templateID)
	if err != nil || !found {
		return
	}
	template, err := h.store.GetAgentTemplate(ctx, templateID)
	if err != nil {
		return
	}
	championID := template.VersionID
	if championID == "" || championID == canary.ID {
		// No distinct champion to compare against; hold (fail-safe).
		return
	}
	canaryEvents, canaryErr := h.store.ListFeedbackEvents(ctx, skillopt.AutoTraceRunID(canary.ID))
	championEvents, championErr := h.store.ListFeedbackEvents(ctx, skillopt.AutoTraceRunID(championID))
	minSamples := 0
	if h.minSamples != nil {
		minSamples = *h.minSamples
	}
	verdict := skillopt.EvaluateCanaryRegression(canaryEvents, championEvents, minSamples, canaryErr != nil, championErr != nil)
	switch verdict.Decision {
	case skillopt.CanaryRollback:
		// Guarantee the prior champion is the live current version BEFORE retiring the
		// canary, so a valid current version exists at every step. In the normal canary
		// flow the champion never lost current (the canary sat behind it), so this is a
		// no-op; the defensive RevertAgentTemplateVersion only fires if the champion had
		// somehow been superseded (reusing the EXISTING rollback primitive).
		if champion, err := h.store.GetAgentTemplateVersionByID(ctx, championID); err == nil && champion.State == "superseded" {
			if _, err := h.store.RevertAgentTemplateVersion(ctx, templateID, championID); err != nil {
				return
			}
		}
		rejected, err := h.store.RejectAgentTemplateVersion(ctx, canary.ID, verdict.Reason)
		if err != nil {
			return
		}
		emitCandidateEvent(ctx, h.sink, events.EventCandidateRolledBack, rejected, "rolled_back", verdict.Reason)
	case skillopt.CanaryGraduate:
		promoted, err := h.store.PromoteAgentTemplateVersion(ctx, canary.ID)
		if err != nil {
			return
		}
		emitCandidateEvent(ctx, h.sink, events.EventCandidateAutoPromoted, promoted, "auto_promoted", verdict.Reason)
	default:
		// CanaryContinue: keep sampling; no state change.
	}
}
