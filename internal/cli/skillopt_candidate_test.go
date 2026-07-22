package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

func TestSkillOptTrainContinuePublishesCandidateReviewPromotesAndStartsNext(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	session.WorkspaceRepo = "owner/workspace"
	if err := store.UpsertSkillOptTrainSession(context.Background(), session); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession workspace returned error: %v", err)
	}
	run, err := store.GetEvalRun(context.Background(), "optimizer-train-review-001")
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	run.OptionsCount = 4
	if err := store.UpsertEvalRun(context.Background(), run); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	optionBlobStore := artifact.NewStore(config.PathsForHome(home).ArtifactBlobs)
	for _, label := range []string{"a", "b", "c", "d"} {
		content := []byte("option " + label)
		blob, err := optionBlobStore.Put(content)
		if err != nil {
			t.Fatalf("Put option %s returned error: %v", label, err)
		}
		if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
			ID:        "option-" + label,
			Hash:      blob.Hash,
			MediaType: "text/markdown",
			SizeBytes: blob.Size,
			Driver:    "text",
		}); err != nil {
			t.Fatalf("UpsertEvalArtifact option %s returned error: %v", label, err)
		}
		if err := store.UpsertEvalReviewOption(context.Background(), db.EvalReviewOption{
			RunID:      run.ID,
			ItemID:     "item-001",
			Label:      label,
			ArtifactID: "option-" + label,
			Role:       "option",
		}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	ranking, err := json.Marshal([]string{"b", "a", "c", "d"})
	if err != nil {
		t.Fatalf("marshal ranking: %v", err)
	}
	if err := store.UpsertRankedFeedbackEvent(context.Background(), db.RankedFeedbackEvent{
		RunID:        run.ID,
		ItemID:       "item-001",
		RankingJSON:  string(ranking),
		Winner:       "b",
		ContinueMode: db.EvalRunModeExplore,
		Reviewer:     "github:jerry",
		Source:       "github",
		SourceURL:    "https://github.com/owner/product/issues/1#issuecomment-ranked",
		CreatedAt:    "2026-06-02T10:01:00Z",
	}); err != nil {
		t.Fatalf("UpsertRankedFeedbackEvent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after options count update returned error: %v", err)
	}
	candidatePackage := cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with candidate review guidance.")
	selectionScore := 0.73
	candidatePackage.Summary.Score = &selectionScore
	candidatePackage.Summary.Metadata = json.RawMessage(`{"best_origin":"candidate","total_accepts":2,"promotable":true,"no_candidate_reason":null}`)
	candidatePackage.EvalReport = json.RawMessage(`{"score":0.86,"hard":0.91,"soft":0.84,"best_selection_hard":0.77,"best_selection_soft":0.78,"baseline_selection_hard":0.55,"baseline_selection_soft":0.56,"test_hard":0.72,"test_soft":0.74,"baseline_test_hard":0.5,"baseline_test_soft":0.52,"dimension_scores":{"hero_quality":0.8},"gate_status":"passed","promotable":true,"no_candidate_reason":"stale_non_blocking_reason"}`)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: candidatePackage,
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--promote", "planner@v2",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("premature promote exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "candidate decisions require train iteration at candidate_review_published") {
		t.Fatalf("premature promote stderr = %q", stderr.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("premature promote published github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}

	fakeGitHub.createIssueErr = errors.New("github unavailable")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue failed candidate review exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "github unavailable") {
		t.Fatalf("failed candidate review stderr = %q", stderr.String())
	}
	store = openCLIJobStore(t, home)
	session, err = store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession after failed publish returned error: %v", err)
	}
	failedIteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after failed publish returned error: %v", err)
	}
	if session.State != skillopt.TrainStateCandidateCreated || failedIteration.State != skillopt.TrainStateCandidateCreated {
		t.Fatalf("state after failed candidate review publish: session=%s iteration=%s", session.State, failedIteration.State)
	}
	if failedIteration.IssueNumber != 0 || strings.TrimSpace(failedIteration.IssueURL) != "" || strings.Contains(failedIteration.MetadataJSON, `"status":"published"`) {
		t.Fatalf("iteration recorded failed candidate review publish: %+v", failedIteration)
	}
	markerPath := skillOptCandidateReviewRecoveryPath(config.PathsForHome(home), session, failedIteration)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("failed candidate review recovery marker err=%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after failed candidate review publish returned error: %v", err)
	}

	fakeGitHub.createIssueErr = nil
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue after ambiguous publish error exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "external post started") || !strings.Contains(stderr.String(), "SkillOpt candidate review: optimizer-train") {
		t.Fatalf("ambiguous publish retry stderr = %q", stderr.String())
	}
	if err := removeSkillOptCandidateReviewRecovery(config.PathsForHome(home), session, failedIteration); err != nil {
		t.Fatalf("remove ambiguous publish recovery marker returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue candidate review exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") || !strings.Contains(stdout.String(), "candidate_review: https://github.com/owner/workspace/issues/8") {
		t.Fatalf("candidate review stdout = %s", stdout.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "owner/workspace" {
		t.Fatalf("created issue repo = %+v", fakeGitHub.createdIssue.Repo)
	}
	for _, want := range []string{
		"SkillOpt Candidate Review",
		"### Artifacts",
		"`candidate-diff`",
		"### GitHub Files",
		"Best skill",
		"best_skill.md",
		"Base skill",
		"base_skill.md",
		"Candidate diff",
		"candidate.diff.md",
		"### Scores And Gate",
		"Selection score: `0.73`",
		"Best selection hard: `0.77`",
		"Best selection soft: `0.78`",
		"Baseline selection hard: `0.55`",
		"Baseline selection soft: `0.56`",
		"Test score: `0.86`",
		"Hard score: `0.91`",
		"Soft score: `0.84`",
		"Test hard: `0.72`",
		"Test soft: `0.74`",
		"Baseline test hard: `0.5`",
		"Baseline test soft: `0.52`",
		"Dimension scores: `hero_quality=0.8`",
		"Gate status: `passed`",
		"No-op status: `not detected; best_origin=candidate; total_accepts=2`",
		"Promotability: `promotable`",
		"planner@v2",
		skillOptTrainCandidateDecisionCommand(true, "optimizer-train", "--promote", "planner@v2", false),
		skillOptTrainCandidateDecisionCommand(true, "optimizer-train", "--reject", "planner@v2", true),
		"Wait: take no action",
		"Keep improving: reject with an actionable reason",
		skillOptTrainStartNextCommand(true, "optimizer-train"),
	} {
		if !strings.Contains(fakeGitHub.createdIssue.Body, want) {
			t.Fatalf("candidate review body missing %q:\n%s", want, fakeGitHub.createdIssue.Body)
		}
	}
	if len(fakeGitHub.upsertedFiles) != 3 {
		t.Fatalf("published candidate review files = %+v, want 3", fakeGitHub.upsertedFiles)
	}
	for _, want := range []string{
		"skillopt/runs/optimizer-train/optimizer-train-001/planner@v2/best_skill.md",
		"skillopt/runs/optimizer-train/optimizer-train-001/planner@v2/base_skill.md",
		"skillopt/runs/optimizer-train/optimizer-train-001/planner@v2/candidate.diff.md",
	} {
		if !skillOptFakeGitHubUpsertedPath(fakeGitHub.upsertedFiles, want) {
			t.Fatalf("candidate review did not publish %s; files=%+v", want, fakeGitHub.upsertedFiles)
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--promote", "planner@v2",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue promote exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_promoted") || !strings.Contains(stdout.String(), "promoted_candidate: planner@v2") {
		t.Fatalf("promote stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--promote", "planner@v2",
		"--start-next",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue promote retry/start-next exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: items_ready") || !strings.Contains(stdout.String(), "promoted_candidate: planner@v2") || !strings.Contains(stdout.String(), "started_iteration: optimizer-train-002") || !strings.Contains(stdout.String(), "base_version: planner@v2") {
		t.Fatalf("promote retry/start-next stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--start-next",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue duplicate start-next exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--start-next requires a promoted candidate, rejected candidate, or no-candidate optimizer result; current phase is items_ready") {
		t.Fatalf("duplicate start-next stderr = %q", stderr.String())
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	current, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if current.VersionID != "planner@v2" {
		t.Fatalf("current template version = %q, want planner@v2", current.VersionID)
	}
	latest, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if latest.ID != "optimizer-train-002" || latest.BaseTemplateVersionID != "planner@v2" || latest.State != skillopt.TrainStateItemsReady || latest.Mode != db.EvalRunModeExplore {
		t.Fatalf("latest iteration = %+v", latest)
	}
	if strings.Contains(latest.MetadataJSON, `"optimizer"`) {
		t.Fatalf("next iteration inherited optimizer metadata: %s", latest.MetadataJSON)
	}
	gate, err := skillOptTrainOptimizerGate(latest, skillOptTrainOptimizerRequest{})
	if err != nil {
		t.Fatalf("skillOptTrainOptimizerGate returned error: %v", err)
	}
	if gate != "mixed" {
		t.Fatalf("next iteration optimizer gate = %q, want mixed", gate)
	}
	items, err := store.ListEvalReviewItems(context.Background(), latest.EvalRunID)
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 1 || items[0].BaselineArtifactID != "" || items[0].CandidateArtifactID != "" {
		t.Fatalf("next iteration items = %+v", items)
	}
	nextRun, err := store.GetEvalRun(context.Background(), latest.EvalRunID)
	if err != nil {
		t.Fatalf("GetEvalRun next returned error: %v", err)
	}
	if nextRun.OptionsCount != 4 {
		t.Fatalf("next run options count = %d, want 4", nextRun.OptionsCount)
	}
	if nextRun.Mode != db.EvalRunModeExplore {
		t.Fatalf("next run mode = %q, want explore", nextRun.Mode)
	}
	if strings.Contains(nextRun.MetadataJSON, `"optimizer"`) {
		t.Fatalf("next run inherited optimizer metadata: %s", nextRun.MetadataJSON)
	}
}

func TestSkillOptTrainContinueSyncsHumanCandidatePromotionAndStartsNext(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with externally promoted candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue candidate review exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") || fakeGitHub.createdIssue.Repo.FullName() != "owner/product" {
		t.Fatalf("candidate review stdout=%s github=%+v", stdout.String(), fakeGitHub.createdIssue)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "candidate", "promote",
		"--home", home,
		"planner@v2",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("candidate promote exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	supersedeStore := openCLIJobStore(t, home)
	newerCandidate, err := supersedeStore.AddPendingAgentTemplateVersion(context.Background(), cliSkillOptTemplate("planner", "Plan with later promoted guidance."))
	if err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion newer candidate returned error: %v", err)
	}
	if _, err := supersedeStore.PromoteAgentTemplateVersion(context.Background(), newerCandidate.ID); err != nil {
		t.Fatalf("PromoteAgentTemplateVersion newer candidate returned error: %v", err)
	}
	superseded, err := supersedeStore.GetAgentTemplateVersionByID(context.Background(), "planner@v2")
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID superseded train candidate returned error: %v", err)
	}
	if superseded.State != "superseded" {
		t.Fatalf("train candidate state after later promotion = %s, want superseded", superseded.State)
	}
	if err := supersedeStore.Close(); err != nil {
		t.Fatalf("Close supersede store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--promote", "planner@v2",
		"--reject", "planner@v2",
		"--reason", "conflicting",
		"--start-next",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue conflicting decision exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "accepts only one of --promote or --reject") {
		t.Fatalf("conflicting decision stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--start-next",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue sync/start-next exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: items_ready") || !strings.Contains(stdout.String(), "promoted_candidate: planner@v2") || !strings.Contains(stdout.String(), "started_iteration: optimizer-train-002") {
		t.Fatalf("sync/start-next stdout = %s", stdout.String())
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	previous, err := store.GetSkillOptTrainIteration(context.Background(), "optimizer-train-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration previous returned error: %v", err)
	}
	if previous.State != skillopt.TrainStateCandidatePromoted {
		t.Fatalf("previous iteration = %+v", previous)
	}
	if !strings.Contains(previous.MetadataJSON, `"source":"gitmoot skillopt train continue synced candidate state"`) {
		t.Fatalf("previous iteration metadata = %s", previous.MetadataJSON)
	}
	latest, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if latest.ID != "optimizer-train-002" || latest.BaseTemplateVersionID != "planner@v2" || latest.State != skillopt.TrainStateItemsReady {
		t.Fatalf("latest iteration = %+v", latest)
	}
}

func TestSkillOptTrainContinueSyncsHumanCandidatePromotionBeforeReviewPublish(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with pre-review human promotion guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_created") {
		t.Fatalf("optimizer stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "candidate", "promote",
		"--home", home,
		"planner@v2",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("candidate promote exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--start-next",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue sync/start-next exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: items_ready") || !strings.Contains(stdout.String(), "promoted_candidate: planner@v2") || !strings.Contains(stdout.String(), "started_iteration: optimizer-train-002") {
		t.Fatalf("sync/start-next stdout = %s", stdout.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("pre-review sync published github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	previous, err := store.GetSkillOptTrainIteration(context.Background(), "optimizer-train-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration previous returned error: %v", err)
	}
	if previous.State != skillopt.TrainStateCandidatePromoted || previous.IssueNumber != 0 || strings.TrimSpace(previous.IssueURL) != "" {
		t.Fatalf("previous iteration = %+v", previous)
	}
	latest, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if latest.ID != "optimizer-train-002" || latest.BaseTemplateVersionID != "planner@v2" || latest.State != skillopt.TrainStateItemsReady {
		t.Fatalf("latest iteration = %+v", latest)
	}
}

func TestSkillOptTrainContinueRequiresReasonForExternalCandidateRejection(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with externally rejected candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "candidate", "reject",
		"--home", home,
		"planner@v2",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("candidate reject exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--start-next",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue external reject without reason exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "train candidate rejection requires --reason") {
		t.Fatalf("external reject without reason stderr = %q", stderr.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("external reject without reason published github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}
	store := openCLIJobStore(t, home)
	previous, err := store.GetSkillOptTrainIteration(context.Background(), "optimizer-train-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration previous returned error: %v", err)
	}
	if previous.State != skillopt.TrainStateCandidateCreated || strings.TrimSpace(previous.DecisionReason) != "" {
		t.Fatalf("previous iteration after failed sync = %+v", previous)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after failed external reject sync returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--reject", "planner@v2",
		"--reason", "too broad",
		"--start-next",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue external reject with reason exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: items_ready") || !strings.Contains(stdout.String(), "rejected_candidate: planner@v2") || !strings.Contains(stdout.String(), "started_iteration: optimizer-train-002") {
		t.Fatalf("external reject with reason stdout = %s", stdout.String())
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	previous, err = store.GetSkillOptTrainIteration(context.Background(), "optimizer-train-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration previous after sync returned error: %v", err)
	}
	if previous.State != skillopt.TrainStateCandidateRejected || previous.DecisionReason != "too broad" {
		t.Fatalf("previous iteration after external reject sync = %+v", previous)
	}
	latest, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if latest.ID != "optimizer-train-002" || latest.BaseTemplateVersionID != baseVersionID || latest.State != skillopt.TrainStateItemsReady {
		t.Fatalf("latest iteration = %+v", latest)
	}
}

func TestSkillOptTrainContinuePublishesCandidateReviewWhenSkillFileUploadFails(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with reviewable candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{upsertFileErr: errors.New("contents write denied")}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue candidate review with file upload failure exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") {
		t.Fatalf("candidate review stdout = %s", stdout.String())
	}
	if !strings.Contains(fakeGitHub.createdIssue.Body, "File publish warning") ||
		!strings.Contains(fakeGitHub.createdIssue.Body, "contents write denied") {
		t.Fatalf("candidate review body missing file publish warning:\n%s", fakeGitHub.createdIssue.Body)
	}
	if len(fakeGitHub.upsertedFiles) != 0 {
		t.Fatalf("file upload failure recorded uploaded files: %+v", fakeGitHub.upsertedFiles)
	}
}

func TestSkillOptTrainCandidateReviewBodyMarksNoOpNotPromotable(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	store := openCLIJobStore(t, home)
	defer store.Close()

	candidate := cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with a changed candidate.")
	version, err := skillopt.ImportCandidatePackageWithOptions(context.Background(), store, candidate, skillopt.CandidateImportOptions{SourcePath: "candidate.json"})
	if err != nil {
		t.Fatalf("ImportCandidatePackageWithOptions returned error: %v", err)
	}
	review, err := store.GetAgentTemplateCandidateReview(context.Background(), version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview returned error: %v", err)
	}
	review.EvalReportJSON = `{"score":0,"hard":0,"soft":0,"gate_status":"blocked","promotable":false,"no_candidate_reason":"best_origin_initial_skill"}`
	review.SummaryMetadataJSON = `{"best_origin":"initial_skill","total_accepts":0,"promotable":false,"no_candidate_reason":"best_origin_initial_skill"}`
	if err := store.UpsertAgentTemplateCandidateReview(context.Background(), review); err != nil {
		t.Fatalf("UpsertAgentTemplateCandidateReview returned error: %v", err)
	}
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	body, err := skillOptTrainCandidateReviewBody(context.Background(), store, session, db.SkillOptTrainIteration{
		ID:                    "optimizer-train-001",
		CandidateVersionID:    version.ID,
		BaseTemplateVersionID: baseVersionID,
	}, home, nil, nil, nil)
	if err != nil {
		t.Fatalf("skillOptTrainCandidateReviewBody returned error: %v", err)
	}
	for _, want := range []string{
		"### Scores And Gate",
		"Test score: `0`",
		"Hard score: `0`",
		"Soft score: `0`",
		"Gate status: `blocked`",
		"No-op status: `blocked: best_origin_initial_skill`",
		"Promotability: `not promotable: best_origin_initial_skill`",
		"### Candidate Sample Preview",
		"Preview: no selected candidate sample artifact was available to publish.",
		"Final eval: `disabled`",
		"Promote: unavailable because best_origin_initial_skill.",
		"Wait: take no action",
		"Keep improving: reject with an actionable reason",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("candidate review body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "--promote "+version.ID) {
		t.Fatalf("candidate review body exposed promote command for no-op metadata:\n%s", body)
	}
	_, err = decideSkillOptTrainCandidate(context.Background(), config.Paths{}, store, session, db.SkillOptTrainIteration{
		ID:                 "optimizer-train-001",
		State:              skillopt.TrainStateCandidateReviewPublished,
		CandidateVersionID: version.ID,
	}, skillOptTrainContinueRequest{PromoteCandidate: version.ID})
	if err == nil || !strings.Contains(err.Error(), "candidate planner@v2 is not promotable: best_origin_initial_skill") {
		t.Fatalf("decideSkillOptTrainCandidate promote error = %v", err)
	}
	blockedVersion, err := store.GetAgentTemplateVersionByID(context.Background(), version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID after blocked promote returned error: %v", err)
	}
	if blockedVersion.State != "pending" {
		t.Fatalf("blocked promote changed version state = %+v", blockedVersion)
	}

	review.EvalReportJSON = `{"score":0,"hard":0,"soft":0,"gate_status":"blocked"}`
	review.SummaryMetadataJSON = `{"best_origin":"initial_skill","total_accepts":0}`
	if err := store.UpsertAgentTemplateCandidateReview(context.Background(), review); err != nil {
		t.Fatalf("UpsertAgentTemplateCandidateReview without reason returned error: %v", err)
	}
	body, err = skillOptTrainCandidateReviewBody(context.Background(), store, session, db.SkillOptTrainIteration{
		ID:                    "optimizer-train-001",
		CandidateVersionID:    version.ID,
		BaseTemplateVersionID: baseVersionID,
	}, home, nil, nil, nil)
	if err != nil {
		t.Fatalf("skillOptTrainCandidateReviewBody without reason returned error: %v", err)
	}
	for _, want := range []string{
		"No-op status: `blocked: best_origin_initial_skill`",
		"Promotability: `not promotable: best_origin_initial_skill`",
		"Promote: unavailable because best_origin_initial_skill.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("candidate review body without reason missing %q:\n%s", want, body)
		}
	}
}

func TestSkillOptTrainCandidateReviewBodyShowsTextSamplePreview(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	paths := config.PathsForHome(home)
	store := openCLIJobStore(t, home)
	defer store.Close()

	candidate := cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with a text reply candidate.")
	version, err := skillopt.ImportCandidatePackageWithOptions(context.Background(), store, candidate, skillopt.CandidateImportOptions{SourcePath: "candidate.json"})
	if err != nil {
		t.Fatalf("ImportCandidatePackageWithOptions returned error: %v", err)
	}
	review, err := store.GetAgentTemplateCandidateReview(context.Background(), version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview returned error: %v", err)
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	content := []byte(`{"reply":"so thats why my limits vanished before lunch","risk":"low"}`)
	blob, err := blobStore.Put(content)
	if err != nil {
		t.Fatalf("Put sample artifact returned error: %v", err)
	}
	sampleID := "optimizer-train/optimizer-train-001/planner@v2/candidate-selection-sample"
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        sampleID,
		Hash:      blob.Hash,
		MediaType: "application/json",
		SizeBytes: blob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact sample returned error: %v", err)
	}
	review.SummaryMetadataJSON = fmt.Sprintf(`{"artifact_ids":[%q]}`, sampleID)
	if err := store.UpsertAgentTemplateCandidateReview(context.Background(), review); err != nil {
		t.Fatalf("UpsertAgentTemplateCandidateReview returned error: %v", err)
	}
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration := db.SkillOptTrainIteration{
		ID:                    "optimizer-train-001",
		CandidateVersionID:    version.ID,
		BaseTemplateVersionID: baseVersionID,
	}
	optionalPolicy, err := skillopt.BuildTrainPreviewPolicy("owner/product", "owner/previews", skillopt.TrainPreviewModeOptional, skillopt.TrainPreviewRendererVueVite, skillopt.TrainPreviewPublisherGitHubPages, "")
	if err != nil {
		t.Fatalf("BuildTrainPreviewPolicy optional returned error: %v", err)
	}
	optionalSession := session
	optionalSession.MetadataJSON = skillOptTrainStartMetadata("Train planner outputs from human feedback.", db.EvalRunModeValidate, db.ExplorationLevelLow, 2, "hard_then_soft", nil, nil, optionalPolicy, skillOptTrainStartConfigDefaults{}, nil)
	for _, tt := range []struct {
		name    string
		session db.SkillOptTrainSession
	}{
		{name: "no preview policy", session: session},
		{name: "optional vue preview policy", session: optionalSession},
	} {
		t.Run(tt.name, func(t *testing.T) {
			previews := publishSkillOptTrainCandidateSamplePreviews(context.Background(), paths, store, tt.session, iteration)
			body, err := skillOptTrainCandidateReviewBody(context.Background(), store, tt.session, iteration, home, nil, previews, nil)
			if err != nil {
				t.Fatalf("skillOptTrainCandidateReviewBody returned error: %v", err)
			}
			for _, want := range []string{
				"### Candidate Sample Preview",
				"| Sample | Preview | Artifact | Renderer | Status |",
				"| Selection sample | `so thats why my limits vanished before lunch` | `optimizer-train/optimizer-train-001/planner@v2/candidate-selection-sample` | `text` | - |",
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("candidate review body missing %q:\n%s", want, body)
				}
			}
			for _, unwanted := range []string{
				"Preview: no selected candidate sample artifact was available to publish.",
				"candidate sample preview publishing is not configured",
				`"risk"`,
				"```text",
			} {
				if strings.Contains(body, unwanted) {
					t.Fatalf("candidate review body contained %q:\n%s", unwanted, body)
				}
			}
		})
	}
	record, err := store.GetEvalArtifact(context.Background(), sampleID)
	if err != nil {
		t.Fatalf("GetEvalArtifact sample returned error: %v", err)
	}
	record.Driver = skillopt.TrainPreviewRendererVueVite
	if err := store.UpsertEvalArtifact(context.Background(), record); err != nil {
		t.Fatalf("UpsertEvalArtifact malformed preview driver returned error: %v", err)
	}
	previews := publishSkillOptTrainCandidateSamplePreviews(context.Background(), paths, store, optionalSession, iteration)
	if len(previews) != 1 || previews[0].Error == "" {
		t.Fatalf("optional malformed preview result = %+v, want bundle validation error", previews)
	}
	if strings.TrimSpace(previews[0].Content) != "" {
		t.Fatalf("optional malformed preview unexpectedly used inline content: %+v", previews[0])
	}
}

func TestSkillOptTrainCandidateReviewRequiredPreviewKeepsBundleFailure(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	paths := config.PathsForHome(home)
	store := openCLIJobStore(t, home)
	defer store.Close()

	candidate := cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with a text reply candidate.")
	version, err := skillopt.ImportCandidatePackageWithOptions(context.Background(), store, candidate, skillopt.CandidateImportOptions{SourcePath: "candidate.json"})
	if err != nil {
		t.Fatalf("ImportCandidatePackageWithOptions returned error: %v", err)
	}
	review, err := store.GetAgentTemplateCandidateReview(context.Background(), version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview returned error: %v", err)
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	content := []byte(`{"reply":"so thats why my limits vanished before lunch"}`)
	blob, err := blobStore.Put(content)
	if err != nil {
		t.Fatalf("Put sample artifact returned error: %v", err)
	}
	sampleID := "optimizer-train/optimizer-train-001/planner@v2/candidate-selection-sample"
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        sampleID,
		Hash:      blob.Hash,
		MediaType: "application/json",
		SizeBytes: blob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact sample returned error: %v", err)
	}
	review.SummaryMetadataJSON = fmt.Sprintf(`{"artifact_ids":[%q]}`, sampleID)
	if err := store.UpsertAgentTemplateCandidateReview(context.Background(), review); err != nil {
		t.Fatalf("UpsertAgentTemplateCandidateReview returned error: %v", err)
	}
	requiredPolicy, err := skillopt.BuildTrainPreviewPolicy("owner/product", "owner/previews", skillopt.TrainPreviewModeRequired, skillopt.TrainPreviewRendererVueVite, skillopt.TrainPreviewPublisherGitHubPages, "")
	if err != nil {
		t.Fatalf("BuildTrainPreviewPolicy required returned error: %v", err)
	}
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	session.MetadataJSON = skillOptTrainStartMetadata("Train planner outputs from human feedback.", db.EvalRunModeValidate, db.ExplorationLevelLow, 2, "hard_then_soft", nil, nil, requiredPolicy, skillOptTrainStartConfigDefaults{}, nil)
	previews := publishSkillOptTrainCandidateSamplePreviews(context.Background(), paths, store, session, db.SkillOptTrainIteration{
		ID:                    "optimizer-train-001",
		CandidateVersionID:    version.ID,
		BaseTemplateVersionID: baseVersionID,
	})
	if len(previews) != 1 || previews[0].Error == "" {
		t.Fatalf("required preview result = %+v, want bundle validation error", previews)
	}
	if !strings.Contains(previews[0].Error, "preview bundle") {
		t.Fatalf("required preview error = %q", previews[0].Error)
	}
	if strings.TrimSpace(previews[0].Content) != "" {
		t.Fatalf("required preview unexpectedly used inline content: %+v", previews[0])
	}
}

func TestSkillOptTrainContinuePublishesCandidateReviewAndRejects(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with rejectable candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{
		host:               "https://github.example.com",
		commentURLOverride: "https://github.example.com/api/v3/repos/owner/review/issues/comments/1",
	}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	legacyStore := openCLIJobStore(t, home)
	iteration, err := legacyStore.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after optimizer returned error: %v", err)
	}
	iteration.IssueNumber = 67
	iteration.IssueRepo = "owner/review"
	if err := legacyStore.UpsertSkillOptTrainIteration(context.Background(), iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration legacy review issue returned error: %v", err)
	}
	if err := legacyStore.Close(); err != nil {
		t.Fatalf("Close after legacy review issue update returned error: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue candidate review exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(fakeGitHub.postedComments) != 1 || fakeGitHub.postedComments[0].Repo.FullName() != "owner/review" || fakeGitHub.postedComments[0].IssueNumber != 67 {
		t.Fatalf("candidate review comments = %+v", fakeGitHub.postedComments)
	}
	commentBody := fakeGitHub.postedComments[0].Body
	for _, want := range []string{
		"## SkillOpt Candidate Review",
		"### Artifacts",
		"`candidate-diff`",
		"Eval report: stored with the pending candidate review record.",
	} {
		if !strings.Contains(commentBody, want) {
			t.Fatalf("candidate review comment missing %q:\n%s", want, commentBody)
		}
	}
	for _, unwanted := range []string{
		"### Eval Report\n```json",
		"### Candidate Template Diff\n```diff",
	} {
		if strings.Contains(commentBody, unwanted) {
			t.Fatalf("candidate review comment contains %q:\n%s", unwanted, commentBody)
		}
	}
	commentStore := openCLIJobStore(t, home)
	publishedIteration, err := commentStore.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after existing issue publish returned error: %v", err)
	}
	if publishedIteration.IssueURL != "https://github.example.com/owner/review/issues/67" {
		t.Fatalf("published issue url = %q", publishedIteration.IssueURL)
	}
	if err := commentStore.Close(); err != nil {
		t.Fatalf("Close after existing issue publish returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--reject", "planner@v2",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue reject without reason exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires --reason") {
		t.Fatalf("reject without reason stderr = %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
		"--reject", "planner@v2",
		"--reason", "too broad",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue reject exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_rejected") || !strings.Contains(stdout.String(), "rejected_candidate: planner@v2") {
		t.Fatalf("reject stdout = %s", stdout.String())
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	current, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if current.VersionID != baseVersionID {
		t.Fatalf("current template version = %q, want %q", current.VersionID, baseVersionID)
	}
	finalIteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if finalIteration.State != skillopt.TrainStateCandidateRejected || finalIteration.DecisionReason != "too broad" {
		t.Fatalf("iteration after reject = %+v", finalIteration)
	}
}

func TestSkillOptTrainContinuePublishesCandidateReviewToExistingPullRequest(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with PR review guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{
		host:               "https://github.example.com",
		commentKinds:       map[int64]string{77: "pull"},
		commentURLOverride: "https://github.example.com/api/v3/repos/owner/review/issues/comments/1",
	}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	store := openCLIJobStore(t, home)
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after optimizer returned error: %v", err)
	}
	iteration.PullRequestNumber = 77
	iteration.PullRequestRepo = "owner/review"
	if err := store.UpsertSkillOptTrainIteration(context.Background(), iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration PR review returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after PR review update returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue candidate PR review exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(fakeGitHub.postedComments) != 1 || fakeGitHub.postedComments[0].Repo.FullName() != "owner/review" || fakeGitHub.postedComments[0].IssueNumber != 77 {
		t.Fatalf("candidate PR review comments = %+v", fakeGitHub.postedComments)
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" {
		t.Fatalf("candidate PR review created issue unexpectedly: %+v", fakeGitHub.createdIssue)
	}
	store = openCLIJobStore(t, home)
	iteration, err = store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after PR publish returned error: %v", err)
	}
	if iteration.PullRequestURL != "https://github.example.com/owner/review/pull/77" {
		t.Fatalf("published pull request url = %q", iteration.PullRequestURL)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after PR publish check returned error: %v", err)
	}
}

func TestSkillOptTrainContinueDoesNotRepostCandidateReviewWhilePublishing(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with publishing recovery guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	marker := map[string]any{
		"status":            "publishing",
		"candidate_version": iteration.CandidateVersionID,
		"issue_repo":        "owner/product",
		"issue_title":       "SkillOpt candidate review: optimizer-train",
		"started_at":        time.Now().UTC().Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", marker)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", marker)
	if err := store.UpsertSkillOptTrainSession(context.Background(), session); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession marker returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainIteration(context.Background(), iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainIteration marker returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close marker store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue publishing marker exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "marked publishing") || !strings.Contains(stderr.String(), "SkillOpt candidate review: optimizer-train") {
		t.Fatalf("publishing marker stderr = %q", stderr.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("publishing marker reposted github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}

	store = openCLIJobStore(t, home)
	session, err = store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession before recovery returned error: %v", err)
	}
	iteration, err = store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration before recovery returned error: %v", err)
	}
	marker["status"] = "published_external"
	marker["issue_number"] = int64(8)
	marker["issue_url"] = "https://github.com/owner/product/issues/8"
	marker["review_url"] = "https://github.com/owner/product/issues/8"
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", marker)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", marker)
	if err := store.UpsertSkillOptTrainSessionAndIteration(context.Background(), session, iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainSessionAndIteration recovery marker returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close recovery marker store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue recovery marker exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") || !strings.Contains(stdout.String(), "candidate_review: https://github.com/owner/product/issues/8") {
		t.Fatalf("recovery marker stdout = %s", stdout.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("recovery marker reposted github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}
}

func TestSkillOptTrainContinueRetriesStaleCandidateReviewPublishingMarker(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with stale publishing recovery guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	marker := map[string]any{
		"status":            "publishing",
		"candidate_version": iteration.CandidateVersionID,
		"issue_repo":        "owner/product",
		"issue_title":       "SkillOpt candidate review: optimizer-train",
		"started_at":        time.Now().UTC().Add(-skillOptTrainCandidateReviewLockTTL - time.Minute).Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", marker)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", marker)
	if err := store.UpsertSkillOptTrainSessionAndIteration(context.Background(), session, iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainSessionAndIteration stale marker returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close stale marker store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue stale publishing marker exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") || !strings.Contains(stdout.String(), "candidate_review: https://github.com/owner/product/issues/8") {
		t.Fatalf("stale publishing marker stdout = %s", stdout.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "owner/product" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("stale publishing marker github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}
}

func TestSkillOptTrainContinueRetriesAfterCandidateReviewMarkerWriteFailure(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with marker failure recovery guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	_, err = publishSkillOptTrainCandidateReview(context.Background(), config.Paths{}, store, session, iteration, home)
	if err == nil || !strings.Contains(err.Error(), "write candidate review pre-publish recovery marker") {
		t.Fatalf("publishSkillOptTrainCandidateReview marker write failure err = %v", err)
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("marker write failure posted github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}
	iteration, err = store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration after marker failure returned error: %v", err)
	}
	if strings.Contains(iteration.MetadataJSON, `"status":"publishing"`) {
		t.Fatalf("marker write failure recorded publishing metadata: %+v", iteration)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close marker failure store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue retry after marker failure exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") || fakeGitHub.createdIssue.Repo.FullName() == "" {
		t.Fatalf("retry after marker failure stdout=%s github=%+v", stdout.String(), fakeGitHub.createdIssue)
	}
}

func TestSkillOptTrainContinueRetriesInterruptedBeforeCandidateReviewExternalPost(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with interrupted pre-external recovery guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	marker := map[string]any{
		"status":            "publishing",
		"candidate_version": iteration.CandidateVersionID,
		"issue_repo":        "owner/product",
		"issue_title":       "SkillOpt candidate review: optimizer-train",
		"started_at":        time.Now().UTC().Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", marker)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", marker)
	if err := writeSkillOptCandidateReviewRecovery(config.PathsForHome(home), session, iteration, marker); err != nil {
		t.Fatalf("writeSkillOptCandidateReviewRecovery pre-external marker returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainSessionAndIteration(context.Background(), session, iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainSessionAndIteration pre-external marker returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close pre-external marker store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue pre-external retry exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_review_published") || fakeGitHub.createdIssue.Repo.FullName() == "" {
		t.Fatalf("pre-external retry stdout=%s github=%+v", stdout.String(), fakeGitHub.createdIssue)
	}
}

func TestSkillOptTrainContinueBlocksInterruptedCandidateReviewExternalPost(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with interrupted external post recovery guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	stalePublishing := map[string]any{
		"status":            "publishing",
		"candidate_version": iteration.CandidateVersionID,
		"issue_repo":        "owner/product",
		"issue_title":       "SkillOpt candidate review: optimizer-train",
		"started_at":        time.Now().UTC().Add(-skillOptTrainCandidateReviewLockTTL - time.Minute).Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", stalePublishing)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", stalePublishing)
	if err := store.UpsertSkillOptTrainSessionAndIteration(context.Background(), session, iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainSessionAndIteration stale marker returned error: %v", err)
	}
	posting := map[string]any{
		"status":                   "posting_external",
		"candidate_version":        iteration.CandidateVersionID,
		"issue_repo":               "owner/product",
		"issue_title":              "SkillOpt candidate review: optimizer-train",
		"external_post_started_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
	}
	if err := writeSkillOptCandidateReviewRecovery(config.PathsForHome(home), session, iteration, posting); err != nil {
		t.Fatalf("writeSkillOptCandidateReviewRecovery posting marker returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close interrupted marker store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue interrupted external post exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "external post started") || !strings.Contains(stderr.String(), "SkillOpt candidate review: optimizer-train") {
		t.Fatalf("interrupted external post stderr = %q", stderr.String())
	}
	if fakeGitHub.createdIssue.Repo.FullName() != "" || len(fakeGitHub.postedComments) != 0 {
		t.Fatalf("interrupted external post reposted github state: issue=%+v comments=%+v", fakeGitHub.createdIssue, fakeGitHub.postedComments)
	}
}

func TestSkillOptTrainContinueRecoversCandidateReviewFromSidecar(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with sidecar recovery guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	session, err := store.GetSkillOptTrainSession(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession returned error: %v", err)
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	publishing := map[string]any{
		"status":            "publishing",
		"candidate_version": iteration.CandidateVersionID,
		"issue_repo":        "owner/product",
		"issue_title":       "SkillOpt candidate review: optimizer-train",
		"started_at":        time.Now().UTC().Format(time.RFC3339Nano),
	}
	session.MetadataJSON = mergeSkillOptTrainMetadata(session.MetadataJSON, "candidate_review", publishing)
	iteration.MetadataJSON = mergeSkillOptTrainMetadata(iteration.MetadataJSON, "candidate_review", publishing)
	if err := store.UpsertSkillOptTrainSessionAndIteration(context.Background(), session, iteration); err != nil {
		t.Fatalf("UpsertSkillOptTrainSessionAndIteration publishing marker returned error: %v", err)
	}
	sidecar := map[string]any{
		"status":            "published_external",
		"candidate_version": iteration.CandidateVersionID,
		"issue_repo":        "owner/product",
		"issue_number":      int64(8),
		"issue_url":         "https://github.com/owner/product/issues/8",
		"review_url":        "https://github.com/owner/product/issues/8",
	}
	paths := config.PathsForHome(home)
	if err := writeSkillOptCandidateReviewRecovery(paths, session, iteration, sidecar); err != nil {
		t.Fatalf("writeSkillOptCandidateReviewRecovery returned error: %v", err)
	}
	sidecarPath := skillOptCandidateReviewRecoveryPath(paths, session, iteration)
	if err := store.Close(); err != nil {
		t.Fatalf("Close sidecar marker store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--promote", "planner@v2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue sidecar recovery exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "current_phase: candidate_promoted") || !strings.Contains(stdout.String(), "promoted_candidate: planner@v2") {
		t.Fatalf("sidecar recovery stdout = %s", stdout.String())
	}
	// The recovery must not re-create the issue, but the promote now posts the
	// decision to the recovered issue (#8) so it reflects the choice.
	if fakeGitHub.createdIssue.Repo.FullName() != "" {
		t.Fatalf("sidecar recovery must not re-create the issue: %+v", fakeGitHub.createdIssue)
	}
	if len(fakeGitHub.postedComments) != 1 {
		t.Fatalf("expected one decision comment on the recovered issue, got %+v", fakeGitHub.postedComments)
	}
	if c := fakeGitHub.postedComments[0]; c.IssueNumber != 8 || !strings.Contains(c.Body, skillOptTrainDecisionMarker) || !strings.Contains(c.Body, "Promoted") {
		t.Fatalf("decision comment = %+v", fakeGitHub.postedComments[0])
	}
	if _, err := os.Stat(sidecarPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sidecar still exists err=%v", err)
	}
}

func TestSkillOptCandidateReviewRecoveryNameAvoidsSanitizationCollisions(t *testing.T) {
	first := skillOptCandidateReviewRecoveryName("feature/foo", "iter")
	second := skillOptCandidateReviewRecoveryName("feature?foo", "iter")
	if first == "" || second == "" {
		t.Fatalf("recovery names are empty: first=%q second=%q", first, second)
	}
	if first == second {
		t.Fatalf("recovery names collided: %q", first)
	}
}

func TestSkillOptTrainContinueStartNextRejectsEvalRunCollision(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with collision guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	fakeGitHub := &skillOptFakeGitHub{}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fakeGitHub }
	defer func() {
		newSkillOptGitHubClient = oldClient
	}()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue optimizer exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue candidate review exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--promote", "planner@v2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("train continue promote exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:         "optimizer-train-review-002",
		TemplateID: "planner",
		TargetRepo: "owner/product",
		State:      "review",
	}); err != nil {
		t.Fatalf("UpsertEvalRun collision returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close collision store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--start-next"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue start-next collision exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "eval run optimizer-train-review-002 already exists") {
		t.Fatalf("start-next collision stderr = %q", stderr.String())
	}
}
