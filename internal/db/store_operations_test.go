package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func openStoreOperationsTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestRevertAgentTemplateVersion(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()
	base := AgentTemplate{ID: "planner", Name: "Planner", SourceRepo: "o/r", SourceRef: "main", SourcePath: "p.md", ResolvedCommit: "abc", Content: "v1 content"}
	if err := store.UpsertAgentTemplate(ctx, base); err != nil {
		t.Fatalf("upsert template: %v", err)
	}
	v1, err := store.GetLatestAgentTemplateVersion(ctx, "planner")
	if err != nil {
		t.Fatalf("latest v1: %v", err)
	}
	// Upsert a changed v2: UpsertAgentTemplate mints it `current` and supersedes v1,
	// which is how a second version now comes to exist — the pending/promote candidate
	// path went with the SkillOpt loop (#1752).
	v2Template := base
	v2Template.Content = "v2 content"
	v2Template.ResolvedCommit = "def"
	if err := store.UpsertAgentTemplate(ctx, v2Template); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	v2, err := store.GetLatestAgentTemplateVersion(ctx, "planner")
	if err != nil {
		t.Fatalf("latest v2: %v", err)
	}
	if v2.VersionID == v1.VersionID {
		t.Fatalf("v2 did not mint a new version: %q", v2.VersionID)
	}

	// Revert to v1.
	reverted, err := store.RevertAgentTemplateVersion(ctx, "planner", v1.VersionID)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if reverted.State != "current" {
		t.Fatalf("reverted state = %q", reverted.State)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if current.Content != "v1 content" {
		t.Fatalf("template content after revert = %q, want v1 content", current.Content)
	}
	// v2 is now superseded.
	v2After, err := store.GetAgentTemplateVersionByID(ctx, v2.VersionID)
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if v2After.State != "superseded" {
		t.Fatalf("v2 state after revert = %q", v2After.State)
	}
	// Reverting a non-superseded version is refused.
	if _, err := store.RevertAgentTemplateVersion(ctx, "planner", v2.VersionID); err != nil {
		t.Fatalf("revert back to v2 (superseded now) should work: %v", err)
	}
	if _, err := store.RevertAgentTemplateVersion(ctx, "planner", v2.VersionID); err == nil {
		t.Fatal("reverting the CURRENT version should be refused")
	}
}

// UpdateAgentRuntimeRef is the self-heal path's re-pin (#443): it updates only
// runtime_ref and leaves every other column alone. Its coverage used to ride
// along inside TestUpdateAgentRuntime, whose subject went with the dashboard
// TUI (#1753), so it is pinned directly here.
func TestUpdateAgentRuntimeRef(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()
	original := Agent{
		Name:           "worker",
		Role:           "implement",
		Runtime:        "codex",
		RuntimeRef:     "sess-abc",
		RepoScope:      "owner/repo",
		TemplateID:     "worker-tpl",
		Capabilities:   []string{"implement", "review"},
		AutonomyPolicy: "auto",
		HealthStatus:   "ok",
	}
	if err := store.UpsertAgent(ctx, original); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}
	if err := store.UpdateAgentRuntimeRef(ctx, "worker", "sess-def"); err != nil {
		t.Fatalf("re-pin runtime_ref: %v", err)
	}
	got, err := store.GetAgent(ctx, "worker")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.RuntimeRef != "sess-def" {
		t.Fatalf("runtime_ref = %q, want sess-def", got.RuntimeRef)
	}
	// In place: the runtime itself and every other field survive the re-pin.
	if got.Runtime != "codex" || got.Role != "implement" || got.RepoScope != "owner/repo" ||
		got.TemplateID != "worker-tpl" || got.AutonomyPolicy != "auto" ||
		strings.Join(got.Capabilities, ",") != "implement,review" {
		t.Fatalf("re-pin altered preserved fields: %+v", got)
	}
	// No agent row matched is an error, not a silent no-op.
	if err := store.UpdateAgentRuntimeRef(ctx, "ghost", "sess-x"); err == nil {
		t.Fatal("re-pinning an unregistered agent must error")
	}
}
