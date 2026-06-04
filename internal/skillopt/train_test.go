package skillopt

import (
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestCanTransitionTrainIterationRequiresSequentialStates(t *testing.T) {
	if err := CanTransitionTrainIteration(TrainStateRequestConfirmed, TrainStateWorkspaceReady); err != nil {
		t.Fatalf("sequential transition returned error: %v", err)
	}
	err := CanTransitionTrainIteration(TrainStateFeedbackSynced, TrainStateOptimizerCompleted)
	if err == nil || !strings.Contains(err.Error(), TrainStateTrainingPackageCreated) {
		t.Fatalf("skip transition error = %v", err)
	}
	err = CanTransitionTrainIteration(TrainStateCandidateReviewPublished, TrainStateOptionsGenerated)
	if err == nil || !strings.Contains(err.Error(), "promote") {
		t.Fatalf("candidate decision transition error = %v", err)
	}
}

func TestCanTransitionTrainIterationAllowsTerminalDecisions(t *testing.T) {
	for _, terminal := range []string{TrainStateCandidatePromoted, TrainStateCandidateRejected, TrainStateRunAbandoned} {
		if err := CanTransitionTrainIteration(TrainStateCandidateReviewPublished, terminal); err != nil {
			t.Fatalf("transition to %s returned error: %v", terminal, err)
		}
	}
	if err := CanTransitionTrainIteration(TrainStateFeedbackSynced, TrainStateCandidatePromoted); err == nil || !strings.Contains(err.Error(), "candidate review") {
		t.Fatalf("early promotion error = %v", err)
	}
	if err := CanTransitionTrainIteration(TrainStateFeedbackSynced, TrainStateRunAbandoned); err != nil {
		t.Fatalf("abandon transition returned error: %v", err)
	}
	if err := CanTransitionTrainIteration(TrainStateCandidatePromoted, TrainStateItemsReady); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("terminal source transition error = %v", err)
	}
}

func TestCanTransitionTrainIterationAllowsNoCandidateAfterOptimizer(t *testing.T) {
	if err := CanTransitionTrainIteration(TrainStateOptimizerCompleted, TrainStateOptimizerCompletedNoCandidate); err != nil {
		t.Fatalf("no-candidate transition returned error: %v", err)
	}
	if err := CanTransitionTrainIteration(TrainStateTrainingPackageCreated, TrainStateOptimizerCompletedNoCandidate); err == nil || !strings.Contains(err.Error(), "optimizer completes") {
		t.Fatalf("early no-candidate transition error = %v", err)
	}
}

func TestCanStartNextTrainIterationRequiresResolvedPreviousIteration(t *testing.T) {
	if err := CanStartNextTrainIteration(db.SkillOptTrainIteration{ID: "iter-1", State: TrainStateCandidateCreated}); err == nil || !strings.Contains(err.Error(), TrainStateCandidateCreated) {
		t.Fatalf("active iteration error = %v", err)
	}
	if err := CanStartNextTrainIteration(db.SkillOptTrainIteration{ID: "iter-1"}); err == nil || !strings.Contains(err.Error(), "iter-1") {
		t.Fatalf("empty state iteration error = %v", err)
	}
	if err := CanStartNextTrainIteration(db.SkillOptTrainIteration{ID: "iter-1", State: TrainStateCandidateRejected}); err == nil || !strings.Contains(err.Error(), "decision reason") {
		t.Fatalf("rejected without reason error = %v", err)
	}
	for _, iteration := range []db.SkillOptTrainIteration{
		{ID: "iter-1", State: TrainStateOptimizerCompletedNoCandidate},
		{ID: "iter-1", State: TrainStateCandidatePromoted},
		{ID: "iter-1", State: TrainStateRunAbandoned},
		{ID: "iter-1", State: TrainStateCandidateRejected, DecisionReason: "too broad"},
	} {
		if err := CanStartNextTrainIteration(iteration); err != nil {
			t.Fatalf("CanStartNextTrainIteration(%+v) returned error: %v", iteration, err)
		}
	}
}

func TestBuildTrainStatusSummaryReportsNextAction(t *testing.T) {
	iteration := db.SkillOptTrainIteration{
		ID:                 "landing-page-001",
		State:              TrainStateFeedbackSynced,
		IssueURL:           "https://github.com/owner/repo/issues/67",
		CandidateVersionID: "planner@v3",
	}
	summary := BuildTrainStatusSummary(
		db.SkillOptTrainSession{ID: "landing-page", State: TrainStateWorkspaceReady},
		&iteration,
		TrainStatusCounts{ReviewItems: 4, RankedFeedbackEvents: 4, PairwisePreferences: 24},
	)
	if summary.SessionID != "landing-page" || summary.IterationID != "landing-page-001" || summary.CurrentPhase != TrainStateFeedbackSynced {
		t.Fatalf("summary identity = %+v", summary)
	}
	if summary.BlockedStep != TrainStateTrainingPackageCreated || !strings.Contains(summary.NextAction, "training package") {
		t.Fatalf("summary next action = %+v", summary)
	}
	if summary.FeedbackCount != 4 || summary.IssueURL == "" || summary.CandidateVersion != "planner@v3" {
		t.Fatalf("summary counts/links = %+v", summary)
	}
}

func TestBuildTrainStatusSummaryWithoutIteration(t *testing.T) {
	summary := BuildTrainStatusSummary(db.SkillOptTrainSession{ID: "session-1", State: TrainStateWorkspaceReady}, nil, TrainStatusCounts{})
	if summary.CurrentPhase != TrainStateWorkspaceReady || summary.BlockedStep != TrainStateItemsReady || !strings.Contains(summary.NextAction, "training items") {
		t.Fatalf("summary without iteration = %+v", summary)
	}
	if len(summary.CompletedSteps) != 2 {
		t.Fatalf("completed steps without iteration = %+v", summary.CompletedSteps)
	}
}

func TestBuildTrainStatusSummaryForResolvedIteration(t *testing.T) {
	iteration := db.SkillOptTrainIteration{
		ID:                 "landing-page-001",
		State:              TrainStateCandidatePromoted,
		CandidateVersionID: "planner@v4",
	}
	summary := BuildTrainStatusSummary(db.SkillOptTrainSession{ID: "landing-page"}, &iteration, TrainStatusCounts{})
	if summary.BlockedStep != "" || !strings.Contains(summary.NextAction, "promoted candidate") {
		t.Fatalf("resolved summary = %+v", summary)
	}
	if len(summary.CompletedSteps) != len(orderedTrainStates) {
		t.Fatalf("completed steps = %+v", summary.CompletedSteps)
	}
}

func TestBuildTrainStatusSummaryForAbandonedIterationDoesNotCompleteAllSteps(t *testing.T) {
	iteration := db.SkillOptTrainIteration{
		ID:    "landing-page-001",
		State: TrainStateRunAbandoned,
	}
	summary := BuildTrainStatusSummary(db.SkillOptTrainSession{ID: "landing-page"}, &iteration, TrainStatusCounts{})
	if summary.BlockedStep != "" || !strings.Contains(summary.NextAction, "abandoned") {
		t.Fatalf("abandoned summary = %+v", summary)
	}
	if len(summary.CompletedSteps) != 0 {
		t.Fatalf("abandoned completed steps = %+v", summary.CompletedSteps)
	}
}

func TestBuildTrainPreviewPolicyDefaultsForTextOnlyAndPreviewRepo(t *testing.T) {
	textOnly, err := BuildTrainPreviewPolicy("owner/product", "", "", "", "", "")
	if err != nil {
		t.Fatalf("text-only BuildTrainPreviewPolicy returned error: %v", err)
	}
	if textOnly.Mode != TrainPreviewModeNone || textOnly.Renderer != TrainPreviewRendererNone || textOnly.Publisher != TrainPreviewPublisherNone || textOnly.ExpectedReviewRepo != "owner/product" {
		t.Fatalf("text-only policy = %+v", textOnly)
	}

	withPreview, err := BuildTrainPreviewPolicy("owner/product", "owner/previews", "", "", "", "")
	if err != nil {
		t.Fatalf("preview BuildTrainPreviewPolicy returned error: %v", err)
	}
	if withPreview.Mode != TrainPreviewModeRequired || withPreview.Renderer != TrainPreviewRendererVueVite || withPreview.Publisher != TrainPreviewPublisherGitHubPages || withPreview.Repo != "owner/previews" || withPreview.ExpectedReviewRepo != "owner/previews" {
		t.Fatalf("preview policy = %+v", withPreview)
	}
	if withPreview.RouteTemplate != DefaultTrainPreviewRouteTemplate {
		t.Fatalf("preview route template = %q", withPreview.RouteTemplate)
	}
}

func TestBuildTrainPreviewPolicyRejectsInvalidRequiredCombinations(t *testing.T) {
	if _, err := BuildTrainPreviewPolicy("owner/product", "owner/previews", TrainPreviewModeRequired, TrainPreviewRendererNone, "", ""); err == nil || !strings.Contains(err.Error(), "renderer") {
		t.Fatalf("required without renderer error = %v", err)
	}
	if _, err := BuildTrainPreviewPolicy("owner/product", "", TrainPreviewModeRequired, TrainPreviewRendererVueVite, TrainPreviewPublisherGitHubPages, ""); err == nil || !strings.Contains(err.Error(), "preview repo") {
		t.Fatalf("required without repo error = %v", err)
	}
	if _, err := BuildTrainPreviewPolicy("owner/product", "", TrainPreviewModeNone, TrainPreviewRendererVueVite, TrainPreviewPublisherNone, ""); err == nil || !strings.Contains(err.Error(), "preview renderer") {
		t.Fatalf("none with renderer error = %v", err)
	}
	if _, err := BuildTrainPreviewPolicy("owner/product", "", "", "", "", "custom/{run_id}/"); err == nil || !strings.Contains(err.Error(), "preview repo") {
		t.Fatalf("route template without preview repo error = %v", err)
	}
	if _, err := BuildTrainPreviewPolicy("owner/product", "", TrainPreviewModeNone, "", "", "custom/{run_id}/"); err == nil || !strings.Contains(err.Error(), "route template") {
		t.Fatalf("route template with disabled publisher error = %v", err)
	}
}

func TestBuildTrainPreviewPolicyAllowsOptionalWithoutPublisher(t *testing.T) {
	policy, err := BuildTrainPreviewPolicy("owner/product", "", TrainPreviewModeOptional, "", "", "")
	if err != nil {
		t.Fatalf("optional without publisher returned error: %v", err)
	}
	if policy.Mode != TrainPreviewModeOptional || policy.Renderer != TrainPreviewRendererNone || policy.Publisher != TrainPreviewPublisherNone || policy.ExpectedReviewRepo != "owner/product" {
		t.Fatalf("optional policy = %+v", policy)
	}
}

func TestResolveTrainPreviewPolicyTreatsLegacyPreviewRepoAsNone(t *testing.T) {
	session := db.SkillOptTrainSession{
		ID:          "legacy",
		TargetRepo:  "owner/product",
		PreviewRepo: "owner/previews",
	}
	policy := ResolveTrainPreviewPolicy(session)
	if policy.Mode != TrainPreviewModeNone || policy.ExpectedReviewRepo != "owner/product" {
		t.Fatalf("legacy policy = %+v", policy)
	}
}
