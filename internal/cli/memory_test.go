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
	"github.com/jerryfane/gitmoot/internal/memory"
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

func TestMemoryRecallRanksAndFiltersConfirmedMemories(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	for _, cm := range []db.ConfirmedMemory{
		{
			Owner: db.MemoryOwner{Kind: "agent", Ref: "lead"}, Repo: "acme/widget", Scope: "repo", Key: "widget-flake",
			Content: "arm64 runner flake arm64 runner flake in widget tests", Provenance: "seed",
		},
		{
			Owner: db.MemoryOwner{Kind: "agent", Ref: "audit"}, Repo: "acme/widget", Scope: "repo", Key: "audit-runner",
			Content: "arm64 runner policy differs for audit", Provenance: "seed",
		},
		{
			Owner: db.MemoryOwner{Kind: "agent", Ref: "lead"}, Repo: "acme/api", Scope: "repo", Key: "api-arm64",
			Content: "arm64 api migrations need a canary", Provenance: "seed",
		},
		{
			Owner: db.MemoryOwner{Kind: "agent", Ref: "audit"}, Scope: "general", Key: "general-arm64",
			Content: "arm64 runner facts apply across repositories", Provenance: "seed",
		},
		{
			Owner: db.MemoryOwner{Kind: "role", Ref: "reviewer"}, Repo: "acme/widget", Scope: "repo", Key: "role-hidden",
			Content: "arm64 runner flake from a role pool", Provenance: "seed",
		},
	} {
		if _, err := store.UpsertConfirmedMemory(ctx, cm); err != nil {
			t.Fatalf("seed %s: %v", cm.Key, err)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := runMemory([]string{"recall", "arm64 runner flake", "--home", home, "--repo", "acme/widget", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory recall exit %d: %s", code, stderr.String())
	}
	var all []memoryRecallEntry
	if err := json.Unmarshal(stdout.Bytes(), &all); err != nil {
		t.Fatalf("parse recall json: %v (%s)", err, stdout.String())
	}
	if len(all) < 3 {
		t.Fatalf("expected all-agent recall to include both owners and general memory, got %+v", all)
	}
	if all[0].Key != "widget-flake" {
		t.Fatalf("ranking top key = %q, want widget-flake; rows=%+v", all[0].Key, all)
	}
	owners := map[string]bool{}
	for _, e := range all {
		owners[e.Owner.Ref] = true
		if e.Owner.Kind != "agent" {
			t.Fatalf("recall without --agent must search only agent pools, got %+v", e.Owner)
		}
	}
	if !owners["lead"] || !owners["audit"] {
		t.Fatalf("expected recall without --agent to search all agent pools, got owners %+v", owners)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runMemory([]string{"recall", "--home", home, "--agent", "lead", "--repo", "acme/widget", "--json", "arm64 runner flake"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory recall --agent exit %d: %s", code, stderr.String())
	}
	var leadOnly []memoryRecallEntry
	if err := json.Unmarshal(stdout.Bytes(), &leadOnly); err != nil {
		t.Fatalf("parse lead recall json: %v (%s)", err, stdout.String())
	}
	for _, e := range leadOnly {
		if e.Owner.Ref != "lead" {
			t.Fatalf("--agent lead returned owner %+v", e.Owner)
		}
	}
	if len(leadOnly) == 0 || leadOnly[0].Owner.Kind != "agent" || leadOnly[0].Owner.Ref != "lead" || leadOnly[0].UpdatedAt == "" {
		t.Fatalf("json shape missing expected owner/updated_at fields: %+v", leadOnly)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runMemory([]string{"recall", "--home", home, "--agent", "lead", "--json", "api migrations"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory recall no repo filter exit %d: %s", code, stderr.String())
	}
	var unfiltered []memoryRecallEntry
	if err := json.Unmarshal(stdout.Bytes(), &unfiltered); err != nil {
		t.Fatalf("parse unfiltered recall json: %v (%s)", err, stdout.String())
	}
	if len(unfiltered) == 0 || unfiltered[0].Key != "api-arm64" || unfiltered[0].Repo != "acme/api" {
		t.Fatalf("omitted --repo should search all repos, got %+v", unfiltered)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runMemory([]string{"recall", "--home", home, "--repo", "acme/api", "--json", "arm64 runner"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory recall --repo exit %d: %s", code, stderr.String())
	}
	var apiRows []memoryRecallEntry
	if err := json.Unmarshal(stdout.Bytes(), &apiRows); err != nil {
		t.Fatalf("parse api recall json: %v (%s)", err, stdout.String())
	}
	keys := map[string]bool{}
	for _, e := range apiRows {
		keys[e.Key] = true
		if e.Scope == "repo" && e.Repo != "acme/api" {
			t.Fatalf("--repo acme/api returned repo-scoped row from %q: %+v", e.Repo, apiRows)
		}
	}
	if !keys["api-arm64"] || !keys["general-arm64"] {
		t.Fatalf("repo filter should include repo row and general row, got keys %+v", keys)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runMemory([]string{"recall", "--home", home, "--repo", "acme/widget", "arm64 runner flake"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory recall text exit %d: %s", code, stderr.String())
	}
	text := stdout.String()
	if !strings.Contains(text, "widget-flake repo=acme/widget scope=repo owner=agent:lead") ||
		!strings.Contains(text, "- [this repo] arm64 runner flake arm64 runner flake in widget tests") {
		t.Fatalf("text recall did not render metadata plus injection bullet format:\n%s", text)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runMemory([]string{"recall", "--home", home, "zzznomatch"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory recall no-match exit %d: %s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "no matches" {
		t.Fatalf("no-match stdout = %q, want no matches", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runMemory([]string{"recall", "--home", home, "--json", "zzznomatch"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory recall no-match json exit %d: %s", code, stderr.String())
	}
	var none []memoryRecallEntry
	if err := json.Unmarshal(stdout.Bytes(), &none); err != nil {
		t.Fatalf("parse empty recall json: %v (%s)", err, stdout.String())
	}
	if len(none) != 0 {
		t.Fatalf("empty recall JSON returned rows: %+v", none)
	}
}

func TestMemoryRecallSharedParity(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	for _, cm := range []db.ConfirmedMemory{
		{
			Owner: db.MemoryOwner{Kind: "agent", Ref: "lead"}, Repo: "acme/widget", Scope: "repo", Key: "lead-private",
			Content: "atlas runner private lead fact", Provenance: "seed",
		},
		{
			Owner: db.MemoryOwner{Kind: "agent", Ref: "audit"}, Repo: "acme/widget", Scope: "repo", Key: "audit-private",
			Content: "atlas runner private audit fact", Provenance: "seed",
		},
		{
			Owner: db.MemoryOwner{Kind: memory.OwnerKindShared, Ref: memory.SharedOwnerRef}, AuthorRef: "lead",
			Repo: "acme/widget", Scope: "repo", Key: "shared-atlas",
			Content: "atlas runner shared fact", Provenance: "seed",
		},
	} {
		if _, err := store.UpsertConfirmedMemory(ctx, cm); err != nil {
			t.Fatalf("seed %s: %v", cm.Key, err)
		}
	}

	var stdout, stderr bytes.Buffer
	if code := runMemory([]string{"recall", "atlas runner", "--home", home, "--repo", "acme/widget", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("default recall exit %d: %s", code, stderr.String())
	}
	var all []memoryRecallEntry
	if err := json.Unmarshal(stdout.Bytes(), &all); err != nil {
		t.Fatalf("parse all recall: %v (%s)", err, stdout.String())
	}
	if !recallHasKey(all, "lead-private") || !recallHasKey(all, "audit-private") || !recallHasKey(all, "shared-atlas") {
		t.Fatalf("default recall should include all agent pools plus shared, got %+v", all)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runMemory([]string{"recall", "atlas runner", "--home", home, "--repo", "acme/widget", "--agent", "lead", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent recall exit %d: %s", code, stderr.String())
	}
	var lead []memoryRecallEntry
	if err := json.Unmarshal(stdout.Bytes(), &lead); err != nil {
		t.Fatalf("parse lead recall: %v (%s)", err, stdout.String())
	}
	if !recallHasKey(lead, "lead-private") || !recallHasKey(lead, "shared-atlas") || recallHasKey(lead, "audit-private") {
		t.Fatalf("--agent lead should include lead private plus shared only, got %+v", lead)
	}
	for _, e := range lead {
		if e.Key == "shared-atlas" && e.AuthorRef != "lead" {
			t.Fatalf("shared recall JSON should expose author_ref lead, got %+v", e)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := runMemory([]string{"recall", "atlas runner", "--home", home, "--repo", "acme/widget", "--shared", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("shared recall exit %d: %s", code, stderr.String())
	}
	var sharedOnly []memoryRecallEntry
	if err := json.Unmarshal(stdout.Bytes(), &sharedOnly); err != nil {
		t.Fatalf("parse shared recall: %v (%s)", err, stdout.String())
	}
	if len(sharedOnly) != 1 || sharedOnly[0].Key != "shared-atlas" {
		t.Fatalf("--shared should return only shared rows, got %+v", sharedOnly)
	}
}

func TestMemoryRecallExpandAddsLinkedFromJSON(t *testing.T) {
	home, store := memoryTestHome(t)
	ctx := context.Background()
	owner := db.MemoryOwner{Kind: "agent", Ref: "lead"}
	if _, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "linked-neighbor",
		Content: "aurora quartz vector hidden neighbor", Provenance: "seed",
	}); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	srcID, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
		Owner: owner, Repo: "acme/widget", Scope: "repo", Key: "direct-source",
		Content: "aurora quartz vector source instructions", Provenance: "seed",
	})
	if err != nil {
		t.Fatalf("seed source: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runMemory([]string{"recall", "instructions", "--home", home, "--repo", "acme/widget", "--agent", "lead", "--limit", "2", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory recall without expand exit %d: %s", code, stderr.String())
	}
	var directOnly []memoryRecallEntry
	if err := json.Unmarshal(stdout.Bytes(), &directOnly); err != nil {
		t.Fatalf("parse direct recall: %v (%s)", err, stdout.String())
	}
	if len(directOnly) != 1 || directOnly[0].Key != "direct-source" || directOnly[0].LinkedFrom != 0 {
		t.Fatalf("without --expand should return only direct source without linked_from, got %+v", directOnly)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runMemory([]string{"recall", "instructions", "--home", home, "--repo", "acme/widget", "--agent", "lead", "--limit", "2", "--expand", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory recall --expand exit %d: %s", code, stderr.String())
	}
	var expanded []memoryRecallEntry
	if err := json.Unmarshal(stdout.Bytes(), &expanded); err != nil {
		t.Fatalf("parse expanded recall: %v (%s)", err, stdout.String())
	}
	if len(expanded) != 2 || expanded[0].Key != "direct-source" || expanded[1].Key != "linked-neighbor" {
		t.Fatalf("--expand should append linked neighbor after direct hit, got %+v", expanded)
	}
	if expanded[0].LinkedFrom != 0 || expanded[1].LinkedFrom != srcID {
		t.Fatalf("linked_from JSON mismatch, source=%d rows=%+v", srcID, expanded)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runMemory([]string{"recall", "instructions", "--home", home, "--repo", "acme/widget", "--agent", "lead", "--limit", "2", "--expand"}, &stdout, &stderr); code != 0 {
		t.Fatalf("memory recall --expand text exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "- [this repo] [linked] aurora quartz vector hidden neighbor") {
		t.Fatalf("text --expand should mark linked bullet, got:\n%s", stdout.String())
	}
}

func recallHasKey(rows []memoryRecallEntry, key string) bool {
	for _, r := range rows {
		if r.Key == key {
			return true
		}
	}
	return false
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
	// A null retriever must NOT show perfect precision when keys were expected:
	// otherwise the gating harness would read "precision@K=1.000" for an
	// empty/null retriever (see PR #626 review).
	recall3, prec3 := recallPrecisionAtK(nil, []string{"a"}, 5)
	if recall3 != 0 {
		t.Fatalf("empty retrieval with expected keys: recall = %v, want 0", recall3)
	}
	if prec3 != 0 {
		t.Fatalf("empty retrieval with expected keys: precision = %v, want 0", prec3)
	}
	// A genuinely correct null (nothing expected, nothing retrieved) still
	// earns full recall and precision credit.
	recall4, prec4 := recallPrecisionAtK(nil, nil, 5)
	if recall4 != 1 || prec4 != 1 {
		t.Fatalf("correct null retrieval: recall = %v prec = %v, want 1, 1", recall4, prec4)
	}
}
