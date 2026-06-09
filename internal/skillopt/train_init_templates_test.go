package skillopt

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestListTrainInitTemplateChoicesIncludesBuiltinInstalledAndAgents(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(ctx, db.AgentTemplate{
		ID:             "custom-writer",
		Name:           "Custom Writer",
		Description:    "Writes short posts.",
		SourceRepo:     "local",
		SourceRef:      "file",
		SourcePath:     "/tmp/custom.md",
		ResolvedCommit: "sha256:custom",
		Content:        trainInitTemplateTestContent("custom-writer", "Custom Writer"),
		MetadataJSON:   `{"id":"custom-writer","name":"Custom Writer","description":"Writes short posts.","kind":"agent-template","version":1,"capabilities":["ask"],"runtime_compatibility":["codex"],"tags":["writing"],"inputs":["post"],"outputs":["reply"]}`,
	}); err != nil {
		t.Fatalf("UpsertAgentTemplate custom returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:         "writer",
		Role:         "writer",
		Runtime:      "codex",
		TemplateID:   "custom-writer",
		Capabilities: []string{"ask"},
	}); err != nil {
		t.Fatalf("UpsertAgent writer returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:         "versioned-writer",
		Role:         "writer",
		Runtime:      "codex",
		TemplateID:   "custom-writer@v1",
		Capabilities: []string{"ask"},
	}); err != nil {
		t.Fatalf("UpsertAgent versioned writer returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:         "missing-agent",
		Role:         "writer",
		Runtime:      "codex",
		TemplateID:   "missing-template",
		Capabilities: []string{"ask"},
	}); err != nil {
		t.Fatalf("UpsertAgent missing returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:         "retired-agent",
		Role:         "planner",
		Runtime:      "codex",
		TemplateID:   "planner-here",
		Capabilities: []string{"ask"},
	}); err != nil {
		t.Fatalf("UpsertAgent retired returned error: %v", err)
	}

	choices, err := ListTrainInitTemplateChoices(ctx, store)
	if err != nil {
		t.Fatalf("ListTrainInitTemplateChoices returned error: %v", err)
	}
	planner := trainInitChoiceByID(t, choices, "planner")
	if planner.Source != TrainInitTemplateChoiceSourceBuiltin || planner.Installed || planner.Label == "" {
		t.Fatalf("planner choice = %+v", planner)
	}
	custom := trainInitChoiceByID(t, choices, "custom-writer")
	if custom.Source != TrainInitTemplateChoiceSourceInstalled || !custom.Installed || !custom.Current || custom.CurrentVersion != "custom-writer@v1" || len(custom.Agents) != 2 || custom.Agents[0] != "versioned-writer" || custom.Agents[1] != "writer" {
		t.Fatalf("custom choice = %+v", custom)
	}
	if _, ok := trainInitFindChoiceByID(choices, "custom-writer@v1"); ok {
		t.Fatalf("versioned agent template ref should be grouped under logical template id: %+v", choices)
	}
	if custom.Metadata.ID != "custom-writer" || len(custom.Metadata.Outputs) != 1 || custom.Metadata.Outputs[0] != "reply" {
		t.Fatalf("custom metadata = %+v", custom.Metadata)
	}
	missing := trainInitChoiceByID(t, choices, "missing-template")
	if missing.Source != TrainInitTemplateChoiceSourceAgent || missing.Installed || len(missing.Agents) != 1 || missing.Agents[0] != "missing-agent" {
		t.Fatalf("missing choice = %+v", missing)
	}
	if _, ok := trainInitFindChoiceByID(choices, "planner-here"); ok {
		t.Fatalf("retired agent-only template should be hidden: %+v", choices)
	}
}

func TestResolveTrainInitTemplateChoiceInstallsAvailableBuiltin(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	template, err := ResolveTrainInitTemplateChoice(ctx, store, trainInitFakeFetcher{content: "# Gitmoot Planner\n\nPlan."}, "planner")
	if err != nil {
		t.Fatalf("ResolveTrainInitTemplateChoice returned error: %v", err)
	}
	if template.ID != "planner" || template.VersionID != "planner@v1" || template.ResolvedCommit != "resolved-sha" {
		t.Fatalf("template = %+v", template)
	}
	choices, err := ListTrainInitTemplateChoices(ctx, store)
	if err != nil {
		t.Fatalf("ListTrainInitTemplateChoices returned error: %v", err)
	}
	planner := trainInitChoiceByID(t, choices, "planner")
	if !planner.Installed || !planner.Current || planner.CurrentVersion != "planner@v1" {
		t.Fatalf("planner after install = %+v", planner)
	}
}

func TestResolveTrainInitTemplateChoiceRejectsMissingCustom(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if _, err := ResolveTrainInitTemplateChoice(ctx, store, trainInitFakeFetcher{}, "custom-writer"); err == nil {
		t.Fatal("ResolveTrainInitTemplateChoice succeeded for missing custom template")
	}
}

func trainInitChoiceByID(t *testing.T, choices []TrainInitTemplateChoice, id string) TrainInitTemplateChoice {
	t.Helper()
	if choice, ok := trainInitFindChoiceByID(choices, id); ok {
		return choice
	}
	t.Fatalf("choice %s not found in %+v", id, choices)
	return TrainInitTemplateChoice{}
}

func trainInitFindChoiceByID(choices []TrainInitTemplateChoice, id string) (TrainInitTemplateChoice, bool) {
	for _, choice := range choices {
		if choice.ID == id {
			return choice, true
		}
	}
	return TrainInitTemplateChoice{}, false
}

func trainInitTemplateTestContent(id string, name string) string {
	return "---\nid: " + id + "\nname: " + name + "\ndescription: Test template.\nkind: agent-template\nversion: 1\ncapabilities:\n  - ask\nruntime_compatibility:\n  - codex\ntags:\n  - test\ninputs:\n  - prompt\noutputs:\n  - response\n---\n# " + name + "\n\nTest body.\n"
}

type trainInitFakeFetcher struct {
	content string
}

func (f trainInitFakeFetcher) ResolveRef(context.Context, string, string) (string, error) {
	return "resolved-sha", nil
}

func (f trainInitFakeFetcher) FetchFile(context.Context, string, string, string) (agenttemplate.File, error) {
	return agenttemplate.File{Content: f.content}, nil
}
