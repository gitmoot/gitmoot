package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// distillController builds a controller with distill-at-terminal ON. enrolled
// lists the agents opted into the read path (memController's set); allJobs mirrors
// [memory].distill_all_jobs.
func distillController(store *db.Store, maxPerJob int, allJobs bool, enrolled ...string) *MemoryController {
	set := map[string]bool{}
	for _, n := range enrolled {
		set[n] = true
	}
	return &MemoryController{
		Store:             store,
		Enabled:           func(name string) bool { return set[name] },
		TokenBudget:       1500,
		MaxEntries:        15,
		DistillAtTerminal: true,
		DistillMaxPerJob:  maxPerJob,
		DistillAllJobs:    allJobs,
	}
}

func distillObsFor(t *testing.T, store *db.Store, repo string) []db.MemoryObservation {
	t.Helper()
	obs, err := store.ListMemoryObservations(context.Background(), "audit", repo)
	if err != nil {
		t.Fatalf("ListMemoryObservations: %v", err)
	}
	return obs
}

func staged(obs []db.MemoryObservation) []db.MemoryObservation {
	var out []db.MemoryObservation
	for _, o := range obs {
		if strings.HasPrefix(o.Provenance, "distill:") {
			out = append(out, o)
		}
	}
	return out
}

func witnesses(obs []db.MemoryObservation) []db.MemoryObservation {
	var out []db.MemoryObservation
	for _, o := range obs {
		if strings.HasPrefix(o.Provenance, "distill-seen:") {
			out = append(out, o)
		}
	}
	return out
}

// TestDistillOffByDefaultNoRows proves that with distill OFF (the default) a
// FAILED terminal carrying failing tests AND a named error stages NOTHING — no
// observation rows, no confirmed rows — so the terminal path is byte-identical.
func TestDistillOffByDefaultNoRows(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	// memController leaves DistillAtTerminal false.
	ctrl := memController(store, 1500, 15, "audit")
	ctrl.record(ctx, "job-1", memAgent(), "implement",
		JobPayload{Repo: "acme/widget"},
		AgentResult{Decision: "failed", Summary: "panic: runtime error: index out of range",
			TestsRun: []string{"TestPaymentFlow"}})

	if obs := distillObsFor(t, store, "acme/widget"); len(obs) != 0 {
		t.Fatalf("distill off must write no observations, got %+v", obs)
	}
	confirmed, _ := store.ListConfirmedMemories(ctx, "audit", "acme/widget")
	if len(confirmed) != 0 {
		t.Fatalf("distill off must write no confirmed rows, got %+v", confirmed)
	}
}

// TestDistillNeverConfirmedAlwaysLowTrust proves the OUTPUT DISCIPLINE: distilled
// rows are PENDING observations, trust=low, provenance "distill:*" — never
// confirmed memory. Uses two jobs so the recurrence gate lets the second stage.
func TestDistillNeverConfirmedAlwaysLowTrust(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ctrl := distillController(store, 3, false, "audit")
	res := AgentResult{Decision: "failed", Summary: "", TestsRun: []string{"TestPaymentFlow"}}
	ctrl.record(ctx, "job-1", memAgent(), "implement", JobPayload{Repo: "acme/widget"}, res)
	ctrl.record(ctx, "job-2", memAgent(), "implement", JobPayload{Repo: "acme/widget"}, res)

	st := staged(distillObsFor(t, store, "acme/widget"))
	if len(st) != 1 {
		t.Fatalf("expected exactly one staged distilled observation, got %+v", st)
	}
	o := st[0]
	if o.Key != "distill-test:testpaymentflow" {
		t.Fatalf("staged key = %q, want distill-test:testpaymentflow", o.Key)
	}
	if o.TrustMark != "low" {
		t.Fatalf("staged trust = %q, want low", o.TrustMark)
	}
	if o.Provenance != "distill:job-2" {
		t.Fatalf("staged provenance = %q, want distill:job-2", o.Provenance)
	}
	// NEVER confirmed.
	confirmed, _ := store.ListConfirmedMemories(ctx, "audit", "acme/widget")
	if len(confirmed) != 0 {
		t.Fatalf("distill must never write confirmed memory, got %+v", confirmed)
	}
}

// TestDistillRecurrenceGate proves gpt-5.5's rule: a one-off anomalous failure
// does NOT stage — the first sighting records only a low-trust witness; the actual
// staged observation appears only on the second (recurring) sighting across jobs.
func TestDistillRecurrenceGate(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ctrl := distillController(store, 3, false, "audit")
	res := AgentResult{Decision: "failed", TestsRun: []string{"TestCheckoutRetry"}}

	// --- Job 1: FIRST sighting → witness only, nothing staged.
	ctrl.record(ctx, "job-1", memAgent(), "implement", JobPayload{Repo: "acme/widget"}, res)
	obs := distillObsFor(t, store, "acme/widget")
	if len(staged(obs)) != 0 {
		t.Fatalf("first sighting must stage nothing, got %+v", staged(obs))
	}
	w := witnesses(obs)
	if len(w) != 1 {
		t.Fatalf("first sighting must record exactly one witness, got %+v", w)
	}
	if w[0].TrustMark != "low" || w[0].Provenance != "distill-seen:job-1" {
		t.Fatalf("witness trust/provenance = %q/%q, want low/distill-seen:job-1", w[0].TrustMark, w[0].Provenance)
	}
	if w[0].Content == "" || strings.Contains(w[0].Content, "TestCheckoutRetry") {
		t.Fatalf("witness content should be the fixed sentinel, got %q", w[0].Content)
	}

	// --- Job 2: recurrence → the real observation stages.
	ctrl.record(ctx, "job-2", memAgent(), "implement", JobPayload{Repo: "acme/widget"}, res)
	st := staged(distillObsFor(t, store, "acme/widget"))
	if len(st) != 1 {
		t.Fatalf("second sighting must stage exactly one observation, got %+v", st)
	}
	if st[0].Key != "distill-test:testcheckoutretry" {
		t.Fatalf("staged key = %q", st[0].Key)
	}
}

// TestDistillDedupOnRepeat proves a THIRD recurrence does not stage a second copy:
// content-hash dedup collapses the repeat, so at most one staged row per key.
func TestDistillDedupOnRepeat(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ctrl := distillController(store, 3, false, "audit")
	res := AgentResult{Decision: "failed", TestsRun: []string{"TestCheckoutRetry"}}
	for _, jobID := range []string{"job-1", "job-2", "job-3", "job-4"} {
		ctrl.record(ctx, jobID, memAgent(), "implement", JobPayload{Repo: "acme/widget"}, res)
	}
	obs := distillObsFor(t, store, "acme/widget")
	if got := len(staged(obs)); got != 1 {
		t.Fatalf("dedup should keep exactly one staged row across repeats, got %d: %+v", got, staged(obs))
	}
	if got := len(witnesses(obs)); got != 1 {
		t.Fatalf("exactly one witness should ever exist per key, got %d", got)
	}
}

// TestDistillNamedError proves the named-error producer extracts a stable,
// normalized error token from result.Summary and stages it on recurrence, with
// volatile parts (addresses/numbers) stripped so the key is closed-category.
func TestDistillNamedError(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ctrl := distillController(store, 3, false, "audit")
	// Two jobs whose summaries differ ONLY in the volatile address/index, so the
	// normalized key must be identical and the second must count as a recurrence.
	ctrl.record(ctx, "job-1", memAgent(), "implement", JobPayload{Repo: "acme/widget"},
		AgentResult{Decision: "failed", Summary: "panic: nil pointer dereference at 0xdeadbeef index 3"})
	ctrl.record(ctx, "job-2", memAgent(), "implement", JobPayload{Repo: "acme/widget"},
		AgentResult{Decision: "failed", Summary: "panic: nil pointer dereference at 0xcafef00d index 9"})

	st := staged(distillObsFor(t, store, "acme/widget"))
	if len(st) != 1 {
		t.Fatalf("named error should stage exactly one row after recurrence, got %+v", st)
	}
	if !strings.HasPrefix(st[0].Key, "distill-error:") {
		t.Fatalf("named-error key = %q, want distill-error: prefix", st[0].Key)
	}
	if strings.Contains(st[0].Key, "deadbeef") || strings.Contains(st[0].Key, "0x") {
		t.Fatalf("named-error key retained a volatile token: %q", st[0].Key)
	}
	if st[0].TrustMark != "low" || !strings.HasPrefix(st[0].Provenance, "distill:") {
		t.Fatalf("named-error trust/provenance = %q/%q", st[0].TrustMark, st[0].Provenance)
	}
}

// TestDistillNamedErrorFromRawOutputSkipsEnvelope proves the named-error producer
// mines a genuine short error line from the raw output tail, but SKIPS the
// structured gitmoot_result JSON envelope (long, contains gitmoot_result) so a
// minified result brick never becomes a distilled "error".
func TestDistillNamedErrorFromRawOutputSkipsEnvelope(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ctrl := distillController(store, 5, false, "audit")
	envelope := `{"gitmoot_result":{"decision":"failed","summary":"see log","tests_run":[],"needs":[]}}`
	raw := "running suite\npanic: connection refused\n" + envelope
	res := AgentResult{Decision: "failed", Summary: "see log"}
	payload := JobPayload{Repo: "acme/widget", RawOutputs: []string{raw}}
	ctrl.record(ctx, "job-1", memAgent(), "implement", payload, res)
	ctrl.record(ctx, "job-2", memAgent(), "implement", payload, res)

	st := staged(distillObsFor(t, store, "acme/widget"))
	if len(st) != 1 {
		t.Fatalf("expected exactly one staged error (the panic line), got %+v", st)
	}
	if !strings.Contains(st[0].Key, "connection-refused") {
		t.Fatalf("staged key should come from the genuine error line, got %q", st[0].Key)
	}
	for _, o := range st {
		if strings.Contains(o.Key, "gitmoot") || strings.Contains(o.Content, "gitmoot_result") {
			t.Fatalf("result envelope was mined as an error: %+v", o)
		}
	}
}

// TestDistillPreFilterRejects proves a directive/secret-shaped error line is
// dropped by PreFilter BEFORE it can even be witnessed — no rows at all.
func TestDistillPreFilterRejects(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ctrl := distillController(store, 3, false, "audit")
	// "you must always" makes the cleaned error line directive-shaped.
	res := AgentResult{Decision: "failed", Summary: "error: you must always rebase before pushing to this repo"}
	ctrl.record(ctx, "job-1", memAgent(), "implement", JobPayload{Repo: "acme/widget"}, res)
	ctrl.record(ctx, "job-2", memAgent(), "implement", JobPayload{Repo: "acme/widget"}, res)
	if obs := distillObsFor(t, store, "acme/widget"); len(obs) != 0 {
		t.Fatalf("PreFilter must reject a directive-shaped error line entirely, got %+v", obs)
	}
}

// TestDistillPerJobCap proves the hard per-job cap bounds distill writes: a single
// job carrying more distinct signals than the cap writes exactly cap rows.
func TestDistillPerJobCap(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	ctrl := distillController(store, 2, false, "audit")
	res := AgentResult{Decision: "failed", TestsRun: []string{"TestA", "TestB", "TestC", "TestD"}}
	ctrl.record(ctx, "job-1", memAgent(), "implement", JobPayload{Repo: "acme/widget"}, res)
	if got := len(distillObsFor(t, store, "acme/widget")); got != 2 {
		t.Fatalf("per-job cap=2 should bound distill to 2 rows, got %d", got)
	}
}

// TestDistillEnrolledOnlyVsAllJobs proves the scoping: with distill_all_jobs=false
// an UN-enrolled agent distills nothing; with distill_all_jobs=true the SAME
// un-enrolled agent distills box-wide.
func TestDistillEnrolledOnlyVsAllJobs(t *testing.T) {
	ctx := context.Background()
	res := AgentResult{Decision: "failed", TestsRun: []string{"TestPaymentFlow"}}

	// distill_all_jobs=false, nobody enrolled → no distill.
	storeA := openTestStore(t)
	ctrlA := distillController(storeA, 3, false /* nobody enrolled */)
	ctrlA.record(ctx, "job-1", memAgent(), "implement", JobPayload{Repo: "acme/widget"}, res)
	if obs := distillObsFor(t, storeA, "acme/widget"); len(obs) != 0 {
		t.Fatalf("un-enrolled agent with distill_all_jobs=false must distill nothing, got %+v", obs)
	}

	// distill_all_jobs=true, still nobody enrolled → distill fires (witness).
	storeB := openTestStore(t)
	ctrlB := distillController(storeB, 3, true /* nobody enrolled, allJobs */)
	ctrlB.record(ctx, "job-1", memAgent(), "implement", JobPayload{Repo: "acme/widget"}, res)
	obs := distillObsFor(t, storeB, "acme/widget")
	if len(witnesses(obs)) != 1 {
		t.Fatalf("distill_all_jobs=true must distill for an un-enrolled agent, got %+v", obs)
	}
}

// TestDistillOnlyOnNotableDecisions proves distill fires only on the anomalous
// terminal decisions (failed/blocked/changes_requested); a routine success with
// the same test list stages nothing.
func TestDistillOnlyOnNotableDecisions(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		decision string
		want     int // expected total distill rows (witness on first sighting)
	}{
		{"failed", 1},
		{"blocked", 1},
		{"changes_requested", 1},
		{"approved", 0},
		{"implemented", 0},
	} {
		t.Run(tc.decision, func(t *testing.T) {
			store := openTestStore(t)
			ctrl := distillController(store, 3, false, "audit")
			ctrl.record(ctx, "job-1", memAgent(), "implement", JobPayload{Repo: "acme/widget"},
				AgentResult{Decision: tc.decision, TestsRun: []string{"TestPaymentFlow"}})
			if got := len(distillObsFor(t, store, "acme/widget")); got != tc.want {
				t.Fatalf("decision %q: distill rows = %d, want %d", tc.decision, got, tc.want)
			}
		})
	}
}

// TestDistillFailSafeNilAgent is a defensive proof that a nil controller / empty
// enrollment never panics through the public record seam.
func TestDistillFailSafeDisabledController(t *testing.T) {
	store := openTestStore(t)
	var nilCtrl *MemoryController
	// Should not panic; distillEnabledFor guards nil.
	nilCtrl.record(context.Background(), "job-1", runtime.Agent{Name: "audit"}, "implement",
		JobPayload{Repo: "acme/widget"}, AgentResult{Decision: "failed", TestsRun: []string{"TestX"}})
	if obs, _ := store.ListMemoryObservations(context.Background(), "audit", "acme/widget"); len(obs) != 0 {
		t.Fatalf("nil controller must write nothing, got %+v", obs)
	}
}
