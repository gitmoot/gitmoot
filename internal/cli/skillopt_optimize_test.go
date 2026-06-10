package cli

import (
	"context"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestBuildAgentOptimizeFieldsSkipsTemplate(t *testing.T) {
	fields := buildAgentOptimizeFields()
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.Name == "template" {
			t.Fatal("the optimize form must not ask for the template (pre-filled from the agent)")
		}
		names = append(names, f.Name)
	}
	want := []string{"name", "review_repo", "workspace_repo", "artifact_kind", "preview", "request", "backend", "model"}
	if len(names) != len(want) {
		t.Fatalf("fields = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("fields = %v, want %v", names, want)
		}
	}
}

func TestAgentOptimizeInterpretModelOptional(t *testing.T) {
	if value, status := agentOptimizeInterpret("model", "  "); status != "ok" || value != "" {
		t.Fatalf("empty model = (%q, %q), want optional ok", value, status)
	}
	if _, status := agentOptimizeInterpret("name", " "); status != "reask" {
		t.Fatalf("empty name should reask, got %q", status)
	}
	if _, status := agentOptimizeInterpret("workspace_repo", "not-a-repo"); status != "reask" {
		t.Fatalf("malformed workspace repo should reask, got %q", status)
	}
}

func TestStartAgentOptimizeSessionPersistsBackendAndModel(t *testing.T) {
	home := t.TempDir()
	_ = chdirTemp(t)
	restoreFetcher := replaceAgentTemplateFetcher(fakeAgentTemplateFetcher{
		commit:  "abc123",
		content: "# Gitmoot Planner\n\nPlan work.",
	})
	defer restoreFetcher()
	fakeGH := &repoCreateFakeGitHub{}
	restoreGH := replaceSkillOptGitHubClient(fakeGH)
	defer restoreGH()

	sessionID, err := startAgentOptimizeSession(home, "planner", map[string]string{
		"name":           "opt-planner",
		"review_repo":    "owner/review",
		"workspace_repo": "owner/workspace",
		"artifact_kind":  "text",
		"preview":        "none",
		"request":        "Make plans sharper.",
		"backend":        "claude",
		"model":          "claude-opus-4-8",
	})
	if err != nil {
		t.Fatalf("startAgentOptimizeSession: %v", err)
	}
	if sessionID == "" {
		t.Fatal("expected a session id")
	}

	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	session, err := store.GetSkillOptTrainSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSkillOptTrainSession: %v", err)
	}
	if session.TemplateID != "planner" || session.TargetRepo != "owner/review" {
		t.Fatalf("session = %+v", session)
	}
	for key, want := range map[string]string{
		"optimizer_backend": "claude",
		"target_backend":    "claude",
		"evaluator_backend": "claude",
		"optimizer_model":   "claude-opus-4-8",
		"target_model":      "claude-opus-4-8",
	} {
		if got := skillOptMetadataString(session.MetadataJSON, "optimizer_defaults", key); got != want {
			t.Fatalf("optimizer_defaults.%s = %q, want %q (metadata=%s)", key, got, want, session.MetadataJSON)
		}
	}

	// The continue path picks the choices up when flags are absent…
	var request skillOptTrainOptimizerRequest
	applySkillOptTrainOptimizerDefaultsFromMetadata(session.MetadataJSON, &request)
	if request.OptimizerBackend != "claude" || request.TargetBackend != "claude" {
		t.Fatalf("applied backends = %+v", request)
	}
	if request.OptimizerModel != "claude-opus-4-8" || request.TargetModel != "claude-opus-4-8" {
		t.Fatalf("applied models = %+v", request)
	}
	// …and explicit flags still win.
	explicit := skillOptTrainOptimizerRequest{Backend: "codex", Model: "gpt-5"}
	applySkillOptTrainOptimizerDefaultsFromMetadata(session.MetadataJSON, &explicit)
	if explicit.Backend != "codex" || explicit.OptimizerBackend != "" {
		t.Fatalf("explicit backend overridden: %+v", explicit)
	}
	if explicit.Model != "gpt-5" || explicit.OptimizerModel != "" || explicit.TargetModel != "" {
		t.Fatalf("explicit model overridden: %+v", explicit)
	}

	// --create-repos records the repos against the session, so deleting the
	// session later offers their cleanup.
	records, err := store.ListCreatedReposForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListCreatedReposForSession: %v", err)
	}
	repos := map[string]bool{}
	for _, record := range records {
		repos[record.Repo] = true
	}
	if !repos["owner/review"] || !repos["owner/workspace"] {
		t.Fatalf("created repo records = %v", records)
	}
}
