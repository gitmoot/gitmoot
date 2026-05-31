package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

func TestSkillOptExportAndImportCommands(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	template := cliSkillOptTemplate("planner", "Plan the work.")
	if err := store.UpsertAgentTemplate(context.Background(), template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "baseline",
		Hash:      artifact.ContentHash([]byte("baseline")),
		MediaType: "text/markdown",
		SizeBytes: int64(len("baseline")),
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact returned error: %v", err)
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "run-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "ready",
		MetadataJSON:      `{"driver":"planner"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:              "run-1",
		ItemID:             "item-001",
		BaselineArtifactID: "baseline",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	exportPath := filepath.Join(t.TempDir(), "training.json")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "export", "--home", home, "--run", "run-1", "--output", exportPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt export exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "exported run-1") {
		t.Fatalf("export stdout = %q", stdout.String())
	}
	exportedContent, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var training skillopt.TrainingPackage
	if err := json.Unmarshal(exportedContent, &training); err != nil {
		t.Fatalf("decode training package: %v\n%s", err, string(exportedContent))
	}
	if training.Template.VersionID != installed.VersionID || len(training.Items) != 1 || len(training.Artifacts) != 1 {
		t.Fatalf("training package = %+v", training)
	}

	candidateContent := cliSkillOptTemplateContent("planner", "Plan the work and include risks.")
	parsed, err := agenttemplate.ParseTemplateContent(candidateContent)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}
	candidate := skillopt.CandidatePackage{
		Kind:            skillopt.CandidatePackageKind,
		ContractVersion: skillopt.ContractVersion,
		TemplateID:      "planner",
		BaseVersionID:   installed.VersionID,
		Candidate: skillopt.CandidateTemplate{
			Content:  candidateContent,
			Metadata: parsed.Metadata,
		},
	}
	candidatePath := filepath.Join(t.TempDir(), "candidate.json")
	encodedCandidate, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	if err := os.WriteFile(candidatePath, encodedCandidate, 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "import", "--home", home, "--file", candidatePath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt import exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported pending candidate planner@v2") {
		t.Fatalf("import stdout = %q", stdout.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after import returned error: %v", err)
	}
	defer store.Close()
	current, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate current returned error: %v", err)
	}
	if current.VersionID != installed.VersionID {
		t.Fatalf("current version = %q, want %q", current.VersionID, installed.VersionID)
	}
	latest, err := store.GetAgentTemplateReference(context.Background(), "planner@latest")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference latest returned error: %v", err)
	}
	if latest.VersionID != "planner@v2" || latest.Content != candidateContent {
		t.Fatalf("latest = %+v", latest)
	}
}

func cliSkillOptTemplate(id string, body string) db.AgentTemplate {
	content := cliSkillOptTemplateContent(id, body)
	parsed, err := agenttemplate.ParseTemplateContent(content)
	if err != nil {
		panic(err)
	}
	metadataJSON, err := agenttemplate.MarshalMetadata(parsed.Metadata)
	if err != nil {
		panic(err)
	}
	return db.AgentTemplate{
		ID:             id,
		Name:           parsed.Metadata.Name,
		Description:    parsed.Metadata.Description,
		SourceRepo:     agenttemplate.LocalSourceRepo,
		SourceRef:      agenttemplate.LocalSourceRef,
		SourcePath:     id + ".md",
		ResolvedCommit: agenttemplate.HashContent(content),
		Content:        content,
		MetadataJSON:   metadataJSON,
	}
}

func cliSkillOptTemplateContent(id string, body string) string {
	return agenttemplate.FormatTemplateContent(agenttemplate.Metadata{
		ID:                   id,
		Name:                 "Planner",
		Description:          "Plans implementation work.",
		Kind:                 agenttemplate.TemplateKind,
		Version:              agenttemplate.TemplateVersion,
		Capabilities:         []string{"ask"},
		RuntimeCompatibility: []string{"codex"},
		Tags:                 []string{"planning"},
		Inputs:               []string{"task"},
		Outputs:              []string{"plan"},
	}, "# Planner\n\n"+body+"\n")
}
