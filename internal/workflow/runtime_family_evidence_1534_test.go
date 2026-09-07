package workflow

import (
	"context"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1534: runtime-family attribution depended on a MUTABLE payload field. With
// `effective_runtime` absent, `ResolveRuntimeFamily` fell back to the agent's
// REGISTRY DEFAULT, so an execution was attributed to the runtime the agent
// usually runs rather than the one that ran.
//
// MEASURED ON THE LIVE STORE, and this is the number that makes it a defect
// rather than a theoretical gap: 56 review and implement jobs carry a
// `runtime_override` event whose runtime DIFFERS from their agent's registered
// default while their payload field is absent. Verified by eye on a sample:
// agent `lead` is registered `claude` and those jobs ran on `codex` or `shell`.
// Separately, 1,813 jobs (1646 ask, 119 implement, 48 review) carry a runtime
// event and no payload field at all.
//
// This compounds directly with #1531, merged earlier as 5ee96763: if `lead`
// implemented on codex and a codex reviewer approved, the family check would
// compare codex-reviewer against claude-implementer, find them different, and
// PASS a same-family approval.
//
// The evidence is `job_events.runtime`, a column and not the message. The event
// message is prose ("job runs on runtime shell (agent default codex); session
// lock ..."), and #1534 forbids resolving a family by parsing it.
func TestRuntimeFamilyPrefersAppendOnlyEvidenceOverTheRegistryDefault(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	// The measured shape: registered claude, actually ran codex, payload empty.
	if err := store.UpsertAgent(ctx, db.Agent{Name: "lead", Runtime: "claude", Capabilities: []string{"implement"}}); err != nil {
		t.Fatal(err)
	}
	insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "lead", Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 9,
	})
	if err := store.AddJobEvent(ctx, db.JobEvent{
		JobID:   "implement-job",
		Kind:    "runtime_override",
		Message: "job runs on runtime codex (agent default claude); session lock runtime:codex:abc",
		Runtime: "codex",
	}); err != nil {
		t.Fatal(err)
	}

	family, ok, err := ResolveRuntimeFamily(ctx, store, "implement-job", "lead", "")
	if err != nil {
		t.Fatalf("ResolveRuntimeFamily: %v", err)
	}
	if !ok {
		t.Fatal("family unresolved even though the job's own append-only runtime evidence names it")
	}
	if family != "codex" {
		t.Fatalf("family = %q, want %q; the registry default claude is what the agent USUALLY runs, not what ran", family, "codex")
	}
}

// Four bounds. The precedence and the fallbacks all have to keep working, or
// this trades one misattribution for another.
func TestRuntimeFamilyEvidencePrecedenceIsBounded(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertAgent(ctx, db.Agent{Name: "lead", Runtime: "claude", Capabilities: []string{"implement"}}); err != nil {
		t.Fatal(err)
	}
	insertCompletedJob(t, store, db.Job{ID: "job-with-event", Agent: "lead", Type: "implement"}, JobPayload{Repo: "r", PullRequest: 1})
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job-with-event", Kind: "runtime_override", Message: "prose", Runtime: "codex"}); err != nil {
		t.Fatal(err)
	}

	// 1. THE PAYLOAD FIELD STILL WINS when present. It is written by the same
	//    run-phase step as the event, so they agree, and reading it first costs
	//    no query. Reordering these two would be a behaviour change with no
	//    measured benefit.
	family, ok, err := ResolveRuntimeFamily(ctx, store, "job-with-event", "lead", "kimi")
	if err != nil || !ok {
		t.Fatalf("payload-first resolution failed: family=%q ok=%v err=%v", family, ok, err)
	}
	if family != "kimi" {
		t.Fatalf("family = %q, want the payload's %q", family, "kimi")
	}

	// 2. NO JOB ID, NO EVENT TIER. The pre-dispatch caller has no job yet and
	//    must keep resolving from the registry exactly as before.
	family, ok, err = ResolveRuntimeFamily(ctx, store, "", "lead", "")
	if err != nil || !ok || family != "claude" {
		t.Fatalf("pre-dispatch resolution = (%q, %v, %v), want (claude, true, nil)", family, ok, err)
	}

	// 3. NO EVIDENCE ANYWHERE STILL FAILS CLOSED. The resolver's contract is that
	//    an unresolvable family is reported as such, never guessed, because a
	//    caller protecting a safety property must be able to see the difference.
	family, ok, err = ResolveRuntimeFamily(ctx, store, "job-with-event", "unregistered-agent", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatalf("family %q was invented for an agent with no evidence and no registration", family)
	}

	// 4. A JOB WITH NO RUNTIME EVENT falls through to the registry rather than
	//    failing, so the append-only tier adds a source and removes none.
	insertCompletedJob(t, store, db.Job{ID: "job-no-event", Agent: "lead", Type: "implement"}, JobPayload{Repo: "r", PullRequest: 2})
	family, ok, err = ResolveRuntimeFamily(ctx, store, "job-no-event", "lead", "")
	if err != nil || !ok || family != "claude" {
		t.Fatalf("eventless resolution = (%q, %v, %v), want (claude, true, nil)", family, ok, err)
	}
}

// The evidence must be readable through the LATEST selection, because a job can
// be re-dispatched inside its lifecycle and the last runtime is the one that ran.
func TestRuntimeFamilyEvidenceUsesTheLatestRecordedSelection(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertAgent(ctx, db.Agent{Name: "lead", Runtime: "claude", Capabilities: []string{"implement"}}); err != nil {
		t.Fatal(err)
	}
	insertCompletedJob(t, store, db.Job{ID: "job", Agent: "lead", Type: "implement"}, JobPayload{Repo: "r", PullRequest: 3})
	for _, recorded := range []string{"codex", "shell"} {
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job", Kind: "runtime_override", Message: "prose", Runtime: recorded}); err != nil {
			t.Fatal(err)
		}
	}
	// An event with NO runtime must not shadow the ones that carry it.
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "job", Kind: "running", Message: "started"}); err != nil {
		t.Fatal(err)
	}

	family, ok, err := ResolveRuntimeFamily(ctx, store, "job", "lead", "")
	if err != nil || !ok {
		t.Fatalf("resolution failed: %v %v", ok, err)
	}
	if family != "shell" {
		t.Fatalf("family = %q, want the latest recorded %q", family, "shell")
	}
}
