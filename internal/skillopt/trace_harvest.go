package skillopt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// gitmootNoCIContext is the synthetic commit-status context the merge gate writes
// (internal/workflow/merge_gate.go) EXACTLY when a PR head carries zero external
// checks and zero external statuses — i.e. it merged through an EMPTY gate. The
// no-CI guard treats its presence (or the absence of any non-gitmoot/ external
// status) as "no real CI" and demotes the merge to near-neutral so the harvester
// never rewards "merges that pass no real CI" (#465, #463 guardrail).
const gitmootNoCIContext = "gitmoot/ci"

// autoTraceSource is the FeedbackEvent.source tag every synthetic auto-trace row
// carries (#465). Combined with autoTraceReviewer it makes auto feedback
// identifiable per-event even within a mixed package, and the UNIQUE
// (run_id,item_id,reviewer,source,source_url) key it participates in is what lets
// a later revert overwrite the prior positive in place.
const autoTraceSource = "auto-trace"

// autoTraceReviewer is the FeedbackEvent.reviewer attributed to every synthetic
// auto-trace row (#465): a sentinel non-human reviewer so auto feedback is never
// mistaken for a human ranking.
const autoTraceReviewer = "gitmoot-auto"

// autoTraceRunIDPrefix namespaces the dedicated per-template-version auto-trace
// eval_run id ("auto-trace:"+versionID) so human-gold runs stay entirely
// untouched and the export/optimizer can filter or down-weight the auto namespace
// independently (#465).
const autoTraceRunIDPrefix = "auto-trace:"

// Score bands for the verifiable-outcome → {score, feedback} projection (#465).
// These feed a synthetic EvaluatorScore that ProjectSignal fuses into one
// NormalizedSignal, keeping signal assembly in the single canonical place.
const (
	// scoreMergedRealCI is the strong-positive Soft score for a merge that passed a
	// genuine external CI (a non-gitmoot/ status or check that succeeded).
	scoreMergedRealCI = 1.0
	// scoreMergedNoCI is the near-neutral Soft score for a merge through an empty
	// gate (the synthetic gitmoot/ci context, or no external CI at head): not a
	// strong positive, but not a negative either — absence of verifiable evidence.
	scoreMergedNoCI = 0.5
	// scoreChangesRequestedBase is the first-fix-round Soft score for a
	// changes_requested negative; each additional fix round subtracts
	// scoreChangesRequestedStep toward 0.
	scoreChangesRequestedBase = 0.6
	scoreChangesRequestedStep = 0.2
)

// CombinedStatusReader is the minimal GitHub read the no-CI guard needs (#465):
// the combined commit status at a merged head SHA. github.Client satisfies it.
// It is its own narrow interface so the harvester can be unit-tested with a stub
// and so the skillopt package depends only on the read it actually uses.
type CombinedStatusReader interface {
	GetCombinedStatus(ctx context.Context, repo github.Repository, ref string) (github.CombinedStatus, error)
}

// OutcomeHarvester implements workflow.OutcomeHarvester (#465, Mode A): on a
// verifiable implement-job outcome transition it projects the outcome into a
// synthetic {score, feedback} FeedbackEvent and writes it (plus its dedicated
// per-template-version eval_run and a per-PR eval_review_item) into the EXISTING
// feedback tables, tagged source=auto-trace / reviewer=gitmoot-auto. It writes
// ONLY eval_runs/eval_review_items/feedback_events — never a candidate or
// promotion path, so promotion stays 100% manual. It is constructed only when
// [skillopt].auto_trace_enabled is on; the engine calls it best-effort and
// nil-safe, so a Harvest error never blocks a job.
type OutcomeHarvester struct {
	// Store is the SQLite store the synthetic rows are written to.
	Store *db.Store
	// Status is the no-CI guard's combined-status reader. It is optional: when nil
	// (or a read fails), a merge is treated conservatively as no-real-CI
	// (near-neutral) rather than rewarded as a strong positive, so a missing/erroring
	// read degrades to the safe band and never blocks the (already-completed) merge.
	Status CombinedStatusReader
}

// NewOutcomeHarvester constructs the Mode-A harvester (#465). The returned value
// satisfies workflow.OutcomeHarvester.
func NewOutcomeHarvester(store *db.Store, status CombinedStatusReader) *OutcomeHarvester {
	return &OutcomeHarvester{Store: store, Status: status}
}

var _ workflow.OutcomeHarvester = (*OutcomeHarvester)(nil)

// Harvest projects a verifiable implement-job outcome into a synthetic
// FeedbackEvent for the job's template version (#465). It is in-scope only for
// implement-family jobs that carry a template attribution and are NOT coordinator
// continuation jobs; an out-of-scope job returns nil (no rows written). All writes
// are best-effort from the engine's perspective: a returned error is swallowed by
// the engine and recorded as an auto_trace_harvest_failed job event.
func (h *OutcomeHarvester) Harvest(ctx context.Context, job db.Job, payload workflow.JobPayload, outcome workflow.Outcome) error {
	if h == nil || h.Store == nil {
		return nil
	}
	if !inScope(job, payload) {
		return nil
	}
	version, ok, err := h.resolveTemplateVersion(ctx, payload)
	if err != nil {
		return err
	}
	if !ok {
		// Unresolvable template version: skip rather than guess (#465 risk note).
		return nil
	}
	signal, choice := h.project(ctx, payload, outcome)
	return h.writeFeedback(ctx, version, payload, outcome, signal, choice)
}

// inScope reports whether a job is an implement-family job the harvester should
// score (#465): it must be an implement job that carries a template attribution
// and must NOT be a coordinator continuation (a job with a parent/delegation but
// no own diff/PR). A coordinator continuation produces no diff of its own, so
// scoring it would mis-attribute the children's outcome to the coordinator's
// template — it is skipped.
func inScope(job db.Job, payload workflow.JobPayload) bool {
	if job.Type != "implement" {
		return false
	}
	if strings.TrimSpace(payload.TemplateID) == "" {
		return false
	}
	if isCoordinatorContinuation(payload) {
		return false
	}
	return true
}

// isCoordinatorContinuation reports whether the payload is a coordinator
// continuation job (the Orchestra pattern's synthesis leg): it carries a
// delegation/parent linkage or is flagged as a finalize continuation but produced
// no PR of its own. Such a job has no diff to attribute an outcome to, so it is
// out of scope for the harvester.
func isCoordinatorContinuation(payload workflow.JobPayload) bool {
	if payload.DelegationFinalize {
		return true
	}
	// A delegation/continuation leg with no own PR has no diff to score.
	if strings.TrimSpace(payload.ParentJobID) != "" && payload.PullRequest <= 0 {
		return true
	}
	if strings.TrimSpace(payload.DelegationID) != "" && payload.PullRequest <= 0 {
		return true
	}
	return false
}

// resolveTemplateVersion resolves the job's template version id from its payload
// attribution (#465). It prefers TemplateResolvedCommit (the exact version the
// job ran) matched against the template's versions, falling back to the
// template's current version when the commit cannot be matched. Returns ok=false
// (skip, do not guess) when neither the template nor a version can be resolved.
func (h *OutcomeHarvester) resolveTemplateVersion(ctx context.Context, payload workflow.JobPayload) (db.AgentTemplateVersion, bool, error) {
	templateID := strings.TrimSpace(payload.TemplateID)
	if templateID == "" {
		return db.AgentTemplateVersion{}, false, nil
	}
	resolvedCommit := strings.TrimSpace(payload.TemplateResolvedCommit)
	if resolvedCommit != "" {
		versions, err := h.Store.ListAgentTemplateVersions(ctx, templateID)
		if err == nil {
			for _, version := range versions {
				if strings.TrimSpace(version.ResolvedCommit) == resolvedCommit {
					return version, true, nil
				}
			}
		}
	}
	// Fall back to the template's current promoted version.
	template, err := h.Store.GetAgentTemplate(ctx, templateID)
	if err != nil {
		// Best-effort: an unresolvable template is skipped, not an error that would
		// spam auto_trace_harvest_failed for every legacy/missing-template job.
		return db.AgentTemplateVersion{}, false, nil
	}
	versionID := strings.TrimSpace(template.VersionID)
	if versionID == "" {
		return db.AgentTemplateVersion{}, false, nil
	}
	version, err := h.Store.GetAgentTemplateVersionByID(ctx, versionID)
	if err != nil {
		return db.AgentTemplateVersion{}, false, nil
	}
	return version, true, nil
}

// project maps a verifiable outcome to a NormalizedSignal (via ProjectSignal over
// a synthetic EvaluatorScore) and the a/b feedback choice (#465). A positive
// outcome is choice "a" (the current promoted template as the implicit baseline
// champion); a negative is choice "b". The no-CI guard runs here for a merge.
func (h *OutcomeHarvester) project(ctx context.Context, payload workflow.JobPayload, outcome workflow.Outcome) (NormalizedSignal, string) {
	switch outcome.Kind {
	case workflow.OutcomeMerged:
		if h.mergeHasRealCI(ctx, outcome) {
			return positiveSignal(scoreMergedRealCI,
				fmt.Sprintf("PR #%d merged with passing external CI.", outcome.PullRequest)), "a"
		}
		return positiveSignal(scoreMergedNoCI,
			fmt.Sprintf("PR #%d merged through an empty gate (no external CI); near-neutral, not a strong positive.", outcome.PullRequest)), "a"
	case workflow.OutcomeBlocked:
		reason := strings.TrimSpace(outcome.Reason)
		if reason == "" {
			reason = "merge gate rejected the action"
		}
		return negativeSignal(0,
			fmt.Sprintf("PR #%d was blocked at the merge gate: %s", outcome.PullRequest, reason)), "b"
	case workflow.OutcomeChangesRequested:
		score := changesRequestedScore(outcome.FixRounds)
		reason := strings.TrimSpace(outcome.Reason)
		detail := fmt.Sprintf("Review requested changes on PR #%d (fix round %d).", outcome.PullRequest, fixRoundFloor(outcome.FixRounds))
		if reason != "" {
			detail += " " + reason
		}
		return gradedSignal(score, detail), "b"
	case workflow.OutcomeReverted:
		return negativeSignal(0,
			fmt.Sprintf("PR #%d was later reverted; the prior positive is corrected to a negative.", outcome.PullRequest)), "b"
	default:
		// An unknown outcome kind is treated as a weak neutral negative rather than a
		// strong signal; it should never occur because the engine only emits the four
		// known kinds.
		return gradedSignal(scoreMergedNoCI,
			fmt.Sprintf("Unclassified outcome on PR #%d.", outcome.PullRequest)), "b"
	}
}

// mergeHasRealCI reports whether the merged head carries a GENUINE external CI
// success — a non-gitmoot/ commit status that succeeded — so the no-CI guard can
// distinguish a real CI pass (strong+) from an empty-gate merge (near-neutral).
// It reads the combined status at the merged head SHA best-effort: a nil reader,
// a read error, or the synthetic gitmoot/ci context (with no external status) all
// degrade to false (no real CI), so a missing read is never rewarded as a strong
// positive (#465).
func (h *OutcomeHarvester) mergeHasRealCI(ctx context.Context, outcome workflow.Outcome) bool {
	if h.Status == nil {
		return false
	}
	head := strings.TrimSpace(outcome.HeadSHA)
	if head == "" {
		return false
	}
	repo, ok := parseRepo(outcome.Repo)
	if !ok {
		return false
	}
	status, err := h.Status.GetCombinedStatus(ctx, repo, head)
	if err != nil {
		return false
	}
	for _, item := range status.Statuses {
		context := strings.TrimSpace(item.Context)
		if context == "" || strings.HasPrefix(context, "gitmoot/") {
			// gitmoot/* (including the synthetic gitmoot/ci and the merge-gate context)
			// are not external CI; skip them.
			continue
		}
		if strings.EqualFold(strings.TrimSpace(item.State), "success") {
			return true
		}
	}
	return false
}

// changesRequestedScore grades a changes_requested negative by fix-round count
// (#465): round 1 ≈ 0.6, each additional round subtracts a step toward 0.
func changesRequestedScore(fixRounds int) float64 {
	round := fixRoundFloor(fixRounds)
	score := scoreChangesRequestedBase - float64(round-1)*scoreChangesRequestedStep
	if score < 0 {
		score = 0
	}
	return score
}

// fixRoundFloor clamps a fix-round count to at least 1 so a first/legacy
// changes_requested always grades as round 1.
func fixRoundFloor(fixRounds int) int {
	if fixRounds < 1 {
		return 1
	}
	return fixRounds
}

// positiveSignal builds a NormalizedSignal for a positive outcome via
// ProjectSignal over a synthetic Soft EvaluatorScore, so signal assembly stays in
// the single canonical place (#465). The textual feedback rides RankedFeedbackEvent.Reasoning.
func positiveSignal(score float64, feedback string) NormalizedSignal {
	soft := score
	return ProjectSignal(&EvaluatorScore{Soft: &soft}, &RankedFeedbackEvent{Reasoning: feedback}, nil)
}

// gradedSignal builds a NormalizedSignal for a graded (Soft) outcome.
func gradedSignal(score float64, feedback string) NormalizedSignal {
	soft := score
	return ProjectSignal(&EvaluatorScore{Soft: &soft}, &RankedFeedbackEvent{Reasoning: feedback}, nil)
}

// negativeSignal builds a NormalizedSignal for an authoritative gate-fail
// negative via a synthetic Hard==0 EvaluatorScore (ProjectSignal reads Hard==0 as
// an informative 0, HasScore=true) and an optimizer-hint failure packet so the
// feedback explains the failure (#465).
func negativeSignal(hard float64, feedback string) NormalizedSignal {
	h := hard
	return ProjectSignal(&EvaluatorScore{Hard: &h}, nil, &EvaluatorFailurePacket{OptimizerHint: feedback})
}

// writeFeedback upserts the dedicated per-template-version auto-trace eval_run,
// the per-PR eval_review_item, and the synthetic FeedbackEvent for one outcome
// (#465). The FeedbackEvent's UNIQUE (run_id,item_id,reviewer,source,source_url)
// key is deterministic per PR, so a later revert re-upserts the SAME row in place
// (corrective overwrite, row count unchanged).
func (h *OutcomeHarvester) writeFeedback(ctx context.Context, version db.AgentTemplateVersion, payload workflow.JobPayload, outcome workflow.Outcome, signal NormalizedSignal, choice string) error {
	runID := autoTraceRunIDPrefix + strings.TrimSpace(version.ID)
	itemID := autoTraceItemID(outcome)
	sourceURL := pullRequestURL(outcome.Repo, outcome.PullRequest)

	if err := h.Store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                runID,
		TemplateID:        strings.TrimSpace(version.TemplateID),
		TemplateVersionID: strings.TrimSpace(version.ID),
		TargetRepo:        strings.TrimSpace(outcome.Repo),
		State:             "ready",
		Mode:              db.EvalRunModeValidate,
		MetadataJSON:      autoTraceRunMetadata(),
	}); err != nil {
		return fmt.Errorf("upsert auto-trace eval_run: %w", err)
	}

	if err := h.Store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:        runID,
		ItemID:       itemID,
		Title:        autoTraceItemTitle(outcome),
		MetadataJSON: autoTraceItemMetadata(outcome),
	}); err != nil {
		return fmt.Errorf("upsert auto-trace eval_review_item: %w", err)
	}

	if err := h.Store.UpsertFeedbackEvent(ctx, db.FeedbackEvent{
		RunID:     runID,
		ItemID:    itemID,
		Choice:    choice,
		Reasoning: signal.Feedback,
		Reviewer:  autoTraceReviewer,
		Source:    autoTraceSource,
		SourceURL: sourceURL,
	}); err != nil {
		return fmt.Errorf("upsert auto-trace feedback_event: %w", err)
	}
	return nil
}

// autoTraceRunMetadata is the eval_run metadata_json for the auto-trace run. It
// carries feedback_source=automatic_trace so ExportTrainingPackage stamps the
// run-level feedback_source as automatic_trace (and the run is filterable),
// WITHOUT a new contract field, and the validate mode so evaluatorConfig stays
// empty.
func autoTraceRunMetadata() string {
	raw, _ := json.Marshal(map[string]any{
		feedbackSourceMetadataKey: FeedbackSourceAutomaticTrace,
		"mode":                    db.EvalRunModeValidate,
	})
	return string(raw)
}

// autoTraceItemID is the per-PR/per-job eval item id (#465 risk note): distinct
// PRs must not collide under one UNIQUE(run_id,item_id), but the SAME PR being
// re-evaluated (e.g. a merge then a revert) MUST map to the same item so the
// revert overwrites the prior row. It is keyed by repo#pr.
func autoTraceItemID(outcome workflow.Outcome) string {
	repo := strings.TrimSpace(outcome.Repo)
	if outcome.PullRequest > 0 {
		return fmt.Sprintf("%s#%d", repo, outcome.PullRequest)
	}
	// No PR (defensive): hash the repo so the item is still deterministic.
	sum := sha256.Sum256([]byte(repo))
	return "pr-" + hex.EncodeToString(sum[:8])
}

func autoTraceItemTitle(outcome workflow.Outcome) string {
	repo := strings.TrimSpace(outcome.Repo)
	if outcome.PullRequest > 0 {
		return fmt.Sprintf("%s PR #%d", repo, outcome.PullRequest)
	}
	return repo
}

func autoTraceItemMetadata(outcome workflow.Outcome) string {
	raw, _ := json.Marshal(map[string]any{
		"repo":         strings.TrimSpace(outcome.Repo),
		"pull_request": outcome.PullRequest,
	})
	return string(raw)
}

// pullRequestURL builds the canonical PR html URL used as the FeedbackEvent
// source_url (part of the UNIQUE corrective-overwrite key). Empty repo/PR yields
// "" so a malformed outcome still produces a valid (empty source_url) key.
func pullRequestURL(repo string, pullRequest int) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || pullRequest <= 0 {
		return ""
	}
	return "https://github.com/" + repo + "/pull/" + strconv.Itoa(pullRequest)
}

// parseRepo splits an "owner/name" repo into a github.Repository for the no-CI
// guard's status read. It reports ok=false for a malformed repo (so the guard
// degrades to no-real-CI rather than panicking).
func parseRepo(value string) (github.Repository, bool) {
	owner, name, ok := strings.Cut(strings.TrimSpace(value), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return github.Repository{}, false
	}
	return github.Repository{Owner: owner, Name: name}, true
}
