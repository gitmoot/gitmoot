package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// #758 ENGINE EDGE: every tree-terminal path for a pipeline-orchestrate root must
// end in a settled, FOLDABLE tail (a finalize continuation), never a bare BlockedError
// that mints no continuation. A pipeline orchestrate stage job carries no task (empty
// taskRef), so the paths that normally e.block(ref) a coordinator — block_parent, and
// the vote/quorum synthesis gates — would strand the stage's chain with nothing for
// the advancer to fold. This file proves those paths route to enqueueFinalizeContinuation
// for an orchestrate root while staying byte-identical (a real block) for every other
// tree (the existing TestEngineDelegationFailurePolicyBlockParent /
// TestEngineDelegationSynthesisRuleVoteBlocksOnFailure are the non-orchestrate controls).

// TestPipelineOrchestrateRootBlockParentFinalizesInsteadOfBlocking is the primary
// engine-edge proof: a first-generation orchestrate coordinator (OrchestrateStage on
// its own payload, RootJobID = its own id, NO TaskID) whose only child fails under the
// default block_parent policy does NOT block — it mints a foldable finalize tail.
func TestPipelineOrchestrateRootBlockParentFinalizesInsteadOfBlocking(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// The stage job IS the sub-tree root: OrchestrateStage set, RootJobID = its own id,
	// and crucially NO TaskID — a pipeline orchestrate stage request sets none, so the
	// taskRef the block_parent path would act on is empty.
	insertCompletedJob(t, store, db.Job{ID: "stage-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:             "jerryfane/gitmoot",
		Sender:           "coord",
		RootJobID:        "stage-job",
		OrchestrateStage: true,
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			// Default (empty) failure policy == block_parent.
			Delegations: []Delegation{{ID: "api", Agent: "api", Action: "review", Prompt: "inspect"}},
		},
	})

	if err := engine.AdvanceJob(ctx, "stage-job"); err != nil {
		t.Fatalf("AdvanceJob(stage-job) fan-out: %v", err)
	}
	completeDelegationChild(t, store, "stage-job/delegation/api", JobFailed, AgentResult{Decision: "failed", Summary: "api broke"})

	// The block_parent path must NOT return a BlockedError for an orchestrate root: it
	// routes to the finalize continuation and returns nil.
	if err := engine.AdvanceJob(ctx, "stage-job/delegation/api"); err != nil {
		var blocked BlockedError
		if errors.As(err, &blocked) {
			t.Fatalf("orchestrate-root block_parent returned BlockedError %v — it must route to a foldable finalize tail instead", err)
		}
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}

	// A foldable tail exists at the deterministic continuation slot, carrying
	// DelegationFinalize (so the engine ignores any delegations it returns → terminal).
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("stage-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(finalize tail): %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("orchestrate-root block_parent tail must carry DelegationFinalize: %+v", cont)
	}
	if got := countJobEvents(t, store, "stage-job", "delegation_finalize_enqueued"); got != 1 {
		t.Fatalf("delegation_finalize_enqueued events = %d, want 1", got)
	}
	// The finalize occupies the continuation slot, so a re-advance never double-mints.
	if err := engine.AdvanceJob(ctx, "stage-job/delegation/api"); err != nil {
		if _, ok := err.(BlockedError); ok {
			t.Fatalf("re-advance of an orchestrate-root block_parent returned BlockedError: %v", err)
		}
	}
	if got := countJobEvents(t, store, "stage-job", "delegation_finalize_enqueued"); got != 1 {
		t.Fatalf("after re-advance: delegation_finalize_enqueued events = %d, want 1 (idempotent)", got)
	}
}

// TestPipelineOrchestrateContinuationBlockParentResolvesRootFlag exercises the
// root-resolution branch of isPipelineOrchestrateRoot: a LATER-generation coordinator
// (a continuation) does NOT copy OrchestrateStage onto its own payload, so the routing
// decision must resolve the tree ROOT (RootJobID) and read the flag there. A block_parent
// failure on a continuation coordinator must still finalize, not block.
func TestPipelineOrchestrateContinuationBlockParentResolvesRootFlag(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// The tree ROOT carries the flag; it need not carry a live result for this test —
	// isPipelineOrchestrateRoot only reads its OrchestrateStage flag.
	insertCompletedJob(t, store, db.Job{ID: "stage-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:             "jerryfane/gitmoot",
		Sender:           "coord",
		RootJobID:        "stage-job",
		OrchestrateStage: true,
		Result:           &AgentResult{Decision: "approved", Summary: "root"},
	})
	// A continuation generation: NO OrchestrateStage flag, RootJobID points at the root.
	insertCompletedJob(t, store, db.Job{ID: "stage-job/continuation", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Sender:    "coord",
		RootJobID: "stage-job",
		Result: &AgentResult{
			Decision:    "approved",
			Summary:     "next generation",
			Delegations: []Delegation{{ID: "api", Agent: "api", Action: "review", Prompt: "inspect"}},
		},
	})

	if err := engine.AdvanceJob(ctx, "stage-job/continuation"); err != nil {
		t.Fatalf("AdvanceJob(continuation) fan-out: %v", err)
	}
	completeDelegationChild(t, store, "stage-job/continuation/delegation/api", JobFailed, AgentResult{Decision: "failed", Summary: "api broke"})

	if err := engine.AdvanceJob(ctx, "stage-job/continuation/delegation/api"); err != nil {
		var blocked BlockedError
		if errors.As(err, &blocked) {
			t.Fatalf("continuation-gen block_parent returned BlockedError %v — the root flag must route it to finalize", err)
		}
		t.Fatalf("AdvanceJob(api) returned error: %v", err)
	}
	cont, err := unmarshalPayload(mustJob(t, store, "stage-job/continuation/continuation").Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(finalize tail): %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("continuation-gen block_parent tail must carry DelegationFinalize: %+v", cont)
	}
}

// TestPipelineOrchestrateRootVoteGateFinalizesInsteadOfBlocking proves the second
// coordinator-terminal e.block site — the vote synthesis gate in maybeEnqueueContinuation
// — is also routed to a foldable finalize tail for an orchestrate root.
func TestPipelineOrchestrateRootVoteGateFinalizesInsteadOfBlocking(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "jerryfane/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	insertCompletedJob(t, store, db.Job{ID: "stage-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:             "jerryfane/gitmoot",
		Sender:           "coord",
		RootJobID:        "stage-job",
		OrchestrateStage: true,
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "vote",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "continue", SynthesisRule: "vote"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui", FailurePolicy: "continue", SynthesisRule: "vote"},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "stage-job"); err != nil {
		t.Fatalf("AdvanceJob(stage-job): %v", err)
	}
	// api approves, ui fails: the vote is not unanimous → gate would block a normal tree.
	completeDelegationChild(t, store, "stage-job/delegation/api", JobSucceeded, AgentResult{Decision: "approved", Summary: "api ok"})
	if err := engine.AdvanceJob(ctx, "stage-job/delegation/api"); err != nil {
		t.Fatalf("AdvanceJob(api): %v", err)
	}
	completeDelegationChild(t, store, "stage-job/delegation/ui", JobFailed, AgentResult{Decision: "failed", Summary: "ui broke"})
	if err := engine.AdvanceJob(ctx, "stage-job/delegation/ui"); err != nil {
		var blocked BlockedError
		if errors.As(err, &blocked) {
			t.Fatalf("orchestrate-root vote gate returned BlockedError %v — it must route to a foldable finalize tail", err)
		}
		t.Fatalf("AdvanceJob(ui) returned error: %v", err)
	}
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("stage-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(finalize tail): %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("orchestrate-root vote-gate tail must carry DelegationFinalize: %+v", cont)
	}
}

// TestPipelineOrchestrateRootDispatchBlockFinalizesInsteadOfBlocking proves the
// DISPATCH-side terminal e.block sites are routed too: a coordinator that fans out
// an implement child whose worktree/branch-lock allocation blocks at dispatch time
// must mint a foldable finalize tail, not a bare BlockedError. Here the engine has no
// worktree manager (shared-checkout fallback) and the coordinator carries no branch,
// so allocateAndEnqueueDelegation's ensureBranchLock blocks ("branch is required") —
// exactly the one-shot dispatch path that, on an orchestrate root's empty taskRef,
// would strand the stage's chain with no continuation for the advancer to fold.
func TestPipelineOrchestrateRootDispatchBlockFinalizesInsteadOfBlocking(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "impl", []string{"implement"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// The stage job IS the sub-tree root (OrchestrateStage, RootJobID = own id, NO
	// TaskID and NO branch), fanning out a single implement child. With no
	// DelegationWorktrees manager the child takes the shared-checkout branch-lock path,
	// which blocks on the empty branch — a dispatch-time BlockedError on an empty ref.
	insertCompletedJob(t, store, db.Job{ID: "stage-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:             "jerryfane/gitmoot",
		Sender:           "coord",
		RootJobID:        "stage-job",
		OrchestrateStage: true,
		Result: &AgentResult{
			Decision:    "approved",
			Summary:     "fan out an implement leg",
			Delegations: []Delegation{{ID: "impl", Agent: "impl", Action: "implement", Prompt: "ship it"}},
		},
	})

	// The dispatch-path block must NOT return a BlockedError for an orchestrate root: it
	// routes to the finalize continuation and returns nil.
	if err := engine.AdvanceJob(ctx, "stage-job"); err != nil {
		var blocked BlockedError
		if errors.As(err, &blocked) {
			t.Fatalf("orchestrate-root dispatch block returned BlockedError %v — it must route to a foldable finalize tail instead", err)
		}
		t.Fatalf("AdvanceJob(stage-job) returned error: %v", err)
	}

	// A foldable tail exists at the deterministic continuation slot, carrying
	// DelegationFinalize (terminal), and no implement child was enqueued (all-or-nothing:
	// the loop stopped at the block).
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("stage-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(finalize tail): %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("orchestrate-root dispatch-block tail must carry DelegationFinalize: %+v", cont)
	}
	if jobExists(t, store, "stage-job/delegation/impl") {
		t.Fatalf("no implement child should be enqueued after the dispatch block minted the finalize tail")
	}
	if got := countJobEvents(t, store, "stage-job", "delegation_finalize_enqueued"); got != 1 {
		t.Fatalf("delegation_finalize_enqueued events = %d, want 1", got)
	}
	// Idempotent: re-advancing the coordinator never double-mints the finalize tail.
	if err := engine.AdvanceJob(ctx, "stage-job"); err != nil {
		var blocked BlockedError
		if errors.As(err, &blocked) {
			t.Fatalf("re-advance of an orchestrate-root dispatch block returned BlockedError: %v", err)
		}
	}
	if got := countJobEvents(t, store, "stage-job", "delegation_finalize_enqueued"); got != 1 {
		t.Fatalf("after re-advance: delegation_finalize_enqueued events = %d, want 1 (idempotent)", got)
	}
}
