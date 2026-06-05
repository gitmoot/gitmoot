package skillopt

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
)

const (
	TrainStateRequestConfirmed              = "request_confirmed"
	TrainStateWorkspaceReady                = "workspace_ready"
	TrainStateItemsReady                    = "items_ready"
	TrainStateOptionsGenerated              = "options_generated"
	TrainStateReviewPublished               = "review_published"
	TrainStateFeedbackSynced                = "feedback_synced"
	TrainStateTrainingPackageCreated        = "training_package_created"
	TrainStateOptimizerCompleted            = "optimizer_completed"
	TrainStateOptimizerCompletedNoCandidate = "optimizer_completed_no_candidate"
	TrainStateCandidateCreated              = "candidate_created"
	TrainStateCandidateReviewPublished      = "candidate_review_published"
	TrainStateCandidatePromoted             = "candidate_promoted"
	TrainStateCandidateRejected             = "candidate_rejected"
	TrainStateRunAbandoned                  = "run_abandoned"
)

const (
	TrainPreviewModeNone     = "none"
	TrainPreviewModeOptional = "optional"
	TrainPreviewModeRequired = "required"

	TrainPreviewRendererNone    = "none"
	TrainPreviewRendererVueVite = "vue-vite"

	TrainPreviewPublisherNone        = "none"
	TrainPreviewPublisherGitHubPages = "github-pages"

	DefaultTrainPreviewRouteTemplate = "runs/{run_id}/{item_id}/{option_label}/"
)

type TrainStatusCounts struct {
	ReviewItems          int
	FeedbackEvents       int
	RankedFeedbackEvents int
	PairwisePreferences  int
}

type TrainPreviewPolicy struct {
	Mode               string
	Renderer           string
	Publisher          string
	Repo               string
	RouteTemplate      string
	ExpectedReviewRepo string
}

type TrainStatusSummary struct {
	SessionID        string
	IterationID      string
	CurrentPhase     string
	CompletedSteps   []string
	BlockedStep      string
	NextAction       string
	IssueURL         string
	PullRequestURL   string
	CandidateVersion string
	FeedbackCount    int
	PreviewPolicy    TrainPreviewPolicy
}

var orderedTrainStates = []string{
	TrainStateRequestConfirmed,
	TrainStateWorkspaceReady,
	TrainStateItemsReady,
	TrainStateOptionsGenerated,
	TrainStateReviewPublished,
	TrainStateFeedbackSynced,
	TrainStateTrainingPackageCreated,
	TrainStateOptimizerCompleted,
	TrainStateCandidateCreated,
	TrainStateCandidateReviewPublished,
}

var trainStateOrder = func() map[string]int {
	order := make(map[string]int, len(orderedTrainStates))
	for index, state := range orderedTrainStates {
		order[state] = index
	}
	return order
}()

func NormalizeTrainState(state string) string {
	return strings.TrimSpace(strings.ToLower(state))
}

func IsTerminalTrainState(state string) bool {
	switch NormalizeTrainState(state) {
	case TrainStateOptimizerCompletedNoCandidate, TrainStateCandidatePromoted, TrainStateCandidateRejected, TrainStateRunAbandoned:
		return true
	default:
		return false
	}
}

func CanTransitionTrainIteration(from string, to string) error {
	from = NormalizeTrainState(from)
	to = NormalizeTrainState(to)
	if from == "" {
		from = TrainStateRequestConfirmed
	}
	if to == "" {
		return errors.New("target train state is required")
	}
	if from == to {
		return nil
	}
	if IsTerminalTrainState(from) {
		return fmt.Errorf("cannot transition train iteration from terminal state %s to %s", from, to)
	}
	if IsTerminalTrainState(to) {
		if to == TrainStateOptimizerCompletedNoCandidate {
			if from != TrainStateOptimizerCompleted {
				return fmt.Errorf("cannot transition train iteration from %s to %s before optimizer completes", from, to)
			}
			return nil
		}
		if to != TrainStateRunAbandoned && from != TrainStateCandidateReviewPublished {
			return fmt.Errorf("cannot transition train iteration from %s to %s before candidate review is published", from, to)
		}
		return nil
	}
	fromIndex, ok := trainStateOrder[from]
	if !ok {
		return fmt.Errorf("unknown source train state %q", from)
	}
	toIndex, ok := trainStateOrder[to]
	if !ok {
		return fmt.Errorf("unknown target train state %q", to)
	}
	if toIndex != fromIndex+1 {
		if fromIndex+1 >= len(orderedTrainStates) {
			return fmt.Errorf("cannot transition train iteration from %s to %s; promote, reject, or abandon the iteration", from, to)
		}
		return fmt.Errorf("cannot transition train iteration from %s to %s; next required state is %s", from, to, orderedTrainStates[fromIndex+1])
	}
	return nil
}

func CanStartNextTrainIteration(previous db.SkillOptTrainIteration) error {
	state := NormalizeTrainState(previous.State)
	switch state {
	case TrainStateOptimizerCompletedNoCandidate, TrainStateCandidatePromoted, TrainStateRunAbandoned:
		return nil
	case TrainStateCandidateRejected:
		if strings.TrimSpace(previous.DecisionReason) == "" {
			return errors.New("cannot start next train iteration after candidate rejection without a decision reason")
		}
		return nil
	default:
		return fmt.Errorf("cannot start next train iteration while previous iteration %s is in state %s", previous.ID, state)
	}
}

func BuildTrainStatusSummary(session db.SkillOptTrainSession, iteration *db.SkillOptTrainIteration, counts TrainStatusCounts) TrainStatusSummary {
	summary := TrainStatusSummary{
		SessionID:     strings.TrimSpace(session.ID),
		CurrentPhase:  NormalizeTrainState(session.State),
		FeedbackCount: counts.FeedbackEvents + counts.RankedFeedbackEvents,
		PreviewPolicy: ResolveTrainPreviewPolicy(session),
	}
	if summary.CurrentPhase == "" {
		summary.CurrentPhase = TrainStateRequestConfirmed
	}
	if iteration == nil {
		summary.CompletedSteps = completedTrainSteps(summary.CurrentPhase)
		summary.BlockedStep, summary.NextAction = trainBlockedStepAndAction(summary.CurrentPhase)
		return summary
	}
	summary.IterationID = strings.TrimSpace(iteration.ID)
	summary.CurrentPhase = NormalizeTrainState(iteration.State)
	if summary.CurrentPhase == "" {
		summary.CurrentPhase = TrainStateRequestConfirmed
	}
	summary.IssueURL = strings.TrimSpace(iteration.IssueURL)
	summary.PullRequestURL = strings.TrimSpace(iteration.PullRequestURL)
	summary.CandidateVersion = strings.TrimSpace(iteration.CandidateVersionID)
	summary.CompletedSteps = completedTrainSteps(summary.CurrentPhase)
	summary.BlockedStep, summary.NextAction = trainBlockedStepAndAction(summary.CurrentPhase)
	return summary
}

func BuildTrainPreviewPolicy(targetRepo string, previewRepo string, mode string, renderer string, publisher string, routeTemplate string) (TrainPreviewPolicy, error) {
	policy := TrainPreviewPolicy{
		Mode:          strings.TrimSpace(strings.ToLower(mode)),
		Renderer:      strings.TrimSpace(strings.ToLower(renderer)),
		Publisher:     strings.TrimSpace(strings.ToLower(publisher)),
		Repo:          strings.TrimSpace(previewRepo),
		RouteTemplate: strings.TrimSpace(routeTemplate),
	}
	targetRepo = strings.TrimSpace(targetRepo)
	if policy.Mode == "" {
		if policy.Repo != "" || policy.Renderer != "" || policy.Publisher != "" || policy.RouteTemplate != "" {
			policy.Mode = TrainPreviewModeRequired
		} else {
			policy.Mode = TrainPreviewModeNone
		}
	}
	if policy.Renderer == "" {
		if policy.Mode == TrainPreviewModeNone || (policy.Mode == TrainPreviewModeOptional && policy.Repo == "") {
			policy.Renderer = TrainPreviewRendererNone
		} else {
			policy.Renderer = TrainPreviewRendererVueVite
		}
	}
	if policy.Publisher == "" {
		if policy.Mode == TrainPreviewModeNone || (policy.Mode == TrainPreviewModeOptional && policy.Repo == "") {
			policy.Publisher = TrainPreviewPublisherNone
		} else {
			policy.Publisher = TrainPreviewPublisherGitHubPages
		}
	}
	if policy.RouteTemplate == "" && policy.Publisher == TrainPreviewPublisherGitHubPages {
		policy.RouteTemplate = DefaultTrainPreviewRouteTemplate
	}
	if err := policy.Validate(); err != nil {
		return TrainPreviewPolicy{}, err
	}
	if policy.Mode == TrainPreviewModeNone || policy.Repo == "" {
		policy.ExpectedReviewRepo = targetRepo
	} else {
		policy.ExpectedReviewRepo = policy.Repo
	}
	return policy, nil
}

func ResolveTrainPreviewPolicy(session db.SkillOptTrainSession) TrainPreviewPolicy {
	targetRepo := strings.TrimSpace(session.TargetRepo)
	metadata := decodedTrainMetadata(session.MetadataJSON)
	preview := decodedTrainMetadataValue(metadata["preview"])
	review := decodedTrainMetadataValue(metadata["review"])
	policy := TrainPreviewPolicy{
		Mode:               strings.ToLower(metadataString(preview, "mode")),
		Renderer:           strings.ToLower(metadataString(preview, "renderer")),
		Publisher:          strings.ToLower(metadataString(preview, "publisher")),
		Repo:               metadataString(preview, "repo"),
		RouteTemplate:      metadataString(preview, "route_template"),
		ExpectedReviewRepo: metadataString(review, "expected_repo"),
	}
	if policy.Mode == "" {
		policy.Mode = TrainPreviewModeNone
	}
	if policy.Renderer == "" {
		policy.Renderer = TrainPreviewRendererNone
	}
	if policy.Publisher == "" {
		policy.Publisher = TrainPreviewPublisherNone
	}
	if policy.ExpectedReviewRepo == "" {
		if policy.Mode == TrainPreviewModeNone || policy.Repo == "" {
			policy.ExpectedReviewRepo = targetRepo
		} else {
			policy.ExpectedReviewRepo = policy.Repo
		}
	}
	return policy
}

func (p TrainPreviewPolicy) Validate() error {
	switch p.Mode {
	case TrainPreviewModeNone, TrainPreviewModeOptional, TrainPreviewModeRequired:
	default:
		return fmt.Errorf("preview mode %q is not supported", p.Mode)
	}
	switch p.Renderer {
	case TrainPreviewRendererNone, TrainPreviewRendererVueVite:
	default:
		return fmt.Errorf("preview renderer %q is not supported", p.Renderer)
	}
	switch p.Publisher {
	case TrainPreviewPublisherNone, TrainPreviewPublisherGitHubPages:
	default:
		return fmt.Errorf("preview publisher %q is not supported", p.Publisher)
	}
	if strings.TrimSpace(p.RouteTemplate) != "" && p.Publisher != TrainPreviewPublisherGitHubPages {
		return fmt.Errorf("preview route template requires preview publisher %q", TrainPreviewPublisherGitHubPages)
	}
	if p.Mode == TrainPreviewModeNone {
		if p.Renderer != TrainPreviewRendererNone {
			return fmt.Errorf("preview renderer must be %q when preview mode is %q", TrainPreviewRendererNone, TrainPreviewModeNone)
		}
		if p.Publisher != TrainPreviewPublisherNone {
			return fmt.Errorf("preview publisher must be %q when preview mode is %q", TrainPreviewPublisherNone, TrainPreviewModeNone)
		}
		return nil
	}
	if p.Mode == TrainPreviewModeRequired {
		if p.Renderer == TrainPreviewRendererNone {
			return errors.New("preview renderer is required when preview mode is required")
		}
		if p.Publisher == TrainPreviewPublisherNone {
			return errors.New("preview publisher is required when preview mode is required")
		}
	}
	if p.Publisher == TrainPreviewPublisherGitHubPages && strings.TrimSpace(p.Repo) == "" {
		return errors.New("preview repo is required when preview publisher is github-pages")
	}
	return nil
}

func (p TrainPreviewPolicy) Metadata() (map[string]any, map[string]any) {
	preview := map[string]any{
		"mode":      p.Mode,
		"renderer":  p.Renderer,
		"publisher": p.Publisher,
	}
	if strings.TrimSpace(p.Repo) != "" {
		preview["repo"] = strings.TrimSpace(p.Repo)
	}
	if strings.TrimSpace(p.RouteTemplate) != "" {
		preview["route_template"] = strings.TrimSpace(p.RouteTemplate)
	}
	review := map[string]any{}
	if strings.TrimSpace(p.ExpectedReviewRepo) != "" {
		review["expected_repo"] = strings.TrimSpace(p.ExpectedReviewRepo)
	}
	return preview, review
}

func decodedTrainMetadata(value string) map[string]any {
	var metadata map[string]any
	if strings.TrimSpace(value) != "" {
		_ = json.Unmarshal([]byte(value), &metadata)
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	return metadata
}

func decodedTrainMetadataValue(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func completedTrainSteps(state string) []string {
	state = NormalizeTrainState(state)
	index, ok := trainStateOrder[state]
	if !ok {
		if IsTerminalTrainState(state) {
			if state == TrainStateRunAbandoned {
				return nil
			}
			if state == TrainStateOptimizerCompletedNoCandidate {
				return append([]string{}, orderedTrainStates[:trainStateOrder[TrainStateOptimizerCompleted]+1]...)
			}
			return append([]string{}, orderedTrainStates...)
		}
		return nil
	}
	return append([]string{}, orderedTrainStates[:index+1]...)
}

func trainBlockedStepAndAction(state string) (string, string) {
	switch NormalizeTrainState(state) {
	case TrainStateRequestConfirmed:
		return TrainStateWorkspaceReady, "prepare or select the training workspace"
	case TrainStateWorkspaceReady:
		return TrainStateItemsReady, "add diverse training items"
	case TrainStateItemsReady:
		return TrainStateOptionsGenerated, "generate review options with temporary agents"
	case TrainStateOptionsGenerated:
		return TrainStateReviewPublished, "publish the human review packet"
	case TrainStateReviewPublished:
		return TrainStateFeedbackSynced, "sync human feedback from the review surface"
	case TrainStateFeedbackSynced:
		return TrainStateTrainingPackageCreated, "export the training package before running the optimizer"
	case TrainStateTrainingPackageCreated:
		return TrainStateOptimizerCompleted, "run the external gitmoot-skillopt optimizer"
	case TrainStateOptimizerCompleted:
		return TrainStateCandidateCreated, "import the optimizer candidate package"
	case TrainStateOptimizerCompletedNoCandidate:
		return "", "no candidate was created; revise feedback and start another iteration, rerun the optimizer, or stop"
	case TrainStateCandidateCreated:
		return TrainStateCandidateReviewPublished, "publish candidate diff and preview review"
	case TrainStateCandidateReviewPublished:
		return "candidate decision", "choose promote, reject with a reason, or wait for human decision; keep improving by rejecting with an actionable reason and starting the next iteration"
	case TrainStateCandidatePromoted:
		return "", "start the next iteration from the promoted candidate or stop"
	case TrainStateCandidateRejected:
		return "", "start another iteration from the previous base or stop"
	case TrainStateRunAbandoned:
		return "", "training run is abandoned"
	default:
		return "state", "inspect train session state"
	}
}
