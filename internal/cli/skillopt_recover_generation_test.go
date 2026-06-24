package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// seedSkillOptTrainItemsReady starts a 2-item, 2-option train session that lands
// at items_ready, then persists generated options for persistItems items (in the
// order returned by ListEvalReviewItems). It returns the home dir and the run id.
// With persistItems == 2 the run is fully recoverable; with 1 it is partial.
func seedSkillOptTrainItemsReady(t *testing.T, persistItems int) (string, string) {
	t.Helper()
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

	runID := "landing-train-review-001"
	store = openCLIJobStore(t, home)
	defer store.Close()
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	items, err := store.ListEvalReviewItems(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	writes := make([]db.EvalReviewGenerationWrite, 0, persistItems)
	for i, item := range items {
		if i >= persistItems {
			break
		}
		var artifacts []db.EvalArtifact
		var options []db.EvalReviewOption
		for _, label := range []string{"a", "b"} {
			artifactRecord, err := prepareReviewItemContentArtifact(blobStore, runID, item.ItemID, "option-"+label, []byte("existing option "+label), "text/markdown", "text")
			if err != nil {
				t.Fatalf("prepareReviewItemContentArtifact returned error: %v", err)
			}
			artifacts = append(artifacts, artifactRecord)
			options = append(options, db.EvalReviewOption{
				RunID:      runID,
				ItemID:     item.ItemID,
				Label:      label,
				ArtifactID: artifactRecord.ID,
				Role:       "option",
			})
		}
		writes = append(writes, db.EvalReviewGenerationWrite{
			ItemID:    item.ItemID,
			Artifacts: artifacts,
			Options:   options,
		})
	}
	if len(writes) > 0 {
		if err := store.ReplaceGeneratedEvalReviewArtifacts(context.Background(), runID, writes); err != nil {
			t.Fatalf("ReplaceGeneratedEvalReviewArtifacts returned error: %v", err)
		}
	}
	return home, runID
}

// seedStrandedGenerationLock writes a generation lock owned by a dead PID, on the
// given host, expiring at expiresAt — emulating a crashed train continue whose
// deferred release never ran.
func seedStrandedGenerationLock(t *testing.T, home string, ownerPID int64, ownerHost string, expiresAt time.Time) {
	t.Helper()
	store := openCLIJobStore(t, home)
	defer store.Close()
	acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey:   skillOptTrainGenerationLockKey("landing-train", "landing-train-001"),
		OwnerJobID:    "local-skillopt-train-generation-landing-train-deadbeef",
		OwnerToken:    "stranded-token",
		OwnerPID:      ownerPID,
		OwnerHostname: ownerHost,
		ExpiresAt:     expiresAt.Format(time.RFC3339Nano),
	}, time.Now().UTC())
	if err != nil || !acquired {
		t.Fatalf("AcquireResourceLock stranded returned acquired=%v err=%v", acquired, err)
	}
}

// deadPID returns a PID that is (almost certainly) not a running process.
func deadPID(t *testing.T) int64 {
	t.Helper()
	// A very high PID that no process should occupy in the test environment.
	return 2147480000
}

func thisHostname(t *testing.T) string {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname returned error: %v", err)
	}
	return host
}

func TestSkillOptTrainRecoverGenerationReclaimsDeadSameHostLock(t *testing.T) {
	home, _ := seedSkillOptTrainItemsReady(t, 2)
	// Stale lock: dead owner on this host, lease still in the future.
	seedStrandedGenerationLock(t, home, deadPID(t), thisHostname(t), time.Now().UTC().Add(time.Hour))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("recover --generation exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"mode: generation",
		"lock_reclaimed: true",
		"recovery_state: generation_complete",
		"expected_items: 2",
		"recovered_items: 2",
		"missing_items: 0",
		"persisted_options: 4",
		"state_advanced: false",
		"current_phase: items_ready",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("recover stdout missing %q:\n%s", want, out)
		}
	}

	// The stranded lock must be gone; a subsequent acquire must succeed.
	store := openCLIJobStore(t, home)
	defer store.Close()
	if _, err := store.GetResourceLock(context.Background(), skillOptTrainGenerationLockKey("landing-train", "landing-train-001")); err == nil {
		t.Fatalf("generation lock still held after reclaim")
	}
	// An audit event for the reclaim must exist.
	events, err := store.ListJobEvents(context.Background(), "local-skillopt-train-generation-landing-train-deadbeef")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	foundReclaim := false
	for _, event := range events {
		if event.Kind == "lock_reclaimed" {
			foundReclaim = true
		}
	}
	if !foundReclaim {
		t.Fatalf("no lock_reclaimed audit event recorded: %+v", events)
	}
	// State must NOT have advanced without --advance-state.
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateItemsReady {
		t.Fatalf("iteration state = %s, want items_ready", iteration.State)
	}
}

func TestSkillOptTrainRecoverGenerationRefusesLiveOwner(t *testing.T) {
	home, _ := seedSkillOptTrainItemsReady(t, 2)
	// Live owner: use this test process's own PID so it is provably alive.
	seedStrandedGenerationLock(t, home, int64(os.Getpid()), thisHostname(t), time.Now().UTC().Add(time.Hour))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("recover live owner exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "recovery_state: generation_active") {
		t.Fatalf("recover live owner stdout = %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "skillopt train generation is already running") {
		t.Fatalf("recover live owner stderr = %s", stderr.String())
	}

	// The live owner's lock must NOT have been stolen.
	store := openCLIJobStore(t, home)
	defer store.Close()
	lock, err := store.GetResourceLock(context.Background(), skillOptTrainGenerationLockKey("landing-train", "landing-train-001"))
	if err != nil {
		t.Fatalf("live owner lock missing after refused recover: %v", err)
	}
	if lock.OwnerPID != int64(os.Getpid()) {
		t.Fatalf("live owner lock owner pid = %d, want %d", lock.OwnerPID, os.Getpid())
	}
}

func TestSkillOptTrainRecoverGenerationReclaimsDeadEmptyHostLock(t *testing.T) {
	// Legacy strand (pre-#303): the lock was written by a binary that did not
	// record owner_hostname, so it is empty. A local-first workflow treats an
	// unrecorded host as this host, so a dead PID with an unexpired lease must be
	// reclaimable — exactly the motivating case #303 exists to clear.
	home, _ := seedSkillOptTrainItemsReady(t, 2)
	seedStrandedGenerationLock(t, home, deadPID(t), "", time.Now().UTC().Add(time.Hour))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("recover empty-host exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"mode: generation",
		"lock_reclaimed: true",
		"recovery_state: generation_complete",
		"expected_items: 2",
		"recovered_items: 2",
		"missing_items: 0",
		"persisted_options: 4",
		"state_advanced: false",
		"current_phase: items_ready",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("recover empty-host stdout missing %q:\n%s", want, out)
		}
	}

	store := openCLIJobStore(t, home)
	defer store.Close()
	// The stranded lock must be gone.
	if _, err := store.GetResourceLock(context.Background(), skillOptTrainGenerationLockKey("landing-train", "landing-train-001")); err == nil {
		t.Fatalf("generation lock still held after empty-host reclaim")
	}
	// An audit event for the reclaim must exist.
	events, err := store.ListJobEvents(context.Background(), "local-skillopt-train-generation-landing-train-deadbeef")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	foundReclaim := false
	for _, event := range events {
		if event.Kind == "lock_reclaimed" {
			foundReclaim = true
		}
	}
	if !foundReclaim {
		t.Fatalf("no lock_reclaimed audit event recorded: %+v", events)
	}
	// State must NOT have advanced without --advance-state.
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateItemsReady {
		t.Fatalf("iteration state = %s, want items_ready", iteration.State)
	}
}

func TestSkillOptTrainRecoverGenerationEmptyHostAdvanceState(t *testing.T) {
	// Legacy empty-host strand with a dead owner: --advance-state must advance the
	// complete run to options_generated, mirroring the same-host advance test.
	home, _ := seedSkillOptTrainItemsReady(t, 2)
	seedStrandedGenerationLock(t, home, deadPID(t), "", time.Now().UTC().Add(time.Hour))

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation", "--advance-state"}, &stdout, &stderr); code != 0 {
		t.Fatalf("recover empty-host --advance-state exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"state_advanced: true",
		"current_phase: options_generated",
		"recovery_state: generation_complete",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("recover empty-host --advance-state stdout missing %q:\n%s", want, stdout.String())
		}
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateOptionsGenerated {
		t.Fatalf("iteration state after empty-host --advance-state = %s, want options_generated", iteration.State)
	}
}

func TestSkillOptTrainRecoverGenerationEmptyHostAbortReclaims(t *testing.T) {
	// --abort must reach the reclaim path for a legacy empty-host dead-owner lock
	// (the host gate must not short-circuit before abort). Persisted items survive.
	home, _ := seedSkillOptTrainItemsReady(t, 2)
	seedStrandedGenerationLock(t, home, deadPID(t), "", time.Now().UTC().Add(time.Hour))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation", "--abort"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("recover empty-host --abort exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"recovery_state: generation_lock_reclaimed",
		"lock_reclaimed: true",
		"current_phase: items_ready",
		"state_advanced: false",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("recover empty-host --abort stdout missing %q:\n%s", want, stdout.String())
		}
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	if _, err := store.GetResourceLock(context.Background(), skillOptTrainGenerationLockKey("landing-train", "landing-train-001")); err == nil {
		t.Fatalf("generation lock still held after empty-host --abort reclaim")
	}
	options, err := store.ListEvalReviewOptions(context.Background(), "landing-train-review-001", "")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 4 {
		t.Fatalf("abort dropped persisted options: got %d, want 4", len(options))
	}
}

func TestSkillOptTrainRecoverGenerationEmptyHostRefusesLiveOwner(t *testing.T) {
	// A LIVE owner with an empty (legacy) hostname must still be refused — never
	// steal a live owner — and the message must NOT claim the owner is on another
	// host (the host is simply unrecorded, and the workflow is local-first).
	home, _ := seedSkillOptTrainItemsReady(t, 2)
	seedStrandedGenerationLock(t, home, int64(os.Getpid()), "", time.Now().UTC().Add(time.Hour))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("recover empty-host live owner exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "recovery_state: generation_active") {
		t.Fatalf("recover empty-host live owner stdout = %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "skillopt train generation is already running") {
		t.Fatalf("recover empty-host live owner stderr = %s", stderr.String())
	}
	// The message must report the running PID, not claim a foreign host.
	if !strings.Contains(stderr.String(), "still running") {
		t.Fatalf("recover empty-host live owner stderr missing 'still running': %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "another host") || strings.Contains(stderr.String(), "owner on host") {
		t.Fatalf("recover empty-host live owner stderr wrongly claims another host: %s", stderr.String())
	}

	// The live owner's lock must NOT have been stolen.
	store := openCLIJobStore(t, home)
	defer store.Close()
	lock, err := store.GetResourceLock(context.Background(), skillOptTrainGenerationLockKey("landing-train", "landing-train-001"))
	if err != nil {
		t.Fatalf("live owner lock missing after refused recover: %v", err)
	}
	if lock.OwnerPID != int64(os.Getpid()) {
		t.Fatalf("live owner lock owner pid = %d, want %d", lock.OwnerPID, os.Getpid())
	}
}

func TestSkillOptTrainRecoverGenerationRequiresTTLForCrossHost(t *testing.T) {
	home, _ := seedSkillOptTrainItemsReady(t, 2)
	// Cross-host dead owner, lease NOT yet expired -> must refuse (cannot verify
	// liveness across hosts).
	seedStrandedGenerationLock(t, home, deadPID(t), "some-other-host.example", time.Now().UTC().Add(time.Hour))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("recover cross-host unexpired exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "recovery_state: generation_active") ||
		!strings.Contains(stderr.String(), "skillopt train generation is already running") {
		t.Fatalf("recover cross-host unexpired stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	if _, err := store.GetResourceLock(context.Background(), skillOptTrainGenerationLockKey("landing-train", "landing-train-001")); err != nil {
		t.Fatalf("cross-host lock removed despite unexpired lease: %v", err)
	}
}

func TestSkillOptTrainRecoverGenerationReclaimsExpiredCrossHostLock(t *testing.T) {
	home, _ := seedSkillOptTrainItemsReady(t, 2)
	// Cross-host owner with an EXPIRED lease -> reclaimable on TTL expiry.
	seedStrandedGenerationLock(t, home, deadPID(t), "some-other-host.example", time.Now().UTC().Add(-time.Hour))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("recover expired cross-host exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "lock_reclaimed: true") ||
		!strings.Contains(stdout.String(), "recovery_state: generation_complete") {
		t.Fatalf("recover expired cross-host stdout = %s", stdout.String())
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	if _, err := store.GetResourceLock(context.Background(), skillOptTrainGenerationLockKey("landing-train", "landing-train-001")); err == nil {
		t.Fatalf("expired cross-host lock still held after reclaim")
	}
}

func TestSkillOptTrainRecoverGenerationAdvanceStateGate(t *testing.T) {
	home, _ := seedSkillOptTrainItemsReady(t, 2)
	seedStrandedGenerationLock(t, home, deadPID(t), thisHostname(t), time.Now().UTC().Add(time.Hour))

	// Without --advance-state, state stays items_ready.
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation"}, &stdout, &stderr); code != 0 {
		t.Fatalf("recover without advance exit code = %d; stderr=%s", code, stderr.String())
	}
	store := openCLIJobStore(t, home)
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateItemsReady {
		t.Fatalf("iteration state after recover (no advance) = %s, want items_ready", iteration.State)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	// With --advance-state, the complete run advances to options_generated.
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation", "--advance-state"}, &stdout, &stderr); code != 0 {
		t.Fatalf("recover with advance exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"state_advanced: true",
		"current_phase: options_generated",
		"recovery_state: generation_complete",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("recover --advance-state stdout missing %q:\n%s", want, stdout.String())
		}
	}
	store = openCLIJobStore(t, home)
	defer store.Close()
	iteration, err = store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateOptionsGenerated {
		t.Fatalf("iteration state after --advance-state = %s, want options_generated", iteration.State)
	}
	if !strings.Contains(iteration.MetadataJSON, `"status":"recovered"`) {
		t.Fatalf("iteration metadata after advance = %s", iteration.MetadataJSON)
	}
}

func TestSkillOptTrainRecoverGenerationIncompleteDoesNotAdvance(t *testing.T) {
	// Only 1 of 2 items persisted -> import-only recovery reports the missing
	// item and refuses to advance even with --advance-state.
	home, _ := seedSkillOptTrainItemsReady(t, 1)
	seedStrandedGenerationLock(t, home, deadPID(t), thisHostname(t), time.Now().UTC().Add(time.Hour))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation", "--advance-state"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("recover incomplete exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"recovery_state: generation_incomplete",
		"expected_items: 2",
		"recovered_items: 1",
		"missing_items: 1",
		"persisted_options: 2",
		"state_advanced: false",
		"missing_item_ids:",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("recover incomplete stdout missing %q:\n%s", want, stdout.String())
		}
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateItemsReady {
		t.Fatalf("incomplete recover advanced state to %s", iteration.State)
	}
	// The lock must still have been reclaimed.
	if _, err := store.GetResourceLock(context.Background(), skillOptTrainGenerationLockKey("landing-train", "landing-train-001")); err == nil {
		t.Fatalf("generation lock still held after incomplete reclaim")
	}
}

func TestSkillOptTrainRecoverGenerationAbortReclaimsAndKeepsItemsReady(t *testing.T) {
	home, _ := seedSkillOptTrainItemsReady(t, 2)
	seedStrandedGenerationLock(t, home, deadPID(t), thisHostname(t), time.Now().UTC().Add(time.Hour))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation", "--abort"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("recover --abort exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"recovery_state: generation_lock_reclaimed",
		"lock_reclaimed: true",
		"current_phase: items_ready",
		"state_advanced: false",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("recover --abort stdout missing %q:\n%s", want, stdout.String())
		}
	}
	store := openCLIJobStore(t, home)
	defer store.Close()
	if _, err := store.GetResourceLock(context.Background(), skillOptTrainGenerationLockKey("landing-train", "landing-train-001")); err == nil {
		t.Fatalf("generation lock still held after --abort reclaim")
	}
	// Persisted items must survive an abort.
	options, err := store.ListEvalReviewOptions(context.Background(), "landing-train-review-001", "")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 4 {
		t.Fatalf("abort dropped persisted options: got %d, want 4", len(options))
	}
	iteration, err := store.GetLatestSkillOptTrainIteration(context.Background(), "landing-train")
	if err != nil {
		t.Fatalf("GetLatestSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateItemsReady {
		t.Fatalf("abort changed state to %s", iteration.State)
	}
}

func TestSkillOptTrainRecoverGenerationNoLock(t *testing.T) {
	// No stranded lock present: recovery still classifies the persisted state.
	home, _ := seedSkillOptTrainItemsReady(t, 2)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("recover no-lock exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"lock_reclaimed: false",
		"recovery_state: generation_complete",
		"recovered_items: 2",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("recover no-lock stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSkillOptTrainRecoverGenerationFlagsRequireGeneration(t *testing.T) {
	home, _ := seedSkillOptTrainItemsReady(t, 2)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--advance-state"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("recover --advance-state without --generation exit code = %d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "require --generation") {
		t.Fatalf("recover flag-guard stderr = %s", stderr.String())
	}
}

func TestSkillOptTrainStatusSurfacesStaleGenerationLock(t *testing.T) {
	home, _ := seedSkillOptTrainItemsReady(t, 1)
	// Dead owner + expired lease => classified "stale" (not active), surfaced
	// separately from the true items_ready phase. (A future-lease dead-owner lock
	// is deliberately still "active" to avoid flapping a legitimate long run; the
	// status "stale" label needs the lease to have expired.)
	seedStrandedGenerationLock(t, home, deadPID(t), thisHostname(t), time.Now().UTC().Add(-time.Hour))

	store := openCLIJobStore(t, home)
	defer store.Close()
	snapshot, err := loadSkillOptTrainStatusSnapshot(context.Background(), store, "landing-train", true)
	if err != nil {
		t.Fatalf("loadSkillOptTrainStatusSnapshot returned error: %v", err)
	}
	if snapshot.Verbose == nil {
		t.Fatalf("status snapshot verbose nil")
	}
	var genLock *skillOptTrainStatusLock
	for i := range snapshot.Verbose.ActiveLocks {
		if snapshot.Verbose.ActiveLocks[i].Name == "generation" {
			genLock = &snapshot.Verbose.ActiveLocks[i]
		}
	}
	if genLock == nil {
		t.Fatalf("generation lock not surfaced in status: %+v", snapshot.Verbose.ActiveLocks)
	}
	if genLock.Status != "stale" {
		t.Fatalf("generation lock status = %q, want stale", genLock.Status)
	}
	// The true phase must not be hidden by the stale lock.
	if snapshot.CurrentPhase != skillopt.TrainStateItemsReady {
		t.Fatalf("status current_phase = %s, want items_ready", snapshot.CurrentPhase)
	}
}

func TestSkillOptTrainRecoverGenerationJSON(t *testing.T) {
	home, _ := seedSkillOptTrainItemsReady(t, 2)
	seedStrandedGenerationLock(t, home, deadPID(t), thisHostname(t), time.Now().UTC().Add(time.Hour))

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "recover", "--home", home, "--session", "landing-train", "--generation", "--advance-state", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("recover --json exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		`"mode": "generation"`,
		`"classification": "generation_complete"`,
		`"lock_reclaimed": true`,
		`"state_advanced": true`,
		`"recovered_items": 2`,
		`"persisted_options": 4`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("recover --json missing %q:\n%s", want, out)
		}
	}
}
