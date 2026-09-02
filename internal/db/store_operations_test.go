package db

import (
	"context"
	"errors"
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

func TestDeleteAgentChecked(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAgent(ctx, Agent{Name: "worker", Runtime: "codex"}); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}
	if err := store.CreateJob(ctx, Job{ID: "j1", Agent: "worker", Type: "ask", State: "queued"}); err != nil {
		t.Fatalf("job: %v", err)
	}
	// Refused while a queued job references the agent — wraps the sentinel so
	// callers can classify with errors.Is, not message text.
	if err := store.DeleteAgentChecked(ctx, "worker"); err == nil || !errors.Is(err, ErrAgentHasActiveJobs) {
		t.Fatalf("expected job-reference refusal (ErrAgentHasActiveJobs), got %v", err)
	}
	if err := store.UpdateJobState(ctx, "j1", "succeeded"); err != nil {
		t.Fatalf("settle job: %v", err)
	}
	if err := store.DeleteAgentChecked(ctx, "worker"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.DeleteAgentChecked(ctx, "worker"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found on second delete, got %v", err)
	}
}

func TestUpdateAgentRuntime(t *testing.T) {
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

	// Unknown runtime is rejected, leaving the row untouched.
	if err := store.UpdateAgentRuntime(ctx, "worker", "gpt"); err == nil || !strings.Contains(err.Error(), "unknown runtime") {
		t.Fatalf("expected unknown-runtime error, got %v", err)
	}
	// Missing agent errors.
	if err := store.UpdateAgentRuntime(ctx, "ghost", "claude"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected not-registered error, got %v", err)
	}

	if err := store.UpdateAgentRuntime(ctx, "worker", "claude"); err != nil {
		t.Fatalf("switch runtime: %v", err)
	}
	got, err := store.GetAgent(ctx, "worker")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Runtime != "claude" {
		t.Fatalf("runtime = %q, want claude", got.Runtime)
	}
	if got.RuntimeRef != "" {
		t.Fatalf("runtime_ref = %q, want cleared", got.RuntimeRef)
	}
	// Everything else preserved.
	if got.Role != "implement" || got.RepoScope != "owner/repo" || got.TemplateID != "worker-tpl" ||
		got.AutonomyPolicy != "auto" || strings.Join(got.Capabilities, ",") != "implement,review" {
		t.Fatalf("switch runtime altered preserved fields: %+v", got)
	}

	// omp is in the allow-list too (#1428). Re-warm the ref first so the clear is a
	// real observation and not vacuously satisfied by the claude switch above.
	if err := store.UpdateAgentRuntimeRef(ctx, "worker", "sess-def"); err != nil {
		t.Fatalf("re-warm runtime_ref: %v", err)
	}
	if err := store.UpdateAgentRuntime(ctx, "worker", "omp"); err != nil {
		t.Fatalf("switch runtime to omp: %v", err)
	}
	got, err = store.GetAgent(ctx, "worker")
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.Runtime != "omp" {
		t.Fatalf("runtime = %q, want omp", got.Runtime)
	}
	// omp is stateless, so the old runtime's warm ref is doubly meaningless here.
	if got.RuntimeRef != "" {
		t.Fatalf("runtime_ref = %q, want cleared on the omp switch", got.RuntimeRef)
	}
}

func TestLatestJobEvents(t *testing.T) {
	store := openStoreOperationsTestStore(t)
	ctx := context.Background()
	if err := store.CreateJob(ctx, Job{ID: "job-a", Agent: "planner", Type: "ask", State: "failed"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := store.CreateJob(ctx, Job{ID: "job-b", Agent: "planner", Type: "review", State: "queued"}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for _, event := range []JobEvent{
		{JobID: "job-a", Kind: "queued", Message: "created"},
		{JobID: "job-a", Kind: "failed", Message: "boom"},
		{JobID: "job-b", Kind: "queued", Message: "created"},
	} {
		if err := store.AddJobEvent(ctx, event); err != nil {
			t.Fatalf("AddJobEvent: %v", err)
		}
	}
	latest, err := store.LatestJobEvents(ctx)
	if err != nil {
		t.Fatalf("LatestJobEvents: %v", err)
	}
	if got := latest["job-a"]; got.Kind != "failed" || got.Message != "boom" {
		t.Fatalf("job-a latest = %+v", got)
	}
	if got := latest["job-b"]; got.Kind != "queued" || got.Message != "created" {
		t.Fatalf("job-b latest = %+v", got)
	}
	if _, ok := latest["job-missing"]; ok {
		t.Fatal("jobs without events must be absent from the map")
	}
}
