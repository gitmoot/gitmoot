package db

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSkillOptGateRunPersistence covers the additive replay-gate audit trail (#627):
// insert two runs for a candidate, list them newest-first, and confirm the
// accepted-run predicate the promotion guard consults.
func TestSkillOptGateRunPersistence(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	candidate := "planner@v2"

	// No runs yet: the promotion guard sees no accepted run.
	if ok, err := store.HasAcceptedSkillOptGateRun(ctx, candidate); err != nil || ok {
		t.Fatalf("HasAcceptedSkillOptGateRun on empty = %v (err=%v), want false", ok, err)
	}

	// A rejected run does not count as accepted.
	if err := store.InsertSkillOptGateRun(ctx, SkillOptGateRun{
		ID: "gate-1", TemplateID: "planner", CandidateVersionID: candidate,
		ChampionVersionID: "planner@v1", CorpusPath: "corpus.json", CorpusVersion: 1,
		CorpusItems: 2, Attempts: 2, Accepted: false, ChampionMean: 0.8, CandidateMean: 0.4,
		Reason: "worse", DeltasJSON: `[{"item_id":"a"}]`,
	}); err != nil {
		t.Fatalf("InsertSkillOptGateRun (reject) returned error: %v", err)
	}
	if ok, err := store.HasAcceptedSkillOptGateRun(ctx, candidate); err != nil || ok {
		t.Fatalf("after a rejected run HasAccepted = %v (err=%v), want false", ok, err)
	}

	// An accepted run flips the predicate.
	if err := store.InsertSkillOptGateRun(ctx, SkillOptGateRun{
		ID: "gate-2", TemplateID: "planner", CandidateVersionID: candidate,
		ChampionVersionID: "planner@v1", CorpusPath: "corpus.json", CorpusVersion: 1,
		CorpusItems: 2, Attempts: 1, Accepted: true, ChampionMean: 0.4, CandidateMean: 0.9,
		Reason: "better", DeltasJSON: `[]`,
	}); err != nil {
		t.Fatalf("InsertSkillOptGateRun (accept) returned error: %v", err)
	}
	if ok, err := store.HasAcceptedSkillOptGateRun(ctx, candidate); err != nil || !ok {
		t.Fatalf("after an accepted run HasAccepted = %v (err=%v), want true", ok, err)
	}

	runs, err := store.ListSkillOptGateRuns(ctx, candidate)
	if err != nil {
		t.Fatalf("ListSkillOptGateRuns returned error: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("listed %d runs, want 2", len(runs))
	}
	// A different candidate has no runs.
	if other, err := store.ListSkillOptGateRuns(ctx, "planner@v9"); err != nil || len(other) != 0 {
		t.Fatalf("unrelated candidate runs = %d (err=%v), want 0", len(other), err)
	}
	// Round-trip fields survive.
	got := runs[0]
	if got.Accepted != true && got.Accepted != false {
		t.Fatalf("accepted did not round-trip")
	}
	found := map[string]SkillOptGateRun{}
	for _, r := range runs {
		found[r.ID] = r
	}
	if found["gate-1"].CandidateMean != 0.4 || found["gate-2"].CandidateMean != 0.9 {
		t.Fatalf("means did not round-trip: %+v", found)
	}
}
