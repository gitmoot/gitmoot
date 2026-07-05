package cli

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	dashboard "github.com/jerryfane/gitmoot-dashboard"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

// reviewScoreSeed pins a candidate-review score (the nullable REAL column, carried
// as *float64) on a template version so Skills() can surface it.
func reviewScoreSeed(t *testing.T, store *db.Store, templateID, versionID string, score float64) {
	t.Helper()
	s := score
	if err := store.UpsertAgentTemplateCandidateReview(context.Background(), db.AgentTemplateCandidateReview{
		VersionID: versionID, TemplateID: templateID, Score: &s,
	}); err != nil {
		t.Fatalf("UpsertAgentTemplateCandidateReview %s: %v", versionID, err)
	}
}

// seedSkillTemplates builds two templates exercising the Skills view: "planner"
// evolves v1(superseded)->v2(current)->v3(pending)->v4(canary) with scored reviews;
// "helper" is a single current version with no pending. Two agents point at planner
// (one via an @latest ref, to prove the reference split) and one at helper.
func seedSkillTemplates(t *testing.T, home string) (v3ID string) {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	base := db.AgentTemplate{ID: "planner", Name: "Planner", Content: "v1"}
	if err := store.UpsertAgentTemplate(ctx, base); err != nil {
		t.Fatalf("UpsertAgentTemplate planner: %v", err)
	}
	tmpl, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate planner: %v", err)
	}
	reviewScoreSeed(t, store, "planner", tmpl.VersionID, 0.60) // v1

	v2 := base
	v2.Content = "v2"
	v2v, err := store.AddPendingAgentTemplateVersion(ctx, v2)
	if err != nil {
		t.Fatalf("AddPending v2: %v", err)
	}
	reviewScoreSeed(t, store, "planner", v2v.ID, 0.70)
	if _, err := store.PromoteAgentTemplateVersion(ctx, v2v.ID); err != nil {
		t.Fatalf("Promote v2: %v", err)
	}

	v3 := base
	v3.Content = "v3"
	v3v, err := store.AddPendingAgentTemplateVersion(ctx, v3)
	if err != nil {
		t.Fatalf("AddPending v3: %v", err)
	}
	reviewScoreSeed(t, store, "planner", v3v.ID, 0.81)
	v3ID = v3v.ID

	v4 := base
	v4.Content = "v4"
	v4v, err := store.AddPendingAgentTemplateVersion(ctx, v4)
	if err != nil {
		t.Fatalf("AddPending v4: %v", err)
	}
	if _, err := store.CanaryPromoteAgentTemplateVersion(ctx, v4v.ID, 0.15); err != nil {
		t.Fatalf("Canary v4: %v", err)
	}

	if err := store.UpsertAgentTemplate(ctx, db.AgentTemplate{ID: "helper", Name: "Helper", Content: "h1"}); err != nil {
		t.Fatalf("UpsertAgentTemplate helper: %v", err)
	}

	for _, a := range []db.Agent{
		{Name: "planner-two", Runtime: "codex", TemplateID: "planner@latest"},
		{Name: "planner-agent", Runtime: "codex", TemplateID: "planner"},
		{Name: "helper-agent", Runtime: "claude", TemplateID: "helper"},
		{Name: "loose-agent", Runtime: "codex"},
	} {
		if err := store.UpsertAgent(ctx, a); err != nil {
			t.Fatalf("UpsertAgent %s: %v", a.Name, err)
		}
	}
	return v3ID
}

func approxEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// TestWebDataSourceSkills asserts Skills() maps the version history + scores,
// resolves the current/canary state, extracts pending candidates, groups the
// agents-per-template, and sorts pending-first.
func TestWebDataSourceSkills(t *testing.T) {
	home := dashboardTestHome(t)
	v3ID := seedSkillTemplates(t, home)

	ds := &webDataSource{home: home}
	skills, err := ds.Skills(context.Background())
	if err != nil {
		t.Fatalf("Skills: %v", err)
	}

	if len(skills.Templates) != 2 {
		t.Fatalf("templates = %d, want 2: %+v", len(skills.Templates), skills.Templates)
	}
	// Pending-first: planner (has a pending candidate) before helper.
	planner := skills.Templates[0]
	helper := skills.Templates[1]
	if planner.TemplateID != "planner" || helper.TemplateID != "helper" {
		t.Fatalf("template order = %s,%s, want planner,helper (pending-first)", planner.TemplateID, helper.TemplateID)
	}

	// Current resolution (current_version_id) + canary fields.
	if planner.CurrentVersion != 2 || planner.CurrentState != "current" {
		t.Fatalf("planner current = v%d/%q, want v2/current", planner.CurrentVersion, planner.CurrentState)
	}
	if !approxEq(planner.CanarySample, 0.15) || planner.CanaryStartedAt <= 0 {
		t.Fatalf("planner canary = sample %v started %d, want 0.15 / >0", planner.CanarySample, planner.CanaryStartedAt)
	}
	if planner.LastPromotedAt <= 0 {
		t.Fatalf("planner LastPromotedAt = %d, want > 0 (v2 promotion)", planner.LastPromotedAt)
	}

	// Version history ascending by number, with the store's real states + scores.
	if len(planner.Versions) != 4 {
		t.Fatalf("planner versions = %d, want 4: %+v", len(planner.Versions), planner.Versions)
	}
	wantState := []string{"superseded", "current", "pending", "canary"}
	for i, v := range planner.Versions {
		if v.Number != i+1 {
			t.Fatalf("versions[%d].Number = %d, want %d (ascending)", i, v.Number, i+1)
		}
		if v.State != wantState[i] {
			t.Fatalf("versions[%d].State = %q, want %q", i, v.State, wantState[i])
		}
	}
	for i, want := range []float64{0.60, 0.70, 0.81} {
		if !planner.Versions[i].HasScore || !approxEq(planner.Versions[i].Score, want) {
			t.Fatalf("versions[%d] score = %v (has %v), want %v", i, planner.Versions[i].Score, planner.Versions[i].HasScore, want)
		}
	}
	if planner.Versions[3].HasScore {
		t.Fatalf("canary v4 must be unscored (mid-canary, no review), got %v", planner.Versions[3].Score)
	}

	// Pending candidate carries the review's raw score string.
	if len(planner.Pending) != 1 {
		t.Fatalf("planner pending = %d, want 1: %+v", len(planner.Pending), planner.Pending)
	}
	cand := planner.Pending[0]
	if cand.VersionID != v3ID || cand.Number != 3 || cand.Score != "0.81" {
		t.Fatalf("pending candidate = %+v, want {%s, 3, 0.81}", cand, v3ID)
	}

	// Agents-per-template, sorted, with the @latest ref normalized to the base id.
	if len(planner.Agents) != 2 || planner.Agents[0] != "planner-agent" || planner.Agents[1] != "planner-two" {
		t.Fatalf("planner agents = %v, want [planner-agent planner-two]", planner.Agents)
	}
	if len(helper.Agents) != 1 || helper.Agents[0] != "helper-agent" {
		t.Fatalf("helper agents = %v, want [helper-agent]", helper.Agents)
	}

	// helper: single current version, no pending, no canary.
	if helper.CurrentVersion != 1 || len(helper.Pending) != 0 || helper.CanarySample != 0 {
		t.Fatalf("helper = v%d pending%d canary%v, want v1/0/0", helper.CurrentVersion, len(helper.Pending), helper.CanarySample)
	}

	// Rollups.
	if skills.ActiveCanaries != 1 || skills.PendingTotal != 1 {
		t.Fatalf("rollups = canaries%d pending%d, want 1/1", skills.ActiveCanaries, skills.PendingTotal)
	}
}

// seedKnowledge seeds confirmed facts across enrolled + unenrolled owners, an
// observation pool (for witness counts), and one superseded chain (set directly,
// since no production write path populates superseded_by). It returns the row ids
// of the older and newer facts of that chain.
func seedKnowledge(t *testing.T, home string) (oldID, newID int64) {
	t.Helper()
	paths := config.PathsForHome(home)
	ctx := context.Background()

	for _, at := range []config.AgentType{
		{Name: "researcher", Runtime: "codex", Memory: true},
		{Name: "reviewer", Runtime: "kimi", Memory: true},
		{Name: "plain", Runtime: "claude", Memory: false},
	} {
		if err := config.SaveAgentType(paths, at); err != nil {
			t.Fatalf("SaveAgentType %s: %v", at.Name, err)
		}
	}

	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	seed := func(ref, repo, key, content string) int64 {
		id, err := store.UpsertConfirmedMemory(ctx, db.ConfirmedMemory{
			Owner: db.MemoryOwner{Kind: "agent", Ref: ref}, Repo: repo, Key: key, Content: content,
		})
		if err != nil {
			t.Fatalf("UpsertConfirmedMemory %s/%s: %v", ref, key, err)
		}
		return id
	}
	seed("researcher", "jerryfane/noted", "outcome:review:changes_requested", "F1")
	seed("researcher", "jerryfane/noted", "fix-rounds:approved", "F2")
	seed("researcher", "", "release-policy", "F3") // general scope, keyless-of-colon
	seed("reviewer", "jerryfane/noted", "outcome:review:changes_requested", "F4")
	seed("ghost", "acme/widgets", "outcome:implement:failed", "F5") // unenrolled owner
	oldID = seed("researcher", "jerryfane/noted", "outcome:review:approved", "F_old")
	newID = seed("researcher", "jerryfane/noted", "outcome:review:approved-new", "F_new")

	// Observations => witness counts: F1 keyed 3x (researcher), F4 keyed 1x (reviewer).
	obs := func(ref, repo, key string) {
		if _, err := store.InsertMemoryObservation(ctx, db.MemoryObservation{
			Owner: db.MemoryOwner{Kind: "agent", Ref: ref}, Repo: repo, Scope: "repo", Key: key, Content: "obs",
		}); err != nil {
			t.Fatalf("InsertMemoryObservation: %v", err)
		}
	}
	obs("researcher", "jerryfane/noted", "outcome:review:changes_requested")
	obs("researcher", "jerryfane/noted", "outcome:review:changes_requested")
	obs("researcher", "jerryfane/noted", "outcome:review:changes_requested")
	obs("reviewer", "jerryfane/noted", "outcome:review:changes_requested")
	store.Close()

	// Link the supersede chain directly (no production writer sets superseded_by).
	raw, err := sql.Open("sqlite", paths.Database)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `UPDATE confirmed_memories SET superseded_by = ? WHERE id = ?`, newID, oldID); err != nil {
		t.Fatalf("set superseded_by: %v", err)
	}
	return oldID, newID
}

// TestWebDataSourceKnowledge asserts Knowledge() maps facts (including flagged
// superseded ghosts), witness counts, enrolled+owner agents, and the owner/
// category/supersede edges deterministically.
func TestWebDataSourceKnowledge(t *testing.T) {
	home := dashboardTestHome(t)
	oldID, newID := seedKnowledge(t, home)

	ds := &webDataSource{home: home}
	k, err := ds.Knowledge(context.Background())
	if err != nil {
		t.Fatalf("Knowledge: %v", err)
	}

	// Agents: enrolled (researcher, reviewer) unioned with the unenrolled fact-owner
	// (ghost). "plain" is enrolled=false AND owns nothing, so it is absent.
	if len(k.Agents) != 3 {
		t.Fatalf("agents = %d, want 3: %+v", len(k.Agents), k.Agents)
	}
	byAgent := map[string]dashboard.KnowledgeAgent{}
	for _, a := range k.Agents {
		byAgent[a.Name] = a
	}
	if !byAgent["researcher"].Enrolled || !byAgent["reviewer"].Enrolled {
		t.Fatalf("researcher/reviewer must be enrolled: %+v", k.Agents)
	}
	if _, ok := byAgent["ghost"]; !ok || byAgent["ghost"].Enrolled {
		t.Fatalf("ghost must be listed with Enrolled=false: %+v", k.Agents)
	}
	if _, ok := byAgent["plain"]; ok {
		t.Fatalf("plain (enrolled=false, no facts) must be absent: %+v", k.Agents)
	}
	// Facts count is the INJECTABLE set (excludes the superseded F_old): researcher
	// owns 5 rows but 4 injectable.
	if byAgent["researcher"].Facts != 4 {
		t.Fatalf("researcher injectable Facts = %d, want 4 (F_old superseded is excluded)", byAgent["researcher"].Facts)
	}
	if byAgent["researcher"].Observations != 3 {
		t.Fatalf("researcher Observations = %d, want 3", byAgent["researcher"].Observations)
	}

	// Facts: all 7 rows present, including the superseded ghost (the graph shows it,
	// diverging from the per-agent injectable count above).
	if len(k.Facts) != 7 {
		t.Fatalf("facts = %d, want 7 (incl the superseded ghost)", len(k.Facts))
	}
	byContent := map[string]dashboard.KnowledgeFact{}
	for _, f := range k.Facts {
		byContent[f.Content] = f
	}
	if !byContent["F_old"].Superseded {
		t.Fatalf("F_old must be flagged Superseded")
	}
	if byContent["F_new"].Superseded || byContent["F1"].Superseded {
		t.Fatalf("non-superseded facts must not be flagged")
	}
	if byContent["F1"].Witnesses != 3 {
		t.Fatalf("F1 witnesses = %d, want 3", byContent["F1"].Witnesses)
	}
	if byContent["F4"].Witnesses != 1 {
		t.Fatalf("F4 witnesses = %d, want 1", byContent["F4"].Witnesses)
	}
	if byContent["F2"].Witnesses != 0 {
		t.Fatalf("F2 witnesses = %d, want 0 (no matching observations)", byContent["F2"].Witnesses)
	}
	if byContent["F3"].Repo != "" {
		t.Fatalf("F3 must be general-scope (empty repo), got %q", byContent["F3"].Repo)
	}

	// Edges by kind.
	var owner, category, supersede int
	var superEdge dashboard.KnowledgeEdge
	catTarget := map[string]int{}
	for _, e := range k.Edges {
		switch e.Kind {
		case "owner":
			owner++
		case "category":
			category++
			if e.Source == byContent["F1"].ID {
				catTarget["F1"] = 1
				if e.Target != "cat:outcome" {
					t.Fatalf("F1 category target = %q, want cat:outcome (leading key dimension)", e.Target)
				}
			}
		case "supersede":
			supersede++
			superEdge = e
		default:
			t.Fatalf("unexpected edge kind %q", e.Kind)
		}
	}
	if owner != 7 || category != 7 || supersede != 1 {
		t.Fatalf("edges = owner%d category%d supersede%d, want 7/7/1", owner, category, supersede)
	}
	// Supersede direction: newer fact -> older fact.
	wantNew := fmt.Sprintf("fact:%d", newID)
	wantOld := fmt.Sprintf("fact:%d", oldID)
	if superEdge.Source != wantNew || superEdge.Target != wantOld {
		t.Fatalf("supersede edge = %s->%s, want %s->%s (newer->older)", superEdge.Source, superEdge.Target, wantNew, wantOld)
	}
	if catTarget["F1"] != 1 {
		t.Fatalf("F1 must have a category edge")
	}

	// Determinism: a second call is byte-identical.
	k2, err := ds.Knowledge(context.Background())
	if err != nil {
		t.Fatalf("Knowledge (2nd): %v", err)
	}
	if fmt.Sprintf("%+v", k) != fmt.Sprintf("%+v", k2) {
		t.Fatalf("Knowledge not deterministic across calls")
	}
}

// TestKnowledgeCategory covers the key -> category-hub derivation.
func TestKnowledgeCategory(t *testing.T) {
	cases := map[string]string{
		"outcome:review:changes_requested": "cat:outcome",
		"fix-rounds:approved":              "cat:fix-rounds",
		"release-policy":                   "cat:release-policy",
		"":                                 "",
		":x":                               "",
		"   ":                              "",
	}
	for key, want := range cases {
		if got := knowledgeCategory(key); got != want {
			t.Fatalf("knowledgeCategory(%q) = %q, want %q", key, got, want)
		}
	}
}

// TestKnowledgeEdgesCategoryCap asserts the category-edge cap truncates
// deterministically while owner edges stay uncapped.
func TestKnowledgeEdgesCategoryCap(t *testing.T) {
	const n = categoryEdgeCap + 100
	facts := make([]dashboard.KnowledgeFact, 0, n)
	for i := 0; i < n; i++ {
		facts = append(facts, dashboard.KnowledgeFact{
			ID: fmt.Sprintf("fact:%d", i), Owner: "researcher", Key: fmt.Sprintf("outcome:%d", i),
		})
	}
	edges := knowledgeEdges(nil, facts, map[int64]string{})

	var owner, category int
	for _, e := range edges {
		switch e.Kind {
		case "owner":
			owner++
		case "category":
			category++
		}
	}
	if owner != n {
		t.Fatalf("owner edges = %d, want %d (uncapped)", owner, n)
	}
	if category != categoryEdgeCap {
		t.Fatalf("category edges = %d, want %d (capped)", category, categoryEdgeCap)
	}
}
