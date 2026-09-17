package cli

import (
	"context"
	"testing"

	dashboard "github.com/gitmoot/gitmoot-dashboard"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
)

// This file covers what is left of the Learning page after #2202 retired the
// brain: Skills, and only Skills. It reads the agent_templates /
// agent_template_versions store that survives the #2201 narrowing (#2204 keeps
// it), which is why Skills stayed a REAL implementation while Knowledge did not.
//
// It briefly also covered two transitional /api/brain/* routes. #2206 removed
// those with the module's Brain page, so nothing here touches them.
//
// The Knowledge tests that used to live beside these were deleted with their
// subject; the Skills tests are the pre-#2202 ones, unchanged, because their
// subject is unchanged.

// seedSkillTemplates installs two templates: "planner" with a three-version
// history whose CURRENT version is v2 (v3 exists but was reverted away from), and
// single-version "helper". It also registers agents against both, including one
// pinned with an @latest ref and one template-less agent, so the
// agents-per-template grouping is exercised. It returns planner's newest
// (non-current) version id.
func seedSkillTemplates(t *testing.T, home string) (newestID string) {
	t.Helper()
	store, err := dbtest.Open(t, config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	base := db.AgentTemplate{ID: "planner", Name: "Planner", Content: "v1"}
	if err := store.UpsertAgentTemplate(ctx, base); err != nil {
		t.Fatalf("UpsertAgentTemplate planner v1: %v", err)
	}
	v2 := base
	v2.Content = "v2"
	if err := store.UpsertAgentTemplate(ctx, v2); err != nil {
		t.Fatalf("UpsertAgentTemplate planner v2: %v", err)
	}
	v2Version, err := store.GetLatestAgentTemplateVersion(ctx, "planner")
	if err != nil {
		t.Fatalf("resolve planner v2: %v", err)
	}
	v3 := base
	v3.Content = "v3"
	if err := store.UpsertAgentTemplate(ctx, v3); err != nil {
		t.Fatalf("UpsertAgentTemplate planner v3: %v", err)
	}
	newest, err := store.GetLatestAgentTemplateVersion(ctx, "planner")
	if err != nil {
		t.Fatalf("resolve planner v3: %v", err)
	}
	newestID = newest.VersionID
	// Revert to v2 so the template's CURRENT version is not its newest row — the
	// case that proves CurrentVersion follows current_version_id, not recency.
	if _, err := store.RevertAgentTemplateVersion(ctx, "planner", v2Version.VersionID); err != nil {
		t.Fatalf("RevertAgentTemplateVersion planner -> v2: %v", err)
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
	return newestID
}

// TestWebDataSourceSkills asserts Skills() still maps every template's real
// version history, resolves the current version through current_version_id (not
// recency), groups agents per template, and keeps its ordering contract
// (LastPromotedAt desc, TemplateID asc on a tie). It also pins the post-#1752
// contract that the candidate/canary fields are zero and Pending is empty but
// NON-NIL: those fields remain in the pinned dashboard module's response shape,
// and a nil slice would marshal as JSON null and break a client that iterates it.
//
// #2202 round 2: this is the test that catches Skills being reduced to an empty
// stub along with Knowledge in #2202, and #2206 removed the Knowledge stub
// entirely once the module stopped requiring it. Its tables survive, so an empty
// answer here is a regression, not a truthful "nothing left to read" — the live
// store has 17 template rows.
func TestWebDataSourceSkills(t *testing.T) {
	home := dashboardTestHome(t)
	newestID := seedSkillTemplates(t, home)

	ds := &webDataSource{home: home}
	skills, err := ds.Skills(context.Background())
	if err != nil {
		t.Fatalf("Skills: %v", err)
	}

	if len(skills.Templates) != 2 {
		t.Fatalf("templates = %d, want 2: %+v", len(skills.Templates), skills.Templates)
	}
	byID := map[string]dashboard.SkillTemplate{}
	for _, tmpl := range skills.Templates {
		byID[tmpl.TemplateID] = tmpl
	}
	planner, ok := byID["planner"]
	if !ok {
		t.Fatalf("planner missing: %+v", skills.Templates)
	}
	helper, ok := byID["helper"]
	if !ok {
		t.Fatalf("helper missing: %+v", skills.Templates)
	}
	if planner.Name != "Planner" || helper.Name != "Helper" {
		t.Fatalf("names = %q/%q, want Planner/Helper", planner.Name, helper.Name)
	}
	// Documented order: LastPromotedAt descending, TemplateID ascending on a tie.
	// Asserted as the invariant rather than a fixed sequence, because both seeds
	// promote inside the same CURRENT_TIMESTAMP second and would otherwise make
	// this test depend on clock granularity.
	for i := 1; i < len(skills.Templates); i++ {
		prev, cur := skills.Templates[i-1], skills.Templates[i]
		if prev.LastPromotedAt < cur.LastPromotedAt {
			t.Fatalf("templates not sorted by LastPromotedAt desc: %s(%d) before %s(%d)", prev.TemplateID, prev.LastPromotedAt, cur.TemplateID, cur.LastPromotedAt)
		}
		if prev.LastPromotedAt == cur.LastPromotedAt && prev.TemplateID > cur.TemplateID {
			t.Fatalf("tied templates not sorted by TemplateID asc: %s before %s", prev.TemplateID, cur.TemplateID)
		}
	}

	// Current resolution follows current_version_id, NOT the newest row.
	if planner.CurrentVersion != 2 || planner.CurrentState != "current" {
		t.Fatalf("planner current = v%d/%q, want v2/current", planner.CurrentVersion, planner.CurrentState)
	}
	if planner.LastPromotedAt <= 0 {
		t.Fatalf("planner LastPromotedAt = %d, want > 0", planner.LastPromotedAt)
	}

	// Full history, ascending, carrying each row's real state.
	if len(planner.Versions) != 3 {
		t.Fatalf("planner versions = %d, want 3: %+v", len(planner.Versions), planner.Versions)
	}
	wantState := []string{"superseded", "current", "superseded"}
	for i, v := range planner.Versions {
		if v.Number != i+1 {
			t.Fatalf("versions[%d].Number = %d, want %d (ascending)", i, v.Number, i+1)
		}
		if v.State != wantState[i] {
			t.Fatalf("versions[%d].State = %q, want %q", i, v.State, wantState[i])
		}
	}
	if newestID == "" {
		t.Fatal("seed returned no newest version id")
	}

	// Agents grouped per template; the template-less agent belongs to neither.
	if len(planner.Agents) != 2 || !containsString(planner.Agents, "planner-agent") || !containsString(planner.Agents, "planner-two") {
		t.Fatalf("planner agents = %v, want planner-agent and planner-two (an @latest ref splits to the base id)", planner.Agents)
	}
	if len(helper.Agents) != 1 || helper.Agents[0] != "helper-agent" {
		t.Fatalf("helper agents = %v, want [helper-agent]", helper.Agents)
	}
	if helper.CurrentVersion != 1 {
		t.Fatalf("helper current = v%d, want v1", helper.CurrentVersion)
	}

	// #1752: the candidate/canary layer is gone, so these are always zero — and
	// Pending must be an empty slice, never nil.
	for _, tmpl := range skills.Templates {
		if tmpl.Pending == nil {
			t.Fatalf("%s Pending is nil; it must serialize as an empty JSON array", tmpl.TemplateID)
		}
		if len(tmpl.Pending) != 0 || tmpl.CanarySample != 0 || tmpl.CanaryStartedAt != 0 {
			t.Fatalf("%s still reports candidate/canary state: %+v", tmpl.TemplateID, tmpl)
		}
		for i, v := range tmpl.Versions {
			if v.HasScore || v.Score != 0 {
				t.Fatalf("%s versions[%d] carries a score %v; the review layer that produced scores is gone", tmpl.TemplateID, i, v.Score)
			}
		}
	}
	if skills.ActiveCanaries != 0 || skills.PendingTotal != 0 {
		t.Fatalf("rollups = canaries%d pending%d, want 0/0", skills.ActiveCanaries, skills.PendingTotal)
	}
}

// TestSkillTemplateFromVersionsSortsAscending feeds the mapper DESCENDING version
// rows so the ascending-order contract depends on the defensive sort rather than on
// the store's ORDER BY. Going through the store path cannot test this:
// ListAgentTemplateVersions always returns ascending, so the sort is unobservable
// there and deleting it would leave an end-to-end test green.
//
// Deleting the sort.SliceStable in skillTemplateFromVersions fails this test.
func TestSkillTemplateFromVersionsSortsAscending(t *testing.T) {
	tmpl := db.AgentTemplate{ID: "planner", Name: "Planner", VersionNumber: 2, VersionState: "current"}
	descending := []db.AgentTemplateVersion{
		{VersionNumber: 3, State: "superseded"},
		{VersionNumber: 2, State: "current"},
		{VersionNumber: 1, State: "superseded"},
	}

	st := skillTemplateFromVersions(tmpl, []string{"planner-agent"}, descending)

	if len(st.Versions) != 3 {
		t.Fatalf("versions = %d, want 3: %+v", len(st.Versions), st.Versions)
	}
	for i, v := range st.Versions {
		if v.Number != i+1 {
			t.Fatalf("versions[%d].Number = %d, want %d; descending input must be sorted ascending", i, v.Number, i+1)
		}
	}
	// The states must travel with their own version, not be re-paired by the sort.
	if st.Versions[1].State != "current" {
		t.Fatalf("versions[1].State = %q, want current (state must follow its version through the sort)", st.Versions[1].State)
	}
	// Shuffled (not merely reversed) input sorts too.
	shuffled := []db.AgentTemplateVersion{
		{VersionNumber: 2, State: "current"},
		{VersionNumber: 3, State: "superseded"},
		{VersionNumber: 1, State: "superseded"},
	}
	st = skillTemplateFromVersions(tmpl, nil, shuffled)
	for i, v := range st.Versions {
		if v.Number != i+1 {
			t.Fatalf("shuffled versions[%d].Number = %d, want %d", i, v.Number, i+1)
		}
	}
	// A nil version list still yields the initialized empty slices the dashboard
	// contract requires (the fail-open path buildSkillTemplate uses on a read error).
	st = skillTemplateFromVersions(tmpl, nil, nil)
	if st.Versions == nil || len(st.Versions) != 0 || st.Pending == nil {
		t.Fatalf("nil versions must yield empty non-nil slices: %+v", st)
	}
}
