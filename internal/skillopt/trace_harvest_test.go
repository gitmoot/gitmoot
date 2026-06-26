package skillopt

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// stubStatusReader is a CombinedStatusReader stub for the no-CI guard tests. It
// returns a canned CombinedStatus (or an error) regardless of repo/ref.
type stubStatusReader struct {
	status github.CombinedStatus
	err    error
	calls  int
}

func (s *stubStatusReader) GetCombinedStatus(_ context.Context, _ github.Repository, _ string) (github.CombinedStatus, error) {
	s.calls++
	if s.err != nil {
		return github.CombinedStatus{}, s.err
	}
	return s.status, nil
}

// realCIStatus is a combined status with a passing external (non-gitmoot/) check.
func realCIStatus() github.CombinedStatus {
	return github.CombinedStatus{
		State: "success",
		Statuses: []github.CommitStatus{
			{Context: "ci/build", State: "success"},
		},
	}
}

// noCIStatus is the empty-gate combined status carrying only the synthetic
// gitmoot/ci context the merge gate writes when no external CI exists.
func noCIStatus() github.CombinedStatus {
	return github.CombinedStatus{
		State: "success",
		Statuses: []github.CommitStatus{
			{Context: gitmootNoCIContext, State: "success"},
		},
	}
}

// installTraceTemplate installs a template and returns its current version, plus
// an implement JobPayload attributed to that exact version (TemplateID +
// TemplateResolvedCommit), so the harvester resolves the version from the payload.
func installTraceTemplate(t *testing.T, store *db.Store, id string) (db.AgentTemplateVersion, workflow.JobPayload) {
	t.Helper()
	ctx := context.Background()
	template := testTemplate(id, "Do the work.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, id)
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	version, err := store.GetAgentTemplateVersionByID(ctx, installed.VersionID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	payload := workflow.JobPayload{
		Repo:                   "owner/repo",
		PullRequest:            7,
		TaskID:                 "task-1",
		TemplateID:             id,
		TemplateResolvedCommit: template.ResolvedCommit,
	}
	return version, payload
}

func newTraceStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func implementJob() db.Job {
	return db.Job{ID: "job-implement-1", Type: "implement"}
}

func feedbackForVersion(t *testing.T, store *db.Store, versionID string) []db.FeedbackEvent {
	t.Helper()
	events, err := store.ListFeedbackEvents(context.Background(), autoTraceRunIDPrefix+versionID)
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	return events
}

// TestHarvestMergedRealCIWritesStrongPositive asserts a merge with a genuine
// external CI pass yields a strong-positive (score 1.0, choice "a") FeedbackEvent
// in the per-version auto-trace run, tagged source=auto-trace/reviewer=gitmoot-auto.
func TestHarvestMergedRealCIWritesStrongPositive(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	status := &stubStatusReader{status: realCIStatus()}
	h := NewOutcomeHarvester(store, status)

	if err := h.Harvest(ctx, implementJob(), payload, workflow.Outcome{
		Kind:        workflow.OutcomeMerged,
		Repo:        "owner/repo",
		PullRequest: 7,
		HeadSHA:     "deadbeef",
	}); err != nil {
		t.Fatalf("Harvest returned error: %v", err)
	}

	if status.calls != 1 {
		t.Fatalf("expected exactly one combined-status read, got %d", status.calls)
	}
	events := feedbackForVersion(t, store, version.ID)
	if len(events) != 1 {
		t.Fatalf("expected exactly one feedback event, got %d: %+v", len(events), events)
	}
	event := events[0]
	if event.Choice != "a" {
		t.Fatalf("merged+real-CI choice = %q, want a", event.Choice)
	}
	if event.Source != autoTraceSource || event.Reviewer != autoTraceReviewer {
		t.Fatalf("source/reviewer = %q/%q, want %q/%q", event.Source, event.Reviewer, autoTraceSource, autoTraceReviewer)
	}
	if event.SourceURL != "https://github.com/owner/repo/pull/7" {
		t.Fatalf("source_url = %q", event.SourceURL)
	}

	// Verify the score via the projection: merged + real CI => Soft 1.0.
	signal, choice := h.project(ctx, payload, workflow.Outcome{Kind: workflow.OutcomeMerged, Repo: "owner/repo", PullRequest: 7, HeadSHA: "deadbeef"})
	if choice != "a" || !signal.HasScore || signal.Score != scoreMergedRealCI {
		t.Fatalf("merged+real-CI projection = %+v choice=%q, want score=%v choice=a", signal, choice, scoreMergedRealCI)
	}

	// Run-level metadata + export must surface feedback_source=automatic_trace.
	run, err := store.GetEvalRun(ctx, autoTraceRunIDPrefix+version.ID)
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	if run.Mode != db.EvalRunModeValidate {
		t.Fatalf("auto-trace run mode = %q, want validate", run.Mode)
	}
	pkg, err := ExportTrainingPackage(ctx, store, run.ID)
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}
	var fc map[string]any
	if err := json.Unmarshal(pkg.FeedbackContext, &fc); err != nil {
		t.Fatalf("feedback context did not unmarshal: %v", err)
	}
	if fc["feedback_source"] != FeedbackSourceAutomaticTrace {
		t.Fatalf("feedback_source = %v, want %q", fc["feedback_source"], FeedbackSourceAutomaticTrace)
	}
	if len(pkg.EvaluatorConfig) != 0 {
		t.Fatalf("auto-trace run leaked evaluator_config = %s", string(pkg.EvaluatorConfig))
	}
}

// TestHarvestMergedNoCINearNeutral asserts a merge through the empty gate (only
// the synthetic gitmoot/ci context) is scored near-neutral (~0.5), NOT a strong
// positive — the no-CI guard.
func TestHarvestMergedNoCINearNeutral(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, &stubStatusReader{status: noCIStatus()})

	outcome := workflow.Outcome{Kind: workflow.OutcomeMerged, Repo: "owner/repo", PullRequest: 7, HeadSHA: "deadbeef"}
	if err := h.Harvest(ctx, implementJob(), payload, outcome); err != nil {
		t.Fatalf("Harvest returned error: %v", err)
	}
	signal, choice := h.project(ctx, payload, outcome)
	if choice != "a" {
		t.Fatalf("merged+no-CI choice = %q, want a (positive but weak)", choice)
	}
	if !signal.HasScore || signal.Score != scoreMergedNoCI {
		t.Fatalf("merged+no-CI score = %v (has=%v), want %v", signal.Score, signal.HasScore, scoreMergedNoCI)
	}
	if signal.Score >= scoreMergedRealCI {
		t.Fatalf("no-CI merge must not score a strong positive, got %v", signal.Score)
	}
	events := feedbackForVersion(t, store, version.ID)
	if len(events) != 1 || events[0].Choice != "a" {
		t.Fatalf("expected one choice=a feedback row, got %+v", events)
	}
}

// TestHarvestMergedNoStatusReaderIsNearNeutral asserts a nil status reader (or a
// status read failure) conservatively degrades a merge to near-neutral rather
// than rewarding it as a strong positive.
func TestHarvestMergedNoStatusReaderIsNearNeutral(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	_, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, nil)
	signal, _ := h.project(ctx, payload, workflow.Outcome{Kind: workflow.OutcomeMerged, Repo: "owner/repo", PullRequest: 7, HeadSHA: "deadbeef"})
	if signal.Score != scoreMergedNoCI {
		t.Fatalf("nil status reader merge score = %v, want near-neutral %v", signal.Score, scoreMergedNoCI)
	}
}

// TestHarvestBlockedWritesHardNegative asserts a merge-gate block yields an
// authoritative gate-fail (Hard 0, choice "b") negative.
func TestHarvestBlockedWritesHardNegative(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, &stubStatusReader{})

	outcome := workflow.Outcome{Kind: workflow.OutcomeBlocked, Repo: "owner/repo", PullRequest: 7, Reason: "external CI check failed"}
	if err := h.Harvest(ctx, implementJob(), payload, outcome); err != nil {
		t.Fatalf("Harvest returned error: %v", err)
	}
	signal, choice := h.project(ctx, payload, outcome)
	if choice != "b" || !signal.HasScore || signal.Score != 0 {
		t.Fatalf("blocked projection = %+v choice=%q, want score=0 choice=b", signal, choice)
	}
	events := feedbackForVersion(t, store, version.ID)
	if len(events) != 1 || events[0].Choice != "b" {
		t.Fatalf("expected one choice=b feedback row, got %+v", events)
	}
}

// TestHarvestChangesRequestedGradedByFixRounds asserts the changes_requested
// score decreases monotonically as the fix-round count grows.
func TestHarvestChangesRequestedGradedByFixRounds(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	_, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, &stubStatusReader{})

	var prev float64 = 2 // larger than any score so the first comparison passes
	for round := 1; round <= 4; round++ {
		signal, choice := h.project(ctx, payload, workflow.Outcome{
			Kind:        workflow.OutcomeChangesRequested,
			Repo:        "owner/repo",
			PullRequest: 7,
			FixRounds:   round,
		})
		if choice != "b" {
			t.Fatalf("changes_requested round %d choice = %q, want b", round, choice)
		}
		if !signal.HasScore {
			t.Fatalf("changes_requested round %d has no score", round)
		}
		if signal.Score >= prev {
			t.Fatalf("changes_requested score did not decrease at round %d: %v >= %v", round, signal.Score, prev)
		}
		prev = signal.Score
	}
	if changesRequestedScore(1) != scoreChangesRequestedBase {
		t.Fatalf("round-1 score = %v, want %v", changesRequestedScore(1), scoreChangesRequestedBase)
	}
}

// TestHarvestCorrectiveOnRevert asserts a later revert re-upserts the SAME
// UNIQUE row (count stays 1) and flips the prior positive choice a -> b in place.
func TestHarvestCorrectiveOnRevert(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, &stubStatusReader{status: realCIStatus()})

	if err := h.Harvest(ctx, implementJob(), payload, workflow.Outcome{
		Kind: workflow.OutcomeMerged, Repo: "owner/repo", PullRequest: 7, HeadSHA: "deadbeef",
	}); err != nil {
		t.Fatalf("Harvest (merge) returned error: %v", err)
	}
	before := feedbackForVersion(t, store, version.ID)
	if len(before) != 1 || before[0].Choice != "a" {
		t.Fatalf("expected one choice=a row before revert, got %+v", before)
	}

	if err := h.Harvest(ctx, implementJob(), payload, workflow.Outcome{
		Kind: workflow.OutcomeReverted, Repo: "owner/repo", PullRequest: 7,
	}); err != nil {
		t.Fatalf("Harvest (revert) returned error: %v", err)
	}
	after := feedbackForVersion(t, store, version.ID)
	if len(after) != 1 {
		t.Fatalf("revert must overwrite in place, got %d rows: %+v", len(after), after)
	}
	if after[0].Choice != "b" {
		t.Fatalf("revert must flip choice a -> b, got %q", after[0].Choice)
	}
	if after[0].ItemID != before[0].ItemID {
		t.Fatalf("revert wrote a different item id %q vs %q", after[0].ItemID, before[0].ItemID)
	}
}

// TestHarvestSkipsCoordinatorContinuation asserts a coordinator continuation job
// (parent set, no own PR) writes no eval_run/feedback row.
func TestHarvestSkipsCoordinatorContinuation(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, base := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, &stubStatusReader{status: realCIStatus()})

	// A continuation leg: carries a parent linkage but produced no PR of its own.
	continuation := base
	continuation.ParentJobID = "coord-1"
	continuation.PullRequest = 0
	if err := h.Harvest(ctx, db.Job{ID: "job-cont", Type: "implement"}, continuation, workflow.Outcome{
		Kind: workflow.OutcomeMerged, Repo: "owner/repo", PullRequest: 7, HeadSHA: "deadbeef",
	}); err != nil {
		t.Fatalf("Harvest returned error: %v", err)
	}
	if events := feedbackForVersion(t, store, version.ID); len(events) != 0 {
		t.Fatalf("coordinator continuation must write no feedback, got %+v", events)
	}

	// A delegation-finalize continuation is also out of scope.
	finalize := base
	finalize.DelegationFinalize = true
	if err := h.Harvest(ctx, db.Job{ID: "job-fin", Type: "implement"}, finalize, workflow.Outcome{
		Kind: workflow.OutcomeMerged, Repo: "owner/repo", PullRequest: 7, HeadSHA: "deadbeef",
	}); err != nil {
		t.Fatalf("Harvest returned error: %v", err)
	}
	if events := feedbackForVersion(t, store, version.ID); len(events) != 0 {
		t.Fatalf("delegation-finalize continuation must write no feedback, got %+v", events)
	}
}

// TestHarvestSkipsNonImplementAndUntemplated asserts a non-implement job and an
// implement job with no template attribution are both skipped.
func TestHarvestSkipsNonImplementAndUntemplated(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	h := NewOutcomeHarvester(store, &stubStatusReader{status: realCIStatus()})

	// A review job is out of scope.
	if err := h.Harvest(ctx, db.Job{ID: "job-review", Type: "review"}, payload, workflow.Outcome{
		Kind: workflow.OutcomeMerged, Repo: "owner/repo", PullRequest: 7, HeadSHA: "deadbeef",
	}); err != nil {
		t.Fatalf("Harvest returned error: %v", err)
	}
	if events := feedbackForVersion(t, store, version.ID); len(events) != 0 {
		t.Fatalf("non-implement job must write no feedback, got %+v", events)
	}

	// An implement job with no TemplateID is out of scope.
	untemplated := payload
	untemplated.TemplateID = ""
	untemplated.TemplateResolvedCommit = ""
	if err := h.Harvest(ctx, db.Job{ID: "job-untemplated", Type: "implement"}, untemplated, workflow.Outcome{
		Kind: workflow.OutcomeMerged, Repo: "owner/repo", PullRequest: 7, HeadSHA: "deadbeef",
	}); err != nil {
		t.Fatalf("Harvest returned error: %v", err)
	}
	if events := feedbackForVersion(t, store, version.ID); len(events) != 0 {
		t.Fatalf("untemplated implement job must write no feedback, got %+v", events)
	}
}

// TestHarvestResolvesByCurrentVersionFallback asserts that when the payload's
// TemplateResolvedCommit does not match any version, the harvester falls back to
// the template's current version rather than guessing or erroring.
func TestHarvestResolvesByCurrentVersionFallback(t *testing.T) {
	ctx := context.Background()
	store := newTraceStore(t)
	version, payload := installTraceTemplate(t, store, "planner")
	payload.TemplateResolvedCommit = "does-not-match-any-version"
	h := NewOutcomeHarvester(store, &stubStatusReader{status: realCIStatus()})

	if err := h.Harvest(ctx, implementJob(), payload, workflow.Outcome{
		Kind: workflow.OutcomeMerged, Repo: "owner/repo", PullRequest: 7, HeadSHA: "deadbeef",
	}); err != nil {
		t.Fatalf("Harvest returned error: %v", err)
	}
	if events := feedbackForVersion(t, store, version.ID); len(events) != 1 {
		t.Fatalf("fallback to current version must write one feedback row, got %+v", events)
	}
}

// TestHarvestNilStoreNoOp asserts a harvester with no store is a safe no-op.
func TestHarvestNilStoreNoOp(t *testing.T) {
	h := &OutcomeHarvester{}
	if err := h.Harvest(context.Background(), db.Job{Type: "implement"}, workflow.JobPayload{TemplateID: "x"}, workflow.Outcome{Kind: workflow.OutcomeMerged}); err != nil {
		t.Fatalf("nil-store Harvest returned error: %v", err)
	}
}
