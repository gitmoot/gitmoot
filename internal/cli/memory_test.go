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
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func memoryTestHome(t *testing.T) (string, *db.Store) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return home, store
}

func TestMemoryListShowsBothTiers(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	owner := db.MemoryOwner{Kind: "agent", Ref: "builder"}
	if _, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "ci-flake", Content: "arm64 CI is flaky",
	}); err != nil {
		t.Fatalf("seed confirmed: %v", err)
	}
	if _, err := store.InsertMemoryObservation(ctx, db.MemoryObservation{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "pend", Content: "a pending note", TrustMark: "normal",
	}); err != nil {
		t.Fatalf("seed pending: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runMemory([]string{"list", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory list exit %d: %s", code, stderr.String())
	}
	var entries []memoryListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("parse json: %v (%s)", err, stdout.String())
	}
	tiers := map[string]bool{}
	for _, e := range entries {
		tiers[e.Tier] = true
	}
	if !tiers["confirmed"] || !tiers["pending"] {
		t.Fatalf("want both tiers, got %+v", entries)
	}

	// --confirmed narrows to the confirmed tier only.
	stdout.Reset()
	stderr.Reset()
	if code := runMemory([]string{"list", "--home", home, "--confirmed", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory list --confirmed exit %d: %s", code, stderr.String())
	}
	var confirmedOnly []memoryListEntry
	if err := json.Unmarshal(stdout.Bytes(), &confirmedOnly); err != nil {
		t.Fatalf("parse json: %v", err)
	}
	for _, e := range confirmedOnly {
		if e.Tier != "confirmed" {
			t.Fatalf("--confirmed returned a %q entry", e.Tier)
		}
	}
}

func TestMemoryReplayReportsInjectionDelta(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	// Seed a confirmed memory the job's instructions will retrieve.
	if _, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
		Owner: db.MemoryOwner{Kind: "agent", Ref: "builder"}, Repo: "acme/widget", Scope: "repo",
		Key: "ci-flake", Content: "arm64 CI is flaky and needs reruns",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Create a real job in the store whose instructions match.
	if _, err := (workflow.Mailbox{Store: store}).Enqueue(ctx, workflow.JobRequest{
		ID: "job-1", Agent: "builder", Action: "implement", Repo: "acme/widget",
		Instructions: "investigate the flaky arm64 runner",
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runMemory([]string{"replay", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory replay exit %d: %s", code, stderr.String())
	}
	var summary memoryReplaySummary
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatalf("parse json: %v (%s)", err, stdout.String())
	}
	if summary.Jobs == 0 {
		t.Fatalf("replay considered no jobs")
	}
	if summary.JobsWithInjection == 0 || summary.TotalEntries == 0 || summary.TotalDeltaTokens == 0 {
		t.Fatalf("replay should report a positive injection delta, got %+v", summary)
	}
}

func TestMemoryEvalRecallPrecision(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	// Two confirmed facts; a query should retrieve the relevant one.
	for _, cm := range []db.ConfirmedMemory{
		{Owner: db.MemoryOwner{Kind: "agent", Ref: "builder"}, Repo: "acme/widget", Scope: "repo", Key: "ci-flake", Content: "arm64 CI runner is flaky"},
		{Owner: db.MemoryOwner{Kind: "agent", Ref: "builder"}, Repo: "acme/widget", Scope: "repo", Key: "docs", Content: "the website sidebar ids must match"},
	} {
		if _, err := store.UpsertConfirmedMemory(ctx, cm); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	fixtures := memoryEvalFixtures{Cases: []memoryEvalCase{
		{Agent: "builder", Repo: "acme/widget", Instructions: "the arm64 runner keeps failing", ExpectedKeys: []string{"ci-flake"}},
	}}
	data, _ := json.Marshal(fixtures)
	fixturesPath := filepath.Join(t.TempDir(), "fixtures.json")
	if err := os.WriteFile(fixturesPath, data, 0o600); err != nil {
		t.Fatalf("write fixtures: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runMemory([]string{"eval", "--home", home, "--fixtures", fixturesPath, "--k", "5", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory eval exit %d: %s", code, stderr.String())
	}
	var result memoryEvalResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("parse json: %v (%s)", err, stdout.String())
	}
	if result.MeanRecallAtK < 1.0 {
		t.Fatalf("expected the relevant fact to be retrieved (recall@K=1.0), got %+v", result)
	}
}

func TestMemoryEvalRequiresFixtures(t *testing.T) {
	home, _ := memoryTestHome(t)
	var stdout, stderr bytes.Buffer
	if code := runMemory([]string{"eval", "--home", home}, &stdout, &stderr); code == 0 {
		t.Fatalf("memory eval without --fixtures should fail")
	}
	if !strings.Contains(stderr.String(), "fixtures") {
		t.Fatalf("expected a helpful error, got %q", stderr.String())
	}
}

func TestRecallPrecisionAtK(t *testing.T) {
	recall, prec := recallPrecisionAtK([]string{"a", "b", "c"}, []string{"a", "z"}, 5)
	if recall != 0.5 {
		t.Fatalf("recall = %v, want 0.5", recall)
	}
	if prec < 0.33 || prec > 0.34 {
		t.Fatalf("precision = %v, want ~0.333", prec)
	}
	// K cutoff drops later retrievals.
	recall2, _ := recallPrecisionAtK([]string{"x", "a"}, []string{"a"}, 1)
	if recall2 != 0 {
		t.Fatalf("with K=1 and 'a' at position 2, recall = %v, want 0", recall2)
	}
}
