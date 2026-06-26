package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// reviewDiffMaxBytes bounds the PR diff text injected into the reviewer prompt so
// a huge PR cannot blow the reviewer's context. Beyond it the diff is truncated
// (the reviewer still has TaskTitle/Instructions/Goal + ChangesMade) — a graceful
// degrade, not a failure.
const reviewDiffMaxBytes = 60_000

// reviewDiffFileReader is the minimal read the cross-family review dispatcher needs
// for the read-only scope-fidelity diff (github.Client satisfies it). It is its own
// narrow interface so the dispatcher is unit-testable with a stub and so the read
// is provably read-only (list files at HEAD, never a write).
type reviewDiffFileReader interface {
	ListPullRequestFiles(ctx context.Context, repo github.Repository, number int64) ([]github.PullRequestFile, error)
}

// reviewAdapterBuilder builds the runtime adapter for the chosen reviewer agent.
// It matches buildRuntimeAdapter's shape (agent, checkout, runner) and is
// injectable so tests can substitute a shell-runtime stub reviewer.
type reviewAdapterBuilder func(agent runtime.Agent, checkout string, runner subprocess.Runner) (workflow.DeliveryAdapter, error)

// reviewAuthedRuntimes reports which runtime families are authed/available so the
// cross-family selector only materializes an ephemeral leg on a runtime that can
// actually run. It is injectable for tests; the daemon supplies the real probe.
type reviewAuthedRuntimes func(ctx context.Context) map[string]bool

// crossFamilyReviewDispatcher is the concrete workflow.ReviewLegDispatcher (#469):
// it picks a cross-family reviewer (else a same-family fallback WITH WARNING,
// REFINEMENT #1), assembles the read-only review prompt (intended scope vs the PR
// diff + ChangesMade), runs the review leg through the runtime adapter, parses the
// rubric, and returns an Outcome{Kind:OutcomeReviewed} for the engine to harvest.
// Every leg is read-only and best-effort: the engine calls it OFF the blocking
// merge path and swallows its error.
type crossFamilyReviewDispatcher struct {
	store        *db.Store
	diff         reviewDiffFileReader
	buildAdapter reviewAdapterBuilder
	authed       reviewAuthedRuntimes
	checkout     string
}

var _ workflow.ReviewLegDispatcher = (*crossFamilyReviewDispatcher)(nil)

// Review runs the cross-family review leg for a just-merged implement job and
// returns its OutcomeReviewed rubric (#469). ok=false means NO review-capable
// runtime was authed at all (skip, no review row). It NEVER mutates the merge: it
// reads the diff read-only, runs a read-only reviewer, and returns a value the
// engine harvests into the auto-trace run.
func (d *crossFamilyReviewDispatcher) Review(ctx context.Context, implementJob db.Job, implementPayload workflow.JobPayload, mergedHead string) (workflow.Outcome, bool, error) {
	implementerRuntime := d.resolveImplementerRuntime(ctx, implementJob, implementPayload)

	authed := map[string]bool{}
	if d.authed != nil {
		authed = d.authed(ctx)
	}
	reviewer, ok, err := workflow.PickCrossFamilyReviewer(ctx, d.store, implementerRuntime, implementPayload.Repo, authed)
	if err != nil {
		return workflow.Outcome{}, false, err
	}
	if !ok {
		return workflow.Outcome{}, false, nil
	}
	if reviewer.SelfFamily {
		// REFINEMENT #1: never silently same-family. Emit a best-effort warning event
		// + log so an operator sees that self-preference bias applies to this row.
		d.warnSameFamily(ctx, implementJob, reviewer)
	}

	goalTitle := d.resolveGoalTitle(ctx, implementPayload.GoalID)
	diff := d.fetchDiff(ctx, implementPayload.Repo, implementPayload.PullRequest)
	prompt := workflow.ReviewLegPrompt(implementPayload, goalTitle, diff)

	rubric, findings, err := d.runReviewLeg(ctx, reviewer, implementPayload, prompt)
	if err != nil {
		return workflow.Outcome{}, false, err
	}

	return workflow.Outcome{
		Kind:        workflow.OutcomeReviewed,
		Repo:        implementPayload.Repo,
		PullRequest: implementPayload.PullRequest,
		HeadSHA:     mergedHead,
		Reviewer:    reviewer.Runtime,
		SelfFamily:  reviewer.SelfFamily,
		Rubric:      rubric,
		Findings:    findings,
	}, true, nil
}

// resolveImplementerRuntime recovers the implement job's runtime family from its
// payload (an ephemeral spec's runtime) or its registered agent. An unrecoverable
// family yields "" so pickCrossFamilyReviewer SKIPs rather than risk a silent
// same-family review (#469 risk: SKIP-not-guess).
func (d *crossFamilyReviewDispatcher) resolveImplementerRuntime(ctx context.Context, job db.Job, payload workflow.JobPayload) string {
	if payload.Ephemeral != nil && strings.TrimSpace(payload.Ephemeral.Runtime) != "" {
		return strings.TrimSpace(payload.Ephemeral.Runtime)
	}
	agentName := strings.TrimSpace(job.Agent)
	if agentName == "" || d.store == nil {
		return ""
	}
	agent, err := d.store.GetAgent(ctx, agentName)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(agent.Runtime)
}

// resolveGoalTitle resolves the owning goal's title for the scope-fidelity intended
// scope (best-effort: empty on any miss).
func (d *crossFamilyReviewDispatcher) resolveGoalTitle(ctx context.Context, goalID string) string {
	goalID = strings.TrimSpace(goalID)
	if goalID == "" || d.store == nil {
		return ""
	}
	goals, err := d.store.ListGoals(ctx)
	if err != nil {
		return ""
	}
	for _, goal := range goals {
		if strings.TrimSpace(goal.ID) == goalID {
			return strings.TrimSpace(goal.Title)
		}
	}
	return ""
}

// fetchDiff reads the PR diff read-only (file patches) for the delivered-work side
// of the scope-fidelity comparison. It degrades to "" (the reviewer then leans on
// ChangesMade) when the read fails or there is no PR, per the #469 risk note.
func (d *crossFamilyReviewDispatcher) fetchDiff(ctx context.Context, repo string, pullRequest int) string {
	if d.diff == nil || pullRequest <= 0 {
		return ""
	}
	parsed, ok := parseReviewRepo(repo)
	if !ok {
		return ""
	}
	files, err := d.diff.ListPullRequestFiles(ctx, parsed, int64(pullRequest))
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, file := range files {
		b.WriteString("--- " + strings.TrimSpace(file.Filename) + " ---\n")
		if patch := strings.TrimSpace(file.Patch); patch != "" {
			b.WriteString(patch + "\n")
		}
		if b.Len() > reviewDiffMaxBytes {
			b.WriteString("\n[diff truncated]\n")
			break
		}
	}
	return b.String()
}

// runReviewLeg builds the read-only reviewer agent + job and delivers the prompt
// through the runtime adapter, then parses the rubric from the worker's output
// (#469). The reviewer agent is read-only (ephemeral) or the registered reviewer.
func (d *crossFamilyReviewDispatcher) runReviewLeg(ctx context.Context, reviewer workflow.CrossFamilyReviewer, payload workflow.JobPayload, prompt string) (map[string]float64, string, error) {
	if d.buildAdapter == nil {
		return nil, "", fmt.Errorf("no reviewer adapter builder")
	}
	agent, err := d.reviewerAgent(ctx, reviewer)
	if err != nil {
		return nil, "", err
	}
	adapter, err := d.buildAdapter(agent, d.checkout, nil)
	if err != nil {
		return nil, "", err
	}
	result, err := adapter.Deliver(ctx, agent, runtime.Job{
		AgentName:   agent.Name,
		Action:      "review",
		Prompt:      prompt,
		Repository:  strings.TrimSpace(payload.Repo),
		PullRequest: payload.PullRequest,
	})
	if err != nil {
		return nil, "", err
	}
	// The reviewer returns the rubric under gitmoot_result.metadata.rubric, which is
	// NOT a field ExtractAgentResult accepts (it is strict), so the rubric AND the
	// summary are parsed directly from the raw output. ParseReviewRubric then keeps
	// only the known dimensions (clamped) and the findings text.
	rubric, summary := parseReviewOutput(result.Raw)
	out := workflow.ParseReviewRubric(workflow.AgentResult{Summary: summary}, rubric)
	return out.Rubric, out.Findings, nil
}

// reviewerAgent builds the runtime.Agent for the chosen reviewer: a registered
// agent is loaded from the store; an ephemeral reviewer is synthesized read-only.
func (d *crossFamilyReviewDispatcher) reviewerAgent(ctx context.Context, reviewer workflow.CrossFamilyReviewer) (runtime.Agent, error) {
	if reviewer.RegisteredAgent != "" && d.store != nil {
		agent, err := d.store.GetAgent(ctx, reviewer.RegisteredAgent)
		if err != nil {
			return runtime.Agent{}, err
		}
		role := strings.TrimSpace(agent.Role)
		if role == "" {
			role = "reviewer"
		}
		return runtime.Agent{
			Name:           agent.Name,
			Role:           role,
			Runtime:        agent.Runtime,
			RuntimeRef:     agent.RuntimeRef,
			RepoScope:      agent.RepoScope,
			Capabilities:   agent.Capabilities,
			AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
			Model:          agent.Model,
		}, nil
	}
	// Ephemeral read-only reviewer.
	return runtime.Agent{
		Name:           "gitmoot-review-" + reviewer.Runtime,
		Role:           "reviewer",
		Runtime:        reviewer.Runtime,
		Capabilities:   []string{"ask", "review"},
		AutonomyPolicy: runtime.AutonomyPolicyReadOnly,
	}, nil
}

// warnSameFamily emits the REFINEMENT #1 best-effort warning (job event + log) on
// the same-family fallback path so the bias is never silent.
func (d *crossFamilyReviewDispatcher) warnSameFamily(ctx context.Context, job db.Job, reviewer workflow.CrossFamilyReviewer) {
	msg := fmt.Sprintf("no different-family reviewer available; fell back to a SAME-family %s reviewer — self-preference bias applies, this signal weights below a cross-family review", reviewer.Runtime)
	if d.store != nil {
		_ = d.store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "cross_family_review_samefamily_fallback",
			Message: msg,
		})
	}
	log.Printf("cross_family_review: %s (job %s)", msg, job.ID)
}

// parseReviewOutput extracts the rubric (under gitmoot_result.metadata.rubric, also
// tolerating a top-level rubric) AND the reviewer's summary/findings from the raw
// worker output. It is lenient: an absent/malformed rubric yields nil so
// ParseReviewRubric produces an empty (HasScore=false) signal rather than a
// fabricated score. The rubric lives under metadata.rubric — a field the strict
// ExtractAgentResult rejects — so it is parsed here directly.
func parseReviewOutput(raw string) (map[string]float64, string) {
	for _, candidate := range jsonCandidates(raw) {
		var envelope struct {
			GitmootResult struct {
				Summary  string `json:"summary"`
				Metadata struct {
					Rubric map[string]float64 `json:"rubric"`
				} `json:"metadata"`
				Rubric map[string]float64 `json:"rubric"`
			} `json:"gitmoot_result"`
		}
		if err := json.Unmarshal([]byte(candidate), &envelope); err != nil {
			continue
		}
		rubric := envelope.GitmootResult.Metadata.Rubric
		if len(rubric) == 0 {
			rubric = envelope.GitmootResult.Rubric
		}
		summary := strings.TrimSpace(envelope.GitmootResult.Summary)
		if len(rubric) > 0 || summary != "" {
			return rubric, summary
		}
	}
	return nil, ""
}

// jsonCandidates returns brace-balanced JSON object candidates from raw output so
// the rubric can be recovered from a result wrapped in surrounding text.
func jsonCandidates(raw string) []string {
	var out []string
	depth := 0
	start := -1
	for i, r := range raw {
		switch r {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, raw[start:i+1])
					start = -1
				}
			}
		}
	}
	return out
}

// parseReviewRepo splits an "owner/name" repo for the diff read.
func parseReviewRepo(value string) (github.Repository, bool) {
	owner, name, ok := strings.Cut(strings.TrimSpace(value), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return github.Repository{}, false
	}
	return github.Repository{Owner: owner, Name: name}, true
}
