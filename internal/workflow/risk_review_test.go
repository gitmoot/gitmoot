package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func highRiskEvent() PullRequestEvent {
	return PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		GoalID:            "goal-1",
		TaskID:            "task-7",
		TaskTitle:         "Auth change",
		LeadAgent:         "lead",
		Sender:            "lead",
		RequiredReviewers: []string{"audit", "sec"},
		ChangedPaths:      []string{"internal/auth/session.go"},
	}
}

// TestHighRiskPRFansOutLensReviewers pins the core acceptance behavior: an
// enabled + high-risk PR event fans out >= 2 lens review jobs (not the single
// native fan-out), seeds a review coordinator carrying synthesis_rule quorum, and
// records the resolved risk as an explainable job event.
func TestHighRiskPRFansOutLensReviewers(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "sec", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true

	if err := engine.HandlePullRequestOpened(ctx, highRiskEvent()); err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReviewing)

	coordID := "review-coordinator/task-7/review-1"
	coord := mustJob(t, store, coordID)
	if coord.Type != "review_coordinator" {
		t.Fatalf("coordinator type = %q, want review_coordinator", coord.Type)
	}
	if got := countJobEvents(t, store, coordID, "risk_tier_resolved"); got != 1 {
		t.Fatalf("risk_tier_resolved events = %d, want 1", got)
	}

	lensIDs := []string{
		coordID + "/delegation/" + LensCorrectness,
		coordID + "/delegation/" + LensSecurity,
	}
	n := 0
	for _, id := range lensIDs {
		if !jobExists(t, store, id) {
			t.Fatalf("expected lens review job %q to be enqueued", id)
		}
		job := mustJob(t, store, id)
		if job.Type != "review" || job.State != string(JobQueued) {
			t.Fatalf("lens job %q = type %q state %q, want queued review", id, job.Type, job.State)
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			t.Fatalf("unmarshal lens payload: %v", err)
		}
		if payload.SynthesisRule != "quorum" {
			t.Fatalf("lens %q synthesis_rule = %q, want quorum", id, payload.SynthesisRule)
		}
		if payload.RiskTier != RiskTierHigh {
			t.Fatalf("lens %q risk_tier = %q, want high", id, payload.RiskTier)
		}
		n++
	}
	if n < 2 {
		t.Fatalf("fanned out %d lens reviewers, want >= 2", n)
	}
	// The single-review job the routine path would have created must NOT exist.
	if jobExists(t, store, "review-audit-task-7-review-1") {
		t.Fatal("high-risk path must NOT enqueue the single native review job")
	}
}

// TestHighRiskCriticalRefutationBlocks is the deterministic no-LLM E2E: it drives
// a high-risk PR through fan-out and then lands one lens with a critical
// refutation (a `blocked` decision, a non-approving quorum outcome). The quorum
// gate must fail and block the shared task — the "blocks on a critical refutation"
// acceptance behavior — with no coordinator continuation enqueued.
func TestHighRiskCriticalRefutationBlocks(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "sec", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true

	if err := engine.HandlePullRequestOpened(ctx, highRiskEvent()); err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	coordID := "review-coordinator/task-7/review-1"
	correctnessID := coordID + "/delegation/" + LensCorrectness
	securityID := coordID + "/delegation/" + LensSecurity

	// Correctness lens approves (survives the lens).
	completeDelegationChild(t, store, correctnessID, JobSucceeded, AgentResult{
		Decision: "approved",
		Summary:  "no correctness defect",
	})
	if err := engine.AdvanceJob(ctx, correctnessID); err != nil {
		t.Fatalf("AdvanceJob(correctness) returned error: %v", err)
	}

	// Security lens reports a CRITICAL refutation via findings and blocks.
	critical, _ := json.Marshal(LensFinding{
		Lens: LensSecurity, Refuted: true, Severity: SeverityCritical, Confidence: 0.95, Evidence: "auth bypass at session.go:41",
	})
	blockDecision := SynthesizeLensDecision([]LensFinding{{Refuted: true, Severity: SeverityCritical}})
	completeDelegationChild(t, store, securityID, stateForDecision(blockDecision), AgentResult{
		Decision: blockDecision,
		Summary:  "critical auth bypass",
		Findings: []json.RawMessage{critical},
	})
	err := engine.AdvanceJob(ctx, securityID)

	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("AdvanceJob(security) error = %v, want BlockedError (quorum failed)", err)
	}
	if !strings.Contains(blocked.Reason, "quorum") {
		t.Fatalf("block reason = %q, want quorum-failure reason", blocked.Reason)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
	if jobExists(t, store, delegationContinuationID(coordID)) {
		t.Fatal("a failed quorum must NOT enqueue a coordinator continuation")
	}
}

// TestHighRiskAllLensesApproveSatisfiesQuorum pins the other side of the
// synthesis mapping: when every lens approves, the quorum is satisfied and the
// coordinator continuation is enqueued (the task is not blocked).
func TestHighRiskAllLensesApproveSatisfiesQuorum(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement", "ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "sec", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true

	if err := engine.HandlePullRequestOpened(ctx, highRiskEvent()); err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	coordID := "review-coordinator/task-7/review-1"
	for _, lens := range []string{LensCorrectness, LensSecurity} {
		id := coordID + "/delegation/" + lens
		completeDelegationChild(t, store, id, JobSucceeded, AgentResult{Decision: "approved", Summary: "clean"})
		if err := engine.AdvanceJob(ctx, id); err != nil {
			t.Fatalf("AdvanceJob(%s) returned error: %v", lens, err)
		}
	}
	if !jobExists(t, store, delegationContinuationID(coordID)) {
		t.Fatal("a satisfied quorum must enqueue the coordinator continuation")
	}
	if task, _ := store.GetTask(ctx, "task-7"); task.State == string(TaskBlocked) {
		t.Fatal("a satisfied quorum must not block the task")
	}
}

// TestHighRiskChangesRequestedLensFailsQuorum pins the #650 review fix: a lens
// that returns `changes_requested` (a valid "real issue, not fatal" reviewer
// decision that maps to a SUCCEEDED job state) must NOT count as an approving
// quorum vote. The quorum stays unmet and the shared task blocks — a
// changes-requested lens can never clear the high-risk gate on a succeeded-state
// short-circuit.
func TestHighRiskChangesRequestedLensFailsQuorum(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "sec", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true

	if err := engine.HandlePullRequestOpened(ctx, highRiskEvent()); err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	coordID := "review-coordinator/task-7/review-1"
	correctnessID := coordID + "/delegation/" + LensCorrectness
	securityID := coordID + "/delegation/" + LensSecurity

	// Correctness approves.
	completeDelegationChild(t, store, correctnessID, JobSucceeded, AgentResult{Decision: "approved", Summary: "ok"})
	if err := engine.AdvanceJob(ctx, correctnessID); err != nil {
		t.Fatalf("AdvanceJob(correctness) returned error: %v", err)
	}
	// Security asks for changes (a SUCCEEDED job state per stateForDecision).
	completeDelegationChild(t, store, securityID, stateForDecision("changes_requested"), AgentResult{
		Decision: "changes_requested",
		Summary:  "please add input validation",
	})
	err := engine.AdvanceJob(ctx, securityID)

	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("AdvanceJob(security) error = %v, want BlockedError (quorum unmet)", err)
	}
	if !strings.Contains(blocked.Reason, "quorum") {
		t.Fatalf("block reason = %q, want quorum-failure reason", blocked.Reason)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
	if jobExists(t, store, delegationContinuationID(coordID)) {
		t.Fatal("an unmet quorum must NOT enqueue a coordinator continuation")
	}
}

// TestHighRiskCriticalFindingNotPreNormalizedBlocks pins that a lens which reports
// a CRITICAL refutation in AgentResult.Findings but leaves its OWN decision at
// `approved` still fails the quorum: the engine wires SynthesizeLensDecision into
// the lens completion path and normalizes the approving decision to `blocked`, so
// a critical finding can never be rubber-stamped by a reviewer that forgot to
// self-normalize.
func TestHighRiskCriticalFindingNotPreNormalizedBlocks(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "sec", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true

	if err := engine.HandlePullRequestOpened(ctx, highRiskEvent()); err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	coordID := "review-coordinator/task-7/review-1"
	correctnessID := coordID + "/delegation/" + LensCorrectness
	securityID := coordID + "/delegation/" + LensSecurity

	completeDelegationChild(t, store, correctnessID, JobSucceeded, AgentResult{Decision: "approved", Summary: "ok"})
	if err := engine.AdvanceJob(ctx, correctnessID); err != nil {
		t.Fatalf("AdvanceJob(correctness) returned error: %v", err)
	}
	// Security leaves its decision at APPROVED but reports a critical refutation.
	critical, _ := json.Marshal(LensFinding{
		Lens: LensSecurity, Refuted: true, Severity: SeverityCritical, Confidence: 0.9, Evidence: "auth bypass at session.go:41",
	})
	completeDelegationChild(t, store, securityID, JobSucceeded, AgentResult{
		Decision: "approved",
		Summary:  "looks fine",
		Findings: []json.RawMessage{critical},
	})
	err := engine.AdvanceJob(ctx, securityID)

	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("AdvanceJob(security) error = %v, want BlockedError (critical refutation normalized)", err)
	}
	assertTaskState(t, store, "task-7", TaskBlocked)
	// The lens decision was normalized to blocked and recorded as an explainable event.
	normalized := mustJob(t, store, securityID)
	np, err := unmarshalPayload(normalized.Payload)
	if err != nil {
		t.Fatalf("unmarshal security payload: %v", err)
	}
	if np.Result == nil || np.Result.Decision != "blocked" {
		t.Fatalf("normalized security decision = %v, want blocked", np.Result)
	}
	if got := countJobEvents(t, store, securityID, "lens_critical_refutation"); got != 1 {
		t.Fatalf("lens_critical_refutation events = %d, want 1", got)
	}
	if jobExists(t, store, delegationContinuationID(coordID)) {
		t.Fatal("a critical refutation must NOT enqueue a coordinator continuation")
	}
}

// TestHighRiskAllApproveReachesMergeWithoutAskCapability pins the #650 review fix
// for the synthesis continuation: an all-approved high-risk review must reach
// TaskReadyToMerge even when the LEAD agent carries NO `ask` capability, and the
// synthesis-only coordinator continuation must be allowed at dispatch instead of
// blocking the already-approved task. A high-risk review must not impose a
// non-additive `ask` grant on a normal lead.
func TestHighRiskAllApproveReachesMergeWithoutAskCapability(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	// Lead has implement + review but deliberately NOT `ask`.
	seedAgent(t, store, "lead", []string{"implement", "review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "sec", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true

	if err := engine.HandlePullRequestOpened(ctx, highRiskEvent()); err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	coordID := "review-coordinator/task-7/review-1"
	for _, lens := range []string{LensCorrectness, LensSecurity} {
		id := coordID + "/delegation/" + lens
		completeDelegationChild(t, store, id, JobSucceeded, AgentResult{Decision: "approved", Summary: "clean"})
		if err := engine.AdvanceJob(ctx, id); err != nil {
			t.Fatalf("AdvanceJob(%s) returned error: %v", lens, err)
		}
	}
	// The native merge gate (MergeGate nil) advances the fully-approved review.
	assertTaskState(t, store, "task-7", TaskReadyToMerge)

	// The synthesis continuation was enqueued on the lead; its dispatch preflight
	// must NOT block despite the lead lacking `ask`.
	contID := delegationContinuationID(coordID)
	cont := mustJob(t, store, contID)
	if cont.Type != "ask" || cont.Agent != "lead" {
		t.Fatalf("continuation = type %q agent %q, want ask/lead", cont.Type, cont.Agent)
	}
	cp, err := unmarshalPayload(cont.Payload)
	if err != nil {
		t.Fatalf("unmarshal continuation payload: %v", err)
	}
	if cp.RiskTier != RiskTierHigh {
		t.Fatalf("continuation risk_tier = %q, want high", cp.RiskTier)
	}
	if err := engine.ensureJobExecutorAllowed(ctx, cont, cp, taskRefFromPayload(cp)); err != nil {
		t.Fatalf("risk-tier synthesis continuation must not block on missing ask capability: %v", err)
	}
	assertTaskState(t, store, "task-7", TaskReadyToMerge)

	// Control: a plain `ask` job on the same lead (no risk tier) DOES block, proving
	// the exemption is what unblocks the synthesis continuation.
	plainPayload := cp
	plainPayload.RiskTier = ""
	plainJob := cont
	plainJob.ID = "plain-ask/task-7"
	err = engine.ensureJobExecutorAllowed(ctx, plainJob, plainPayload, taskRefFromPayload(plainPayload))
	var plainBlocked BlockedError
	if !errors.As(err, &plainBlocked) {
		t.Fatalf("a non-risk ask on a lead lacking ask must block; got %v", err)
	}
}

// TestHighRiskSeamErrorDefersInsteadOfRoutine pins the #650 review fix for a
// transient classification failure: when risk tiers are enabled and the
// PullRequestSignals seam ERRORS (signals unknown), the engine must NOT fall
// through to the routine single-review fan-out (which a later high classification
// could no longer supersede) — it defers to the next poll. A subsequent poll whose
// seam resolves `high` then dispatches the lens quorum cleanly, with no stray
// routine review job coexisting on the round.
func TestHighRiskSeamErrorDefersInsteadOfRoutine(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "sec", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true

	fail := true
	engine.PullRequestSignals = func(context.Context, string, int) ([]string, []string, error) {
		if fail {
			return nil, nil, errors.New("transient GitHub error")
		}
		return nil, []string{"internal/auth/session.go"}, nil
	}
	event := PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		TaskID:            "task-7",
		TaskTitle:         "Auth change",
		LeadAgent:         "lead",
		Sender:            "lead",
		RequiredReviewers: []string{"audit", "sec"},
		// No event signals -> the seam is consulted (and errors on poll #1).
	}

	// Poll #1: seam errors -> defer. Neither a routine single review job nor a
	// high-risk coordinator may exist.
	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("HandlePullRequestOpened (poll 1) returned error: %v", err)
	}
	if jobExists(t, store, "review-audit-task-7-review-1") || jobExists(t, store, "review-sec-task-7-review-1") {
		t.Fatal("a deferred classification must NOT enqueue the routine single review job")
	}
	if jobExists(t, store, "review-coordinator/task-7/review-1") {
		t.Fatal("a deferred classification must NOT dispatch the high-risk coordinator")
	}

	// Poll #2: seam recovers and resolves high -> clean lens fan-out on review-1.
	fail = false
	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("HandlePullRequestOpened (poll 2) returned error: %v", err)
	}
	if !jobExists(t, store, "review-coordinator/task-7/review-1") {
		t.Fatal("the recovered high classification must dispatch the coordinator")
	}
	if jobExists(t, store, "review-audit-task-7-review-1") || jobExists(t, store, "review-sec-task-7-review-1") {
		t.Fatal("no routine single review job may coexist with the lens quorum")
	}
}

// TestPullRequestSignalsSeamClassifiesInProcess pins the in-process trigger path:
// when an event carries no risk signals, the engine resolves them through the
// best-effort PullRequestSignals seam and classifies from those.
func TestPullRequestSignalsSeamClassifiesInProcess(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "sec", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true
	called := 0
	engine.PullRequestSignals = func(_ context.Context, repo string, number int) ([]string, []string, error) {
		called++
		if repo != "jerryfane/gitmoot" || number != 7 {
			t.Fatalf("seam called with repo=%q number=%d", repo, number)
		}
		return nil, []string{"internal/auth/session.go"}, nil
	}

	event := PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		TaskID:            "task-7",
		TaskTitle:         "Auth change",
		LeadAgent:         "lead",
		Sender:            "lead",
		RequiredReviewers: []string{"audit", "sec"},
		// No Labels/ChangedPaths on the event -> the seam supplies them.
	}
	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	if called != 1 {
		t.Fatalf("PullRequestSignals called %d times, want 1", called)
	}
	if !jobExists(t, store, "review-coordinator/task-7/review-1") {
		t.Fatal("seam-resolved high-risk path must create the coordinator")
	}
}

func TestPullRequestSignalsSeamSkippedWhenEventCarriesSignals(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true
	engine.PullRequestSignals = func(context.Context, string, int) ([]string, []string, error) {
		t.Fatal("seam must not be consulted when the event already carries signals")
		return nil, nil, nil
	}
	event := PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		TaskID:            "task-7",
		TaskTitle:         "Docs",
		LeadAgent:         "lead",
		Sender:            "lead",
		RequiredReviewers: []string{"audit"},
		ChangedPaths:      []string{"README.md"},
	}
	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	// Event signals are routine -> single review path, no coordinator.
	if jobExists(t, store, "review-coordinator/task-7/review-1") {
		t.Fatal("routine event signals must not create a coordinator")
	}
}

// TestRiskTiersOffIsByteIdentical is the invariant test: with risk tiers OFF, a
// PR whose changed paths WOULD classify high still takes the single native
// review fan-out, byte-for-byte identical to a control with no risk signals. No
// coordinator is created and the payloads match exactly.
func TestRiskTiersOffIsByteIdentical(t *testing.T) {
	run := func(t *testing.T, enabled bool, withSignals bool) string {
		store := openEngineStore(t)
		seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
		seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
		engine := testEngine(store)
		engine.RiskTiersEnabled = enabled
		event := PullRequestEvent{
			Repo:              "jerryfane/gitmoot",
			Branch:            "task-7",
			PullRequest:       7,
			GoalID:            "goal-1",
			TaskID:            "task-7",
			TaskTitle:         "Workflow Engine",
			LeadAgent:         "lead",
			Sender:            "lead",
			RequiredReviewers: []string{"audit"},
		}
		if withSignals {
			event.ChangedPaths = []string{"internal/auth/session.go"}
			event.Labels = []string{"risk:high"}
		}
		if err := engine.HandlePullRequestOpened(context.Background(), event); err != nil {
			t.Fatalf("HandlePullRequestOpened returned error: %v", err)
		}
		// The single native review job must exist; no coordinator.
		if jobExists(t, store, "review-coordinator/task-7/review-1") {
			t.Fatal("risk-tiers-off path must not create a coordinator")
		}
		job := mustJob(t, store, "review-audit-task-7-review-1")
		return job.Payload
	}

	// Control: risk tiers off, no signals.
	control := run(t, false, false)
	// Off but signals present on the event (the daemon never populates them when
	// off, but assert defensively that the engine ignores them).
	offWithSignals := run(t, false, true)
	if control != offWithSignals {
		t.Fatalf("risk-tiers-off payload changed when signals were present:\n control=%s\n signals=%s", control, offWithSignals)
	}
}

// TestRiskTiersEnabledRoutineTakesSingleReviewPath pins that an ENABLED engine on
// a ROUTINE-classified PR still uses the unchanged single-review fan-out.
func TestRiskTiersEnabledRoutineTakesSingleReviewPath(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true

	err := engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo:              "jerryfane/gitmoot",
		Branch:            "task-7",
		PullRequest:       7,
		TaskID:            "task-7",
		TaskTitle:         "Docs tweak",
		LeadAgent:         "lead",
		Sender:            "lead",
		RequiredReviewers: []string{"audit"},
		ChangedPaths:      []string{"README.md", "docs/guide.md"},
	})
	if err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}
	if jobExists(t, store, "review-coordinator/task-7/review-1") {
		t.Fatal("routine tier must not create a coordinator")
	}
	if !jobExists(t, store, "review-audit-task-7-review-1") {
		t.Fatal("routine tier must take the single native review path")
	}
}
