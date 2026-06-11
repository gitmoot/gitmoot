package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestBuildOptimizerCommandDefaultsCodexModel(t *testing.T) {
	prev := skillOptTrainOptimizerRunner
	skillOptTrainOptimizerRunner = &skillOptTrainFakeOptimizerRunner{}
	defer func() { skillOptTrainOptimizerRunner = prev }()

	// A configured codex model the default should pick up.
	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model = \"gpt-5.5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)

	build := func(req skillOptTrainOptimizerRequest) []string {
		req.SkillOptBin = "gitmoot-skillopt"
		_, args, err := buildSkillOptTrainOptimizerCommand(db.SkillOptTrainIteration{}, req, skillOptTrainOptimizerPaths{})
		if err != nil {
			t.Fatalf("buildSkillOptTrainOptimizerCommand: %v", err)
		}
		return args
	}

	// codex preset, empty model → optimizer AND target default to the codex model.
	// The preset resolves the optimizer to "codex" and the target to "codex_exec";
	// both pass an explicit model to codex, so both need the override. The
	// evaluator inherits the optimizer model in gitmoot-skillopt (no flag emitted).
	args := build(skillOptTrainOptimizerRequest{Backend: "codex"})
	for _, flag := range []string{"--optimizer-model", "--target-model"} {
		if got := argValue(args, flag); got != "gpt-5.5" {
			t.Fatalf("codex+empty: %s = %q, want gpt-5.5 (args=%v)", flag, got, args)
		}
	}
	if got := argValue(args, "--evaluator-model"); got != "" {
		t.Fatalf("evaluator should inherit the optimizer model, not get an explicit flag: %q", got)
	}

	// An explicit model still wins for optimizer + target.
	args = build(skillOptTrainOptimizerRequest{Backend: "codex", Model: "o3"})
	for _, flag := range []string{"--optimizer-model", "--target-model"} {
		if got := argValue(args, flag); got != "o3" {
			t.Fatalf("explicit model overridden: %s = %q, want o3", flag, got)
		}
	}

	// A non-codex backend gets no codex-derived default.
	args = build(skillOptTrainOptimizerRequest{OptimizerBackend: "openai_chat"})
	if got := argValue(args, "--optimizer-model"); got == "gpt-5.5" {
		t.Fatalf("non-codex backend must not pick up the codex model: args=%v", args)
	}
}

func TestRemoveSkillOptTrainTargetAgents(t *testing.T) {
	home := t.TempDir()
	if err := config.Initialize(config.PathsForHome(home)); err != nil {
		t.Fatalf("init: %v", err)
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	sid := "train-plan-20260101-000000-111"
	otherTarget := "skillopt-target-train-other-20260101-000000-222-item-001-generator-ccc"
	for _, name := range []string{
		"skillopt-target-" + sid + "-item-001-generator-aaa",
		"skillopt-target-" + sid + "-item-002-generator-bbb",
		"planner",   // a real user agent
		otherTarget, // another session's plumbing
	} {
		if err := store.UpsertAgent(ctx, db.Agent{Name: name, Runtime: "codex"}); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
	}

	removeSkillOptTrainTargetAgents(ctx, store, sid)

	got, err := store.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	names := map[string]bool{}
	for _, a := range got {
		names[a.Name] = true
	}
	// This session's target agents are gone; the user agent and the other
	// session's plumbing remain.
	if names["skillopt-target-"+sid+"-item-001-generator-aaa"] || names["skillopt-target-"+sid+"-item-002-generator-bbb"] {
		t.Fatalf("session target agents should be removed: %v", names)
	}
	if !names["planner"] || !names[otherTarget] {
		t.Fatalf("unrelated agents must remain: %v", names)
	}
}

func TestBuildAgentOptimizeFieldsSkipsTemplate(t *testing.T) {
	fields := buildAgentOptimizeFields("", nil)
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.Name == "template" {
			t.Fatal("the optimize form must not ask for the template (pre-filled from the agent)")
		}
		names = append(names, f.Name)
	}
	want := []string{"name", "review_repo", "workspace_repo", "items", "artifact_kind", "preview", "request", "backend", "model"}
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
		"items":          "4",
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

	// The requested item count flows through the scaffold into the session.
	items, err := store.ListEvalReviewItems(ctx, sessionID+"-review-001")
	if err != nil {
		t.Fatalf("ListEvalReviewItems: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("review items = %d, want 4", len(items))
	}
}

func TestStarterReviewItemsYAMLCount(t *testing.T) {
	values := skillOptTrainInitInputs{ArtifactKind: "text"}
	// N=2 stays byte-identical to the historical fixed output.
	if got, want := string(skillOptTrainInitStarterReviewItemsYAMLN(values, 2)), string(skillOptTrainInitStarterReviewItemsYAML(values)); got != want {
		t.Fatalf("N=2 output drifted:\n%s", got)
	}
	four := string(skillOptTrainInitStarterReviewItemsYAMLN(values, 4))
	for _, want := range []string{"item-001", "item-002", "item-003", "item-004", "Variation scenario 4"} {
		if !strings.Contains(four, want) {
			t.Fatalf("N=4 missing %q:\n%s", want, four)
		}
	}
	// Below the floor clamps to 2.
	if got := string(skillOptTrainInitStarterReviewItemsYAMLN(values, 1)); strings.Contains(got, "item-003") || !strings.Contains(got, "item-002") {
		t.Fatalf("floor clamp wrong:\n%s", got)
	}
}

func TestRepoPickerChoices(t *testing.T) {
	if skillOptRepoPickerChoices(nil) != nil {
		t.Fatal("no repos → free text (nil choices)")
	}
	choices := skillOptRepoPickerChoices([]string{"o/a", "o/b"})
	if len(choices) != 3 || choices[0].Value != "o/a" || choices[1].Value != "o/b" {
		t.Fatalf("choices = %+v", choices)
	}
	last := choices[2]
	if !last.Custom || last.Placeholder != "owner/repo" {
		t.Fatalf("trailing custom entry wrong: %+v", last)
	}
}

func TestAgentOptimizeInterpretItems(t *testing.T) {
	if _, status := agentOptimizeInterpret("items", "1"); status != "reask" {
		t.Fatal("items below 2 must reask")
	}
	if _, status := agentOptimizeInterpret("items", "abc"); status != "reask" {
		t.Fatal("non-numeric items must reask")
	}
	if value, status := agentOptimizeInterpret("items", " 5 "); status != "ok" || value != "5" {
		t.Fatalf("items 5 = (%q, %q)", value, status)
	}
}
