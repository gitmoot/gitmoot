package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// seedSkillOptTrainOptionsGeneratedForReview seeds a train session parked at
// options_generated with an eval run whose review packet is ready. When
// includeFeedback is true the run also carries an imported human feedback event,
// so loadSkillOptReviewStatus reports TrainingReady (the local-review surface is
// already complete, #738); when false the run is packet-ready but training-blocked
// so GitHub stays the default publish surface. It returns the isolated home.
func seedSkillOptTrainOptionsGeneratedForReview(t *testing.T, includeFeedback bool) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := cliSkillOptTemplate("planner", "Plan the work.")
	if err := store.UpsertAgentTemplate(context.Background(), template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	baselineBlob, err := blobStore.Put([]byte("# Baseline\n\nPlan the edit.\n"))
	if err != nil {
		t.Fatalf("Put baseline returned error: %v", err)
	}
	candidateBlob, err := blobStore.Put([]byte("# Candidate\n\nPlan the edit and verification.\n"))
	if err != nil {
		t.Fatalf("Put candidate returned error: %v", err)
	}
	for _, record := range []db.EvalArtifact{
		{ID: "baseline-artifact", Hash: baselineBlob.Hash, MediaType: "text/markdown", SizeBytes: baselineBlob.Size, Driver: "text"},
		{ID: "candidate-artifact", Hash: candidateBlob.Hash, MediaType: "text/markdown", SizeBytes: candidateBlob.Size, Driver: "text"},
	} {
		if err := store.UpsertEvalArtifact(context.Background(), record); err != nil {
			t.Fatalf("UpsertEvalArtifact returned error: %v", err)
		}
	}
	previewPolicy, err := skillopt.BuildTrainPreviewPolicy("owner/product", "", "", "", "", "")
	if err != nil {
		t.Fatalf("BuildTrainPreviewPolicy returned error: %v", err)
	}
	metadata := skillOptTrainStartMetadata("Train planner outputs from human feedback.", db.EvalRunModeValidate, db.ExplorationLevelLow, 2, "hard_then_soft", nil, nil, previewPolicy, skillOptTrainStartConfigDefaults{}, nil)
	session := db.SkillOptTrainSession{
		ID:                "optimizer-train",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/product",
		RequestSummary:    "Train planner outputs from human feedback.",
		TaskKind:          "custom",
		State:             skillopt.TrainStateOptionsGenerated,
		MetadataJSON:      metadata,
	}
	iteration := db.SkillOptTrainIteration{
		ID:                    "optimizer-train-001",
		SessionID:             session.ID,
		EvalRunID:             "optimizer-train-review-001",
		BaseTemplateVersionID: installed.VersionID,
		Mode:                  db.EvalRunModeValidate,
		ExplorationLevel:      db.ExplorationLevelLow,
		State:                 skillopt.TrainStateOptionsGenerated,
		MetadataJSON:          metadata,
	}
	run := db.EvalRun{
		ID:                iteration.EvalRunID,
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/product",
		State:             "review",
		Mode:              db.EvalRunModeValidate,
		ExplorationLevel:  db.ExplorationLevelLow,
		OptionsCount:      2,
		MetadataJSON:      metadata,
	}
	if err := store.UpsertSkillOptTrainSession(context.Background(), session); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainIteration(context.Background(), iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration returned error: %v", err)
	}
	if err := store.UpsertEvalRun(context.Background(), run); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:               run.ID,
		ItemID:              "item-001",
		Title:               "README plan",
		BaselineArtifactID:  "baseline-artifact",
		CandidateArtifactID: "candidate-artifact",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if includeFeedback {
		if err := store.UpsertFeedbackEvent(context.Background(), db.FeedbackEvent{
			RunID:     run.ID,
			ItemID:    "item-001",
			Choice:    "b",
			Reasoning: "Candidate is more complete.",
			Reviewer:  "markdown:jerry",
			Source:    "markdown",
			SourceURL: "local://feedback.md",
			CreatedAt: "2026-07-08T10:00:00Z",
		}); err != nil {
			t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
		}
	}
	return home
}

// recordingGitHubClientFactory installs a newSkillOptGitHubClient factory that
// counts constructions and hands back a call-recording fake, restoring the prior
// factory on cleanup. A local-review continue hop (#738) must never construct a
// client; *constructed stays 0 and the fake's fields stay empty.
func recordingGitHubClientFactory(t *testing.T) (constructed *int, fake **skillOptFakeGitHub) {
	t.Helper()
	var count int
	shared := &skillOptFakeGitHub{}
	prev := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client {
		count++
		return shared
	}
	t.Cleanup(func() { newSkillOptGitHubClient = prev })
	return &count, &shared
}

// TestSkillOptTrainContinueLocalReviewSkipsGitHub asserts that when the eval run is
// already TrainingReady at the publish step, `train continue` records the review
// against the local surface and advances to review_published without constructing
// a GitHub client or registering a review watch (#738, scenario a).
func TestSkillOptTrainContinueLocalReviewSkipsGitHub(t *testing.T) {
	home := seedSkillOptTrainOptionsGeneratedForReview(t, true)
	constructed, fake := recordingGitHubClientFactory(t)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"current_phase: review_published",
		"review_surface: local",
		"continue_ready: true",
		"next: run train continue to sync the imported feedback",
	} {
		if !bytes.Contains(stdout.Bytes(), []byte(want)) {
			t.Fatalf("train continue stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if *constructed != 0 {
		t.Fatalf("GitHub client constructed %d times on the local-review path, want 0", *constructed)
	}
	if len((*fake).preflightRepos) != 0 || (*fake).createdIssue.Repo.FullName() != "" {
		t.Fatalf("GitHub was invoked on the local-review path: preflight=%+v issue=%+v", (*fake).preflightRepos, (*fake).createdIssue)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if session.State != skillopt.TrainStateReviewPublished || iteration.State != skillopt.TrainStateReviewPublished {
		t.Fatalf("states = session %s iteration %s, want review_published", session.State, iteration.State)
	}
	for _, want := range []string{`"review_surface":"local"`, `"status":"published"`} {
		if !bytes.Contains([]byte(iteration.MetadataJSON), []byte(want)) {
			t.Fatalf("iteration metadata missing %q: %s", want, iteration.MetadataJSON)
		}
		if !bytes.Contains([]byte(session.MetadataJSON), []byte(want)) {
			t.Fatalf("session metadata missing %q: %s", want, session.MetadataJSON)
		}
	}
	// No GitHub issue was recorded and no review watch was registered.
	if iteration.IssueRepo != "" || iteration.IssueNumber != 0 || iteration.IssueURL != "" {
		t.Fatalf("local review recorded a GitHub issue: repo=%q number=%d url=%q", iteration.IssueRepo, iteration.IssueNumber, iteration.IssueURL)
	}
	watches, err := store.ListSkillOptReviewWatches(context.Background(), "")
	if err != nil {
		t.Fatalf("ListSkillOptReviewWatches returned error: %v", err)
	}
	if len(watches) != 0 {
		t.Fatalf("local review registered %d watches, want 0: %+v", len(watches), watches)
	}
}

// TestSkillOptTrainContinuePublishesToGitHubWhenFeedbackNotReady is the regression
// guard for #738: when feedback is not yet imported at the publish step, continue
// must still publish the review packet to GitHub exactly as before — construct the
// client, create the issue, register a watch, and advance to review_published.
func TestSkillOptTrainContinuePublishesToGitHubWhenFeedbackNotReady(t *testing.T) {
	home := seedSkillOptTrainOptionsGeneratedForReview(t, false)
	constructed, fake := recordingGitHubClientFactory(t)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"current_phase: review_published",
		"review_repo: owner/product",
		"preview_urls: 0",
		"next: wait for feedback, then run train continue after sync",
	} {
		if !bytes.Contains(stdout.Bytes(), []byte(want)) {
			t.Fatalf("train continue stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if bytes.Contains(stdout.Bytes(), []byte("review_surface: local")) {
		t.Fatalf("GitHub publish path emitted a local-surface line:\n%s", stdout.String())
	}
	if *constructed == 0 {
		t.Fatal("GitHub client was never constructed on the not-ready publish path")
	}
	if (*fake).createdIssue.Repo.FullName() != "owner/product" {
		t.Fatalf("review issue was not created on the not-ready path: %+v", (*fake).createdIssue)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateReviewPublished {
		t.Fatalf("iteration state = %s, want review_published", iteration.State)
	}
	if iteration.IssueRepo != "owner/product" || iteration.IssueNumber != 8 {
		t.Fatalf("GitHub issue not recorded: repo=%q number=%d", iteration.IssueRepo, iteration.IssueNumber)
	}
	if bytes.Contains([]byte(iteration.MetadataJSON), []byte(`"review_surface":"local"`)) {
		t.Fatalf("GitHub publish recorded a local surface: %s", iteration.MetadataJSON)
	}
	watches, err := store.ListSkillOptReviewWatches(context.Background(), "")
	if err != nil {
		t.Fatalf("ListSkillOptReviewWatches returned error: %v", err)
	}
	if len(watches) != 1 {
		t.Fatalf("GitHub publish registered %d watches, want 1", len(watches))
	}
}

// TestDowngradeReviewMarkerOnDefinitiveRejection asserts #738 part 2: a definitive
// 4xx API rejection (422/404/403) downgrades the posting_external latch so the next
// attempt proceeds fresh, while an ambiguous failure (5xx/timeout) preserves it.
func TestDowngradeReviewMarkerOnDefinitiveRejection(t *testing.T) {
	paths := config.PathsForHome(t.TempDir())
	session := db.SkillOptTrainSession{ID: "sess-738"}
	iteration := db.SkillOptTrainIteration{ID: "iter-738"}
	posting := map[string]any{
		"status":                   "posting_external",
		"repo":                     "owner/reviews",
		"external_post_started_at": "2026-07-08T00:00:00Z",
	}
	writePosting := func(t *testing.T) {
		t.Helper()
		if err := writeSkillOptTrainReviewRecovery(paths, session, iteration, posting); err != nil {
			t.Fatalf("write posting_external marker: %v", err)
		}
	}
	markerStatus := func(t *testing.T) string {
		t.Helper()
		review, ok, err := readSkillOptTrainReviewRecovery(paths, session, iteration)
		if err != nil {
			t.Fatalf("read recovery marker: %v", err)
		}
		if !ok {
			return "<absent>"
		}
		return metadataString(review, "status")
	}

	downgrades := []struct {
		name string
		err  error
	}{
		{"http 422 validation", errors.New("gh: Validation Failed (HTTP 422): exit status 1")},
		{"http 404 repo", errors.New("HTTP 404: Not Found (https://api.github.com/...): exit status 1")},
		{"http 403 forbidden", errors.New("HTTP 403: Resource not accessible by integration: exit status 1")},
	}
	for _, tc := range downgrades {
		t.Run(tc.name+" clears the latch and permits a retry", func(t *testing.T) {
			writePosting(t)
			downgradeReviewMarkerOnLaunchFailure(paths, session, iteration, posting, tc.err)
			if got := markerStatus(t); got != "failed_external_rejected" {
				t.Fatalf("marker status after definitive rejection = %q, want failed_external_rejected", got)
			}
			_, ok, err := recoverSkillOptTrainReviewPublication(context.Background(), paths, (*db.Store)(nil), session, iteration)
			if err != nil {
				t.Fatalf("recover after downgrade returned error: %v", err)
			}
			if ok {
				t.Fatal("recover latched a downgraded marker; a retry must be permitted")
			}
		})
	}

	ambiguous := []struct {
		name string
		err  error
	}{
		{"http 502 gateway", errors.New("HTTP 502 Bad Gateway: exit status 1")},
		{"http 503 unavailable", errors.New("HTTP 503 Service Unavailable: exit status 1")},
		{"mid-flight timeout", errors.New("context deadline exceeded")},
		{"killed mid-flight", errors.New("signal: killed")},
		{"socket drop", errors.New("unexpected EOF")},
	}
	for _, tc := range ambiguous {
		t.Run(tc.name+" preserves the conservative latch", func(t *testing.T) {
			writePosting(t)
			downgradeReviewMarkerOnLaunchFailure(paths, session, iteration, posting, tc.err)
			if got := markerStatus(t); got != "posting_external" {
				t.Fatalf("marker status after ambiguous failure = %q, want posting_external", got)
			}
			_, ok, err := recoverSkillOptTrainReviewPublication(context.Background(), paths, (*db.Store)(nil), session, iteration)
			if err == nil {
				t.Fatal("recover did not latch posting_external; duplicate-issue guard lost")
			}
			if !ok {
				t.Fatal("recover should report ok=true when latched")
			}
		})
	}
}

// TestSkillOptTrainContinueLocalReviewEndToEnd drives the full local-review path
// (#738, scenario d): a session parked at options_generated with imported
// TrainingReady feedback reaches feedback_synced across two continue hops without
// ever constructing a GitHub client.
func TestSkillOptTrainContinueLocalReviewEndToEnd(t *testing.T) {
	home := seedSkillOptTrainOptionsGeneratedForReview(t, true)
	constructed, fake := recordingGitHubClientFactory(t)

	// Hop 1: options_generated -> review_published (local surface, no GitHub).
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("hop 1 exit code = %d, stderr=%s", code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("current_phase: review_published")) ||
		!bytes.Contains(stdout.Bytes(), []byte("review_surface: local")) {
		t.Fatalf("hop 1 stdout = %s", stdout.String())
	}

	// Hop 2: review_published -> feedback_synced (feedback already imported).
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("hop 2 exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"current_phase: feedback_synced",
		"feedback_events: 1",
		"next: export the training package before running the optimizer",
	} {
		if !bytes.Contains(stdout.Bytes(), []byte(want)) {
			t.Fatalf("hop 2 stdout missing %q:\n%s", want, stdout.String())
		}
	}
	// No GitHub feedback sync should have run on the local path.
	if bytes.Contains(stdout.Bytes(), []byte("github_feedback_sync")) {
		t.Fatalf("hop 2 attempted a GitHub feedback sync on the local path:\n%s", stdout.String())
	}
	if *constructed != 0 {
		t.Fatalf("GitHub client constructed %d times across the local-review E2E, want 0", *constructed)
	}
	if len((*fake).preflightRepos) != 0 || (*fake).createdIssue.Repo.FullName() != "" {
		t.Fatalf("GitHub was invoked across the local-review E2E: preflight=%+v issue=%+v", (*fake).preflightRepos, (*fake).createdIssue)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if session.State != skillopt.TrainStateFeedbackSynced || iteration.State != skillopt.TrainStateFeedbackSynced {
		t.Fatalf("states = session %s iteration %s, want feedback_synced", session.State, iteration.State)
	}
}
