package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// seedCanaryTemplate installs a template (v1 = current champion) and adds a fresh
// pending v2 candidate, returning the champion v1 and the pending v2.
func seedCanaryTemplate(t *testing.T, store *Store) (champion, pending AgentTemplateVersion) {
	t.Helper()
	ctx := context.Background()
	base := AgentTemplate{ID: "planner", Name: "Planner", SourceRepo: "o/r", SourceRef: "main", SourcePath: "p.md", ResolvedCommit: "commit-v1", Content: "v1 content"}
	if err := store.UpsertAgentTemplate(ctx, base); err != nil {
		t.Fatalf("upsert template: %v", err)
	}
	v1, err := store.GetLatestAgentTemplateVersion(ctx, "planner")
	if err != nil {
		t.Fatalf("latest v1: %v", err)
	}
	v2Template := base
	v2Template.Content = "v2 content"
	v2Template.ResolvedCommit = "commit-v2"
	v2, err := store.AddPendingAgentTemplateVersion(ctx, v2Template)
	if err != nil {
		t.Fatalf("add v2: %v", err)
	}
	championV1, err := store.GetAgentTemplateVersionByID(ctx, v1.VersionID)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	return championV1, v2
}

// TestCanaryPromoteLeavesChampionCurrent proves CanaryPromoteAgentTemplateVersion
// (#484) transitions the pending candidate to `canary` WITHOUT changing the
// template's current_version_id: the champion stays current, the canary records
// its sample/window, and the canary never becomes latest_version_id.
func TestCanaryPromoteLeavesChampionCurrent(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	champion, pending := seedCanaryTemplate(t, store)

	canary, err := store.CanaryPromoteAgentTemplateVersion(ctx, pending.ID, 0.2)
	if err != nil {
		t.Fatalf("canary promote: %v", err)
	}
	if canary.State != "canary" {
		t.Fatalf("canary state = %q, want canary", canary.State)
	}
	if canary.CanarySample != 0.2 {
		t.Fatalf("canary sample = %v, want 0.2", canary.CanarySample)
	}
	if canary.CanaryStartedAt == "" {
		t.Fatal("canary_started_at must be set")
	}
	// The champion is STILL current; the live template content is unchanged.
	tmpl, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if tmpl.VersionID != champion.ID {
		t.Fatalf("current version = %q, want champion %q", tmpl.VersionID, champion.ID)
	}
	if tmpl.Content != "v1 content" {
		t.Fatalf("live content = %q, want v1 content (champion stays current)", tmpl.Content)
	}
	// The canary must NOT be latest_version_id (it would otherwise leak into latest).
	latest, err := store.GetLatestAgentTemplateVersion(ctx, "planner")
	if err != nil {
		t.Fatalf("get latest: %v", err)
	}
	if latest.VersionID == canary.ID {
		t.Fatalf("canary must not be latest_version_id, got %q", latest.VersionID)
	}
	// GetActiveCanaryVersion finds it.
	got, found, err := store.GetActiveCanaryVersion(ctx, "planner")
	if err != nil || !found {
		t.Fatalf("GetActiveCanaryVersion found=%v err=%v", found, err)
	}
	if got.ID != canary.ID {
		t.Fatalf("active canary = %q, want %q", got.ID, canary.ID)
	}
}

// TestCanaryPromoteRejectsSecondCanary proves at most one active canary per template.
func TestCanaryPromoteRejectsSecondCanary(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	_, pending := seedCanaryTemplate(t, store)
	if _, err := store.CanaryPromoteAgentTemplateVersion(ctx, pending.ID, 0.2); err != nil {
		t.Fatalf("first canary: %v", err)
	}
	// A second pending candidate cannot also become a canary while one is active.
	base := AgentTemplate{ID: "planner", Name: "Planner", SourceRepo: "o/r", SourceRef: "main", SourcePath: "p.md", ResolvedCommit: "commit-v3", Content: "v3 content"}
	v3, err := store.AddPendingAgentTemplateVersion(ctx, base)
	if err != nil {
		t.Fatalf("add v3: %v", err)
	}
	if _, err := store.CanaryPromoteAgentTemplateVersion(ctx, v3.ID, 0.2); err == nil {
		t.Fatal("a second active canary must be refused")
	}
}

// TestCanaryGraduate proves PromoteAgentTemplateVersion (#484-extended) graduates a
// canary to current and supersedes the prior champion, and that the EXISTING
// RevertAgentTemplateVersion can then restore the superseded champion (the
// graduated-regret rollback round-trip).
func TestCanaryGraduate(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	champion, pending := seedCanaryTemplate(t, store)
	canary, err := store.CanaryPromoteAgentTemplateVersion(ctx, pending.ID, 1.0)
	if err != nil {
		t.Fatalf("canary promote: %v", err)
	}

	graduated, err := store.PromoteAgentTemplateVersion(ctx, canary.ID)
	if err != nil {
		t.Fatalf("graduate canary: %v", err)
	}
	if graduated.State != "current" {
		t.Fatalf("graduated state = %q, want current", graduated.State)
	}
	if graduated.CanarySample != 0 || graduated.CanaryStartedAt != "" {
		t.Fatalf("graduated canary must clear canary window, got sample=%v started=%q", graduated.CanarySample, graduated.CanaryStartedAt)
	}
	tmpl, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if tmpl.VersionID != canary.ID || tmpl.Content != "v2 content" {
		t.Fatalf("after graduate current = %q content = %q, want canary v2", tmpl.VersionID, tmpl.Content)
	}
	// The prior champion is now superseded; no active canary remains.
	championAfter, err := store.GetAgentTemplateVersionByID(ctx, champion.ID)
	if err != nil {
		t.Fatalf("get champion: %v", err)
	}
	if championAfter.State != "superseded" {
		t.Fatalf("champion state after graduate = %q, want superseded", championAfter.State)
	}
	if _, found, _ := store.GetActiveCanaryVersion(ctx, "planner"); found {
		t.Fatal("no canary should remain active after graduate")
	}
	// Graduated-regret rollback: the EXISTING RevertAgentTemplateVersion restores the
	// superseded champion as current.
	reverted, err := store.RevertAgentTemplateVersion(ctx, "planner", champion.ID)
	if err != nil {
		t.Fatalf("revert to champion: %v", err)
	}
	if reverted.State != "current" {
		t.Fatalf("reverted champion state = %q, want current", reverted.State)
	}
}

// TestCanaryRollbackKeepsChampion proves the daemon's rollback shape at the store
// level: rejecting the canary leaves the champion as the live current version (the
// canary never displaced it), and the reject is idempotent.
func TestCanaryRollbackKeepsChampion(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	champion, pending := seedCanaryTemplate(t, store)
	canary, err := store.CanaryPromoteAgentTemplateVersion(ctx, pending.ID, 0.5)
	if err != nil {
		t.Fatalf("canary promote: %v", err)
	}

	rejected, err := store.RejectAgentTemplateVersion(ctx, canary.ID, "auto-rollback: regression")
	if err != nil {
		t.Fatalf("reject canary: %v", err)
	}
	if rejected.State != "rejected" {
		t.Fatalf("rejected canary state = %q, want rejected", rejected.State)
	}
	if rejected.CanarySample != 0 || rejected.CanaryStartedAt != "" {
		t.Fatalf("rejected canary must clear window, got sample=%v started=%q", rejected.CanarySample, rejected.CanaryStartedAt)
	}
	// The champion is still current with its content; never left without a current.
	tmpl, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if tmpl.VersionID != champion.ID || tmpl.Content != "v1 content" {
		t.Fatalf("after rollback current = %q content = %q, want champion v1", tmpl.VersionID, tmpl.Content)
	}
	// Idempotent: rejecting an already-rejected canary is a no-op success.
	if _, err := store.RejectAgentTemplateVersion(ctx, canary.ID, "again"); err != nil {
		t.Fatalf("re-reject (idempotent) returned error: %v", err)
	}
	if _, found, _ := store.GetActiveCanaryVersion(ctx, "planner"); found {
		t.Fatal("no canary should remain active after rollback")
	}
}

// TestCanaryPromoteRequiresPending proves only a pending candidate can become a
// canary (a superseded/current/rejected target is refused) and the sample must be
// in (0,1].
func TestCanaryPromoteValidation(t *testing.T) {
	store := openCockpitStore(t)
	ctx := context.Background()
	champion, pending := seedCanaryTemplate(t, store)
	if _, err := store.CanaryPromoteAgentTemplateVersion(ctx, pending.ID, 0); err == nil {
		t.Fatal("sample 0 must be refused")
	}
	if _, err := store.CanaryPromoteAgentTemplateVersion(ctx, pending.ID, 1.5); err == nil {
		t.Fatal("sample > 1 must be refused")
	}
	// The current champion is not pending and cannot become a canary.
	if _, err := store.CanaryPromoteAgentTemplateVersion(ctx, champion.ID, 0.2); err == nil {
		t.Fatal("a non-pending target must be refused")
	}
}

// TestCanaryMigrationOnPreExistingDB proves the #484 columns are added with safe
// DEFAULTs on a pre-existing DB: a row inserted before the canary migration reads
// canary_sample=0 / canary_started_at="" after the upgrade, and the migration is
// idempotent (re-Open does not re-apply).
func TestCanaryMigrationOnPreExistingDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gitmoot.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Apply every migration EXCEPT the last (the #484 canary migration), then seed a
	// version row through the schema-before-canary.
	store := &Store{db: raw}
	for version, migration := range migrations[:len(migrations)-1] {
		if err := store.applyMigration(ctx, version+1, migration); err != nil {
			t.Fatalf("applyMigration(%d): %v", version+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO agent_template_versions(id, template_id, version, state, name, source_repo, source_ref, source_path, resolved_commit, content)
		VALUES ('planner@v1', 'planner', 1, 'current', 'Planner', 'o/r', 'main', 'p.md', 'abc', 'v1')`); err != nil {
		t.Fatalf("seed legacy version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer upgraded.Close()
	v, err := upgraded.GetAgentTemplateVersionByID(ctx, "planner@v1")
	if err != nil {
		t.Fatalf("get migrated version: %v", err)
	}
	if v.CanarySample != 0 || v.CanaryStartedAt != "" {
		t.Fatalf("migrated row canary defaults = (%v, %q), want (0, \"\")", v.CanarySample, v.CanaryStartedAt)
	}
	// Idempotent: re-opening applies nothing new.
	again, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open (idempotent migrate): %v", err)
	}
	again.Close()
}
