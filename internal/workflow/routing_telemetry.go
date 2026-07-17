package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// maxRouterContextRows bounds the observed-performance table injected into a
// coordinator prompt (#530) so the block never dominates the prompt. With the 2
// header lines + trailing note this keeps the whole block at or below 12 lines,
// matching the spec's bound.
const maxRouterContextRows = 9

// recordRoutingTelemetry writes one advisory routing observation at the job
// terminal chokepoint (#530). It is best-effort and fail-safe: EVERY error is
// swallowed so a telemetry write can never fail a job. It is called AFTER the
// terminal transition so it records the settled job state. Model/runtime are the
// effective values the delivery used (a #531 per-job runtime override has already
// been applied to agent.Runtime by the caller); tokens are re-read from the job
// row where the delivery persisted them (0 for a runtime that reports none).
func (m Mailbox) recordRoutingTelemetry(ctx context.Context, job db.Job, agent runtime.Agent, payload JobPayload, result AgentResult, state JobState, duration time.Duration) {
	if m.Store == nil {
		return
	}
	// Use the delivery resolver so telemetry cannot drift from the runtime's model
	// precedence. Inputs are trimmed here to preserve this advisory surface's
	// historical normalization behavior.
	runtimeDefaultModel := ""
	if m.RuntimeDefaultModel != nil {
		runtimeDefaultModel = m.RuntimeDefaultModel(strings.TrimSpace(agent.Runtime))
	}
	model := runtime.EffectiveModel(
		runtime.Agent{Model: strings.TrimSpace(agent.Model)},
		runtime.Job{Model: strings.TrimSpace(payload.Model), RuntimeDefaultModel: runtimeDefaultModel},
	)
	durationMS := duration.Milliseconds()
	if durationMS < 0 {
		durationMS = 0
	}
	// Re-read the persisted per-job usage so the telemetry token counts match the
	// delegation budget's view (deliver() accumulates usage onto the job row). A
	// read error just leaves tokens at 0 — advisory only.
	var inTok, outTok int
	if fresh, err := m.Store.GetJob(ctx, job.ID); err == nil {
		inTok, outTok = fresh.InputTokens, fresh.OutputTokens
	}
	_ = m.Store.InsertRoutingTelemetry(ctx, db.RoutingTelemetry{
		JobID:          job.ID,
		Repo:           payload.Repo,
		Action:         job.Type,
		Phase:          strings.TrimSpace(payload.Phase),
		Runtime:        strings.TrimSpace(agent.Runtime),
		Model:          model,
		Agent:          job.Agent,
		TemplateID:     strings.TrimSpace(payload.TemplateID),
		TemplateCommit: strings.TrimSpace(payload.TemplateResolvedCommit),
		JobState:       string(state),
		Decision:       strings.TrimSpace(result.Decision),
		Approved:       delegationDecisionApproves(result.Decision),
		TestsRun:       len(result.TestsRun),
		DurationMS:     durationMS,
		InputTokens:    inTok,
		OutputTokens:   outTok,
	})
}

// buildRouterContextBlock renders the bounded, off-by-default observed-performance
// table injected into a coordinator prompt (#530) so a coordinator can weigh which
// runtime/model/template has historically done well on this repo. It is ADVISORY:
// the block is explicitly labeled as local observed performance, not a benchmark,
// and nothing forces a route. It returns "" (append nothing) when there are no
// observations yet, so an enabled-but-empty install is still byte-identical to the
// no-context prompt. The caller gates invocation on the [router] context_enabled
// knob AND on the job being a top-level (coordinator) job, so with the knob off no
// query runs and the prompt is byte-identical.
func (m Mailbox) buildRouterContextBlock(ctx context.Context, payload JobPayload) string {
	if m.Store == nil {
		return ""
	}
	rows, err := m.Store.ListRoutingTelemetry(ctx, db.RoutingTelemetryFilter{Repo: strings.TrimSpace(payload.Repo)})
	if err != nil || len(rows) == 0 {
		return ""
	}
	groups := db.AggregateRoutingTelemetry(rows)
	if len(groups) == 0 {
		return ""
	}
	if len(groups) > maxRouterContextRows {
		groups = groups[:maxRouterContextRows]
	}
	var b strings.Builder
	b.WriteString("Observed routing performance (local observed performance, not a benchmark):\n")
	b.WriteString("action | runtime | model | n | success | approval\n")
	for _, g := range groups {
		model := g.Model
		if model == "" {
			model = "default"
		}
		b.WriteString(fmt.Sprintf("%s | %s | %s | %d | %.0f%% | %.0f%%\n",
			nonEmpty(g.Action), nonEmpty(g.Runtime), model, g.Count,
			g.SuccessRate*100, g.ApprovalRate*100))
	}
	return strings.TrimRight(b.String(), "\n")
}

func nonEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
