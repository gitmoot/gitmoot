package workflow

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1277. The skip-native-review-fanout intent must survive an engine-initiated
// hop. A coordinator dispatched with --skip-native-review-fanout that delegates
// its implement leg produced a child whose payload had the flag CLEARED, so the
// child's PR-open re-armed the native fanout and enlisted the implementer as its
// own reviewer (#1236). delegationRequest inherits many parent fields, including
// RiskTier and ActingOrgRole, but dropped this one.
func TestDelegationChildInheritsSkipNativeReviewFanout(t *testing.T) {
	store := openEngineStore(t)
	engine := testEngine(store)

	parent := db.Job{ID: "coordinator-job", Agent: "lead", Type: "ask"}
	payload := JobPayload{
		Repo:                   "gitmoot/gitmoot",
		Branch:                 "task-7",
		TaskID:                 "task-7",
		LeadAgent:              "lead",
		SkipNativeReviewFanout: true,
	}

	child := engine.delegationRequest(context.Background(), parent, payload, Delegation{
		ID:     "leg-1",
		Agent:  "impl",
		Action: "implement",
		Prompt: "do the work",
	})

	if !child.SkipNativeReviewFanout {
		t.Fatalf("delegation child dropped SkipNativeReviewFanout: %+v", child)
	}
}

// #1277, the escape g7-review reproduced against the first version of this fix.
// The branch lock cannot rescue a multi-generation tree, because at the moment
// the intent is dropped NO intent-bearing implement job has advanced yet, so
// nothing has published the intent onto the lock.
//
// The path: an intent-bearing coordinator delegates a child; the child's
// continuation is built by a constructor that does NOT set the flag (there are
// nine such constructors and none set it); that continuation then delegates the
// implement leg, which opens the PR and re-arms the fanout.
//
// Enqueue-time inheritance closes it for every constructor at once, which is why
// the continuation request below deliberately leaves SkipNativeReviewFanout
// unset — exactly as engine_continuation_synthesis.go builds it.
func TestIntentSurvivesContinuationThenImplementChild(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coordinator", []string{"ask", "review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "impl", []string{"implement"}, "gitmoot/gitmoot")
	mailbox := Mailbox{store: store}

	root, err := mailbox.Enqueue(ctx, JobRequest{
		ID:                     "root-coordinator",
		Agent:                  "coordinator",
		Action:                 "ask",
		Repo:                   "gitmoot/gitmoot",
		Branch:                 "task-9",
		TaskID:                 "task-9",
		Instructions:           "coordinate",
		SkipNativeReviewFanout: true,
	})
	if err != nil {
		t.Fatalf("Enqueue root returned error: %v", err)
	}

	// Generation 2: a continuation constructor that drops the flag.
	continuation, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "continuation-1",
		Agent:        "coordinator",
		Action:       "ask",
		Repo:         "gitmoot/gitmoot",
		Branch:       "task-9",
		TaskID:       "task-9",
		Instructions: "synthesize",
		ParentJobID:  root.ID,
	})
	if err != nil {
		t.Fatalf("Enqueue continuation returned error: %v", err)
	}
	continuationPayload, err := ParseJobPayload(continuation.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(continuation) returned error: %v", err)
	}
	if !continuationPayload.SkipNativeReviewFanout {
		t.Fatalf("continuation dropped the intent: %+v", continuationPayload)
	}

	// Generation 3: the implement child that would open the PR.
	child, err := mailbox.Enqueue(ctx, JobRequest{
		ID:           "implement-child",
		Agent:        "impl",
		Action:       "implement",
		Repo:         "gitmoot/gitmoot",
		Branch:       "task-9",
		TaskID:       "task-9",
		Instructions: "do the work",
		ParentJobID:  continuation.ID,
	})
	if err != nil {
		t.Fatalf("Enqueue implement child returned error: %v", err)
	}
	childPayload, err := ParseJobPayload(child.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(child) returned error: %v", err)
	}
	if !childPayload.SkipNativeReviewFanout {
		t.Fatalf("implement child after continuation re-armed native fanout: %+v", childPayload)
	}
}

// #1277 x #1236 — the MIXED-IDENTITY matrix guard.
//
// runtime_session_busy makes executor identity diverge from the implementer: a
// temporary worker runs the job while OriginalAgent is restored as LeadAgent.
// The roster filter must key on LeadAgent (the restored ORIGINAL implementer),
// never on Sender (the worker), or the real implementer reviews its own head.
// The branch-lock cells retired with branch locks (#2591).
func TestMixedIdentityFanoutIntentMatrix(t *testing.T) {
	const repo = "gitmoot/gitmoot"

	cases := []struct {
		name string
		// branch setup
		branch      string
		roster      []string
		payloadIntt bool
		pullRequest int
		assert      func(t *testing.T, ctx context.Context, store *db.Store, gate *fakeMergeGate)
	}{
		{
			// Mutant killed: SENDER_AS_IMPLEMENTER — filtering the roster by
			// event.Sender (the temporary worker) instead of event.LeadAgent (the
			// restored original implementer). The real implementer was then eligible
			// and got enlisted to review its OWN head.
			name:        "implementer exclusion: roster must key on the restored LeadAgent, not the worker",
			branch:      "task-m3",
			roster:      []string{"orig"},
			payloadIntt: false,
			pullRequest: 17,
			assert: func(t *testing.T, ctx context.Context, store *db.Store, gate *fakeMergeGate) {
				jobs, err := store.ListJobs(ctx)
				if err != nil {
					t.Fatalf("ListJobs returned error: %v", err)
				}
				for _, job := range jobs {
					if job.Type == "review" && job.Agent == "orig" {
						t.Fatalf("the original implementer was enlisted to review its own head via the worker's Sender: %+v", job)
					}
				}
				assertNoReviewJobs(t, ctx, store)
				// Roster filtered to empty must fail CLOSED, never reach the gate.
				if len(gate.requests) != 0 {
					t.Fatalf("all-ineligible roster reached the merge gate: %+v", gate.requests)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			// "orig" owns the branch and is the real implementer; it carries review
			// capability so that a filter keying on the WRONG identity would in fact
			// enlist it — otherwise cell 3's mutant could not be observed.
			seedAgent(t, store, "orig", []string{"implement", "review"}, repo)
			seedAgent(t, store, "temp-worker", []string{"implement"}, repo)
			engine := testEngine(store)
			engine.RequiredReviewers = tc.roster
			gate := &fakeMergeGate{decision: MergeDecision{Reason: PlainReason("ci is pending")}}
			engine.MergeGate = gate

			// Every cell advances a runtime_session_busy job: the DELEGATED worker
			// executes while "orig" owns the lock and is restored as LeadAgent.
			insertCompletedJob(t, store, db.Job{
				ID:    "matrix-" + tc.branch,
				Agent: "temp-worker",
				Type:  "implement",
			}, JobPayload{
				Repo:                   repo,
				Branch:                 tc.branch,
				PullRequest:            tc.pullRequest,
				HeadSHA:                "head-" + tc.branch,
				TaskID:                 tc.branch,
				LeadAgent:              "",
				DelegationReason:       "runtime_session_busy",
				DelegatedAgent:         "temp-worker",
				OriginalAgent:          "orig",
				SkipNativeReviewFanout: tc.payloadIntt,
				Result:                 &AgentResult{Decision: "implemented", Summary: "delegated worker advanced the branch"},
			})

			// Assert side effects before any advance error: a gate that runs can also
			// error, and a naive ordering would hide the real defect behind it.
			advanceErr := engine.AdvanceJob(ctx, "matrix-"+tc.branch)
			tc.assert(t, ctx, store, gate)
			if advanceErr != nil {
				t.Fatalf("AdvanceJob returned error: %v", advanceErr)
			}
		})
	}
}

func assertNoReviewJobs(t *testing.T, ctx context.Context, store *db.Store) {
	t.Helper()
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "review" {
			t.Fatalf("expected no review jobs, found %+v", job)
		}
	}
}
