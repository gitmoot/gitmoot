package cli

import (
	"context"
	"fmt"
	"os"
	"testing"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// seedAttentionHome seeds a home exercising all three #528 buckets: a blocked job
// with an OPEN gate (plus a second job whose gate is satisfied, to prove the
// open-only filter) and recorded result-check failures; a pending synth item (plus a
// rejected one, to prove the status filter); a pending template candidate; and a
// SkillOpt run with binary verdicts.
func seedAttentionHome(t *testing.T, home string) {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
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

	// One pending synth item + one rejected (must be filtered out).
	if err := store.CreateSynthReviewItem(ctx, db.SynthReviewItem{
		ID: "synth-1", TemplateID: "tmpl-reviewer", Repo: "jerryfane/gitmoot", Status: db.SynthItemStatusPending,
		Question: "flag an unearned pass?", Gap: 0.29, WeakAgent: "r@v2", StrongAgent: "r@v3", JudgeAgent: "judge",
	}); err != nil {
		t.Fatalf("CreateSynthReviewItem pending: %v", err)
	}
	if err := store.CreateSynthReviewItem(ctx, db.SynthReviewItem{
		ID: "synth-2", TemplateID: "tmpl-reviewer", Repo: "jerryfane/gitmoot", Status: db.SynthItemStatusRejected,
		Question: "already decided", Gap: 0.10,
	}); err != nil {
		t.Fatalf("CreateSynthReviewItem rejected: %v", err)
	}

	// One pending template candidate (a version awaiting promotion) with a score.
	if err := store.UpsertAgentTemplate(ctx, db.AgentTemplate{ID: "reviewer", Name: "Reviewer", Content: "v1"}); err != nil {
		t.Fatalf("UpsertAgentTemplate: %v", err)
	}
	v2, err := store.AddPendingAgentTemplateVersion(ctx, db.AgentTemplate{ID: "reviewer", Name: "Reviewer", Content: "v2"})
	if err != nil {
		t.Fatalf("AddPendingAgentTemplateVersion: %v", err)
	}
	score := 0.81
	if err := store.UpsertAgentTemplateCandidateReview(ctx, db.AgentTemplateCandidateReview{VersionID: v2.ID, TemplateID: "reviewer", Score: &score}); err != nil {
		t.Fatalf("UpsertAgentTemplateCandidateReview: %v", err)
	}

	// SkillOpt binary verdicts for a run (mixed yes/no across two dimensions).
	for _, v := range []db.BinaryVerdict{
		{RunID: "eval-1", QuestionID: "q-cites", Dimension: "correctness", Verdict: "yes", Explanation: "cites sources"},
		{RunID: "eval-1", QuestionID: "q-false-pass", Dimension: "correctness", Verdict: "no", Explanation: "unearned pass"},
		{RunID: "eval-1", QuestionID: "q-scoped", Dimension: "usefulness", Verdict: "no", Explanation: "out of scope"},
	} {
		if err := store.UpsertBinaryVerdict(ctx, v); err != nil {
			t.Fatalf("UpsertBinaryVerdict %s: %v", v.QuestionID, err)
		}
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

	// Synth: only the pending one.
	if len(att.SynthItems) != 1 || att.SynthItems[0].ID != "synth-1" {
		t.Fatalf("synth items = %+v, want only synth-1", att.SynthItems)
	}
	if att.SynthItems[0].Gap != 0.29 || att.SynthItems[0].StrongAgent != "r@v3" {
		t.Fatalf("synth item fields wrong: %+v", att.SynthItems[0])
	}

	// Candidates: the one pending version, with its score passed through.
	if len(att.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1: %+v", len(att.Candidates), att.Candidates)
	}
	c := att.Candidates[0]
	if c.TemplateID != "reviewer" || c.Number != 2 || c.Score != "0.81" {
		t.Fatalf("candidate wrong: %+v", c)
	}

	if att.Total != 3 {
		t.Fatalf("Total = %d, want 3", att.Total)
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

func TestWebDataSourceBinaryVerdicts(t *testing.T) {
	home := dashboardTestHome(t)
	seedAttentionHome(t, home)
	ds := &webDataSource{home: home}
	ctx := context.Background()

	v, err := ds.BinaryVerdicts(ctx, "eval-1")
	if err != nil {
		t.Fatalf("BinaryVerdicts: %v", err)
	}
	if v.RunID != "eval-1" {
		t.Fatalf("RunID = %q", v.RunID)
	}
	if len(v.Verdicts) != 3 {
		t.Fatalf("verdicts = %d, want 3", len(v.Verdicts))
	}
	if v.Passed != 1 || v.Failed != 2 {
		t.Fatalf("passed=%d failed=%d, want 1/2", v.Passed, v.Failed)
	}
	// Ordered by (dimension, questionId): correctness rows before usefulness.
	if v.Verdicts[0].Dimension != "correctness" || v.Verdicts[2].Dimension != "usefulness" {
		t.Fatalf("verdict order wrong: %+v", v.Verdicts)
	}
	for _, q := range v.Verdicts {
		if q.Pass != (q.Verdict == "yes") {
			t.Fatalf("Pass/Verdict mismatch: %+v", q)
		}
		if q.Weight <= 0 {
			t.Fatalf("weight should default > 0: %+v", q)
		}
	}

	// Unknown run: zero counts, empty non-nil list, no error.
	empty, err := ds.BinaryVerdicts(ctx, "nope")
	if err != nil {
		t.Fatalf("BinaryVerdicts unknown: %v", err)
	}
	if empty.Verdicts == nil || len(empty.Verdicts) != 0 || empty.Passed != 0 || empty.Failed != 0 {
		t.Fatalf("unknown run should be empty: %+v", empty)
	}
}
