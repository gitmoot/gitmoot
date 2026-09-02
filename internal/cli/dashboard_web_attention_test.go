package cli

import (
	"context"
	"fmt"
	"os"
	"testing"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// seedAttentionHome seeds a home exercising the surviving #528 bucket: a blocked job
// with an OPEN gate (plus a second job whose gate is satisfied, to prove the
// open-only filter) and recorded result-check failures. The synth-review and
// pending-candidate buckets went with the SkillOpt loop in #1752, so there is
// nothing left to seed for them.
func seedAttentionHome(t *testing.T, home string) {
	t.Helper()
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.UpsertAgent(ctx, db.Agent{Name: "integrator", Runtime: "codex"}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	blockedPayload := workflow.JobPayload{Repo: "jerryfane/noted", TaskTitle: "integrate + open PR", PullRequest: 42}
	mustCreateJob(t, store, db.Job{ID: "blocked-job", Agent: "integrator", Type: "implement", State: "blocked", Payload: mustJSON(t, blockedPayload)}, "", "")
	cleanPayload := workflow.JobPayload{Repo: "jerryfane/noted", TaskTitle: "clean job"}
	mustCreateJob(t, store, db.Job{ID: "clean-job", Agent: "integrator", Type: "implement", State: "running", Payload: mustJSON(t, cleanPayload)}, "", "")
	// A job that recorded an open gate while blocked, then was cancelled (or retried
	// back to queued): its gate row is retained (CancelJob/RetryJob never clear gates),
	// but it is no longer parked on a human so it must NOT surface (#528 review fix).
	cancelledPayload := workflow.JobPayload{Repo: "jerryfane/noted", TaskTitle: "abandoned job"}
	mustCreateJob(t, store, db.Job{ID: "cancelled-job", Agent: "integrator", Type: "implement", State: "cancelled", Payload: mustJSON(t, cancelledPayload)}, "", "")
	queuedPayload := workflow.JobPayload{Repo: "jerryfane/noted", TaskTitle: "requeued job"}
	mustCreateJob(t, store, db.Job{ID: "queued-job", Agent: "integrator", Type: "implement", State: "queued", Payload: mustJSON(t, queuedPayload)}, "", "")

	// blocked-job has one OPEN gate; clean-job has a gate that is then satisfied;
	// cancelled-job and queued-job each keep an OPEN gate on a non-blocked job.
	if _, err := store.RecordJobGates(ctx, "blocked-job", []string{"human:confirm-pr-target"}); err != nil {
		t.Fatalf("RecordJobGates blocked: %v", err)
	}
	if _, err := store.RecordJobGates(ctx, "clean-job", []string{"human:already-cleared"}); err != nil {
		t.Fatalf("RecordJobGates clean: %v", err)
	}
	if _, err := store.RecordJobGates(ctx, "cancelled-job", []string{"human:confirm-pr-target"}); err != nil {
		t.Fatalf("RecordJobGates cancelled: %v", err)
	}
	if _, err := store.RecordJobGates(ctx, "queued-job", []string{"human:confirm-pr-target"}); err != nil {
		t.Fatalf("RecordJobGates queued: %v", err)
	}
	if ok, err := store.SatisfyJobGate(ctx, "clean-job", "human:already-cleared"); err != nil || !ok {
		t.Fatalf("SatisfyJobGate clean: ok=%v err=%v", ok, err)
	}

	// Recorded result-check failures for blocked-job.
	if err := store.RecordResultCheckFailures(ctx, "blocked-job", "blocked-job", "implement", []db.ResultCheckFailure{
		{CheckID: "pr-opened", Question: "Did the job open a PR?", Explanation: "no PR url recorded"},
		{CheckID: "tests-run", Question: "Were tests run?", Explanation: "no command output present"},
	}); err != nil {
		t.Fatalf("RecordResultCheckFailures: %v", err)
	}

}

func TestWebDataSourceAttention(t *testing.T) {
	home := dashboardTestHome(t)
	seedAttentionHome(t, home)
	ds := &webDataSource{home: home}
	ctx := context.Background()

	att, err := ds.Attention(ctx)
	if err != nil {
		t.Fatalf("Attention: %v", err)
	}

	// Gates: only the open gate on a still-blocked job, enriched with job context.
	// The satisfied gate (clean-job) and the open gates on the cancelled/queued jobs
	// must all be excluded — a job that left blocked without clearing its gates is no
	// longer "Needs a human" (#528 review fix).
	if len(att.Gates) != 1 {
		t.Fatalf("gates = %d, want 1 (open + still blocked only): %+v", len(att.Gates), att.Gates)
	}
	g := att.Gates[0]
	if g.JobID != "blocked-job" || g.Need != "human:confirm-pr-target" {
		t.Fatalf("gate identity wrong: %+v", g)
	}
	if g.Repo != "jerryfane/noted" || g.PR != 42 || g.Agent != "integrator" || g.State != dashboard.NodeState("blocked") {
		t.Fatalf("gate not enriched from job: %+v", g)
	}
	if g.Title == "" {
		t.Fatalf("gate title should be resolved from the job payload: %+v", g)
	}

	// The synth-review and pending-candidate buckets lost their backing tables with
	// the SkillOpt loop (#1752). Their DataSource fields remain part of the pinned
	// dashboard module's contract, so they must serialize as EMPTY BUT NON-NIL and
	// contribute nothing to Total — a nil here would marshal as JSON null and break
	// a client that iterates them.
	if att.SynthItems == nil || len(att.SynthItems) != 0 {
		t.Fatalf("synth items = %+v, want empty non-nil", att.SynthItems)
	}
	if att.Candidates == nil || len(att.Candidates) != 0 {
		t.Fatalf("candidates = %+v, want empty non-nil", att.Candidates)
	}
	if att.Total != 1 {
		t.Fatalf("Total = %d, want 1 (the single open gate)", att.Total)
	}

	// Non-nil, deterministic across calls.
	att2, err := ds.Attention(ctx)
	if err != nil {
		t.Fatalf("Attention (2nd): %v", err)
	}
	if fmt.Sprintf("%+v", att) != fmt.Sprintf("%+v", att2) {
		t.Fatalf("Attention not deterministic:\n%+v\n%+v", att, att2)
	}
}

func TestWebDataSourceAttentionEmpty(t *testing.T) {
	home := dashboardTestHome(t)
	ds := &webDataSource{home: home}
	att, err := ds.Attention(context.Background())
	if err != nil {
		t.Fatalf("Attention empty: %v", err)
	}
	if att.Gates == nil || att.SynthItems == nil || att.Candidates == nil {
		t.Fatalf("empty-store lists must be non-nil: %+v", att)
	}
	if att.Total != 0 {
		t.Fatalf("Total = %d, want 0", att.Total)
	}
}

func TestWebDataSourceJobChecks(t *testing.T) {
	home := dashboardTestHome(t)
	seedAttentionHome(t, home)
	ds := &webDataSource{home: home}
	ctx := context.Background()

	jc, err := ds.JobChecks(ctx, "blocked-job")
	if err != nil {
		t.Fatalf("JobChecks: %v", err)
	}
	if jc.JobID != "blocked-job" {
		t.Fatalf("JobID = %q", jc.JobID)
	}
	// No [workflow] config file => the documented default policy (warn).
	if jc.Mode != string(config.DefaultResultChecksMode) {
		t.Fatalf("Mode = %q, want %q (default)", jc.Mode, config.DefaultResultChecksMode)
	}
	if len(jc.Failed) != 2 {
		t.Fatalf("failed = %d, want 2: %+v", len(jc.Failed), jc.Failed)
	}
	// Insertion order preserved.
	if jc.Failed[0].CheckID != "pr-opened" || jc.Failed[1].CheckID != "tests-run" {
		t.Fatalf("failed check order wrong: %+v", jc.Failed)
	}
	if jc.Failed[0].Question == "" || jc.Failed[0].Explanation == "" {
		t.Fatalf("failed check missing question/explanation: %+v", jc.Failed[0])
	}

	// A job with no recorded failures still resolves the mode with an empty list.
	jc2, err := ds.JobChecks(ctx, "clean-job")
	if err != nil {
		t.Fatalf("JobChecks clean: %v", err)
	}
	if jc2.Mode == "" || jc2.Failed == nil || len(jc2.Failed) != 0 {
		t.Fatalf("clean-job checks wrong: %+v", jc2)
	}

	// An unknown job is not an error — mode still resolves, Failed is empty.
	jc3, err := ds.JobChecks(ctx, "does-not-exist")
	if err != nil {
		t.Fatalf("JobChecks unknown: %v", err)
	}
	if jc3.Mode == "" || len(jc3.Failed) != 0 {
		t.Fatalf("unknown-job checks wrong: %+v", jc3)
	}
}

func TestWebDataSourceJobChecksBlockMode(t *testing.T) {
	home := dashboardTestHome(t)
	seedAttentionHome(t, home)
	// Prime the store/config so config.Initialize has run, then set block policy.
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[workflow]\nresult_checks = block\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	ds := &webDataSource{home: home}
	jc, err := ds.JobChecks(context.Background(), "blocked-job")
	if err != nil {
		t.Fatalf("JobChecks: %v", err)
	}
	if jc.Mode != string(config.ResultChecksBlock) {
		t.Fatalf("Mode = %q, want block", jc.Mode)
	}
}

// TestWebDataSourceBinaryVerdicts pins the post-#1752 contract: the
// skillopt_binary_verdicts table is gone, so every run — seeded or not — resolves to
// zero counts and an empty, never-nil list, and the method never errors. It stays
// wired because the pinned dashboard module's DataSource interface requires it.
func TestWebDataSourceBinaryVerdicts(t *testing.T) {
	home := dashboardTestHome(t)
	seedAttentionHome(t, home)
	ds := &webDataSource{home: home}
	ctx := context.Background()

	for _, runID := range []string{"eval-1", "nope", ""} {
		v, err := ds.BinaryVerdicts(ctx, runID)
		if err != nil {
			t.Fatalf("BinaryVerdicts(%q): %v", runID, err)
		}
		if v.RunID != runID {
			t.Fatalf("BinaryVerdicts(%q) RunID = %q, want the requested run echoed back", runID, v.RunID)
		}
		if v.Verdicts == nil || len(v.Verdicts) != 0 || v.Passed != 0 || v.Failed != 0 {
			t.Fatalf("BinaryVerdicts(%q) = %+v, want empty non-nil with zero counts", runID, v)
		}
	}
}
