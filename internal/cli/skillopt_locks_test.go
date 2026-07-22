package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

func TestSkillOptTrainContinueRefusesConcurrentGeneration(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/workspace.git")
	t.Chdir(repoDir)
	itemsPath := writeSkillOptTrainItemsFile(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after template seed returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"skillopt", "train", "start",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/product",
		"--session", "landing-train",
		"--workspace-repo", "owner/workspace",
		"--request", "Train landing page plans.",
		"--task-kind", "design",
		"--mode", "explore",
		"--options", "2",
		"--items-file", itemsPath,
		"--yes",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("train start exit code = %d, stderr=%s", code, stderr.String())
	}

	store = openCLIJobStore(t, home)
	acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: skillOptTrainGenerationLockKey("landing-train", "landing-train-001"),
		OwnerJobID:  "test-concurrent-continue",
		OwnerToken:  "test-token",
		ExpiresAt:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("AcquireResourceLock returned error: %v", err)
	}
	if !acquired {
		t.Fatal("AcquireResourceLock did not acquire generation lock")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close before continue returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "landing-train", "--generator-type", "skillopt-generator"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "skillopt train generation is already running") {
		t.Fatalf("train continue stderr = %q", stderr.String())
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateItemsReady {
		t.Fatalf("iteration state = %s, want %s", iteration.State, skillopt.TrainStateItemsReady)
	}
	if strings.Contains(iteration.MetadataJSON, `"status":"failed"`) {
		t.Fatalf("busy generation lock recorded failed metadata: %s", iteration.MetadataJSON)
	}
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("busy generation lock dispatched jobs: %+v", jobs)
	}
}

func TestSkillOptTrainGenerationLockTTLScalesWithWorkload(t *testing.T) {
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
	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "type", "set", "skillopt-generator",
		"--home", home,
		"--runtime", "codex",
		"--role", "generator",
		"--max-background", "2",
		"--job-timeout", "45m",
		"--capability", "ask",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent type set exit code = %d, stderr=%s", code, stderr.String())
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:               "landing-train-review-001",
		Mode:             db.EvalRunModeExplore,
		ExplorationLevel: db.ExplorationLevelHigh,
		OptionsCount:     4,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	for index := 0; index < 7; index++ {
		itemID := fmt.Sprintf("item-%03d", index+1)
		if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
			RunID:  "landing-train-review-001",
			ItemID: itemID,
			Title:  itemID,
		}); err != nil {
			t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
		}
	}
	ttl, err := estimateSkillOptTrainGenerationLockTTL(context.Background(), store, skillOptTrainContinueRequest{Home: home, GeneratorType: "skillopt-generator"}, db.SkillOptTrainIteration{
		EvalRunID: "landing-train-review-001",
	})
	if err != nil {
		t.Fatalf("estimateSkillOptTrainGenerationLockTTL returned error: %v", err)
	}
	want := 16*45*time.Minute + skillOptTrainGenerationLockBuffer
	if ttl != want {
		t.Fatalf("ttl = %s, want %s", ttl, want)
	}
	if ttl <= skillOptTrainGenerationLockTTL {
		t.Fatalf("ttl = %s, want greater than fixed minimum %s", ttl, skillOptTrainGenerationLockTTL)
	}
	previewPolicy, err := skillopt.BuildTrainPreviewPolicy("owner/product", "owner/previews", skillopt.TrainPreviewModeRequired, skillopt.TrainPreviewRendererVueVite, skillopt.TrainPreviewPublisherGitHubPages, "")
	if err != nil {
		t.Fatalf("BuildTrainPreviewPolicy returned error: %v", err)
	}
	if err := store.UpsertSkillOptTrainSession(context.Background(), db.SkillOptTrainSession{
		ID:           "preview-train",
		TemplateID:   "planner",
		TargetRepo:   "owner/product",
		PreviewRepo:  "owner/previews",
		TaskKind:     "design",
		State:        skillopt.TrainStateItemsReady,
		MetadataJSON: skillOptTrainStartMetadata("Train landing page previews.", db.EvalRunModeExplore, db.ExplorationLevelHigh, 4, "soft", nil, nil, previewPolicy, skillOptTrainStartConfigDefaults{}, nil),
	}); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession returned error: %v", err)
	}
	previewTTL, err := estimateSkillOptTrainGenerationLockTTL(context.Background(), store, skillOptTrainContinueRequest{Home: home, GeneratorType: "skillopt-generator"}, db.SkillOptTrainIteration{
		SessionID: "preview-train",
		EvalRunID: "landing-train-review-001",
	})
	if err != nil {
		t.Fatalf("estimateSkillOptTrainGenerationLockTTL preview returned error: %v", err)
	}
	previewWant := 32*45*time.Minute + skillOptTrainGenerationLockBuffer
	if previewTTL != previewWant {
		t.Fatalf("preview ttl = %s, want %s", previewTTL, previewWant)
	}
}

func TestSkillOptTrainContinueStartNextRejectsBusyLock(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with start-next lock guidance."),
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
	now := time.Now().UTC()
	acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: skillOptTrainStartNextLockKey("optimizer-train"),
		OwnerJobID:  "test-start-next",
		OwnerToken:  "token",
		ExpiresAt:   now.Add(skillOptTrainStartNextLockTTL).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		t.Fatalf("AcquireResourceLock returned error: %v", err)
	}
	if !acquired {
		t.Fatal("AcquireResourceLock did not acquire start-next lock")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close lock store returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--start-next"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue start-next busy exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "next iteration is already starting") {
		t.Fatalf("start-next busy stderr = %q", stderr.String())
	}
}

func TestSkillOptTrainRecoverRefusesActiveOptimizerLock(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	outRoot := filepath.Join(t.TempDir(), "optimizer")
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate:          cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with locked recovery guidance."),
		failAfterCandidate: true,
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "continue", "--home", home, "--session", "optimizer-train", "--out-root", outRoot}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue recoverable failure exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	release, _, err := acquireSkillOptTrainOptimizerLock(context.Background(), store, "optimizer-train", "optimizer-train-001", time.Hour, skillOptTrainOptimizerRequest{OutRoot: outRoot})
	if err != nil {
		t.Fatalf("acquire optimizer lock returned error: %v", err)
	}
	defer store.Close()
	defer func() {
		_ = release(context.Background())
	}()

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "optimizer-train"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train recover locked optimizer exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "recovery_state: optimizer_active") ||
		!strings.Contains(stderr.String(), "skillopt train optimizer is already running") {
		t.Fatalf("train recover locked stdout=%s stderr=%s", stdout.String(), stderr.String())
	}

	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "optimizer-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateTrainingPackageCreated || iteration.CandidateVersionID != "" {
		t.Fatalf("iteration after locked recovery = %+v", iteration)
	}
	if _, err := store.GetAgentTemplateCandidateReview(context.Background(), "planner@v2"); err == nil {
		t.Fatalf("locked recovery unexpectedly imported candidate review")
	}
}

func TestSkillOptTrainContinueRefusesConcurrentOptimizer(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with lock-safe candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()
	store := openCLIJobStore(t, home)
	release, _, err := acquireSkillOptTrainOptimizerLock(context.Background(), store, "optimizer-train", "optimizer-train-001", time.Hour, skillOptTrainOptimizerRequest{Backend: "codex"})
	if err != nil {
		t.Fatalf("acquire optimizer lock returned error: %v", err)
	}
	lock, err := store.GetResourceLock(context.Background(), skillOptTrainOptimizerLockKey("optimizer-train", "optimizer-train-001"))
	if err != nil {
		t.Fatalf("GetResourceLock optimizer returned error: %v", err)
	}
	if lock.ResourceKey != "skillopt-train:optimizer-train:optimizer-train-001" ||
		lock.OwnerPID <= 0 ||
		strings.TrimSpace(lock.OwnerHostname) == "" ||
		strings.TrimSpace(lock.CommandHash) == "" {
		t.Fatalf("optimizer lock metadata = %+v", lock)
	}
	acquiredAt, ok := parseSkillOptStatusTime(lock.AcquiredAt)
	if !ok {
		t.Fatalf("optimizer lock acquired_at = %q, want parseable time", lock.AcquiredAt)
	}
	expiresAt, ok := parseSkillOptStatusTime(lock.ExpiresAt)
	if !ok {
		t.Fatalf("optimizer lock expires_at = %q, want parseable time", lock.ExpiresAt)
	}
	if lease := expiresAt.Sub(acquiredAt); lease <= 0 || lease > skillOptTrainOptimizerHeartbeatLeaseTTL+time.Second {
		t.Fatalf("optimizer lock lease = %s, want short heartbeat lease around %s", lease, skillOptTrainOptimizerHeartbeatLeaseTTL)
	}
	legacyLock, err := store.GetResourceLock(context.Background(), skillOptTrainLegacyOptimizerLockKey("optimizer-train", "optimizer-train-001"))
	if err != nil {
		t.Fatalf("GetResourceLock legacy optimizer returned error: %v", err)
	}
	if legacyLock.OwnerJobID != lock.OwnerJobID ||
		legacyLock.OwnerPID != lock.OwnerPID ||
		legacyLock.OwnerHostname != lock.OwnerHostname ||
		legacyLock.CommandHash != lock.CommandHash {
		t.Fatalf("legacy optimizer lock metadata = %+v, want owner metadata matching %+v", legacyLock, lock)
	}
	defer store.Close()
	defer func() {
		_ = release(context.Background())
	}()

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue locked optimizer exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "skillopt train optimizer is already running") {
		t.Fatalf("locked optimizer stderr = %q", stderr.String())
	}
	for _, want := range []string{
		"skillopt-train:optimizer-train:optimizer-train-001",
		"active owner=",
		"pid=",
		"heartbeat=",
		"hash=",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("locked optimizer stderr missing %q:\n%s", want, stderr.String())
		}
	}
	if len(runner.calls) != 0 {
		t.Fatalf("optimizer ran while lock was held: %+v", runner.calls)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status with optimizer lock exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"status_phase: optimizer_running",
		"active_lock: optimizer skillopt-train:optimizer-train:optimizer-train-001 status=active",
		"active_lock: optimizer_legacy skillopt-train-optimizer:optimizer-train:optimizer-train-001 status=active",
		"owner=local-skillopt-train-optimizer-optimizer-train",
		"pid=",
		"heartbeat=",
		"expires=",
		"elapsed=",
		"hash=",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train status active lock stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSkillOptTrainContinueReportsStaleOptimizerLock(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with stale-lock-safe candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	store := openCLIJobStore(t, home)
	now := time.Now().UTC()
	acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey:   skillOptTrainOptimizerLockKey("optimizer-train", "optimizer-train-001"),
		OwnerJobID:    "stale-optimizer",
		OwnerToken:    "stale-token",
		OwnerPID:      0,
		OwnerHostname: "stale-host",
		CommandHash:   "stale-hash",
		ExpiresAt:     now.Add(-time.Minute).Format(time.RFC3339Nano),
	}, now.Add(-2*time.Minute))
	if err != nil {
		t.Fatalf("AcquireResourceLock stale returned error: %v", err)
	}
	if !acquired {
		t.Fatal("AcquireResourceLock did not create stale optimizer lock")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close stale lock store returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status stale lock exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status_phase: blocked_stale_lock") ||
		!strings.Contains(stdout.String(), "active_lock: optimizer skillopt-train:optimizer-train:optimizer-train-001 status=stale") {
		t.Fatalf("train status stale lock stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status stale lock json exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var statusJSON skillOptTrainStatusSnapshot
	if err := json.Unmarshal(stdout.Bytes(), &statusJSON); err != nil {
		t.Fatalf("train status stale lock json did not decode: %v\n%s", err, stdout.String())
	}
	if statusJSON.StatusPhase != "blocked_stale_lock" || statusJSON.Verbose != nil {
		t.Fatalf("train status stale lock json = %+v", statusJSON)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--watch", "--verbose", "--poll", "1ms"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status watch stale lock exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "active_lock: optimizer skillopt-train:optimizer-train:optimizer-train-001 status=stale") ||
		!strings.Contains(stdout.String(), "status_phase: blocked_stale_lock") ||
		!strings.Contains(stdout.String(), "watch_state: waiting") {
		t.Fatalf("train status watch stale lock stdout = %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train continue stale lock recovery exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"optimizer_lock: recovered_stale",
		"current_phase: candidate_created",
		"imported_candidate: planner@v2",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("train continue stale lock recovery stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if len(runner.calls) != 1 {
		t.Fatalf("optimizer calls after stale lock recovery = %+v, want one", runner.calls)
	}
}

func TestSkillOptTrainContinueRefusesLegacyOptimizerLock(t *testing.T) {
	home, baseVersionID := seedSkillOptTrainFeedbackSynced(t)
	runner := &skillOptTrainFakeOptimizerRunner{
		candidate: cliSkillOptCandidatePackage(t, "planner", baseVersionID, "Plan with legacy-lock-safe candidate guidance."),
	}
	previousRunner := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = runner
	defer func() {
		skillOptTrainOptimizerRunner = previousRunner
	}()

	store := openCLIJobStore(t, home)
	now := time.Now().UTC()
	acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: skillOptTrainLegacyOptimizerLockKey("optimizer-train", "optimizer-train-001"),
		OwnerJobID:  "legacy-optimizer",
		OwnerToken:  "legacy-token",
		ExpiresAt:   now.Add(time.Minute).Format(time.RFC3339Nano),
	}, now)
	if err != nil {
		t.Fatalf("AcquireResourceLock legacy returned error: %v", err)
	}
	if !acquired {
		t.Fatal("AcquireResourceLock did not create legacy optimizer lock")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close legacy lock store returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "train", "continue",
		"--home", home,
		"--session", "optimizer-train",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("train continue legacy lock exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "skillopt-train-optimizer:optimizer-train:optimizer-train-001") {
		t.Fatalf("legacy optimizer stderr = %q", stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("optimizer ran while legacy lock was held: %+v", runner.calls)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "train", "status", "--home", home, "--session", "optimizer-train", "--verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("train status legacy lock exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "active_lock: optimizer_legacy skillopt-train-optimizer:optimizer-train:optimizer-train-001 status=active") {
		t.Fatalf("train status legacy lock stdout = %s", stdout.String())
	}
}
