package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/events"
	"github.com/jerryfane/gitmoot/internal/skillopt"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

// markerScoringRunner is a DETERMINISTIC fake replay driver: it reads the template
// file the gate staged (GITMOOT_GATE_TEMPLATE_FILE), counts a marker token, and
// emits a deterministic per-item rubric score. No live LLM, no real subprocess — so
// the E2E drives the REAL candidate plumbing (store, corpus, protocol, persistence)
// with reproducible scores. A better template (more markers) scores higher; a worse
// one scores lower, exactly the signal the strict-improvement gate turns on.
type markerScoringRunner struct {
	marker string
	calls  int
}

func (r *markerScoringRunner) RunEnv(_ context.Context, _ string, env []string, _ string, _ ...string) (subprocess.Result, error) {
	r.calls++
	var templatePath string
	for _, kv := range env {
		if strings.HasPrefix(kv, "GITMOOT_GATE_TEMPLATE_FILE=") {
			templatePath = strings.TrimPrefix(kv, "GITMOOT_GATE_TEMPLATE_FILE=")
		}
	}
	content, err := os.ReadFile(templatePath)
	if err != nil {
		return subprocess.Result{Stderr: err.Error()}, err
	}
	count := strings.Count(string(content), r.marker)
	score := float64(count) * 0.3
	if score > 1 {
		score = 1
	}
	out, _ := json.Marshal(skillopt.GateReplayResult{Rubric: map[string]float64{"quality": score}})
	return subprocess.Result{Stdout: string(out)}, nil
}

func (r *markerScoringRunner) LookPath(file string) (string, error) { return file, nil }

// seedGateTemplate installs a champion template (its body carries championMarkers
// copies of "STRONG") and imports a candidate (candidateMarkers copies), returning
// the store, home, and the pending candidate version.
func seedGateTemplate(t *testing.T, championMarkers, candidateMarkers int) (*db.Store, string, db.AgentTemplateVersion) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	championBody := strings.TrimSpace(strings.Repeat("STRONG ", championMarkers)) + " guidance."
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", championBody)); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	candidateBody := strings.TrimSpace(strings.Repeat("STRONG ", candidateMarkers)) + " guidance."
	candidate := cliSkillOptCandidatePackage(t, "planner", installed.VersionID, candidateBody)
	version, err := skillopt.ImportCandidatePackage(context.Background(), store, candidate, "candidate.json")
	if err != nil {
		t.Fatalf("ImportCandidatePackage returned error: %v", err)
	}
	return store, home, version
}

// writeGateCorpus writes a minimal valid two-item corpus and returns its path.
func writeGateCorpus(t *testing.T, dir string) string {
	t.Helper()
	corpus := skillopt.GateCorpus{
		Kind:          skillopt.GateCorpusKind,
		Version:       1,
		ReplayCommand: "true",
		Items: []skillopt.GateCorpusItem{
			{ID: "item-1", Prompt: "implement widget A", Expected: "builds"},
			{ID: "item-2", Prompt: "implement widget B", Expected: "builds"},
		},
	}
	raw, err := json.MarshalIndent(corpus, "", "  ")
	if err != nil {
		t.Fatalf("marshal corpus: %v", err)
	}
	path := filepath.Join(dir, "corpus.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write corpus: %v", err)
	}
	return path
}

func withGateReplayRunner(t *testing.T, runner gateReplayEnvRunner) {
	t.Helper()
	prev := gateReplayRunner
	gateReplayRunner = runner
	t.Cleanup(func() { gateReplayRunner = prev })
}

// TestSkillOptGateRunRejectsWorseCandidate is the mutation-anchored E2E: a
// deliberately-worse candidate (fewer markers than the champion) is replayed
// through the REAL candidate plumbing with a seeded corpus and MUST be rejected.
// Inverting the strict-improvement comparison in EvaluateGate would let this worse
// candidate pass — turning this assertion red.
func TestSkillOptGateRunRejectsWorseCandidate(t *testing.T) {
	store, home, version := seedGateTemplate(t, 3 /*champion*/, 1 /*candidate is worse*/)
	corpusPath := writeGateCorpus(t, home)
	withGateReplayRunner(t, &markerScoringRunner{marker: "STRONG"})

	var stdout, stderr bytes.Buffer
	code := runSkillOptGateRun([]string{"--home", home, "--candidate", version.ID, "--corpus", corpusPath, "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("worse candidate gate run exit = %d (want 1 reject); stderr=%s", code, stderr.String())
	}
	var report skillOptGateRunReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v; stdout=%s", err, stdout.String())
	}
	if report.Accepted {
		t.Fatalf("worse candidate was ACCEPTED; champion mean %.3f candidate mean %.3f", report.ChampionMean, report.CandidateMean)
	}
	if report.CandidateMean >= report.ChampionMean {
		t.Fatalf("expected candidate mean < champion mean, got champ=%.3f cand=%.3f", report.ChampionMean, report.CandidateMean)
	}
	// The run is persisted for audit, and the candidate carries NO accepted gate run.
	accepted, err := store.HasAcceptedSkillOptGateRun(context.Background(), version.ID)
	if err != nil {
		t.Fatalf("HasAcceptedSkillOptGateRun returned error: %v", err)
	}
	if accepted {
		t.Fatalf("a rejected gate must not record an accepted run")
	}
	runs, err := store.ListSkillOptGateRuns(context.Background(), version.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected exactly 1 persisted gate run, got %d (err=%v)", len(runs), err)
	}
}

func TestSkillOptGateRunAcceptsBetterCandidate(t *testing.T) {
	store, home, version := seedGateTemplate(t, 1 /*champion*/, 3 /*candidate is better*/)
	corpusPath := writeGateCorpus(t, home)
	withGateReplayRunner(t, &markerScoringRunner{marker: "STRONG"})

	var stdout, stderr bytes.Buffer
	code := runSkillOptGateRun([]string{"--home", home, "--candidate", version.ID, "--corpus", corpusPath, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("better candidate gate run exit = %d (want 0 accept); stderr=%s", code, stderr.String())
	}
	var report skillOptGateRunReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if !report.Accepted {
		t.Fatalf("better candidate was rejected: %s", report.Reason)
	}
	accepted, err := store.HasAcceptedSkillOptGateRun(context.Background(), version.ID)
	if err != nil || !accepted {
		t.Fatalf("expected an accepted gate run on record (accepted=%v err=%v)", accepted, err)
	}
}

// TestSkillOptGateReplayDeterminism proves two replays over the same corpus +
// template yield IDENTICAL per-item scores (the gate has no live-LLM variance).
func TestSkillOptGateReplayDeterminism(t *testing.T) {
	home := t.TempDir()
	corpusPath := writeGateCorpus(t, home)
	corpus, err := skillopt.LoadGateCorpus(corpusPath)
	if err != nil {
		t.Fatalf("LoadGateCorpus returned error: %v", err)
	}
	withGateReplayRunner(t, &markerScoringRunner{marker: "STRONG"})
	driver := gateReplayDriver(corpus, "true")

	content := "# Planner\n\nSTRONG STRONG guidance.\n"
	first, _, err := driver(context.Background(), content)
	if err != nil {
		t.Fatalf("first replay returned error: %v", err)
	}
	second, _, err := driver(context.Background(), content)
	if err != nil {
		t.Fatalf("second replay returned error: %v", err)
	}
	if len(first) != len(second) || len(first) != len(corpus.Items) {
		t.Fatalf("score counts differ: %d vs %d (corpus %d)", len(first), len(second), len(corpus.Items))
	}
	for i := range first {
		if first[i].ItemID != second[i].ItemID || first[i].Score != second[i].Score || first[i].HasScore != second[i].HasScore {
			t.Fatalf("non-deterministic score for %s: %+v vs %+v", first[i].ItemID, first[i], second[i])
		}
	}
}

// TestGatePromotionGuardBlocksThenAllows drives the REAL runCandidateNotify
// promotion seam: with [skillopt].gate_enabled on and guardrails otherwise passing,
// a candidate with NO passing gate run is blocked (stays pending, gate_blocked
// event); after an accepted gate run is recorded, the same notify promotes it.
func TestGatePromotionGuardBlocksThenAllows(t *testing.T) {
	ctx := context.Background()
	store, _, version := seedGateTemplate(t, 1, 3)

	minSamples := 1
	minScore := 0.5
	score := 0.82
	policy := config.DefaultSkillOptPolicy()
	policy.AutoPromote = true
	policy.AutoPromoteMinSamples = &minSamples
	policy.AutoPromoteMinScore = &minScore
	policy.Gate = true

	candidate := skillopt.CandidatePackage{Summary: skillopt.CandidateSummary{Score: &score}}
	feedback := []db.FeedbackEvent{{Choice: "a"}}
	sink := &recordingSink{}

	// (1) Gate enabled, no accepted gate run -> blocked, no promotion.
	if err := runCandidateNotify(ctx, store, sink, policy, candidate, version, feedback, false, nil, 0, ""); err != nil {
		t.Fatalf("runCandidateNotify (blocked) returned error: %v", err)
	}
	after, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if after.State != "pending" {
		t.Fatalf("gate-blocked candidate state = %q, want pending (no promotion)", after.State)
	}
	if !hasGateBlockedEvent(sink) {
		t.Fatalf("expected a gate_blocked awaiting event")
	}

	// (2) Record an accepted gate run, then notify again -> promotes to current.
	if err := store.InsertSkillOptGateRun(ctx, db.SkillOptGateRun{
		ID: "gate-accepted", TemplateID: version.TemplateID, CandidateVersionID: version.ID,
		ChampionVersionID: "planner@v1", Accepted: true, Attempts: 1,
	}); err != nil {
		t.Fatalf("InsertSkillOptGateRun returned error: %v", err)
	}
	sink2 := &recordingSink{}
	if err := runCandidateNotify(ctx, store, sink2, policy, candidate, version, feedback, false, nil, 0, ""); err != nil {
		t.Fatalf("runCandidateNotify (allowed) returned error: %v", err)
	}
	promoted, err := store.GetAgentTemplateVersionByID(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if promoted.State != "current" {
		t.Fatalf("gate-passed candidate state = %q, want current (promoted)", promoted.State)
	}
}

func hasGateBlockedEvent(sink *recordingSink) bool {
	for _, e := range sink.byType(events.EventCandidateAwaitingPromotion) {
		if e.Status == "gate_blocked" {
			return true
		}
	}
	return false
}
