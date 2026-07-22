package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/feedback"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/skillopt"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

var skillOptTrainPreviewRunner subprocess.Runner = subprocess.ExecRunner{}

type skillOptTrainReviewPublishResult struct {
	Repo        github.Repository
	IssueNumber int64
	URL         string
	PreviewURLs int
	// LocalSurface is set when the review was recorded against the local markdown
	// surface (#738) — the human already imported feedback (TrainingReady) before
	// the publish step, so no GitHub issue was created and no watch was registered.
	LocalSurface bool
}

type skillOptPreviewPublication struct {
	URL          string
	CommitSHA    string
	PagesStatus  string
	StatusReason string
}

type skillOptLatestPagesBuild struct {
	Status string `json:"status"`
	Error  struct {
		Message string `json:"message"`
	} `json:"error"`
	CommitSHA string `json:"commit_sha"`
	Commit    string `json:"commit"`
}

const skillOptReviewWatchDefaultStaleThreshold = 24 * time.Hour

func autoSyncSkillOptTrainReviewFeedback(ctx context.Context, paths config.Paths, store *db.Store, iteration db.SkillOptTrainIteration) ([]string, bool) {
	repoText := strings.TrimSpace(iteration.IssueRepo)
	issueNumber := iteration.IssueNumber
	if repoText == "" || issueNumber <= 0 {
		return nil, false
	}
	repo, err := daemon.ParseRepository(repoText)
	if err != nil {
		return []string{
			"github_feedback_sync: failed",
			fmt.Sprintf("github_feedback_error: invalid review repo %q: %v", repoText, err),
		}, false
	}
	client := newSkillOptGitHubClient()
	if err := client.Preflight(ctx, repo); err != nil {
		return []string{
			"github_feedback_sync: failed",
			fmt.Sprintf("github_feedback_error: %v", err),
		}, false
	}
	collector := feedback.GitHubCollector{
		BlobStore: artifact.NewStore(paths.ArtifactBlobs),
		GitHub:    client,
	}
	result, err := collector.Sync(ctx, store, iteration.EvalRunID, repo, issueNumber)
	if err != nil {
		return []string{
			"github_feedback_sync: failed",
			fmt.Sprintf("github_feedback_error: %v", err),
		}, false
	}
	lines := []string{
		"github_feedback_sync: imported",
		fmt.Sprintf("github_feedback_events: %d", result.Count()),
	}
	for _, diagnostic := range result.Diagnostics {
		lines = append(lines, fmt.Sprintf("github_feedback_diagnostic: %s", diagnostic))
	}
	return lines, result.Count() > 0
}

// maybePublishSkillOptTrainReviewLocally records the review against the local
// markdown surface and advances to review_published when the iteration's eval run
// already reports TrainingReady — i.e. the human imported feedback locally before
// the publish step (#738). It reports ok=true only when it took the local path.
//
// When feedback is not yet ready it returns (_, false, nil) so the caller falls
// through to the byte-identical GitHub publish path. It also declines the local
// path (returns false) when a prior GitHub post actually completed but its state
// update was interrupted (a published_external recovery marker): that real issue
// must be honored via recover, not shadowed by a local record. Unlike the GitHub
// path it registers NO review watch (there is no issue to poll) and writes review
// metadata {status: published, review_surface: local}.
func maybePublishSkillOptTrainReviewLocally(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (skillOptTrainReviewPublishResult, bool, error) {
	status, err := loadSkillOptReviewStatus(ctx, store, artifact.NewStore(paths.ArtifactBlobs), iteration.EvalRunID)
	if err != nil {
		return skillOptTrainReviewPublishResult{}, false, err
	}
	if !status.TrainingReady {
		return skillOptTrainReviewPublishResult{}, false, nil
	}
	if review, ok, rerr := readSkillOptTrainReviewRecovery(paths, session, iteration); rerr == nil && ok {
		if metadataString(review, "status") == "published_external" {
			return skillOptTrainReviewPublishResult{}, false, nil
		}
	}
	localMetadata := map[string]any{
		"status":         "published",
		"review_surface": "local",
		"preview_urls":   0,
		"published_at":   time.Now().UTC().Format(time.RFC3339Nano),
		"source":         "gitmoot skillopt train continue",
	}
	session.State = skillopt.TrainStateReviewPublished
	iteration.State = skillopt.TrainStateReviewPublished
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "review", localMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "review", localMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainReviewPublishResult{}, false, err
	}
	_ = removeSkillOptTrainReviewRecovery(paths, session, iteration)
	return skillOptTrainReviewPublishResult{LocalSurface: true}, true, nil
}

func publishSkillOptTrainReview(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (skillOptTrainReviewPublishResult, error) {
	if err := skillopt.CanTransitionTrainIteration(iteration.State, skillopt.TrainStateReviewPublished); err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	if strings.TrimSpace(iteration.EvalRunID) == "" {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("train iteration %s has no eval run id", iteration.ID)
	}
	// Local-review short-circuit (#738): publish is a review SURFACE, and the local
	// markdown surface is already first-class for feedback import. If the human
	// already imported feedback locally (the eval run reports TrainingReady) BEFORE
	// this publish step, there is nothing to post — a GitHub issue would only be a
	// doomed duplicate (its packet exceeds GitHub's 64KB issue-body limit anyway).
	// Record the review as published against the local surface and advance to
	// review_published without any GitHub call or watch registration; the next
	// continue hop takes the existing TrainingReady -> feedback_synced path. This
	// runs BEFORE recover, so it also unwedges a session whose prior GitHub attempt
	// left a stale posting_external marker. When feedback is not yet ready this is a
	// no-op and GitHub stays the default publish surface (byte-identical).
	if local, ok, err := maybePublishSkillOptTrainReviewLocally(ctx, paths, store, session, iteration); err != nil {
		return skillOptTrainReviewPublishResult{}, err
	} else if ok {
		return local, nil
	}
	if recovered, ok, err := recoverSkillOptTrainReviewPublication(ctx, paths, store, session, iteration); err != nil {
		return skillOptTrainReviewPublishResult{}, err
	} else if ok {
		return recovered, nil
	}
	previewURLs, err := publishSkillOptTrainPreviewURLs(ctx, paths, store, session, iteration)
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	repo, err := resolveSkillOptFeedbackRepo(ctx, paths, store, run, "")
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	client := newSkillOptGitHubClient()
	if err := client.Preflight(ctx, repo); err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	publishingMetadata := map[string]any{
		"status":       "publishing",
		"repo":         repo.FullName(),
		"preview_urls": previewURLs,
		"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
		"source":       "gitmoot skillopt train continue",
	}
	if err := writeSkillOptTrainReviewRecovery(paths, session, iteration, publishingMetadata); err != nil {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("write review pre-publish recovery marker: %w", err)
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "review", publishingMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "review", publishingMetadata)
	if err := store.UpsertSkillOptTrainSessionAndIteration(ctx, session, iteration); err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	collector := feedback.GitHubCollector{BlobStore: artifact.NewStore(paths.ArtifactBlobs)}
	body, err := collector.Body(ctx, store, run.ID)
	if err != nil {
		return skillOptTrainReviewPublishResult{}, err
	}
	// TODO(#738 part 3): when body exceeds GitHub's ~64KB issue-body limit, degrade
	// gracefully instead of letting CreateIssue 422 — post a summary issue plus the
	// packet as trailing comments (each <=64KB) or a linked artifact. Until then a
	// 422 is caught below and the marker is downgraded (part 2) so the retry falls
	// through to the local surface when feedback is already imported (part 1).
	postingMetadata := make(map[string]any, len(publishingMetadata)+2)
	for key, value := range publishingMetadata {
		postingMetadata[key] = value
	}
	postingMetadata["status"] = "posting_external"
	postingMetadata["external_post_started_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeSkillOptTrainReviewRecovery(paths, session, iteration, postingMetadata); err != nil {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("write review external-post recovery marker: %w", err)
	}
	issue, err := client.CreateIssue(ctx, github.CreateIssueInput{
		Repo:  repo,
		Title: fmt.Sprintf("Gitmoot SkillOpt feedback: %s", strings.TrimSpace(run.ID)),
		Body:  body,
	})
	if err != nil {
		downgradeReviewMarkerOnLaunchFailure(paths, session, iteration, postingMetadata, err)
		return skillOptTrainReviewPublishResult{}, err
	}
	published := feedback.GitHubPublishResult{Repo: repo, IssueNumber: issue.Number, URL: issue.URL, Mode: "issue"}
	externalMetadata := map[string]any{
		"status":       "published_external",
		"repo":         published.Repo.FullName(),
		"issue_number": published.IssueNumber,
		"url":          published.URL,
		"preview_urls": previewURLs,
		"published_at": time.Now().UTC().Format(time.RFC3339Nano),
		"source":       "gitmoot skillopt train continue",
	}
	externalMarkerErr := writeSkillOptTrainReviewRecovery(paths, session, iteration, externalMetadata)
	session.State = skillopt.TrainStateReviewPublished
	iteration.State = skillopt.TrainStateReviewPublished
	iteration.IssueRepo = published.Repo.FullName()
	iteration.IssueNumber = published.IssueNumber
	iteration.IssueURL = published.URL
	dbMetadata := make(map[string]any, len(externalMetadata))
	for key, value := range externalMetadata {
		dbMetadata[key] = value
	}
	dbMetadata["status"] = "published"
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "review", dbMetadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "review", dbMetadata)
	watch, watchErr := skillOptTrainReviewWatch(ctx, store, iteration, published.Repo, published.IssueNumber, published.URL, previewURLs, "gitmoot skillopt train continue")
	if watchErr != nil {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("review was published at %s but watch registration preparation failed: %w", published.URL, watchErr)
	}
	if err := store.UpsertSkillOptTrainSessionIterationAndReviewWatch(ctx, session, iteration, watch); err != nil {
		if externalMarkerErr != nil {
			return skillOptTrainReviewPublishResult{}, fmt.Errorf("%w; review was published at %s but recovery marker write failed: %v", err, published.URL, externalMarkerErr)
		}
		return skillOptTrainReviewPublishResult{}, err
	}
	if externalMarkerErr != nil {
		return skillOptTrainReviewPublishResult{}, fmt.Errorf("review was published at %s and recorded in local state but recovery marker write failed: %w", published.URL, externalMarkerErr)
	}
	_ = removeSkillOptTrainReviewRecovery(paths, session, iteration)
	return skillOptTrainReviewPublishResult{Repo: published.Repo, IssueNumber: published.IssueNumber, URL: published.URL, PreviewURLs: previewURLs}, nil
}

func recoverSkillOptTrainReviewPublication(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (skillOptTrainReviewPublishResult, bool, error) {
	review, ok, err := readSkillOptTrainReviewRecovery(paths, session, iteration)
	if err != nil || !ok {
		return skillOptTrainReviewPublishResult{}, ok, err
	}
	status := metadataString(review, "status")
	if status == "posting_external" {
		return skillOptTrainReviewPublishResult{}, true, errors.New("train review publication was interrupted after the external GitHub post started; inspect the review repo before retrying to avoid duplicate issues")
	}
	if status != "published_external" {
		return skillOptTrainReviewPublishResult{}, false, nil
	}
	repo, err := daemon.ParseRepository(metadataString(review, "repo"))
	if err != nil {
		return skillOptTrainReviewPublishResult{}, true, err
	}
	issueNumber := int64(metadataNumber(review, "issue_number"))
	url := metadataString(review, "url")
	previewURLs := metadataNumber(review, "preview_urls")
	session.State = skillopt.TrainStateReviewPublished
	iteration.State = skillopt.TrainStateReviewPublished
	iteration.IssueRepo = repo.FullName()
	iteration.IssueNumber = issueNumber
	iteration.IssueURL = url
	metadata := map[string]any{
		"status":       "published",
		"repo":         repo.FullName(),
		"issue_number": issueNumber,
		"url":          url,
		"preview_urls": previewURLs,
		"published_at": time.Now().UTC().Format(time.RFC3339Nano),
		"source":       "gitmoot skillopt train continue",
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "review", metadata)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "review", metadata)
	watch, err := skillOptTrainReviewWatch(ctx, store, iteration, repo, issueNumber, url, previewURLs, "gitmoot skillopt train recover")
	if err != nil {
		return skillOptTrainReviewPublishResult{}, true, err
	}
	if err := store.UpsertSkillOptTrainSessionIterationAndReviewWatch(ctx, session, iteration, watch); err != nil {
		return skillOptTrainReviewPublishResult{}, true, err
	}
	_ = removeSkillOptTrainReviewRecovery(paths, session, iteration)
	return skillOptTrainReviewPublishResult{Repo: repo, IssueNumber: issueNumber, URL: url, PreviewURLs: previewURLs}, true, nil
}

func skillOptTrainReviewWatch(ctx context.Context, store *db.Store, iteration db.SkillOptTrainIteration, repo github.Repository, issueNumber int64, url string, previewURLs int, source string) (db.SkillOptReviewWatch, error) {
	if store == nil {
		return db.SkillOptReviewWatch{}, errors.New("store is required")
	}
	runID := strings.TrimSpace(iteration.EvalRunID)
	if runID == "" {
		return db.SkillOptReviewWatch{}, errors.New("train review watch run id is required")
	}
	items, err := store.ListEvalReviewItems(ctx, runID)
	if err != nil {
		return db.SkillOptReviewWatch{}, err
	}
	itemIDs := make([]string, 0, len(items))
	for _, item := range items {
		if itemID := strings.TrimSpace(item.ItemID); itemID != "" {
			itemIDs = append(itemIDs, itemID)
		}
	}
	itemIDsJSON, err := json.Marshal(itemIDs)
	if err != nil {
		return db.SkillOptReviewWatch{}, err
	}
	metadata := map[string]any{
		"session_id":   strings.TrimSpace(iteration.SessionID),
		"iteration_id": strings.TrimSpace(iteration.ID),
		"issue_url":    strings.TrimSpace(url),
		"preview_urls": previewURLs,
		"source":       strings.TrimSpace(source),
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return db.SkillOptReviewWatch{}, err
	}
	return db.SkillOptReviewWatch{
		Repo:                  repo.FullName(),
		IssueNumber:           issueNumber,
		RunID:                 runID,
		ExpectedItemIDsJSON:   string(itemIDsJSON),
		Status:                db.SkillOptReviewWatchStatusWatching,
		StaleAfter:            time.Now().UTC().Add(skillOptReviewWatchDefaultStaleThreshold).Format(time.RFC3339Nano),
		StaleThresholdSeconds: int64(skillOptReviewWatchDefaultStaleThreshold.Seconds()),
		MetadataJSON:          string(metadataJSON),
	}, nil
}

// isExecLaunchFailure reports whether err is a failure to even launch the gh
// subprocess — a missing binary (exec.ErrNotFound) or a failed fork/exec such as
// ARG_MAX "argument list too long" (#734). In every such case the child process
// never started, so no GitHub request left this machine.
func isExecLaunchFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "fork/exec") ||
		strings.Contains(msg, "argument list too long")
}

// downgradeReviewMarkerOnLaunchFailure clears the conservative posting_external
// latch when the gh publish failed in a way that PROVES no GitHub issue was
// created, so the next attempt can proceed fresh instead of wedging behind a
// phantom duplicate. recover treats the posting_external marker as "the post may
// have happened, a human must inspect before retrying"; both downgraded statuses
// below fall outside {posting_external, published_external}, so recover reports
// "no external post occurred" and lets the retry through.
//
// Two failure classes qualify:
//   - an exec-launch failure (#734/#735): fork/exec / ARG_MAX / a missing binary —
//     the gh child never started, so the request never left this machine. Marker
//     rewritten as failed_pre_exec.
//   - a definitive 4xx API rejection (#738): HTTP 422 validation (e.g. a body over
//     GitHub's 64KB issue-body limit), 404, or 403 — gh ran and GitHub declined the
//     request outright, so nothing was created. Marker rewritten as
//     failed_external_rejected.
//
// Every ambiguous failure (a timeout, a mid-flight kill, a 5xx, a dropped socket)
// may have reached GitHub and mutated state, so the latch is deliberately left
// intact and a human must inspect before retrying.
func downgradeReviewMarkerOnLaunchFailure(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, postingMetadata map[string]any, err error) {
	downgraded, ok := reviewMarkerDowngradeStatus(err)
	if !ok {
		return
	}
	failed := make(map[string]any, len(postingMetadata)+2)
	for key, value := range postingMetadata {
		failed[key] = value
	}
	failed["status"] = downgraded
	failed["downgraded_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	_ = writeSkillOptTrainReviewRecovery(paths, session, iteration, failed)
}

// reviewMarkerDowngradeStatus reports the recovery-marker status a failed gh
// publish should be downgraded to, and whether it qualifies for a downgrade at
// all. It maps an exec-launch failure to failed_pre_exec and a definitive 4xx API
// rejection to failed_external_rejected; every ambiguous failure returns ok=false
// so the conservative posting_external latch is preserved (see
// downgradeReviewMarkerOnLaunchFailure).
func reviewMarkerDowngradeStatus(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if isExecLaunchFailure(err) {
		return "failed_pre_exec", true
	}
	if github.IsDefinitiveRejectionMessage(err.Error()) {
		return "failed_external_rejected", true
	}
	return "", false
}

func writeSkillOptTrainReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration, metadata map[string]any) error {
	path := skillOptTrainReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return errors.New("review recovery path is required")
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

func readSkillOptTrainReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (map[string]any, bool, error) {
	path := skillOptTrainReviewRecoveryPath(paths, session, iteration)
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
		return nil, true, fmt.Errorf("read review recovery marker %s: %w", path, err)
	}
	return metadata, true, nil
}

func removeSkillOptTrainReviewRecovery(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) error {
	path := skillOptTrainReviewRecoveryPath(paths, session, iteration)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func skillOptTrainReviewRecoveryPath(paths config.Paths, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) string {
	if strings.TrimSpace(paths.Home) == "" {
		return ""
	}
	name := skillOptCandidateReviewRecoveryName(session.ID, iteration.ID)
	if name == "" {
		return ""
	}
	return filepath.Join(paths.Home, "skillopt", "reviews", name+".json")
}

func publishSkillOptTrainPreviewURLs(ctx context.Context, paths config.Paths, store *db.Store, session db.SkillOptTrainSession, iteration db.SkillOptTrainIteration) (int, error) {
	policy := skillopt.ResolveTrainPreviewPolicy(session)
	if policy.Mode == skillopt.TrainPreviewModeNone || policy.Renderer == skillopt.TrainPreviewRendererNone || policy.Publisher == skillopt.TrainPreviewPublisherNone {
		return 0, nil
	}
	if policy.Renderer != skillopt.TrainPreviewRendererVueVite || policy.Publisher != skillopt.TrainPreviewPublisherGitHubPages {
		if policy.Mode == skillopt.TrainPreviewModeRequired {
			return 0, fmt.Errorf("preview renderer %s with publisher %s is not implemented", policy.Renderer, policy.Publisher)
		}
		return 0, nil
	}
	run, err := store.GetEvalRun(ctx, iteration.EvalRunID)
	if err != nil {
		return 0, err
	}
	options, err := store.ListEvalReviewOptions(ctx, run.ID, "")
	if err != nil {
		return 0, err
	}
	if len(options) == 0 {
		if policy.Mode == skillopt.TrainPreviewModeRequired {
			return 0, fmt.Errorf("train run %s has no generated options to publish previews", run.ID)
		}
		return 0, nil
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	previewRepo, err := previewRepoRecord(ctx, store, policy)
	if err != nil {
		if policy.Mode == skillopt.TrainPreviewModeRequired {
			return 0, err
		}
		return convertOptionalPreviewBundlesToFallback(ctx, store, blobStore, options)
	}
	if err := requireCleanPreviewRepo(ctx, previewRepo.CheckoutPath); err != nil {
		if policy.Mode == skillopt.TrainPreviewModeRequired {
			return 0, err
		}
		return convertOptionalPreviewBundlesToFallback(ctx, store, blobStore, options)
	}
	publishedCount := 0
	for _, option := range options {
		metadata := optionMetadataMap(option.MetadataJSON)
		if metadataStringValue(metadata, "preview_url") != "" || metadataStringValue(metadata, "url") != "" {
			publishedCount++
			continue
		}
		if metadata["preview_bundle"] == nil {
			if policy.Mode == skillopt.TrainPreviewModeRequired {
				return publishedCount, fmt.Errorf("item %s option %s is missing preview bundle metadata", option.ItemID, option.Label)
			}
			continue
		}
		record, err := store.GetEvalArtifact(ctx, option.ArtifactID)
		if err != nil {
			return publishedCount, fmt.Errorf("item %s option %s artifact: %w", option.ItemID, option.Label, err)
		}
		content, err := blobStore.Read(record.Hash)
		if err != nil {
			return publishedCount, fmt.Errorf("item %s option %s preview bundle blob: %w", option.ItemID, option.Label, err)
		}
		bundle, err := skillopt.ParsePreviewBundle(content)
		if err != nil {
			if policy.Mode == skillopt.TrainPreviewModeRequired {
				return publishedCount, fmt.Errorf("item %s option %s preview bundle: %w", option.ItemID, option.Label, err)
			}
			continue
		}
		distDir, cleanup, err := renderVueVitePreviewBundle(ctx, bundle)
		if err != nil {
			if policy.Mode == skillopt.TrainPreviewModeRequired {
				return publishedCount, fmt.Errorf("item %s option %s render preview: %w", option.ItemID, option.Label, err)
			}
			continue
		}
		route, err := skillOptPreviewRoute(policy.RouteTemplate, run.ID, option.ItemID, option.Label)
		if err != nil {
			_ = cleanup()
			return publishedCount, err
		}
		publication, err := publishGitHubPagesPreview(ctx, previewRepo, route, distDir)
		_ = cleanup()
		if err != nil {
			if policy.Mode == skillopt.TrainPreviewModeRequired {
				return publishedCount, fmt.Errorf("item %s option %s publish preview: %w", option.ItemID, option.Label, err)
			}
			continue
		}
		metadata["preview_url"] = publication.URL
		metadata["preview_route"] = route
		metadata["preview_repo"] = previewRepo.FullName()
		metadata["preview_commit"] = publication.CommitSHA
		metadata["preview_status"] = publication.PagesStatus
		if strings.TrimSpace(publication.StatusReason) != "" {
			metadata["preview_status_reason"] = publication.StatusReason
		}
		option.MetadataJSON = encodeOptionMetadata(metadata)
		if err := store.UpsertEvalReviewOption(ctx, option); err != nil {
			return publishedCount, err
		}
		publishedCount++
	}
	if policy.Mode == skillopt.TrainPreviewModeRequired && publishedCount != len(options) {
		return publishedCount, fmt.Errorf("required preview run has %d/%d preview URLs", publishedCount, len(options))
	}
	if policy.Mode == skillopt.TrainPreviewModeOptional {
		freshOptions, err := store.ListEvalReviewOptions(ctx, run.ID, "")
		if err != nil {
			return publishedCount, err
		}
		if _, err := convertOptionalPreviewBundlesToFallback(ctx, store, blobStore, freshOptions); err != nil {
			return publishedCount, err
		}
	}
	return publishedCount, nil
}

func convertOptionalPreviewBundlesToFallback(ctx context.Context, store *db.Store, blobStore artifact.Store, options []db.EvalReviewOption) (int, error) {
	for _, option := range options {
		metadata := optionMetadataMap(option.MetadataJSON)
		if metadata["preview_bundle"] == nil || metadataStringValue(metadata, "preview_url") != "" || metadataStringValue(metadata, "url") != "" {
			continue
		}
		record, err := store.GetEvalArtifact(ctx, option.ArtifactID)
		if err != nil {
			return 0, err
		}
		content, err := blobStore.Read(record.Hash)
		if err != nil {
			return 0, err
		}
		bundle, err := skillopt.ParsePreviewBundle(content)
		if err != nil {
			return 0, err
		}
		fallbackContent := []byte(previewBundleInlineFallback(bundle))
		blob, err := blobStore.Put(fallbackContent)
		if err != nil {
			return 0, err
		}
		record.Hash = blob.Hash
		record.MediaType = "text/markdown"
		record.SizeBytes = blob.Size
		record.Driver = "text"
		if err := store.UpsertEvalArtifact(ctx, record); err != nil {
			return 0, err
		}
		delete(metadata, "preview_bundle")
		metadata["preview_fallback"] = "inline"
		metadata["preview_fallback_renderer"] = skillopt.TrainPreviewRendererVueVite
		option.MetadataJSON = encodeOptionMetadata(metadata)
		if err := store.UpsertEvalReviewOption(ctx, option); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

func previewBundleInlineFallback(bundle skillopt.PreviewBundle) string {
	var builder strings.Builder
	builder.WriteString("# Vue/Vite preview source\n\n")
	builder.WriteString("A clickable preview was not available, so this option is included as inline Vue/Vite source for review.\n\n")
	for _, file := range bundle.Files {
		fmt.Fprintf(&builder, "## `%s`\n\n", file.Path)
		writeSkillOptMarkdownFence(&builder, file.Content)
		builder.WriteString("\n")
	}
	return builder.String()
}

func skillOptMarkdownInlineCode(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\n", " ")
	value = strings.ReplaceAll(value, "`", "'")
	if value == "" {
		return "-"
	}
	return "`" + value + "`"
}

func skillOptMarkdownTableCell(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\n", " ")
	value = strings.ReplaceAll(value, "|", "\\|")
	if value == "" {
		return "-"
	}
	return value
}

func previewRepoRecord(ctx context.Context, store *db.Store, policy skillopt.TrainPreviewPolicy) (db.Repo, error) {
	repoName := strings.TrimSpace(policy.Repo)
	if repoName == "" {
		return db.Repo{}, errors.New("preview repo is required")
	}
	repo, err := store.GetRepo(ctx, repoName)
	if err != nil {
		return db.Repo{}, fmt.Errorf("preview repo %s is not registered with a checkout path; run `gitmoot repo add %s --path /path/to/checkout`: %w", repoName, repoName, err)
	}
	if strings.TrimSpace(repo.CheckoutPath) == "" {
		return db.Repo{}, fmt.Errorf("preview repo %s has no checkout path; run `gitmoot repo add %s --path /path/to/checkout`", repoName, repoName)
	}
	if _, err := os.Stat(repo.CheckoutPath); err != nil {
		return db.Repo{}, fmt.Errorf("preview repo %s checkout is not ready: %w", repo.FullName(), err)
	}
	return repo, nil
}

func requireCleanPreviewRepo(ctx context.Context, checkout string) error {
	result, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("check preview repo status: %w", err)
	}
	if strings.TrimSpace(result.Stdout) != "" {
		return fmt.Errorf("preview repo checkout %s is dirty; commit or clean it before publishing previews", checkout)
	}
	return nil
}

func renderVueVitePreviewBundle(ctx context.Context, bundle skillopt.PreviewBundle) (string, func() error, error) {
	workDir, err := os.MkdirTemp("", "gitmoot-vue-preview-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() error { return os.RemoveAll(workDir) }
	for _, file := range bundle.Files {
		target, err := safeJoinPreviewPath(workDir, file.Path)
		if err != nil {
			_ = cleanup()
			return "", nil, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			_ = cleanup()
			return "", nil, err
		}
		if err := os.WriteFile(target, []byte(file.Content), 0o644); err != nil {
			_ = cleanup()
			return "", nil, err
		}
	}
	if err := writeTrustedVueViteScaffold(workDir); err != nil {
		_ = cleanup()
		return "", nil, err
	}
	if _, err := skillOptTrainPreviewRunner.Run(ctx, workDir, "npm", "install", "--ignore-scripts"); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("npm install: %w", err)
	}
	if _, err := skillOptTrainPreviewRunner.Run(ctx, workDir, "npm", "run", "build"); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("%s: %w", bundle.BuildCommand, err)
	}
	distDir, err := safeJoinPreviewPath(workDir, bundle.DistDir)
	if err != nil {
		_ = cleanup()
		return "", nil, err
	}
	if _, err := os.Stat(filepath.Join(distDir, "index.html")); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("preview build output missing index.html in %s: %w", bundle.DistDir, err)
	}
	return distDir, cleanup, nil
}

func publishGitHubPagesPreview(ctx context.Context, repo db.Repo, route string, distDir string) (skillOptPreviewPublication, error) {
	checkout := strings.TrimSpace(repo.CheckoutPath)
	if err := requireCleanPreviewRepo(ctx, checkout); err != nil {
		return skillOptPreviewPublication{}, err
	}
	head, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "rev-parse", "HEAD")
	if err != nil {
		return skillOptPreviewPublication{}, fmt.Errorf("read preview repo head: %w", err)
	}
	headSHA := strings.TrimSpace(head.Stdout)
	target, err := safeJoinPreviewPath(checkout, route)
	if err != nil {
		return skillOptPreviewPublication{}, err
	}
	if err := os.RemoveAll(target); err != nil {
		return skillOptPreviewPublication{}, err
	}
	if err := copyDir(distDir, target); err != nil {
		restorePreviewRoute(ctx, checkout, route, headSHA)
		return skillOptPreviewPublication{}, err
	}
	if _, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "add", "--", route); err != nil {
		restorePreviewRoute(ctx, checkout, route, headSHA)
		return skillOptPreviewPublication{}, fmt.Errorf("git add preview route: %w", err)
	}
	status, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "status", "--porcelain", "--", route)
	if err != nil {
		restorePreviewRoute(ctx, checkout, route, headSHA)
		return skillOptPreviewPublication{}, fmt.Errorf("check preview route status: %w", err)
	}
	if strings.TrimSpace(status.Stdout) != "" {
		if _, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "commit", "-m", "Publish SkillOpt preview "+strings.TrimSuffix(route, "/")); err != nil {
			restorePreviewRoute(ctx, checkout, route, headSHA)
			return skillOptPreviewPublication{}, fmt.Errorf("git commit preview route: %w", err)
		}
		if _, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "push"); err != nil {
			restorePreviewRoute(ctx, checkout, route, headSHA)
			return skillOptPreviewPublication{}, fmt.Errorf("git push preview route: %w", err)
		}
	}
	commit, err := skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "rev-parse", "HEAD")
	if err != nil {
		return skillOptPreviewPublication{}, fmt.Errorf("read published preview commit: %w", err)
	}
	commitSHA := strings.TrimSpace(commit.Stdout)
	pagesStatus, pagesReason := observeGitHubPagesBuildStatus(ctx, repo, commitSHA)
	return skillOptPreviewPublication{
		URL:          githubPagesURL(repo, route),
		CommitSHA:    commitSHA,
		PagesStatus:  pagesStatus,
		StatusReason: pagesReason,
	}, nil
}

func observeGitHubPagesBuildStatus(ctx context.Context, repo db.Repo, commitSHA string) (string, string) {
	return observeGitHubPagesBuildStatusWithPoll(ctx, repo, commitSHA, 15*time.Second, 2*time.Second)
}

func observeGitHubPagesBuildStatusWithPoll(ctx context.Context, repo db.Repo, commitSHA string, timeout time.Duration, interval time.Duration) (string, string) {
	fullName := repo.FullName()
	if strings.TrimSpace(fullName) == "" {
		return "pending", "preview repo is unknown"
	}
	deadline := time.Now().Add(timeout)
	for {
		status, reason, done := readGitHubPagesBuildStatus(ctx, repo, commitSHA)
		if done {
			return status, reason
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			return status, reason
		}
		select {
		case <-ctx.Done():
			return "pending", "latest GitHub Pages build wait was canceled: " + ctx.Err().Error()
		case <-time.After(interval):
		}
	}
}

func readGitHubPagesBuildStatus(ctx context.Context, repo db.Repo, commitSHA string) (string, string, bool) {
	fullName := repo.FullName()
	result, err := skillOptTrainPreviewRunner.Run(ctx, strings.TrimSpace(repo.CheckoutPath), "gh", "api", "repos/"+fullName+"/pages/builds/latest")
	if err != nil {
		return "pending", "latest GitHub Pages build could not be read: " + err.Error(), true
	}
	var build skillOptLatestPagesBuild
	if err := json.Unmarshal([]byte(result.Stdout), &build); err != nil {
		return "pending", "latest GitHub Pages build response could not be decoded: " + err.Error(), true
	}
	status := normalizeGitHubPagesBuildStatus(build.Status)
	buildCommit := firstNonEmpty(strings.TrimSpace(build.CommitSHA), strings.TrimSpace(build.Commit))
	if buildCommit != "" && commitSHA != "" && !strings.EqualFold(buildCommit, commitSHA) {
		return "stale", fmt.Sprintf("latest GitHub Pages build is for commit %s, expected %s", buildCommit, commitSHA), false
	}
	if status == "failed" && strings.TrimSpace(build.Error.Message) != "" {
		return status, strings.TrimSpace(build.Error.Message), true
	}
	if status == "pending" {
		return status, "", false
	}
	return status, "", true
}

func normalizeGitHubPagesBuildStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "built":
		return "ready"
	case "errored":
		return "failed"
	case "building", "queued":
		return "pending"
	case "":
		return "pending"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func restorePreviewRoute(ctx context.Context, checkout string, route string, headSHA string) {
	if strings.TrimSpace(headSHA) != "" {
		_, _ = skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "reset", "--hard", headSHA)
	}
	_, _ = skillOptTrainPreviewRunner.Run(ctx, checkout, "git", "clean", "-fd", "--", route)
}

func skillOptPreviewRoute(template string, runID string, itemID string, label string) (string, error) {
	if strings.TrimSpace(template) == "" {
		template = skillopt.DefaultTrainPreviewRouteTemplate
	}
	route := strings.ReplaceAll(template, "{run_id}", previewRouteSlug(runID))
	route = strings.ReplaceAll(route, "{item_id}", previewRouteSlug(itemID))
	route = strings.ReplaceAll(route, "{option_label}", previewRouteSlug(label))
	route = strings.TrimSpace(route)
	if route == "" {
		return "", errors.New("preview route is required")
	}
	route = strings.TrimPrefix(route, "/")
	clean := filepath.ToSlash(filepath.Clean(route))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, ":") {
		return "", fmt.Errorf("preview route %q is unsafe", route)
	}
	if !strings.HasSuffix(clean, "/") {
		clean += "/"
	}
	return clean, nil
}

func safeJoinPreviewPath(root string, relative string) (string, error) {
	root = filepath.Clean(root)
	relative = filepath.FromSlash(strings.TrimSpace(relative))
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("preview path %q must be relative", relative)
	}
	target := filepath.Clean(filepath.Join(root, relative))
	if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("preview path %q must stay inside %s", relative, root)
	}
	return target, nil
}

func copyDir(source string, target string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		targetPath := filepath.Join(target, entry.Name())
		if entry.IsDir() {
			if err := copyDir(sourcePath, targetPath); err != nil {
				return err
			}
			continue
		}
		content, err := os.ReadFile(sourcePath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(targetPath, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func githubPagesURL(repo db.Repo, route string) string {
	if strings.EqualFold(repo.Name, repo.Owner+".github.io") {
		return fmt.Sprintf("https://%s.github.io/%s", repo.Owner, strings.TrimLeft(route, "/"))
	}
	return fmt.Sprintf("https://%s.github.io/%s/%s", repo.Owner, repo.Name, strings.TrimLeft(route, "/"))
}

func writeTrustedVueViteScaffold(workDir string) error {
	packageJSON := `{"type":"module","scripts":{"build":"vite build"},"dependencies":{"@vitejs/plugin-vue":"latest","vite":"latest","vue":"latest"}}`
	if err := os.WriteFile(filepath.Join(workDir, "package.json"), []byte(packageJSON), 0o644); err != nil {
		return err
	}
	indexHTML := `<div id="app"></div><script type="module" src="/src/main.js"></script>`
	if err := os.WriteFile(filepath.Join(workDir, "index.html"), []byte(indexHTML), 0o644); err != nil {
		return err
	}
	mainPath := filepath.Join(workDir, "src", "main.js")
	if err := os.MkdirAll(filepath.Dir(mainPath), 0o755); err != nil {
		return err
	}
	mainJS := `import { createApp } from 'vue'; import App from './App.vue'; createApp(App).mount('#app');`
	if err := os.WriteFile(mainPath, []byte(mainJS), 0o644); err != nil {
		return err
	}
	viteConfig := "import { defineConfig } from 'vite';\nimport vue from '@vitejs/plugin-vue';\n\nexport default defineConfig({ base: './', plugins: [vue()] });\n"
	return os.WriteFile(filepath.Join(workDir, "vite.config.js"), []byte(viteConfig), 0o644)
}

func previewRouteSlug(value string) string {
	trimmed := strings.TrimSpace(value)
	var builder strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(trimmed) {
		allowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if allowed {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(builder.String(), ".-_")
	if slug == "" {
		slug = "value"
	}
	if slug != trimmed {
		slug = slug + "-" + shortHash(trimmed)
	}
	return slug
}

func optionMetadataMap(metadataJSON string) map[string]any {
	metadata := map[string]any{}
	if strings.TrimSpace(metadataJSON) != "" {
		_ = json.Unmarshal([]byte(metadataJSON), &metadata)
	}
	return metadata
}

func metadataStringValue(metadata map[string]any, key string) string {
	if value, ok := metadata[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func metadataNumber(metadata map[string]any, key string) int {
	switch value := metadata[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	default:
		return 0
	}
}

func encodeOptionMetadata(metadata map[string]any) string {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return ""
	}
	return string(encoded)
}
