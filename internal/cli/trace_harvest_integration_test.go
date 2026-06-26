package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// TestTraceHarvestFullChain is the FULL-CHAIN integration test for #465 (Mode A
// trace-harvester). The package's existing tests cover each LINK separately —
// internal/workflow/outcome_harvester_test.go proves the engine fires Harvest on
// AdvanceJob but against a MOCK recordingHarvester, while
// internal/skillopt/trace_harvest_test.go proves the REAL skillopt harvester
// projects an outcome into a feedback row but invoked directly (no engine). The
// chain "engine fires on AdvanceJob -> the REAL skillopt OutcomeHarvester (built
// via the SAME daemon construction path) -> a REAL auto-trace feedback row in the
// store" is never exercised end-to-end. This test closes that gap with NO live
// GitHub: the MergeGate is a stub returning a canned MergeDecision and the
// no-CI-guard combined-status reader is a canned stub, while EVERYTHING in
// between (workflow.Engine.AdvanceJob, the engine's harvestOutcomeForMergeGate
// attribution to the implement job, daemonOutcomeHarvester's [skillopt]
// admission gate, skillopt.OutcomeHarvester's projection + the real feedback
// upserts, and the real db.Store reads) is production code.
//
// It is TEST-ONLY: no production behavior is changed and no new exported seam is
// added — internal/cli already imports BOTH internal/workflow and
// internal/skillopt, so it can wire the real harvester into a real engine over a
// real store, which internal/workflow cannot (the import cycle that forces the
// engine test to mock the harvester).

// stubMergeGate is a workflow.MergeGate that returns a canned decision (and
// records the requests it saw) with no live GitHub — the same shape as the
// engine package's fakeMergeGate, re-declared here because that helper is
// unexported test code in another package.
type stubMergeGate struct {
	decision workflow.MergeDecision
	requests []workflow.MergeRequest
}

func (s *stubMergeGate) Evaluate(_ context.Context, request workflow.MergeRequest) (workflow.MergeDecision, error) {
	s.requests = append(s.requests, request)
	return s.decision, nil
}

// harvestGitHubStub satisfies the full github.Client interface (by embedding
// NoopClient) so it can flow through the REAL daemonOutcomeHarvester construction
// path, while overriding ONLY the two reads the no-CI guard consults
// (GetCombinedStatus + ListPullRequestChecks) with canned data. This is the
// genuine harvester's Status reader — no live GitHub, no new seam.
type harvestGitHubStub struct {
	github.NoopClient
	status github.CombinedStatus
	checks []github.PullRequestCheck
}

func (s harvestGitHubStub) GetCombinedStatus(context.Context, github.Repository, string) (github.CombinedStatus, error) {
	return s.status, nil
}

func (s harvestGitHubStub) ListPullRequestChecks(context.Context, github.Repository, int64) ([]github.PullRequestCheck, error) {
	return s.checks, nil
}

// openTraceChainStore opens a throwaway SQLite store for the full-chain test.
func openTraceChainStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// installChainTemplate installs a REAL agent template (via the public
// agenttemplate + db.Store APIs, the same way internal/skillopt's test seeds one)
// and returns its current version. The version's ResolvedCommit is what the
// seeded implement job is attributed to, so the harvester resolves THIS version
// from the job payload and writes into auto-trace:<version.ID>.
func installChainTemplate(t *testing.T, store *db.Store, id string) db.AgentTemplateVersion {
	t.Helper()
	ctx := context.Background()
	content := agenttemplate.FormatTemplateContent(agenttemplate.Metadata{
		ID:                   id,
		Name:                 "Planner",
		Description:          "Plans implementation work.",
		Kind:                 agenttemplate.TemplateKind,
		Version:              agenttemplate.TemplateVersion,
		Capabilities:         []string{"implement"},
		RuntimeCompatibility: []string{"codex"},
		Tags:                 []string{"planning"},
		Inputs:               []string{"task"},
		Outputs:              []string{"plan"},
	}, "# Planner\n\nDo the work.\n")
	parsed, err := agenttemplate.ParseTemplateContent(content)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}
	metadataJSON, err := agenttemplate.MarshalMetadata(parsed.Metadata)
	if err != nil {
		t.Fatalf("MarshalMetadata returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(ctx, db.AgentTemplate{
		ID:             id,
		Name:           parsed.Metadata.Name,
		Description:    parsed.Metadata.Description,
		SourceRepo:     agenttemplate.LocalSourceRepo,
		SourceRef:      agenttemplate.LocalSourceRef,
		SourcePath:     id + ".md",
		ResolvedCommit: agenttemplate.HashContent(content),
		Content:        content,
		MetadataJSON:   metadataJSON,
	}); err != nil {
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
	return version
}

// insertChainJob inserts a completed job carrying the given payload, mirroring
// internal/workflow's insertCompletedJob helper (which is unexported test code in
// another package). The payload is JSON-encoded exactly as the mailbox's
// marshalPayload does (plain json.Marshal), so workflow's unmarshalPayload reads
// it back identically.
func insertChainJob(t *testing.T, store *db.Store, job db.Job, payload workflow.JobPayload) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload returned error: %v", err)
	}
	job.State = string(workflow.JobSucceeded)
	job.Payload = string(encoded)
	if err := store.CreateJobWithEvent(context.Background(), job, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "done"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
}

// seedChainImplementJob inserts a completed implement job attributed to the
// version's template + ResolvedCommit, so the engine's harvestOutcomeForMergeGate
// resolves it as the diff owner for the task's merge/block outcome. It mirrors
// internal/workflow's seedImplementJobForHarvest.
func seedChainImplementJob(t *testing.T, store *db.Store, version db.AgentTemplateVersion) {
	t.Helper()
	insertChainJob(t, store, db.Job{
		ID:    "implement-job",
		Agent: "lead",
		Type:  "implement",
	}, workflow.JobPayload{
		Repo:                   "jerryfane/gitmoot",
		Branch:                 "task-7",
		PullRequest:            7,
		TaskID:                 "task-7",
		TaskTitle:              "Workflow Engine",
		LeadAgent:              "lead",
		TemplateID:             version.TemplateID,
		TemplateResolvedCommit: version.ResolvedCommit,
		Result:                 &workflow.AgentResult{Decision: "implemented", Summary: "did the work"},
	})
}

// seedChainApprovingReview inserts a completed approving review job that, when
// advanced, drives the engine's approved -> runMergeGate path so the stub merge
// gate's decision fires the outcome. headSHA is the PR head the no-CI guard reads
// at (the engine carries it onto the merged Outcome).
func seedChainApprovingReview(t *testing.T, store *db.Store, headSHA string) {
	t.Helper()
	insertChainJob(t, store, db.Job{
		ID:    "review-job",
		Agent: "audit",
		Type:  "review",
	}, workflow.JobPayload{
		Repo:        "jerryfane/gitmoot",
		Branch:      "task-7",
		PullRequest: 7,
		HeadSHA:     headSHA,
		TaskID:      "task-7",
		TaskTitle:   "Workflow Engine",
		LeadAgent:   "lead",
		Result:      &workflow.AgentResult{Decision: "approved", Summary: "looks good"},
	})
}

// chainEngine builds a workflow.Engine over the real store with the deterministic
// JobID helper the engine tests use, plus the stub merge gate. The real harvester
// is wired separately by each subtest (so the off-by-default case can leave it
// nil through the SAME admission gate).
func chainEngine(store *db.Store, gate workflow.MergeGate) workflow.Engine {
	return workflow.Engine{
		Store:     store,
		MergeGate: gate,
		JobID: func(request workflow.JobRequest) string {
			parts := []string{request.Action, request.Agent, request.TaskID}
			if request.ReviewRound != "" {
				parts = append(parts, request.ReviewRound)
			}
			return strings.Join(parts, "-")
		},
		// Run the detached cross-family review leg SYNCHRONOUSLY in the integration
		// tests so its dispatch + harvest are deterministic; the daemon defaults to a
		// goroutine (the review is genuinely off the AdvanceJob path in production).
		ReviewSpawner: func(fn func()) { fn() },
	}
}

// realCIChainStatus is a combined status with a passing external (non-gitmoot/)
// commit status — i.e. genuine external CI succeeded.
func realCIChainStatus() github.CombinedStatus {
	return github.CombinedStatus{
		State:    "success",
		Statuses: []github.CommitStatus{{Context: "ci/build", State: "success"}},
	}
}

// noCIChainStatus is the empty-gate combined status carrying only the synthetic
// gitmoot/ci context the merge gate writes when no external CI exists.
func noCIChainStatus() github.CombinedStatus {
	return github.CombinedStatus{
		State:    "success",
		Statuses: []github.CommitStatus{{Context: "gitmoot/ci", State: "success"}},
	}
}

// enabledHarvesterFor builds the REAL skillopt OutcomeHarvester through the SAME
// daemon construction path the daemon uses — daemonOutcomeHarvester with a real
// [skillopt].auto_trace_enabled=true config and the canned-CI github stub as its
// status reader. It asserts the harvester is non-nil so the test fails loudly if
// the admission gate ever stops wiring it on. The returned value is the genuine
// *skillopt.OutcomeHarvester (it satisfies workflow.OutcomeHarvester).
func enabledHarvesterFor(t *testing.T, store *db.Store, gh github.Client) workflow.OutcomeHarvester {
	t.Helper()
	root := writeHarvestConfig(t, "[skillopt]\nauto_trace_enabled = true\n")
	h := daemonOutcomeHarvester(store, gh, root)
	if h == nil {
		t.Fatal("expected a non-nil harvester with auto_trace_enabled = true")
	}
	if _, ok := h.(*skillopt.OutcomeHarvester); !ok {
		t.Fatalf("expected the real *skillopt.OutcomeHarvester, got %T", h)
	}
	return h
}

// autoTraceFeedback reads the auto-trace feedback rows for a template version via
// the real store (the canonical auto-trace:<versionID> run id), so every
// assertion is against persisted rows, never a mock.
func autoTraceFeedback(t *testing.T, store *db.Store, versionID string) []db.FeedbackEvent {
	t.Helper()
	events, err := store.ListFeedbackEvents(context.Background(), "auto-trace:"+versionID)
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	return events
}

// TestTraceHarvestFullChainMergedRealCI drives the full chain for case (a): an
// approved review advances the engine through the stub merge gate to a MERGE,
// the no-CI guard reads a PASSING external (non-gitmoot/) commit status, and the
// REAL harvester lands EXACTLY ONE strong-positive (choice "a") feedback row in
// auto-trace:<versionID>, tagged source=auto-trace/reviewer=gitmoot-auto — all
// through AdvanceJob, never a direct Harvest call.
func TestTraceHarvestFullChainMergedRealCI(t *testing.T) {
	ctx := context.Background()
	store := openTraceChainStore(t)
	version := installChainTemplate(t, store, "planner")
	gate := &stubMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine := chainEngine(store, gate)
	engine.OutcomeHarvester = enabledHarvesterFor(t, store, harvestGitHubStub{status: realCIChainStatus()})

	seedChainImplementJob(t, store, version)
	seedChainApprovingReview(t, store, "head123")

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	events := autoTraceFeedback(t, store, version.ID)
	if len(events) != 1 {
		t.Fatalf("merged+real-CI: feedback rows = %d, want exactly 1: %+v", len(events), events)
	}
	event := events[0]
	if event.Choice != "a" {
		t.Fatalf("merged+real-CI choice = %q, want a", event.Choice)
	}
	if event.Source != "auto-trace" || event.Reviewer != "gitmoot-auto" {
		t.Fatalf("source/reviewer = %q/%q, want auto-trace/gitmoot-auto", event.Source, event.Reviewer)
	}
	if event.RunID != "auto-trace:"+version.ID {
		t.Fatalf("run id = %q, want auto-trace:%s", event.RunID, version.ID)
	}
	if event.SourceURL != "https://github.com/jerryfane/gitmoot/pull/7" {
		t.Fatalf("source_url = %q", event.SourceURL)
	}
	// The strong-positive band is the persisted, observable marker that the no-CI
	// guard saw genuine external CI and rewarded it (NOT the near-neutral no-CI
	// band case (b) earns). Asserting on the real reasoning keeps the test reading
	// the store while proving the strong-positive projection fired end-to-end.
	if !strings.Contains(event.Reasoning, "passing external CI") {
		t.Fatalf("merged+real-CI reasoning = %q, want the strong-positive marker", event.Reasoning)
	}
	if strings.Contains(event.Reasoning, "near-neutral") {
		t.Fatalf("merged+real-CI must not carry the near-neutral no-CI marker: %q", event.Reasoning)
	}

	// The strong-positive score is verifiable through the real run: a merge with
	// genuine external CI projects the strong-positive band, NOT the near-neutral
	// no-CI band. The skillopt package owns the score bands; here we assert the
	// run landed as a ready validate auto-trace run and that case (b) below stays
	// distinct from this one.
	run, err := store.GetEvalRun(ctx, "auto-trace:"+version.ID)
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	if run.Mode != db.EvalRunModeValidate {
		t.Fatalf("auto-trace run mode = %q, want validate", run.Mode)
	}
	if run.TemplateVersionID != version.ID {
		t.Fatalf("auto-trace run version id = %q, want %s", run.TemplateVersionID, version.ID)
	}

	// Confirm the engine actually went through the merge gate (the chain, not a
	// shortcut) and the stub saw the approving review's PR head SHA.
	if len(gate.requests) != 1 || gate.requests[0].HeadSHA != "head123" {
		t.Fatalf("merge gate requests = %+v, want one carrying head123", gate.requests)
	}
}

// TestTraceHarvestFullChainMergedNoCI drives case (b): a MERGE whose head carries
// ONLY the synthetic gitmoot/ci context (no real external CI). The no-CI guard
// must demote it to the near-neutral band — still choice "a", but NOT the strong
// positive that case (a) earns. Asserted by comparing the two real runs' rows:
// both are choice "a", and the no-CI reasoning explicitly marks it near-neutral.
func TestTraceHarvestFullChainMergedNoCI(t *testing.T) {
	ctx := context.Background()
	store := openTraceChainStore(t)
	version := installChainTemplate(t, store, "planner")
	gate := &stubMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine := chainEngine(store, gate)
	// Only the synthetic gitmoot/ci status, no external CI and no passing checks.
	engine.OutcomeHarvester = enabledHarvesterFor(t, store, harvestGitHubStub{status: noCIChainStatus()})

	seedChainImplementJob(t, store, version)
	seedChainApprovingReview(t, store, "head123")

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	events := autoTraceFeedback(t, store, version.ID)
	if len(events) != 1 {
		t.Fatalf("merged+no-CI: feedback rows = %d, want exactly 1: %+v", len(events), events)
	}
	event := events[0]
	if event.Choice != "a" {
		t.Fatalf("merged+no-CI choice = %q, want a (positive but weak)", event.Choice)
	}
	// The no-CI guard's near-neutral reasoning is the persisted, observable marker
	// that this merge was NOT rewarded as a strong positive (the strong-positive
	// reasoning instead says "merged with passing external CI"). Asserting on the
	// real persisted reasoning keeps the test reading the store, not a mock, while
	// still proving the no-CI demotion fired end-to-end.
	if got := event.Reasoning; got == "" ||
		!strings.Contains(got, "no external CI") || !strings.Contains(got, "near-neutral") {
		t.Fatalf("merged+no-CI reasoning = %q, want the near-neutral no-CI marker", got)
	}
	if strings.Contains(event.Reasoning, "passing external CI") {
		t.Fatalf("merged+no-CI must not carry the strong-positive reasoning: %q", event.Reasoning)
	}
}

// TestTraceHarvestFullChainBlockedQuality drives case (c): an authoritative
// template-quality block (MergeBlockQuality) at the merge gate. AdvanceJob
// returns a BlockedError (the block transition still happens) AND the real
// harvester lands EXACTLY ONE choice "b" negative for the version.
func TestTraceHarvestFullChainBlockedQuality(t *testing.T) {
	ctx := context.Background()
	store := openTraceChainStore(t)
	version := installChainTemplate(t, store, "planner")
	gate := &stubMergeGate{decision: workflow.MergeDecision{
		Reason:     "external CI failed",
		BlockClass: workflow.MergeBlockQuality,
	}}
	engine := chainEngine(store, gate)
	engine.OutcomeHarvester = enabledHarvesterFor(t, store, harvestGitHubStub{status: realCIChainStatus()})

	seedChainImplementJob(t, store, version)
	seedChainApprovingReview(t, store, "head123")

	err := engine.AdvanceJob(ctx, "review-job")
	var blocked workflow.BlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "external CI failed" {
		t.Fatalf("AdvanceJob error = %v, want a blocked transition carrying the reason", err)
	}

	events := autoTraceFeedback(t, store, version.ID)
	if len(events) != 1 {
		t.Fatalf("blocked: feedback rows = %d, want exactly 1: %+v", len(events), events)
	}
	if events[0].Choice != "b" {
		t.Fatalf("blocked choice = %q, want b", events[0].Choice)
	}
	if events[0].Source != "auto-trace" || events[0].Reviewer != "gitmoot-auto" {
		t.Fatalf("blocked source/reviewer = %q/%q, want auto-trace/gitmoot-auto", events[0].Source, events[0].Reviewer)
	}
}

// TestTraceHarvestFullChainOffByDefault drives case (d): with
// auto_trace_enabled UNSET, daemonOutcomeHarvester returns nil through the SAME
// admission gate, so the engine constructs NO Outcome and writes ZERO auto-trace
// rows — yet the merge transition itself still happens (byte-identical default).
func TestTraceHarvestFullChainOffByDefault(t *testing.T) {
	ctx := context.Background()
	store := openTraceChainStore(t)
	version := installChainTemplate(t, store, "planner")
	gate := &stubMergeGate{decision: workflow.MergeDecision{Ready: true, Merged: true, MergeCommitSHA: "merge123"}}
	engine := chainEngine(store, gate)

	// Off-by-default: a config with [skillopt] present but auto_trace_enabled
	// absent (the same gate as no config at all). The harvester MUST be nil.
	root := writeHarvestConfig(t, "[skillopt]\n")
	if h := daemonOutcomeHarvester(store, harvestGitHubStub{status: realCIChainStatus()}, root); h != nil {
		t.Fatalf("off-by-default: harvester must be nil, got %T", h)
	}
	// Leave engine.OutcomeHarvester nil (what the daemon would wire here).

	seedChainImplementJob(t, store, version)
	seedChainApprovingReview(t, store, "head123")

	if err := engine.AdvanceJob(ctx, "review-job"); err != nil {
		t.Fatalf("AdvanceJob with off-by-default harvester returned error: %v", err)
	}

	// The merge still happened (the chain's default behavior is unchanged) ...
	if len(gate.requests) != 1 {
		t.Fatalf("merge gate requests = %d, want 1 (the merge still runs off-by-default)", len(gate.requests))
	}
	// ... and ZERO auto-trace rows were written.
	if events := autoTraceFeedback(t, store, version.ID); len(events) != 0 {
		t.Fatalf("off-by-default must write zero auto-trace rows, got %+v", events)
	}
	// The auto-trace run row must not exist either.
	if _, err := store.GetEvalRun(ctx, "auto-trace:"+version.ID); err == nil {
		t.Fatal("off-by-default must not create the auto-trace eval_run")
	}
}
