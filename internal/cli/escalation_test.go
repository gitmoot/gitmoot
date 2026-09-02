package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// escalationTestHome builds an ISOLATED home with a migrated store. It never touches
// the live daemon home: every test here writes escalation state, and a legacy-replay
// bug would be destructive.
func escalationTestHome(t *testing.T) (string, *db.Store) {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".gitmoot"), 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	store, err := dbtest.Open(t, filepath.Join(home, ".gitmoot", "gitmoot.db"))
	if err != nil {
		t.Fatalf("open isolated store: %v", err)
	}
	return home, store
}

func seedParkedRound(t *testing.T, home string, jobID string, roundID string, verb string) {
	t.Helper()
	ctx := context.Background()
	if err := withStore(home, func(store *db.Store) error {
		if err := store.CreateJob(ctx, db.Job{ID: jobID, Agent: "coord", Type: "ask", State: "succeeded",
			Payload: `{"repo":"o/r","result":{"decision":"approved","summary":"s"}}`}); err != nil {
			return err
		}
		if _, err := store.AdoptLegacyEscalationRound(ctx, jobID, roundID, "", time.Now().UTC()); err != nil {
			return err
		}
		if _, err := store.ClaimEscalationRound(ctx, jobID, roundID, verb, 0, `{"reason":"`+verb+`"}`, time.Now().UTC()); err != nil {
			return err
		}
		_, err := store.MarkEscalationRoundNeedsRepair(ctx, jobID, roundID, "retry_exhausted",
			db.JobEvent{JobID: jobID, Kind: "delegation_escalation_needs_repair", Message: "parked"}, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatalf("seed parked round: %v", err)
	}
}

// TestEscalationRepairSurfaceIsReachableFromTheCLI exercises BOTH repair arms through
// the real operator entry point. A needs_repair round resolvable only by database
// surgery would be a permanent block, so the surface is part of the fix.
func TestEscalationRepairSurfaceIsReachableFromTheCLI(t *testing.T) {
	ctx := context.Background()
	home, store := escalationTestHome(t)
	seedParkedRound(t, home, "coord-a", "round-a", "continue")

	// LIST shows the block and its cause.
	var stdout, stderr bytes.Buffer
	if code := runEscalation([]string{"list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("escalation list exit=%d stderr=%s", code, stderr.String())
	}
	var rows []escalationRepairRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("decode list output %q: %v", stdout.String(), err)
	}
	if len(rows) != 1 || rows[0].JobID != "coord-a" || rows[0].Cause != "retry_exhausted" {
		t.Fatalf("list rows = %+v, want the parked round with its cause", rows)
	}

	// REPAIR --retry re-arms the PRESERVED decision.
	stdout.Reset()
	stderr.Reset()
	if code := runEscalation([]string{"repair", "coord-a", "--round", "round-a", "--retry", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("escalation repair --retry exit=%d stderr=%s", code, stderr.String())
	}
	round, err := store.GetEscalationRound(ctx, "coord-a", "round-a")
	if err != nil {
		t.Fatalf("GetEscalationRound: %v", err)
	}
	if round.NeedsRepair() {
		t.Fatal("--retry did not clear the integrity state")
	}
	if round.ClaimVerb != "continue" || round.EffectsCompletedAt != "" {
		t.Fatalf("round = %+v, want the claim preserved and unsettled", round)
	}

	// REPAIR --supersede requires a reason, then discards on the record.
	seedParkedRound(t, home, "coord-b", "round-b", "retry")
	stdout.Reset()
	stderr.Reset()
	if code := runEscalation([]string{"repair", "coord-b", "--round", "round-b", "--supersede", "--home", home}, &stdout, &stderr); code == 0 {
		t.Fatal("supersede without --reason was accepted")
	}
	stdout.Reset()
	stderr.Reset()
	if code := runEscalation([]string{"repair", "coord-b", "--round", "round-b", "--supersede",
		"--reason", "leg deleted upstream", "--by", "jerry", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("escalation repair --supersede exit=%d stderr=%s", code, stderr.String())
	}
	superseded, err := store.GetEscalationRound(ctx, "coord-b", "round-b")
	if err != nil {
		t.Fatalf("GetEscalationRound: %v", err)
	}
	if superseded.EffectsCompletedAt == "" {
		t.Fatal("--supersede did not settle the round")
	}
	if superseded.SettledBy != "jerry" || !strings.Contains(superseded.SettledReason, "deleted upstream") {
		t.Fatalf("supersede recorded by=%q reason=%q", superseded.SettledBy, superseded.SettledReason)
	}
	// Both arms are mutually exclusive and the command says so.
	stdout.Reset()
	stderr.Reset()
	if code := runEscalation([]string{"repair", "coord-b", "--round", "round-b", "--retry", "--supersede", "--home", home}, &stdout, &stderr); code != 2 {
		t.Fatalf("both arms together exit=%d, want 2", code)
	}
}

// TestLegacyResolvedEventsAreNeverReplayed is the DEPLOYMENT GATE (directive 104623
// step 2 / 104761 condition G). On an isolated home seeded with pre-upgrade resolved
// events and NO receipts, plus one post-upgrade unfinished claim, the sweep must
// replay ZERO legacy effects and recover the post-upgrade claim exactly once.
//
// Legacy rounds have no escalation_rounds row, so they cannot be candidates: the
// protection is structural, not a predicate. Never run against /root/.gitmoot.
func TestLegacyResolvedEventsAreNeverReplayed(t *testing.T) {
	ctx := context.Background()
	_, store := escalationTestHome(t)

	// LEGACY: two coordinators resolved before this protocol existed. One 'ttl', one
	// 'continue' — the exact shapes measured on the live home.
	for _, legacy := range []struct{ id, verb string }{{"legacy-ttl", "ttl"}, {"legacy-continue", "continue"}} {
		if err := store.CreateJob(ctx, db.Job{ID: legacy.id, Agent: "coord", Type: "ask", State: "succeeded",
			Payload: `{"repo":"o/r","task_id":"t-` + legacy.id + `","result":{"decision":"approved","summary":"s"}}`}); err != nil {
			t.Fatalf("CreateJob(%s): %v", legacy.id, err)
		}
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: legacy.id, Kind: "delegation_escalation_requested",
			Message: `{"delegation_id":"api","reason":"leg failed"}`}); err != nil {
			t.Fatalf("seed legacy requested: %v", err)
		}
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: legacy.id, Kind: "delegation_escalation_resolved",
			Message: `{"reason":"` + legacy.verb + `"}`}); err != nil {
			t.Fatalf("seed legacy resolved: %v", err)
		}
	}

	// POST-UPGRADE: one genuinely unfinished claim, with a row.
	if err := store.CreateJob(ctx, db.Job{ID: "modern", Agent: "coord", Type: "ask", State: "succeeded",
		Payload: `{"repo":"o/r","result":{"decision":"approved","summary":"s"}}`}); err != nil {
		t.Fatalf("CreateJob(modern): %v", err)
	}
	if _, err := store.AdoptLegacyEscalationRound(ctx, "modern", "round-modern", "", time.Now().UTC()); err != nil {
		t.Fatalf("seed modern round: %v", err)
	}
	// A post-upgrade round's request carries its round id: that pairing is what lets
	// recovery read this round and only this round.
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: "modern", Kind: "delegation_escalation_requested",
		Message: `{"delegation_id":"api","reason":"leg failed","round_id":"round-modern"}`}); err != nil {
		t.Fatalf("seed modern requested: %v", err)
	}
	if _, err := store.ClaimEscalationRound(ctx, "modern", "round-modern", "continue", 0, `{"reason":"continue"}`, time.Now().UTC()); err != nil {
		t.Fatalf("claim modern round: %v", err)
	}

	// The candidate query sees ONLY the post-upgrade claim.
	rounds, err := store.UnfinishedEscalationRounds(ctx)
	if err != nil {
		t.Fatalf("UnfinishedEscalationRounds: %v", err)
	}
	if len(rounds) != 1 || rounds[0].JobID != "modern" {
		t.Fatalf("candidates = %+v, want only the post-upgrade claim", rounds)
	}

	engine := workflow.Engine{Store: store}
	beforeEvents := map[string]int{}
	for _, id := range []string{"legacy-ttl", "legacy-continue"} {
		list, err := store.ListJobEvents(ctx, id)
		if err != nil {
			t.Fatalf("ListJobEvents(%s): %v", id, err)
		}
		beforeEvents[id] = len(list)
	}

	recovered, err := engine.RecoverUnfinishedEscalationResolutions(ctx)
	if err != nil {
		t.Fatalf("RecoverUnfinishedEscalationResolutions: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want exactly 1 (the post-upgrade claim)", recovered)
	}
	for _, id := range []string{"legacy-ttl", "legacy-continue"} {
		list, err := store.ListJobEvents(ctx, id)
		if err != nil {
			t.Fatalf("ListJobEvents(%s): %v", id, err)
		}
		if len(list) != beforeEvents[id] {
			t.Fatalf("legacy job %s gained %d events: a pre-upgrade resolution was replayed", id, len(list)-beforeEvents[id])
		}
		if _, err := store.GetJob(ctx, id+"/continuation"); err == nil {
			t.Fatalf("legacy job %s had a continuation enqueued by replay", id)
		}
		if round, ok, err := store.UnsettledEscalationRound(ctx, id); err != nil {
			t.Fatalf("UnsettledEscalationRound(%s): %v", id, err)
		} else if ok {
			t.Fatalf("legacy job %s was given a round row by the sweep: %+v", id, round)
		}
	}
}
