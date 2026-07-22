package cli

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/feedback"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

func continueSkillOptTrainCandidateDecision(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, counts skillopt.TrainStatusCounts, request skillOptTrainContinueRequest) (skillOptTrainContinueOutput, error) {
	summary := skillopt.BuildTrainStatusSummary(session, &iteration, counts)
	output := skillOptTrainContinueOutput{Summary: summary, Counts: counts}
	result, err := decideSkillOptTrainCandidate(ctx, paths, store, session, iteration, request)
	if err != nil {
		return skillOptTrainContinueOutput{}, err
	}
	if !result.Decided {
		if request.StartNext {
			return skillOptTrainContinueOutput{}, fmt.Errorf("--start-next requires a promoted or rejected candidate; current phase is %s", summary.CurrentPhase)
		}
		lines := []string{}
		if url := strings.TrimSpace(iteration.IssueURL); url != "" {
			lines = append(lines, fmt.Sprintf("continue_from_github: %s", url))
		}
		lines = append(lines, "next: promote with --promote <candidate-version> or reject with --reject <candidate-version> --reason <text>")
		output.Lines = lines
		return output, nil
	}
	// Reflect the decision on the candidate-review issue so it doesn't sit open
	// with no record of the choice (works for a TUI p/x or the --promote/--reject
	// flags). Best-effort: a GitHub failure never undoes the recorded decision.
	decisionNotice := postSkillOptTrainCandidateDecisionComment(ctx, iteration, result, request)
	out, err := continueSkillOptTrainAfterCandidateDecision(ctx, store, session.ID, request, result)
	// Surface the GitHub notice whether or not the follow-up (e.g. --start-next)
	// succeeded — the decision was made and posted regardless.
	if decisionNotice != "" {
		out.Lines = append(out.Lines, decisionNotice)
	}
	return out, err
}

// postSkillOptTrainCandidateDecisionComment posts a promote/reject decision to the
// candidate-review issue (or PR) and closes the issue. It is best-effort and
// returns a one-line notice (or a warning) for the continue output; the recorded
// decision stands regardless. The comment carries a gitmoot marker so the review
// watcher skips it.
func postSkillOptTrainCandidateDecisionComment(ctx context.Context, iteration db.SkillOptTrainIteration, result skillOptTrainCandidateDecisionResult, request skillOptTrainContinueRequest) string {
	repoName := strings.TrimSpace(iteration.IssueRepo)
	number := iteration.IssueNumber
	onIssue := repoName != "" && number > 0
	if !onIssue {
		repoName = strings.TrimSpace(iteration.PullRequestRepo)
		number = iteration.PullRequestNumber
	}
	if repoName == "" || number <= 0 {
		return ""
	}
	repo, err := daemon.ParseRepository(repoName)
	if err != nil {
		return fmt.Sprintf("warning: candidate-review repo %q is unparseable; decision not posted: %v", repoName, err)
	}
	version := strings.TrimSpace(result.CandidateVersionID)
	if version == "" {
		version = strings.TrimSpace(firstNonEmpty(request.PromoteCandidate, request.RejectCandidate))
	}
	var body strings.Builder
	body.WriteString(skillOptTrainDecisionMarker + "\n")
	switch {
	case strings.Contains(result.Decision, "promot"):
		fmt.Fprintf(&body, "✅ Promoted `%s` from gitmoot.", version)
	case strings.Contains(result.Decision, "reject"):
		fmt.Fprintf(&body, "❌ Rejected `%s` from gitmoot.", version)
		if reason := strings.TrimSpace(request.DecisionReason); reason != "" {
			fmt.Fprintf(&body, "\n\nReason: %s", reason)
		}
	default:
		fmt.Fprintf(&body, "Decision recorded for `%s`: %s", version, result.Decision)
	}
	client := newSkillOptGitHubClient()
	if _, err := client.PostIssueComment(ctx, repo, number, body.String()); err != nil {
		return fmt.Sprintf("warning: could not post the decision to %s#%d: %v", repo.FullName(), number, err)
	}
	notice := fmt.Sprintf("candidate_review: posted the decision to %s#%d", repo.FullName(), number)
	// Only close a dedicated review issue; never close a user's pull request.
	if onIssue {
		if _, err := client.CloseIssue(ctx, repo, number); err != nil {
			return notice + fmt.Sprintf(" (could not close it: %v)", err)
		}
		notice += " and closed it"
	}
	return notice
}

func continueSkillOptTrainAfterCandidateDecision(ctx context.Context, store *db.Store, sessionID string, request skillOptTrainContinueRequest, result skillOptTrainCandidateDecisionResult) (skillOptTrainContinueOutput, error) {
	updatedSession, updatedIteration, updatedCounts, err := loadSkillOptTrainStatus(ctx, store, sessionID)
	if err != nil {
		return skillOptTrainContinueOutput{}, err
	}
	updatedSummary := skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
	if request.StartNext {
		if updatedIteration == nil {
			return skillOptTrainContinueOutput{}, errors.New("train session has no decided iteration to continue")
		}
		next, err := startNextSkillOptTrainIteration(ctx, store, updatedSession, *updatedIteration)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSession, updatedIteration, updatedCounts, err = loadSkillOptTrainStatus(ctx, store, sessionID)
		if err != nil {
			return skillOptTrainContinueOutput{}, err
		}
		updatedSummary = skillopt.BuildTrainStatusSummary(updatedSession, updatedIteration, updatedCounts)
		lines := []string{
			fmt.Sprintf("%s_candidate: %s", result.Decision, result.CandidateVersionID),
			fmt.Sprintf("started_iteration: %s", next.ID),
			fmt.Sprintf("base_version: %s", next.BaseTemplateVersionID),
			"next: generate review options with train continue",
		}
		return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
	}
	lines := []string{
		fmt.Sprintf("%s_candidate: %s", result.Decision, result.CandidateVersionID),
		"next: stop or run --start-next",
	}
	return skillOptTrainContinueOutput{Summary: updatedSummary, Counts: updatedCounts, ContinueReady: true, Lines: lines}, nil
}

func validateTerminalSkillOptTrainDecisionRequest(iteration db.SkillOptTrainIteration, request skillOptTrainContinueRequest) error {
	decision := requestedSkillOptTrainCandidateDecision(request)
	if decision == "" {
		return nil
	}
	candidateID := requestedSkillOptTrainCandidateID(request)
	expected := strings.TrimSpace(iteration.CandidateVersionID)
	if candidateID != expected {
		return fmt.Errorf("candidate %s does not match train iteration candidate %s", candidateID, expected)
	}
	currentDecision := ""
	switch skillopt.NormalizeTrainState(iteration.State) {
	case skillopt.TrainStateCandidatePromoted:
		currentDecision = "promoted"
	case skillopt.TrainStateCandidateRejected:
		currentDecision = "rejected"
	}
	if currentDecision != "" && decision != currentDecision {
		return fmt.Errorf("candidate %s is already %s, not %s", candidateID, currentDecision, decision)
	}
	return nil
}

type skillOptTrainCandidateReviewResult struct {
	URL                string
	CandidateVersionID string
	PublishedFiles     []skillOptTrainCandidateReviewFile
	PublishedPreviews  []skillOptTrainCandidateReviewPreview
}

type skillOptTrainCandidateReviewFile struct {
	Label string
	Path  string
	URL   string
}

type skillOptTrainCandidateReviewPreview struct {
	Label        string
	ArtifactID   string
	Route        string
	URL          string
	Renderer     string
	Content      string
	Status       string
	StatusReason string
	Error        string
}

type skillOptTrainCandidateDecisionResult struct {
	Decided            bool
	Decision           string
	CandidateVersionID string
}

func publishSkillOptTrainCandidateReview(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, commandHome string) (skillOptTrainCandidateReviewResult, error) {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateCandidateReviewPublished); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	candidateID := strings.TrimSpace(iteration.CandidateVersionID)
	if candidateID == "" {
		return skillOptTrainCandidateReviewResult{}, errors.New("train iteration has no candidate version to review")
	}
	if result, recovered, err := recoverSkillOptCandidateReviewPublication(ctx, paths, store, session, iteration, candidateID); recovered || err != nil {
		return result, err
	}
	refreshedSession, err := store.GetSkillOptTrainSession(ctx, session.ID)
	if err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	refreshedIteration, err := store.GetSkillOptTrainIteration(ctx, iteration.ID)
	if err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	session = refreshedSession
	iteration = refreshedIteration
	if err := preventDuplicateSkillOptCandidateReviewPublish(session, iteration, candidateID, time.Now().UTC()); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	repo, err := resolveSkillOptTrainCandidateReviewRepo(session, iteration)
	if err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	if iteration.PullRequestNumber > 0 && iteration.IssueNumber == 0 {
		iteration.PullRequestRepo = repo.FullName()
	} else {
		iteration.IssueRepo = repo.FullName()
	}
	client := newSkillOptGitHubClient()
	if err := client.Preflight(ctx, repo); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	publishedFiles := existingSkillOptCandidateReviewPublishedFiles(session, iteration, repo, candidateID)
	var filePublishErr error
	if len(publishedFiles) == 0 {
		publishedFiles, filePublishErr = publishSkillOptTrainCandidateReviewFiles(ctx, paths, store, client, repo, session, iteration)
	}
	filePublishWarnings := []string{}
	if filePublishErr != nil {
		filePublishWarnings = append(filePublishWarnings, filePublishErr.Error())
	}
	publishedPreviews := existingSkillOptCandidateReviewPublishedPreviews(session, iteration, candidateID)
	if len(publishedPreviews) == 0 {
		publishedPreviews = publishSkillOptTrainCandidateSamplePreviews(ctx, paths, store, session, iteration)
	}
	body, err := skillOptTrainCandidateReviewBody(ctx, store, session, iteration, commandHome, publishedFiles, publishedPreviews, filePublishWarnings)
	if err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	title := fmt.Sprintf("SkillOpt candidate review: %s", session.ID)
	publishingMetadata := map[string]any{
		"status":              "publishing",
		"candidate_version":   candidateID,
		"issue_repo":          iteration.IssueRepo,
		"issue_number":        iteration.IssueNumber,
		"issue_url":           iteration.IssueURL,
		"pull_request_repo":   iteration.PullRequestRepo,
		"pull_request_number": iteration.PullRequestNumber,
		"pull_request_url":    iteration.PullRequestURL,
		"issue_title":         title,
		"published_files":     skillOptCandidateReviewFilesMetadata(publishedFiles),
		"published_previews":  skillOptCandidateReviewPreviewsMetadata(publishedPreviews),
		"file_publish_errors": filePublishWarnings,
		"started_at":          time.Now().UTC().Format(time.RFC3339Nano),
		"source":              "gitmoot skillopt train continue",
	}
	if err := writeSkillOptCandidateReviewRecovery(paths, session, iteration, publishingMetadata); err != nil {
		return skillOptTrainCandidateReviewResult{}, fmt.Errorf("write candidate review pre-publish recovery marker: %w", err)
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", publishingMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", publishingMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	postingMetadata := make(map[string]any, len(publishingMetadata)+2)
	for key, value := range publishingMetadata {
		postingMetadata[key] = value
	}
	postingMetadata["status"] = "posting_external"
	postingMetadata["external_post_started_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeSkillOptCandidateReviewRecovery(paths, session, iteration, postingMetadata); err != nil {
		if metaErr := recordFailedSkillOptCandidateReviewPublish(ctx, store, session, iteration, publishingMetadata, err); metaErr != nil {
			return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; failed to record candidate review publish failure: %v", err, metaErr)
		}
		return skillOptTrainCandidateReviewResult{}, fmt.Errorf("write candidate review external-post recovery marker: %w", err)
	}
	var url string
	if iteration.IssueNumber > 0 {
		comment, err := client.PostIssueComment(ctx, repo, iteration.IssueNumber, body)
		if err != nil {
			if metaErr := recordFailedSkillOptCandidateReviewPublish(ctx, store, session, iteration, publishingMetadata, err); metaErr != nil {
				return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; failed to record candidate review publish failure: %v", err, metaErr)
			}
			return skillOptTrainCandidateReviewResult{}, err
		}
		url = comment.URL
		if strings.TrimSpace(iteration.IssueURL) == "" {
			iteration.IssueURL = skillOptReviewTargetURLFromCommentOrHost(comment.URL, repo, "issues", iteration.IssueNumber)
		}
	} else if iteration.PullRequestNumber > 0 {
		comment, err := client.PostIssueComment(ctx, repo, iteration.PullRequestNumber, body)
		if err != nil {
			if metaErr := recordFailedSkillOptCandidateReviewPublish(ctx, store, session, iteration, publishingMetadata, err); metaErr != nil {
				return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; failed to record candidate review publish failure: %v", err, metaErr)
			}
			return skillOptTrainCandidateReviewResult{}, err
		}
		url = comment.URL
		if strings.TrimSpace(iteration.PullRequestURL) == "" {
			iteration.PullRequestURL = skillOptReviewTargetURLFromCommentOrHost(comment.URL, repo, "pull", iteration.PullRequestNumber)
		}
	} else {
		issue, err := client.CreateIssue(ctx, github.CreateIssueInput{
			Repo:  repo,
			Title: title,
			Body:  body,
		})
		if err != nil {
			if metaErr := recordFailedSkillOptCandidateReviewPublish(ctx, store, session, iteration, publishingMetadata, err); metaErr != nil {
				return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; failed to record candidate review publish failure: %v", err, metaErr)
			}
			return skillOptTrainCandidateReviewResult{}, err
		}
		iteration.IssueNumber = issue.Number
		iteration.IssueURL = issue.URL
		url = issue.URL
	}
	externalMetadata := skillOptCandidateReviewPublicationMetadata(publishingMetadata, iteration, url, "published_external")
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", externalMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", externalMetadata)
	recoveryErr := writeSkillOptCandidateReviewRecovery(paths, session, iteration, externalMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		if recoveryErr != nil {
			return skillOptTrainCandidateReviewResult{}, fmt.Errorf("%w; candidate review was published at %s but recovery marker write failed: %v", err, url, recoveryErr)
		}
		return skillOptTrainCandidateReviewResult{}, err
	}
	iteration.State = skillopt.TrainStateCandidateReviewPublished
	session.State = skillopt.TrainStateCandidateReviewPublished
	metadata := skillOptCandidateReviewPublicationMetadata(publishingMetadata, iteration, url, "published")
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", metadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainCandidateReviewResult{}, err
	}
	_ = removeSkillOptCandidateReviewRecovery(paths, session, iteration)
	return skillOptTrainCandidateReviewResult{URL: url, CandidateVersionID: candidateID, PublishedFiles: publishedFiles, PublishedPreviews: publishedPreviews}, nil
}

func recoverSkillOptCandidateReviewPublication(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidateID string) (skillOptTrainCandidateReviewResult, bool, error) {
	sources := []map[string]any{
		decodedSkillOptMetadataValue(decodedSkillOptMetadata(iteration.MetadataJSON)["candidate_review"]),
		decodedSkillOptMetadataValue(decodedSkillOptMetadata(session.MetadataJSON)["candidate_review"]),
	}
	if review, ok, err := readSkillOptCandidateReviewRecovery(paths, session, iteration); err != nil {
		return skillOptTrainCandidateReviewResult{}, true, err
	} else if ok {
		if metadataString(review, "status") == "publishing" && metadataString(review, "external_post_started_at") == "" {
			metadata := make(map[string]any, len(review)+3)
			for key, value := range review {
				metadata[key] = value
			}
			metadata["status"] = "failed"
			metadata["error"] = "candidate review publication interrupted before external post started"
			metadata["failed_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", metadata)
			iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", metadata)
			if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
				return skillOptTrainCandidateReviewResult{}, true, err
			}
			_ = removeSkillOptCandidateReviewRecovery(paths, session, iteration)
			return skillOptTrainCandidateReviewResult{}, false, nil
		}
		sources = append(sources, review)
	}
	for _, review := range sources {
		status := metadataString(review, "status")
		if status == "posting_external" {
			target := skillOptCandidateReviewRecoveryTarget(review)
			if target == "" {
				target = "inspect the configured GitHub review surface before retrying"
			}
			return skillOptTrainCandidateReviewResult{}, true, fmt.Errorf("candidate review publication for %s was interrupted after external post started; %s", candidateID, target)
		}
		if status != "published_external" && status != "published" {
			continue
		}
		reviewCandidate := metadataString(review, "candidate_version")
		if reviewCandidate != "" && reviewCandidate != candidateID {
			continue
		}
		url := skillOptCandidateReviewURLFromMetadata(review)
		if url == "" {
			return skillOptTrainCandidateReviewResult{}, true, fmt.Errorf("candidate review publication for %s is marked %s but has no recoverable review URL", candidateID, status)
		}
		applySkillOptCandidateReviewMetadataToIteration(review, &iteration)
		iteration.State = skillopt.TrainStateCandidateReviewPublished
		session.State = skillopt.TrainStateCandidateReviewPublished
		metadata := make(map[string]any, len(review)+3)
		for key, value := range review {
			metadata[key] = value
		}
		metadata["status"] = "published"
		metadata["review_url"] = url
		metadata["recovered_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", metadata)
		iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", metadata)
		if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
			return skillOptTrainCandidateReviewResult{}, true, err
		}
		_ = removeSkillOptCandidateReviewRecovery(paths, session, iteration)
		return skillOptTrainCandidateReviewResult{URL: url, CandidateVersionID: candidateID}, true, nil
	}
	return skillOptTrainCandidateReviewResult{}, false, nil
}

func preventDuplicateSkillOptCandidateReviewPublish(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidateID string, now time.Time) error {
	for _, source := range []struct {
		name     string
		metadata string
	}{
		{name: "iteration", metadata: iteration.MetadataJSON},
		{name: "session", metadata: session.MetadataJSON},
	} {
		review := decodedSkillOptMetadataValue(decodedSkillOptMetadata(source.metadata)["candidate_review"])
		status := metadataString(review, "status")
		if status == "publishing" && !skillOptCandidateReviewPublishingFresh(review, now) {
			continue
		}
		if status != "publishing" && status != "published" {
			continue
		}
		reviewCandidate := metadataString(review, "candidate_version")
		if reviewCandidate != "" && reviewCandidate != candidateID {
			continue
		}
		target := skillOptCandidateReviewRecoveryTarget(review)
		if target == "" {
			target = "inspect candidate_review metadata before retrying"
		}
		return fmt.Errorf("candidate review publication for %s is marked %s in %s metadata; %s", candidateID, status, source.name, target)
	}
	return nil
}

func skillOptCandidateReviewPublishingFresh(review map[string]any, now time.Time) bool {
	startedAt := metadataString(review, "started_at")
	if startedAt == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return now.Before(started.Add(skillOptTrainCandidateReviewLockTTL))
}

func writeSkillOptCandidateReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, metadata map[string]any) error {
	path := skillOptCandidateReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return errors.New("candidate review recovery path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	tmpPath := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmpPath, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func readSkillOptCandidateReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (map[string]any, bool, error) {
	path := skillOptCandidateReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return nil, false, nil
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var metadata map[string]any
	if err := json.Unmarshal(content, &metadata); err != nil {
		return nil, true, fmt.Errorf("read candidate review recovery marker %s: %w", path, err)
	}
	return metadata, true, nil
}

func removeSkillOptCandidateReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) error {
	path := skillOptCandidateReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func skillOptCandidateReviewRecoveryPath(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) string {
	if strings.TrimSpace(paths.Home) == "" {
		return ""
	}
	name := skillOptCandidateReviewRecoveryName(session.ID, iteration.ID)
	if name == "" {
		return ""
	}
	return filepath.Join(paths.Home, "skillopt", "candidate-reviews", name+".json")
}

func skillOptCandidateReviewRecoveryName(sessionID string, iterationID string) string {
	sessionID = encodeSkillOptCandidateReviewRecoveryToken(sessionID)
	iterationID = encodeSkillOptCandidateReviewRecoveryToken(iterationID)
	if sessionID == "" || iterationID == "" {
		return ""
	}
	return sessionID + "-" + iterationID
}

func encodeSkillOptCandidateReviewRecoveryToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func skillOptCandidateReviewPublicationMetadata(base map[string]any, iteration db.SkillOptTrainIteration, reviewURL string, status string) map[string]any {
	metadata := make(map[string]any, len(base)+9)
	for key, value := range base {
		metadata[key] = value
	}
	metadata["status"] = status
	metadata["issue_repo"] = iteration.IssueRepo
	metadata["issue_number"] = iteration.IssueNumber
	metadata["issue_url"] = iteration.IssueURL
	metadata["pull_request_repo"] = iteration.PullRequestRepo
	metadata["pull_request_number"] = iteration.PullRequestNumber
	metadata["pull_request_url"] = iteration.PullRequestURL
	metadata["review_url"] = reviewURL
	metadata["published_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	return metadata
}

func recordFailedSkillOptCandidateReviewPublish(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, publishingMetadata map[string]any, publishErr error) error {
	metadata := make(map[string]any, len(publishingMetadata)+3)
	for key, value := range publishingMetadata {
		metadata[key] = value
	}
	metadata["status"] = "failed"
	metadata["error"] = truncateForMetadata(publishErr.Error())
	metadata["failed_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", metadata)
	return store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration)
}

func applySkillOptCandidateReviewMetadataToIteration(review map[string]any, iteration *db.SkillOptTrainIteration) {
	if value := metadataString(review, "issue_repo"); value != "" {
		iteration.IssueRepo = value
	}
	if value := metadataString(review, "issue_number"); value != "" {
		if number, err := strconv.ParseInt(value, 10, 64); err == nil {
			iteration.IssueNumber = number
		}
	}
	if value := metadataString(review, "issue_url"); value != "" {
		iteration.IssueURL = value
	}
	if value := metadataString(review, "pull_request_repo"); value != "" {
		iteration.PullRequestRepo = value
	}
	if value := metadataString(review, "pull_request_number"); value != "" {
		if number, err := strconv.ParseInt(value, 10, 64); err == nil {
			iteration.PullRequestNumber = number
		}
	}
	if value := metadataString(review, "pull_request_url"); value != "" {
		iteration.PullRequestURL = value
	}
}

func skillOptCandidateReviewURLFromMetadata(review map[string]any) string {
	for _, key := range []string{"review_url", "issue_url", "pull_request_url"} {
		if value := metadataString(review, key); value != "" {
			return value
		}
	}
	repo := metadataString(review, "issue_repo")
	number := metadataString(review, "issue_number")
	if repo != "" && number != "" && number != "0" {
		return "https://github.com/" + repo + "/issues/" + number
	}
	repo = metadataString(review, "pull_request_repo")
	number = metadataString(review, "pull_request_number")
	if repo != "" && number != "" && number != "0" {
		return "https://github.com/" + repo + "/pull/" + number
	}
	return ""
}

func skillOptCandidateReviewRecoveryTarget(review map[string]any) string {
	for _, key := range []string{"review_url", "issue_url", "pull_request_url"} {
		if value := metadataString(review, key); value != "" {
			return "review target: " + value
		}
	}
	repo := metadataString(review, "issue_repo")
	number := metadataString(review, "issue_number")
	if repo != "" && number != "" && number != "0" {
		return "review issue: " + repo + "#" + number
	}
	repo = metadataString(review, "pull_request_repo")
	number = metadataString(review, "pull_request_number")
	if repo != "" && number != "" && number != "0" {
		return "review pull request: " + repo + "#" + number
	}
	if title := metadataString(review, "issue_title"); title != "" {
		return "search for review issue title: " + title
	}
	return ""
}

func resolveSkillOptTrainCandidateReviewRepo(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (github.Repository, error) {
	repoName := strings.TrimSpace(iteration.IssueRepo)
	if iteration.IssueNumber > 0 {
		if repoName == "" {
			repoName = skillOptGitHubIssueURLRepo(iteration.IssueURL)
		}
		if repoName == "" {
			return github.Repository{}, errors.New("candidate review issue repo is required when reusing an existing review issue")
		}
	} else if iteration.PullRequestNumber > 0 {
		repoName = strings.TrimSpace(iteration.PullRequestRepo)
		if repoName == "" {
			repoName = skillOptGitHubPullRequestURLRepo(iteration.PullRequestURL)
		}
		if repoName == "" {
			return github.Repository{}, errors.New("candidate review pull request repo is required when reusing an existing review pull request")
		}
	} else if repoName == "" {
		repoName = strings.TrimSpace(session.WorkspaceRepo)
		if repoName == "" {
			repoName = strings.TrimSpace(session.TargetRepo)
		}
	}
	repo, err := daemon.ParseRepository(repoName)
	if err != nil {
		return github.Repository{}, fmt.Errorf("candidate review repo: %w", err)
	}
	return repo, nil
}

func skillOptGitHubIssueURLRepo(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := neturl.Parse(value)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) < 4 || parts[2] != "issues" {
		return ""
	}
	repo, err := daemon.ParseRepository(parts[0] + "/" + parts[1])
	if err != nil {
		return ""
	}
	return repo.FullName()
}

func skillOptGitHubPullRequestURLRepo(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := neturl.Parse(value)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return ""
	}
	repo, err := daemon.ParseRepository(parts[0] + "/" + parts[1])
	if err != nil {
		return ""
	}
	return repo.FullName()
}

func skillOptReviewTargetURLFromCommentURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := neturl.Parse(value)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) < 4 || (parts[2] != "issues" && parts[2] != "pull") {
		return ""
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func skillOptReviewTargetURLFromCommentOrHost(commentURL string, repo github.Repository, kind string, number int64) string {
	if target := skillOptReviewTargetURLFromCommentURL(commentURL); target != "" {
		return target
	}
	parsed, err := neturl.Parse(strings.TrimSpace(commentURL))
	if err != nil || strings.TrimSpace(parsed.Host) == "" {
		return ""
	}
	scheme := strings.TrimSpace(parsed.Scheme)
	if scheme == "" {
		scheme = "https"
	}
	target := neturl.URL{
		Scheme: scheme,
		Host:   parsed.Host,
		Path:   "/" + repo.FullName() + "/" + strings.Trim(kind, "/") + "/" + fmt.Sprint(number),
	}
	return target.String()
}

func skillOptTrainDecisionRequested(request skillOptTrainContinueRequest) bool {
	return strings.TrimSpace(request.PromoteCandidate) != "" || strings.TrimSpace(request.RejectCandidate) != ""
}

func requestedSkillOptTrainCandidateDecision(request skillOptTrainContinueRequest) string {
	if strings.TrimSpace(request.PromoteCandidate) != "" {
		return "promoted"
	}
	if strings.TrimSpace(request.RejectCandidate) != "" {
		return "rejected"
	}
	return ""
}

func requestedSkillOptTrainCandidateID(request skillOptTrainContinueRequest) string {
	if value := strings.TrimSpace(request.PromoteCandidate); value != "" {
		return value
	}
	return strings.TrimSpace(request.RejectCandidate)
}

func publishSkillOptTrainCandidateReviewFiles(ctx context.Context, paths config.Paths, store *db.Store, client github.Client, repo github.Repository, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) ([]skillOptTrainCandidateReviewFile, error) {
	candidateID := strings.TrimSpace(iteration.CandidateVersionID)
	if candidateID == "" {
		return nil, nil
	}
	version, err := store.GetAgentTemplateVersionByID(ctx, candidateID)
	if err != nil {
		return nil, fmt.Errorf("load candidate version %s for review files: %w", candidateID, err)
	}
	review, err := store.GetAgentTemplateCandidateReview(ctx, candidateID)
	if err != nil {
		return nil, fmt.Errorf("load candidate review %s for review files: %w", candidateID, err)
	}
	basePath := skillOptCandidateReviewFileBasePath(session.ID, iteration.ID, candidateID)
	if basePath == "" {
		return nil, errors.New("candidate review file path is required")
	}
	files := []struct {
		label   string
		name    string
		content []byte
	}{
		{
			label:   "Best skill",
			name:    "best_skill.md",
			content: []byte(strings.TrimRight(version.Content, "\n") + "\n"),
		},
	}
	if baseID := firstNonEmpty(review.BaseVersionID, iteration.BaseTemplateVersionID); baseID != "" {
		if baseVersion, err := store.GetAgentTemplateVersionByID(ctx, baseID); err == nil {
			files = append(files, struct {
				label   string
				name    string
				content []byte
			}{
				label:   "Base skill",
				name:    "base_skill.md",
				content: []byte(strings.TrimRight(baseVersion.Content, "\n") + "\n"),
			})
		}
	}
	if diffContent, err := skillOptCandidateReviewDiffContent(ctx, paths, store, review); err == nil && len(diffContent) > 0 {
		files = append(files, struct {
			label   string
			name    string
			content []byte
		}{
			label:   "Candidate diff",
			name:    "candidate.diff.md",
			content: diffContent,
		})
	} else if err != nil {
		return nil, err
	}
	published := make([]skillOptTrainCandidateReviewFile, 0, len(files))
	for _, file := range files {
		repoPath := basePath + "/" + file.name
		result, err := client.UpsertFile(ctx, github.UpsertFileInput{
			Repo:    repo,
			Path:    repoPath,
			Content: file.content,
			Message: fmt.Sprintf("Publish SkillOpt candidate review file %s", candidateID),
		})
		if err != nil {
			return published, fmt.Errorf("publish candidate review file %s: %w", repoPath, err)
		}
		published = append(published, skillOptTrainCandidateReviewFile{
			Label: file.label,
			Path:  firstNonEmpty(result.Path, repoPath),
			URL:   result.URL,
		})
	}
	return published, nil
}

func skillOptCandidateReviewDiffContent(ctx context.Context, paths config.Paths, store *db.Store, review db.AgentTemplateCandidateReview) ([]byte, error) {
	diffID := strings.TrimSpace(review.DiffArtifactID)
	if diffID == "" {
		return nil, nil
	}
	record, err := store.GetEvalArtifact(ctx, diffID)
	if err != nil {
		return nil, fmt.Errorf("load candidate diff artifact %s: %w", diffID, err)
	}
	if strings.TrimSpace(record.Hash) == "" {
		return nil, nil
	}
	content, err := artifact.NewStore(paths.ArtifactBlobs).Read(record.Hash)
	if err != nil {
		return nil, fmt.Errorf("read candidate diff artifact %s: %w", diffID, err)
	}
	return content, nil
}

func skillOptCandidateReviewFileBasePath(sessionID string, iterationID string, candidateID string) string {
	parts := []string{
		"skillopt",
		"runs",
		skillOptCandidateReviewFileToken(sessionID),
		skillOptCandidateReviewFileToken(iterationID),
		skillOptCandidateReviewFileToken(candidateID),
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return ""
		}
	}
	return strings.Join(parts, "/")
}

func skillOptCandidateReviewFileToken(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.', r == '-', r == '_', r == '@':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}

func skillOptCandidateReviewFilesMetadata(files []skillOptTrainCandidateReviewFile) []map[string]string {
	if len(files) == 0 {
		return nil
	}
	metadata := make([]map[string]string, 0, len(files))
	for _, file := range files {
		metadata = append(metadata, map[string]string{
			"label": file.Label,
			"path":  file.Path,
			"url":   file.URL,
		})
	}
	return metadata
}

func existingSkillOptCandidateReviewPublishedFiles(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, repo github.Repository, candidateID string) []skillOptTrainCandidateReviewFile {
	for _, metadataJSON := range []string{iteration.MetadataJSON, session.MetadataJSON} {
		review := decodedSkillOptMetadataValue(decodedSkillOptMetadata(metadataJSON)["candidate_review"])
		if metadataString(review, "candidate_version") != candidateID {
			continue
		}
		if firstNonEmpty(metadataString(review, "issue_repo"), metadataString(review, "pull_request_repo")) != repo.FullName() {
			continue
		}
		if len(metadataSlice(review["file_publish_errors"])) > 0 {
			continue
		}
		files := skillOptCandidateReviewFilesFromMetadata(review["published_files"])
		if len(files) > 0 {
			return files
		}
	}
	return nil
}

func skillOptCandidateReviewFilesFromMetadata(value any) []skillOptTrainCandidateReviewFile {
	values := metadataSlice(value)
	if len(values) == 0 {
		return nil
	}
	files := make([]skillOptTrainCandidateReviewFile, 0, len(values))
	for _, raw := range values {
		metadata := decodedSkillOptMetadataValue(raw)
		label := metadataString(metadata, "label")
		path := metadataString(metadata, "path")
		url := metadataString(metadata, "url")
		if label == "" || path == "" {
			continue
		}
		files = append(files, skillOptTrainCandidateReviewFile{
			Label: label,
			Path:  path,
			URL:   url,
		})
	}
	return files
}

func publishSkillOptTrainCandidateSamplePreviews(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) []skillOptTrainCandidateReviewPreview {
	candidateID := strings.TrimSpace(iteration.CandidateVersionID)
	artifactID := skillOptCandidateSelectionSampleArtifactID(ctx, store, candidateID)
	if candidateID == "" || artifactID == "" {
		return nil
	}
	preview := skillOptTrainCandidateReviewPreview{
		Label:      "Selection sample",
		ArtifactID: artifactID,
		Renderer:   skillopt.TrainPreviewRendererVueVite,
	}
	record, err := store.GetEvalArtifact(ctx, artifactID)
	if err != nil {
		preview.Error = err.Error()
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	content, err := artifact.NewStore(paths.ArtifactBlobs).Read(record.Hash)
	if err != nil {
		preview.Error = err.Error()
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	bundle, err := skillopt.ParsePreviewBundle(content)
	if err != nil {
		if policy.Mode == skillopt.TrainPreviewModeRequired && policy.Renderer == skillopt.TrainPreviewRendererVueVite {
			preview.Error = err.Error()
			return []skillOptTrainCandidateReviewPreview{preview}
		}
		textPreview, ok := skillOptTextArtifactPreview(record, content)
		if !ok {
			preview.Error = err.Error()
			return []skillOptTrainCandidateReviewPreview{preview}
		}
		preview.Renderer = firstNonEmpty(strings.TrimSpace(record.Driver), strings.TrimSpace(record.MediaType), "text")
		preview.Content = textPreview
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	if policy.Mode == skillopt.TrainPreviewModeNone || policy.Renderer != skillopt.TrainPreviewRendererVueVite || policy.Publisher != skillopt.TrainPreviewPublisherGitHubPages {
		preview.Error = "candidate sample preview publishing is not configured for vue-vite GitHub Pages"
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	preview.Renderer = bundle.Renderer
	previewRepo, err := previewRepoRecord(ctx, store, policy)
	if err != nil {
		preview.Error = err.Error()
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	distDir, cleanup, err := renderVueVitePreviewBundle(ctx, bundle)
	if err != nil {
		preview.Error = err.Error()
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	defer func() { _ = cleanup() }()
	route, err := skillOptPreviewRoute(policy.RouteTemplate, session.ID, iteration.ID, "candidate-selection-sample-"+candidateID)
	if err != nil {
		preview.Error = err.Error()
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	publication, err := publishGitHubPagesPreview(ctx, previewRepo, route, distDir)
	if err != nil {
		preview.Error = err.Error()
		return []skillOptTrainCandidateReviewPreview{preview}
	}
	preview.Route = route
	preview.URL = publication.URL
	preview.Status = publication.PagesStatus
	preview.StatusReason = publication.StatusReason
	return []skillOptTrainCandidateReviewPreview{preview}
}

func skillOptTextArtifactPreview(record db.EvalArtifact, content []byte) (string, bool) {
	if !utf8.Valid(content) {
		return "", false
	}
	mediaType := strings.ToLower(strings.TrimSpace(record.MediaType))
	driver := strings.ToLower(strings.TrimSpace(record.Driver))
	if driver != "" && driver != "text" {
		return "", false
	}
	if !strings.HasPrefix(mediaType, "text/") &&
		mediaType != "application/json" &&
		mediaType != "application/x-ndjson" {
		return "", false
	}
	text := truncateForMetadata(feedback.TextArtifactPreview(string(content)))
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	return text, true
}

func skillOptCandidateSelectionSampleArtifactID(ctx context.Context, store *db.Store, candidateID string) string {
	if strings.TrimSpace(candidateID) == "" {
		return ""
	}
	review, err := store.GetAgentTemplateCandidateReview(ctx, candidateID)
	if err != nil {
		return ""
	}
	for _, artifactID := range skillOptMetadataStringSlice(decodedSkillOptMetadata(review.SummaryMetadataJSON), "artifact_ids") {
		if strings.HasSuffix(strings.TrimSpace(artifactID), "/candidate-selection-sample") {
			return strings.TrimSpace(artifactID)
		}
	}
	artifactID := strings.TrimSuffix(strings.TrimSpace(review.DiffArtifactID), "/candidate-diff") + "/candidate-selection-sample"
	if strings.TrimSpace(artifactID) == "/candidate-selection-sample" {
		return ""
	}
	if _, err := store.GetEvalArtifact(ctx, artifactID); err == nil {
		return artifactID
	}
	return ""
}

func skillOptMetadataStringSlice(metadata map[string]any, key string) []string {
	values := metadataSlice(metadata[key])
	output := make([]string, 0, len(values))
	for _, value := range values {
		text := strings.TrimSpace(fmt.Sprint(value))
		if text != "" {
			output = append(output, text)
		}
	}
	return output
}

func skillOptCandidateReviewPreviewsMetadata(previews []skillOptTrainCandidateReviewPreview) []map[string]string {
	if len(previews) == 0 {
		return nil
	}
	metadata := make([]map[string]string, 0, len(previews))
	for _, preview := range previews {
		metadata = append(metadata, map[string]string{
			"label":         preview.Label,
			"artifact_id":   preview.ArtifactID,
			"route":         preview.Route,
			"url":           preview.URL,
			"renderer":      preview.Renderer,
			"content":       preview.Content,
			"status":        preview.Status,
			"status_reason": preview.StatusReason,
			"error":         preview.Error,
		})
	}
	return metadata
}

func existingSkillOptCandidateReviewPublishedPreviews(session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidateID string) []skillOptTrainCandidateReviewPreview {
	for _, metadataJSON := range []string{iteration.MetadataJSON, session.MetadataJSON} {
		review := decodedSkillOptMetadataValue(decodedSkillOptMetadata(metadataJSON)["candidate_review"])
		if metadataString(review, "candidate_version") != candidateID {
			continue
		}
		previews := skillOptCandidateReviewPreviewsFromMetadata(review["published_previews"])
		if len(previews) > 0 {
			return previews
		}
	}
	return nil
}

func skillOptCandidateReviewPreviewsFromMetadata(value any) []skillOptTrainCandidateReviewPreview {
	values := metadataSlice(value)
	if len(values) == 0 {
		return nil
	}
	previews := make([]skillOptTrainCandidateReviewPreview, 0, len(values))
	for _, raw := range values {
		metadata := decodedSkillOptMetadataValue(raw)
		previews = append(previews, skillOptTrainCandidateReviewPreview{
			Label:        metadataString(metadata, "label"),
			ArtifactID:   metadataString(metadata, "artifact_id"),
			Route:        metadataString(metadata, "route"),
			URL:          metadataString(metadata, "url"),
			Renderer:     metadataString(metadata, "renderer"),
			Content:      metadataString(metadata, "content"),
			Status:       metadataString(metadata, "status"),
			StatusReason: metadataString(metadata, "status_reason"),
			Error:        metadataString(metadata, "error"),
		})
	}
	return previews
}

func skillOptTrainCandidateReviewBody(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, commandHome string, publishedFiles []skillOptTrainCandidateReviewFile, publishedPreviews []skillOptTrainCandidateReviewPreview, filePublishWarnings []string) (string, error) {
	candidateID := strings.TrimSpace(iteration.CandidateVersionID)
	version, err := store.GetAgentTemplateVersionByID(ctx, candidateID)
	if err != nil {
		return "", fmt.Errorf("load candidate version %s: %w", candidateID, err)
	}
	candidateID = strings.TrimSpace(version.ID)
	review, err := store.GetAgentTemplateCandidateReview(ctx, candidateID)
	if err != nil {
		return "", fmt.Errorf("load candidate review %s: %w", candidateID, err)
	}
	baseRef := strings.TrimSpace(review.BaseVersionID)
	if baseRef == "" {
		baseRef = strings.TrimSpace(iteration.BaseTemplateVersionID)
	}
	var builder strings.Builder
	builder.WriteString("## SkillOpt Candidate Review\n\n")
	fmt.Fprintf(&builder, "Session: `%s`\n", session.ID)
	fmt.Fprintf(&builder, "Iteration: `%s`\n", iteration.ID)
	fmt.Fprintf(&builder, "Template: `%s`\n", session.TemplateID)
	fmt.Fprintf(&builder, "Base: `%s`\n", emptyText(baseRef))
	fmt.Fprintf(&builder, "Candidate: `%s`\n", candidateID)
	if summary := strings.TrimSpace(review.PreferenceSummary); summary != "" {
		fmt.Fprintf(&builder, "\n### Candidate Summary\n%s\n", summary)
	}
	skillOptWriteCandidateReviewScores(&builder, review)
	if strings.TrimSpace(session.PreviewRepo) != "" {
		fmt.Fprintf(&builder, "\nPreview repo: `%s`\n", session.PreviewRepo)
	}
	if strings.TrimSpace(iteration.PullRequestURL) != "" {
		fmt.Fprintf(&builder, "\nCandidate PR: %s\n", iteration.PullRequestURL)
	}
	if artifactIDs := skillOptCandidateReviewArtifactIDs(review); len(artifactIDs) > 0 {
		builder.WriteString("\n### Artifacts\n")
		for _, artifactID := range artifactIDs {
			fmt.Fprintf(&builder, "- `%s`\n", artifactID)
		}
	}
	if len(publishedFiles) > 0 {
		builder.WriteString("\n### GitHub Files\n")
		for _, file := range publishedFiles {
			label := strings.TrimSpace(file.Label)
			if label == "" {
				label = "File"
			}
			if strings.TrimSpace(file.URL) != "" {
				fmt.Fprintf(&builder, "- %s: [%s](%s)\n", label, file.Path, file.URL)
			} else {
				fmt.Fprintf(&builder, "- %s: `%s`\n", label, file.Path)
			}
		}
	}
	builder.WriteString("\n### Candidate Sample Preview\n")
	if len(publishedPreviews) == 0 {
		builder.WriteString("- Preview: no selected candidate sample artifact was available to publish.\n")
	} else {
		writeSkillOptCandidateSamplePreviewTable(&builder, publishedPreviews)
	}
	skillOptWriteCandidateReviewFinalEval(&builder, review)
	if len(filePublishWarnings) > 0 {
		if len(publishedFiles) == 0 {
			builder.WriteString("\n### GitHub Files\n")
		}
		for _, warning := range filePublishWarnings {
			fmt.Fprintf(&builder, "- File publish warning: `%s`\n", truncateForMetadata(warning))
		}
	}
	if strings.TrimSpace(review.EvalReportJSON) != "" {
		builder.WriteString("\nEval report: stored with the pending candidate review record.\n")
	}
	builder.WriteString("\n### Decision\n")
	usesCustomHome := strings.TrimSpace(commandHome) != ""
	promotable, reason := skillOptCandidateReviewPromotability(review)
	if promotable {
		fmt.Fprintf(&builder, "- Promote: `%s`\n", skillOptTrainCandidateDecisionCommand(usesCustomHome, session.ID, "--promote", candidateID, false))
	} else {
		fmt.Fprintf(&builder, "- Promote: unavailable because %s.\n", reason)
	}
	fmt.Fprintf(&builder, "- Reject: `%s`\n", skillOptTrainCandidateDecisionCommand(usesCustomHome, session.ID, "--reject", candidateID, true))
	fmt.Fprintf(&builder, "- Wait: take no action; `%s` will keep reporting that a candidate decision is required.\n", skillOptTrainStatusCommand(usesCustomHome, session.ID))
	fmt.Fprintf(&builder, "- Keep improving: reject with an actionable reason, then run `%s` after the rejection completes.\n", skillOptTrainStartNextCommand(usesCustomHome, session.ID))
	return builder.String(), nil
}

func writeSkillOptCandidateSamplePreviewTable(builder *strings.Builder, previews []skillOptTrainCandidateReviewPreview) {
	builder.WriteString("| Sample | Preview | Artifact | Renderer | Status |\n")
	builder.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, preview := range previews {
		label := firstNonEmpty(strings.TrimSpace(preview.Label), "Selection sample")
		fmt.Fprintf(
			builder,
			"| %s | %s | %s | %s | %s |\n",
			skillOptMarkdownTableCell(label),
			skillOptCandidateSamplePreviewCell(preview),
			skillOptMarkdownTableCell(skillOptMarkdownInlineCode(preview.ArtifactID)),
			skillOptMarkdownTableCell(skillOptMarkdownInlineCode(preview.Renderer)),
			skillOptCandidateSamplePreviewStatusCell(preview),
		)
	}
}

func skillOptCandidateSamplePreviewCell(preview skillOptTrainCandidateReviewPreview) string {
	if strings.TrimSpace(preview.Error) != "" {
		return skillOptMarkdownTableCell(skillOptMarkdownInlineCode("publish failed"))
	}
	if strings.TrimSpace(preview.Content) != "" {
		return skillOptMarkdownTableCell(skillOptMarkdownInlineCode(preview.Content))
	}
	if strings.TrimSpace(preview.URL) != "" {
		return skillOptMarkdownTableCell(fmt.Sprintf("[open](%s)", strings.TrimSpace(preview.URL)))
	}
	return skillOptMarkdownTableCell(skillOptMarkdownInlineCode(preview.ArtifactID))
}

func skillOptCandidateSamplePreviewStatusCell(preview skillOptTrainCandidateReviewPreview) string {
	if strings.TrimSpace(preview.Error) != "" {
		return skillOptMarkdownTableCell(skillOptMarkdownInlineCode("publish failed: " + truncateForMetadata(preview.Error)))
	}
	statusText := strings.TrimSpace(preview.Status)
	if statusText == "" {
		return "-"
	}
	if strings.TrimSpace(preview.StatusReason) != "" {
		statusText += ": " + strings.TrimSpace(preview.StatusReason)
	}
	return skillOptMarkdownTableCell(skillOptMarkdownInlineCode(truncateForMetadata(statusText)))
}

func skillOptWriteCandidateReviewScores(builder *strings.Builder, review db.AgentTemplateCandidateReview) {
	evalReport := decodedSkillOptMetadata(review.EvalReportJSON)
	summaryMetadata := decodedSkillOptMetadata(review.SummaryMetadataJSON)
	builder.WriteString("\n### Scores And Gate\n")
	fmt.Fprintf(builder, "- Selection score: `%s`\n", scoreText(review.Score))
	skillOptWriteCandidateReviewScoreLine(builder, "Best selection hard", metadataFloatPtr(evalReport, "best_selection_hard"))
	skillOptWriteCandidateReviewScoreLine(builder, "Best selection soft", metadataFloatPtr(evalReport, "best_selection_soft"))
	skillOptWriteCandidateReviewScoreLine(builder, "Baseline selection hard", metadataFloatPtr(evalReport, "baseline_selection_hard"))
	skillOptWriteCandidateReviewScoreLine(builder, "Baseline selection soft", metadataFloatPtr(evalReport, "baseline_selection_soft"))
	if score := metadataFloatPtr(evalReport, "score"); score != nil {
		fmt.Fprintf(builder, "- Test score: `%s`\n", scoreText(score))
	} else {
		builder.WriteString("- Test score: `-`\n")
	}
	if hard := metadataFloatPtr(evalReport, "hard"); hard != nil {
		fmt.Fprintf(builder, "- Hard score: `%s`\n", scoreText(hard))
	}
	if soft := metadataFloatPtr(evalReport, "soft"); soft != nil {
		fmt.Fprintf(builder, "- Soft score: `%s`\n", scoreText(soft))
	}
	skillOptWriteCandidateReviewScoreLine(builder, "Test hard", metadataFloatPtr(evalReport, "test_hard"))
	skillOptWriteCandidateReviewScoreLine(builder, "Test soft", metadataFloatPtr(evalReport, "test_soft"))
	skillOptWriteCandidateReviewScoreLine(builder, "Baseline test hard", metadataFloatPtr(evalReport, "baseline_test_hard"))
	skillOptWriteCandidateReviewScoreLine(builder, "Baseline test soft", metadataFloatPtr(evalReport, "baseline_test_soft"))
	if dimensions := metadataScoreMap(evalReport, "dimension_scores"); len(dimensions) > 0 {
		labels := make([]string, 0, len(dimensions))
		for label := range dimensions {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		parts := make([]string, 0, len(labels))
		for _, label := range labels {
			score := dimensions[label]
			parts = append(parts, fmt.Sprintf("%s=%s", label, scoreText(&score)))
		}
		fmt.Fprintf(builder, "- Dimension scores: `%s`\n", strings.Join(parts, ", "))
	}
	fmt.Fprintf(builder, "- Gate status: `%s`\n", firstNonEmpty(
		metadataString(evalReport, "gate_status"),
		metadataString(evalReport, "gate"),
		metadataString(summaryMetadata, "gate_status"),
		metadataString(summaryMetadata, "gate"),
		"unknown",
	))
	fmt.Fprintf(builder, "- No-op status: `%s`\n", skillOptCandidateReviewNoOpStatus(evalReport, summaryMetadata))
	promotable, reason := skillOptCandidateReviewPromotability(review)
	if promotable {
		builder.WriteString("- Promotability: `promotable`\n")
	} else {
		fmt.Fprintf(builder, "- Promotability: `not promotable: %s`\n", reason)
	}
}

func skillOptWriteCandidateReviewFinalEval(builder *strings.Builder, review db.AgentTemplateCandidateReview) {
	evalReport := decodedSkillOptMetadata(review.EvalReportJSON)
	enabled := metadataBool(evalReport, "final_eval_enabled")
	ran := metadataBool(evalReport, "final_eval_ran")
	skippedReason := metadataString(evalReport, "final_test_skipped_reason")
	if !enabled {
		builder.WriteString("- Final eval: `disabled`\n")
		builder.WriteString("  - Reason: candidate review uses selection eval by default.\n")
		return
	}
	if !ran {
		fmt.Fprintf(builder, "- Final eval: `enabled, skipped%s`\n", finalEvalReasonSuffix(skippedReason))
		return
	}
	builder.WriteString("- Final eval: `enabled, ran`\n")
	skillOptWriteCandidateReviewScoreLine(builder, "  - Final test hard", metadataFloatPtr(evalReport, "test_hard"))
	skillOptWriteCandidateReviewScoreLine(builder, "  - Final test soft", metadataFloatPtr(evalReport, "test_soft"))
}

func finalEvalReasonSuffix(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ""
	}
	return ": " + reason
}

func skillOptWriteCandidateReviewScoreLine(builder *strings.Builder, label string, score *float64) {
	if score != nil {
		fmt.Fprintf(builder, "- %s: `%s`\n", label, scoreText(score))
	}
}

func skillOptCandidateReviewPromotability(review db.AgentTemplateCandidateReview) (bool, string) {
	for _, metadata := range []map[string]any{
		decodedSkillOptMetadata(review.EvalReportJSON),
		decodedSkillOptMetadata(review.SummaryMetadataJSON),
	} {
		if promotable := metadataBoolPtr(metadata, "promotable"); promotable != nil && !*promotable {
			reason := metadataString(metadata, "no_candidate_reason")
			if reason == "" {
				reason = metadataString(metadata, "promotability_reason")
			}
			if reason == "" {
				reason = metadataString(metadata, "reason")
			}
			if reason == "" {
				reason = skillOptCandidateReviewNoOpMetadataReason(metadata)
			}
			if reason == "" {
				reason = "candidate metadata marks it as not promotable"
			}
			return false, reason
		}
		if skillOptCandidateReviewExplicitPromotable(metadata) {
			continue
		}
		if reason := metadataString(metadata, "no_candidate_reason"); reason != "" {
			return false, reason
		}
		if reason := skillOptCandidateReviewNoOpMetadataReason(metadata); reason != "" {
			return false, reason
		}
	}
	return true, ""
}

func skillOptCandidateReviewNoOpStatus(evalReport map[string]any, summaryMetadata map[string]any) string {
	for _, metadata := range []map[string]any{evalReport, summaryMetadata} {
		if skillOptCandidateReviewExplicitPromotable(metadata) {
			continue
		}
		if reason := metadataString(metadata, "no_candidate_reason"); reason != "" {
			return "blocked: " + reason
		}
	}
	for _, metadata := range []map[string]any{summaryMetadata, evalReport} {
		if skillOptCandidateReviewExplicitPromotable(metadata) {
			continue
		}
		if reason := skillOptCandidateReviewNoOpMetadataReason(metadata); reason != "" {
			return "blocked: " + reason
		}
	}
	bestOrigin := firstNonEmpty(metadataString(summaryMetadata, "best_origin"), metadataString(evalReport, "best_origin"))
	totalAccepts := firstNonEmpty(metadataString(summaryMetadata, "total_accepts"), metadataString(evalReport, "total_accepts"))
	if bestOrigin != "" || totalAccepts != "" {
		parts := []string{"not detected"}
		if bestOrigin != "" {
			parts = append(parts, "best_origin="+bestOrigin)
		}
		if totalAccepts != "" {
			parts = append(parts, "total_accepts="+totalAccepts)
		}
		return strings.Join(parts, "; ")
	}
	return "not reported"
}

func skillOptCandidateReviewExplicitPromotable(metadata map[string]any) bool {
	promotable := metadataBoolPtr(metadata, "promotable")
	return promotable != nil && *promotable
}

func skillOptCandidateReviewNoOpMetadataReason(metadata map[string]any) string {
	if strings.EqualFold(metadataString(metadata, "best_origin"), "initial_skill") {
		return "best_origin_initial_skill"
	}
	if metadataString(metadata, "total_accepts") == "0" {
		return "total_accepts_zero"
	}
	return ""
}

func metadataFloatPtr(metadata map[string]any, key string) *float64 {
	switch value := metadata[key].(type) {
	case float64:
		return &value
	case int:
		score := float64(value)
		return &score
	case int64:
		score := float64(value)
		return &score
	case json.Number:
		if score, err := value.Float64(); err == nil {
			return &score
		}
	case string:
		if score, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			return &score
		}
	}
	return nil
}

func metadataBoolPtr(metadata map[string]any, key string) *bool {
	switch value := metadata[key].(type) {
	case bool:
		return &value
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "yes":
			parsed := true
			return &parsed
		case "false", "no":
			parsed := false
			return &parsed
		}
	}
	return nil
}

func metadataBool(metadata map[string]any, key string) bool {
	value := metadataBoolPtr(metadata, key)
	return value != nil && *value
}

func metadataScoreMap(metadata map[string]any, key string) map[string]float64 {
	raw, ok := metadata[key].(map[string]any)
	if !ok {
		return nil
	}
	scores := map[string]float64{}
	for label, value := range raw {
		nested := map[string]any{"score": value}
		if score := metadataFloatPtr(nested, "score"); score != nil {
			scores[strings.TrimSpace(label)] = *score
		}
	}
	return scores
}

func skillOptCandidateReviewArtifactIDs(review db.AgentTemplateCandidateReview) []string {
	seen := map[string]struct{}{}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	add(review.DiffArtifactID)
	metadata := decodedSkillOptMetadata(review.SummaryMetadataJSON)
	rawIDs, ok := metadata["artifact_ids"].([]any)
	if !ok {
		return ids
	}
	for _, rawID := range rawIDs {
		add(fmt.Sprint(rawID))
	}
	return ids
}

func skillOptTrainCandidateDecisionCommand(usesCustomHome bool, sessionID, decisionFlag, candidateID string, includeReason bool) string {
	args := []string{"gitmoot", "skillopt", "train", "continue"}
	if usesCustomHome {
		args = append(args, "--home", "<train-home>")
	}
	args = append(args, "--session", strings.TrimSpace(sessionID), decisionFlag, strings.TrimSpace(candidateID))
	if includeReason {
		args = append(args, "--reason", "...")
	}
	return shellArgs(args)
}

func skillOptTrainStartNextCommand(usesCustomHome bool, sessionID string) string {
	args := []string{"gitmoot", "skillopt", "train", "continue"}
	if usesCustomHome {
		args = append(args, "--home", "<train-home>")
	}
	args = append(args, "--session", strings.TrimSpace(sessionID), "--start-next")
	return shellArgs(args)
}

func skillOptTrainStatusCommand(usesCustomHome bool, sessionID string) string {
	args := []string{"gitmoot", "skillopt", "train", "status"}
	if usesCustomHome {
		args = append(args, "--home", "<train-home>")
	}
	args = append(args, "--session", strings.TrimSpace(sessionID))
	return shellArgs(args)
}

func decideSkillOptTrainCandidate(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, request skillOptTrainContinueRequest) (skillOptTrainCandidateDecisionResult, error) {
	promote := strings.TrimSpace(request.PromoteCandidate)
	reject := strings.TrimSpace(request.RejectCandidate)
	if promote != "" && reject != "" {
		return skillOptTrainCandidateDecisionResult{}, errors.New("train continue accepts only one of --promote or --reject")
	}
	expected := strings.TrimSpace(iteration.CandidateVersionID)
	if expected == "" {
		return skillOptTrainCandidateDecisionResult{}, errors.New("train iteration has no candidate version")
	}
	candidateID := promote
	decision := ""
	if promote != "" {
		decision = "promoted"
	} else if reject != "" {
		candidateID = reject
		decision = "rejected"
		if strings.TrimSpace(request.DecisionReason) == "" {
			return skillOptTrainCandidateDecisionResult{}, errors.New("train candidate rejection requires --reason")
		}
	}
	if candidateID != "" && candidateID != expected {
		return skillOptTrainCandidateDecisionResult{}, fmt.Errorf("candidate %s does not match train iteration candidate %s", candidateID, expected)
	}
	if decision == "promoted" {
		if err := validateSkillOptTrainCandidatePromotableForDecision(ctx, store, candidateID); err != nil {
			return skillOptTrainCandidateDecisionResult{}, err
		}
	}
	if decision == "" {
		return syncSkillOptTrainCandidateDecision(ctx, store, session, iteration, expected, "", "")
	}
	if result, err := syncSkillOptTrainCandidateDecision(ctx, store, session, iteration, expected, decision, strings.TrimSpace(request.DecisionReason)); err != nil || result.Decided {
		return result, err
	}
	if err := skillopt.CanTransitionTrainIteration(iteration.State, map[string]string{
		"promoted": skillopt.TrainStateCandidatePromoted,
		"rejected": skillopt.TrainStateCandidateRejected,
	}[decision]); err != nil {
		return skillOptTrainCandidateDecisionResult{}, err
	}
	if decision == "promoted" {
		session.TemplateVersionID = candidateID
		session.State = skillopt.TrainStateCandidatePromoted
		iteration.State = skillopt.TrainStateCandidatePromoted
	} else {
		session.State = skillopt.TrainStateCandidateRejected
		iteration.State = skillopt.TrainStateCandidateRejected
		iteration.DecisionReason = strings.TrimSpace(request.DecisionReason)
	}
	metadata := map[string]any{
		"decision":          decision,
		"candidate_version": candidateID,
		"reason":            strings.TrimSpace(request.DecisionReason),
		"decided_at":        time.Now().UTC().Format(time.RFC3339Nano),
		"source":            "gitmoot skillopt train continue",
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_decision", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_decision", metadata)
	promoted, err := store.DecideSkillOptTrainCandidate(ctx, session, iteration, candidateID, decision)
	if err != nil {
		return skillOptTrainCandidateDecisionResult{}, err
	}
	if decision == "promoted" {
		stageSkillOptPromotionObservation(ctx, store, paths, promoted)
	}
	return skillOptTrainCandidateDecisionResult{Decided: true, Decision: decision, CandidateVersionID: candidateID}, nil
}

func validateSkillOptTrainCandidatePromotableForDecision(ctx context.Context, store *db.Store, candidateID string) error {
	review, err := store.GetAgentTemplateCandidateReview(ctx, candidateID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("load candidate review %s: %w", candidateID, err)
	}
	promotable, reason := skillOptCandidateReviewPromotability(review)
	if promotable {
		return nil
	}
	return fmt.Errorf("candidate %s is not promotable: %s", candidateID, reason)
}

func syncSkillOptTrainCandidateDecision(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, candidateID string, expectedDecision string, fallbackReason string) (skillOptTrainCandidateDecisionResult, error) {
	candidateID = strings.TrimSpace(candidateID)
	if candidateID == "" {
		return skillOptTrainCandidateDecisionResult{}, nil
	}
	candidate, err := store.GetAgentTemplateVersionByID(ctx, candidateID)
	if err != nil {
		return skillOptTrainCandidateDecisionResult{}, fmt.Errorf("load candidate version %s: %w", candidateID, err)
	}
	review, reviewErr := store.GetAgentTemplateCandidateReview(ctx, candidateID)
	if reviewErr != nil && !errors.Is(reviewErr, sql.ErrNoRows) {
		return skillOptTrainCandidateDecisionResult{}, fmt.Errorf("load candidate review %s: %w", candidateID, reviewErr)
	}
	var decision string
	switch candidate.State {
	case "current":
		decision = "promoted"
	case "rejected":
		decision = "rejected"
	default:
		if reviewErr == nil {
			switch strings.TrimSpace(review.State) {
			case "promoted":
				decision = "promoted"
			case "rejected":
				decision = "rejected"
			}
		}
		if decision == "" {
			return skillOptTrainCandidateDecisionResult{}, nil
		}
	}
	if expectedDecision != "" && expectedDecision != decision {
		return skillOptTrainCandidateDecisionResult{}, fmt.Errorf("candidate %s is already %s, not %s", candidateID, decision, expectedDecision)
	}
	targetState := map[string]string{
		"promoted": skillopt.TrainStateCandidatePromoted,
		"rejected": skillopt.TrainStateCandidateRejected,
	}[decision]
	switch skillopt.NormalizeTrainState(iteration.State) {
	case skillopt.TrainStateCandidateCreated, skillopt.TrainStateCandidateReviewPublished:
	default:
		if err := skillopt.CanTransitionTrainIteration(iteration.State, targetState); err != nil {
			return skillOptTrainCandidateDecisionResult{}, err
		}
	}
	reason := strings.TrimSpace(fallbackReason)
	if decision == "rejected" {
		if reviewErr == nil && strings.TrimSpace(review.DecisionReason) != "" {
			reason = strings.TrimSpace(review.DecisionReason)
		}
		if reason == "" {
			return skillOptTrainCandidateDecisionResult{}, errors.New("train candidate rejection requires --reason")
		}
	}
	if decision == "promoted" {
		session.TemplateVersionID = candidateID
		session.State = skillopt.TrainStateCandidatePromoted
		iteration.State = skillopt.TrainStateCandidatePromoted
	} else {
		session.State = skillopt.TrainStateCandidateRejected
		iteration.State = skillopt.TrainStateCandidateRejected
		iteration.DecisionReason = reason
	}
	metadata := map[string]any{
		"decision":          decision,
		"candidate_version": candidateID,
		"reason":            reason,
		"decided_at":        time.Now().UTC().Format(time.RFC3339Nano),
		"source":            "gitmoot skillopt train continue synced candidate state",
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_decision", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_decision", metadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainCandidateDecisionResult{}, err
	}
	return skillOptTrainCandidateDecisionResult{Decided: true, Decision: decision, CandidateVersionID: candidateID}, nil
}

func startNextSkillOptTrainIteration(ctx context.Context, store *db.Store, session db.SkillOptTrainSession, previous db.SkillOptTrainIteration) (db.SkillOptTrainIteration, error) {
	if err := skillopt.CanStartNextTrainIteration(previous); err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	releaseStartNextLock, _, err := acquireSkillOptTrainStartNextLock(ctx, store, session.ID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	defer func() {
		_ = releaseStartNextLock(context.Background())
	}()
	baseVersion := strings.TrimSpace(previous.BaseTemplateVersionID)
	if skillopt.NormalizeTrainState(previous.State) == skillopt.TrainStateCandidatePromoted {
		baseVersion = strings.TrimSpace(previous.CandidateVersionID)
	}
	if baseVersion == "" {
		return db.SkillOptTrainIteration{}, errors.New("next train iteration base version is required")
	}
	previousRun, err := store.GetEvalRun(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, fmt.Errorf("load previous eval run %s: %w", previous.EvalRunID, err)
	}
	iterations, err := store.ListSkillOptTrainIterations(ctx, session.ID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	nextNumber := len(iterations) + 1
	nextID := fmt.Sprintf("%s-%03d", session.ID, nextNumber)
	nextRunID := fmt.Sprintf("%s-review-%03d", session.ID, nextNumber)
	if _, err := store.GetSkillOptTrainIteration(ctx, nextID); err == nil {
		return db.SkillOptTrainIteration{}, fmt.Errorf("train iteration %s already exists; inspect it with gitmoot skillopt train status --session %s", nextID, session.ID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return db.SkillOptTrainIteration{}, err
	}
	if _, err := store.GetEvalRun(ctx, nextRunID); err == nil {
		return db.SkillOptTrainIteration{}, fmt.Errorf("eval run %s already exists; inspect it with gitmoot skillopt review status --run %s", nextRunID, nextRunID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return db.SkillOptTrainIteration{}, err
	}
	metadata := skillOptTrainNextIterationMetadata(session.MetadataJSON, previous.MetadataJSON, map[string]any{
		"id":         previous.ID,
		"state":      previous.State,
		"candidate":  previous.CandidateVersionID,
		"started_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	items, err := store.ListEvalReviewItems(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	feedbackEvents, err := store.ListFeedbackEvents(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	rankedFeedbackEvents, err := store.ListRankedFeedbackEvents(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	pairwisePreferences, err := store.ListPairwisePreferences(ctx, previous.EvalRunID)
	if err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	recommendation := skillopt.RecommendPhaseForItems(previousRun, items, feedbackEvents, rankedFeedbackEvents, pairwisePreferences)
	nextMode := skillOptTrainNextIterationMode(previous.Mode, recommendation.RecommendedMode)
	nextExplorationLevel := strings.TrimSpace(recommendation.ExplorationLevel)
	if nextExplorationLevel == "" {
		nextExplorationLevel = previous.ExplorationLevel
	}
	metadata = mergeSkillOptTrainMetadata(metadata, "phase_recommendation", map[string]any{
		"current_mode":     recommendation.CurrentMode,
		"recommended_mode": recommendation.RecommendedMode,
		"selected_mode":    nextMode,
		"reason":           recommendation.Reason,
	})
	next := db.SkillOptTrainIteration{
		ID:                    nextID,
		SessionID:             session.ID,
		EvalRunID:             nextRunID,
		BaseTemplateVersionID: baseVersion,
		Mode:                  nextMode,
		ExplorationLevel:      nextExplorationLevel,
		State:                 skillopt.TrainStateItemsReady,
		MetadataJSON:          metadata,
	}
	run := db.EvalRun{
		ID:                nextRunID,
		TemplateID:        session.TemplateID,
		TemplateVersionID: baseVersion,
		TargetRepo:        session.TargetRepo,
		State:             "review",
		Mode:              nextMode,
		ExplorationLevel:  nextExplorationLevel,
		OptionsCount:      previousRun.OptionsCount,
		MetadataJSON:      metadata,
	}
	session.TemplateVersionID = baseVersion
	session.State = skillopt.TrainStateItemsReady
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "next_iteration", map[string]any{
		"id":           next.ID,
		"base_version": baseVersion,
		"source":       "gitmoot skillopt train continue",
	})
	nextItems := make([]db.EvalReviewItem, 0, len(items))
	for _, item := range items {
		item.RunID = nextRunID
		item.ID = ""
		item.BaselineArtifactID = ""
		item.CandidateArtifactID = ""
		item.PreviewArtifactID = ""
		item.DiffArtifactID = ""
		nextItems = append(nextItems, item)
	}
	if err := store.UpsertSkillOptTrainNextIteration(ctx, session, next, run, nextItems); err != nil {
		return db.SkillOptTrainIteration{}, err
	}
	return next, nil
}

func skillOptTrainNextIterationMetadata(sessionMetadata string, previousMetadata string, previousIteration map[string]any) string {
	metadata := map[string]any{
		"previous_iteration": previousIteration,
	}
	for _, source := range []string{previousMetadata, sessionMetadata} {
		evaluation := decodedSkillOptMetadataValue(decodedSkillOptMetadata(source)["evaluation"])
		if len(evaluation) > 0 {
			metadata["evaluation"] = evaluation
			break
		}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func skillOptTrainNextIterationMode(previousMode string, recommendedMode string) string {
	switch strings.TrimSpace(recommendedMode) {
	case db.EvalRunModeExplore, db.EvalRunModeRefine, db.EvalRunModeDistill, db.EvalRunModeValidate:
		return strings.TrimSpace(recommendedMode)
	default:
		return strings.TrimSpace(previousMode)
	}
}

func skillOptTrainDecisionMetadata(existing string, reason string) string {
	var metadata map[string]any
	if strings.TrimSpace(existing) != "" {
		_ = json.Unmarshal([]byte(existing), &metadata)
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["decision_reason"] = strings.TrimSpace(reason)
	metadata["decision"] = skillopt.TrainStateRunAbandoned
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return existing
	}
	return string(encoded)
}

func runSkillOptCandidate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSkillOptUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runSkillOptCandidateList(args[1:], stdout, stderr)
	case "show":
		return runSkillOptCandidateShow(args[1:], stdout, stderr)
	case "promote":
		return runSkillOptCandidatePromote(args[1:], stdout, stderr)
	case "reject":
		return runSkillOptCandidateReject(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown skillopt candidate command %q\n\n", args[0])
		printSkillOptUsage(stderr)
		return 2
	}
}

func runSkillOptCandidateList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt candidate list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	templateID := fs.String("template", "", "template id to filter")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "skillopt candidate list does not accept positional arguments")
		return 2
	}
	var versions []db.AgentTemplateVersion
	var reviews map[string]db.AgentTemplateCandidateReview
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		versions, err = store.ListPendingAgentTemplateVersions(context.Background(), *templateID)
		if err != nil {
			return err
		}
		reviews = make(map[string]db.AgentTemplateCandidateReview, len(versions))
		for _, version := range versions {
			review, err := store.GetAgentTemplateCandidateReview(context.Background(), version.ID)
			if err == nil {
				reviews[version.ID] = review
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate list: %v\n", err)
		return 1
	}
	if len(versions) == 0 {
		writeLine(stdout, "no pending candidates")
		return 0
	}
	fmt.Fprintf(stdout, "%-18s %-14s %-9s %-8s %s\n", "VERSION", "TEMPLATE", "STATE", "SCORE", "SUMMARY")
	for _, version := range versions {
		review := reviews[version.ID]
		fmt.Fprintf(stdout, "%-18s %-14s %-9s %-8s %s\n", version.ID, version.TemplateID, version.State, scoreText(review.Score), firstLine(review.PreferenceSummary))
	}
	return 0
}

func runSkillOptCandidateShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt candidate show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "skillopt candidate show requires exactly one version id")
		return 2
	}
	versionID := fs.Arg(0)
	var version db.AgentTemplateVersion
	var review db.AgentTemplateCandidateReview
	var hasReview bool
	var base db.AgentTemplate
	var hasBase bool
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		version, err = store.GetAgentTemplateVersionByID(context.Background(), versionID)
		if err != nil {
			return err
		}
		review, err = store.GetAgentTemplateCandidateReview(context.Background(), version.ID)
		if err == nil {
			hasReview = true
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		baseRef := strings.TrimSpace(review.BaseVersionID)
		if baseRef == "" {
			current, err := store.GetAgentTemplate(context.Background(), version.TemplateID)
			if err != nil {
				return err
			}
			baseRef = current.VersionID
		}
		if baseRef != "" && baseRef != version.ID {
			base, err = store.GetAgentTemplateReference(context.Background(), baseRef)
			if err == nil {
				hasBase = true
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate show: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "version: %s\n", version.ID)
	fmt.Fprintf(stdout, "template: %s\n", version.TemplateID)
	fmt.Fprintf(stdout, "state: %s\n", version.State)
	fmt.Fprintf(stdout, "source: %s@%s:%s\n", version.SourceRepo, version.SourceRef, version.SourcePath)
	fmt.Fprintf(stdout, "content_hash: %s\n", version.ContentHash)
	if hasReview {
		fmt.Fprintf(stdout, "base_version: %s\n", emptyText(review.BaseVersionID))
		fmt.Fprintf(stdout, "score: %s\n", scoreText(review.Score))
		fmt.Fprintf(stdout, "preference_summary: %s\n", emptyText(review.PreferenceSummary))
		fmt.Fprintf(stdout, "diff_artifact: %s\n", emptyText(review.DiffArtifactID))
		if strings.TrimSpace(review.EvalReportJSON) != "" {
			fmt.Fprintf(stdout, "eval_report:\n%s\n", indentJSON(review.EvalReportJSON))
		}
		if strings.TrimSpace(review.DecisionReason) != "" {
			fmt.Fprintf(stdout, "decision_reason: %s\n", review.DecisionReason)
		}
	}
	if hasBase {
		diff := artifact.TextDriver{}.Diff(base.VersionID+".md", version.ID+".md", []byte(base.Content), []byte(version.Content))
		fmt.Fprintf(stdout, "content_diff:\n%s", diff)
	}
	return 0
}

func runSkillOptCandidatePromote(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skillopt candidate promote", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "skillopt candidate promote requires exactly one version id")
		return 2
	}
	var promoted db.AgentTemplateVersion
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		var err error
		promoted, err = store.PromoteAgentTemplateVersion(context.Background(), fs.Arg(0))
		if err == nil {
			stageSkillOptPromotionObservation(context.Background(), store, paths, promoted)
		}
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate promote: %v\n", err)
		return 1
	}
	writeLine(stdout, "promoted candidate %s", promoted.ID)
	return 0
}

func runSkillOptCandidateReject(args []string, stdout, stderr io.Writer) int {
	parsed, help, ok := parseSkillOptCandidateRejectArgs(args, stderr)
	if help {
		printSkillOptUsage(stdout)
		return 0
	}
	if !ok {
		return 2
	}
	if parsed.versionID == "" {
		fmt.Fprintln(stderr, "skillopt candidate reject requires exactly one version id")
		return 2
	}
	if parsed.extraVersion {
		fmt.Fprintln(stderr, "skillopt candidate reject requires exactly one version id")
		return 2
	}
	var rejected db.AgentTemplateVersion
	if err := withStore(parsed.home, func(store *db.Store) error {
		var err error
		rejected, _, err = store.RejectAgentTemplateVersion(context.Background(), parsed.versionID, parsed.reason)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "skillopt candidate reject: %v\n", err)
		return 1
	}
	writeLine(stdout, "rejected candidate %s", rejected.ID)
	return 0
}

type skillOptCandidateRejectArgs struct {
	home         string
	reason       string
	versionID    string
	extraVersion bool
}

func parseSkillOptCandidateRejectArgs(args []string, stderr io.Writer) (skillOptCandidateRejectArgs, bool, bool) {
	var parsed skillOptCandidateRejectArgs
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			return parsed, true, true
		case arg == "--home" || arg == "--reason":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "skillopt candidate reject: %s requires a value\n", arg)
				return parsed, false, false
			}
			i++
			if arg == "--home" {
				parsed.home = args[i]
			} else {
				parsed.reason = args[i]
			}
		case strings.HasPrefix(arg, "--home="):
			parsed.home = strings.TrimPrefix(arg, "--home=")
		case strings.HasPrefix(arg, "--reason="):
			parsed.reason = strings.TrimPrefix(arg, "--reason=")
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(stderr, "skillopt candidate reject: unknown flag %s\n", arg)
			return parsed, false, false
		case parsed.versionID == "":
			parsed.versionID = arg
		default:
			parsed.extraVersion = true
		}
	}
	return parsed, false, true
}

func writeSkillOptFile(path string, content []byte) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("output path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	return os.WriteFile(path, content, 0o644)
}
