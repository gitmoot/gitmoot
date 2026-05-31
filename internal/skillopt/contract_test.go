package skillopt

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/db"
)

func TestExportTrainingPackage(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := testTemplate("planner", "Plan carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	blobHash := artifact.ContentHash([]byte("baseline output"))
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{
		ID:        "baseline",
		Hash:      blobHash,
		MediaType: "text/markdown",
		SizeBytes: int64(len("baseline output")),
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact returned error: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "run-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "ready",
		MetadataJSON:      `{"driver":"planner"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(ctx, db.EvalReviewItem{
		RunID:              "run-1",
		ItemID:             "item-001",
		Title:              "README",
		BaselineArtifactID: "baseline",
		MetadataJSON:       `{"path":"README.md"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}

	pkg, err := ExportTrainingPackage(ctx, store, "run-1")
	if err != nil {
		t.Fatalf("ExportTrainingPackage returned error: %v", err)
	}

	if pkg.Kind != TrainingPackageKind || pkg.ContractVersion != ContractVersion {
		t.Fatalf("package header = %s v%d", pkg.Kind, pkg.ContractVersion)
	}
	if pkg.Template.ID != "planner" || pkg.Template.VersionID != installed.VersionID || pkg.Template.Content == "" {
		t.Fatalf("template snapshot = %+v", pkg.Template)
	}
	if len(pkg.Items) != 1 || pkg.Items[0].BaselineArtifactID != "baseline" {
		t.Fatalf("items = %+v", pkg.Items)
	}
	if len(pkg.Artifacts) != 1 || pkg.Artifacts[0].Hash != blobHash {
		t.Fatalf("artifacts = %+v", pkg.Artifacts)
	}
	if string(pkg.EvaluatorConfig) != `{"driver":"planner"}` {
		t.Fatalf("evaluator config = %s", string(pkg.EvaluatorConfig))
	}
	if _, err := json.Marshal(pkg); err != nil {
		t.Fatalf("exported package did not marshal: %v", err)
	}
}

func TestImportCandidatePackageCreatesPendingVersion(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	template := testTemplate("planner", "Plan carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{
		ID:        "candidate-diff",
		Hash:      artifact.ContentHash([]byte("diff")),
		MediaType: "text/plain",
		SizeBytes: int64(len("diff")),
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact returned error: %v", err)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	candidateContent := testTemplateContent("planner", "Plan carefully with a concise risk section.")
	parsed, err := agenttemplate.ParseTemplateContent(candidateContent)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}

	version, err := ImportCandidatePackage(ctx, store, CandidatePackage{
		Kind:            CandidatePackageKind,
		ContractVersion: ContractVersion,
		TemplateID:      "planner",
		BaseVersionID:   current.VersionID,
		Candidate: CandidateTemplate{
			Content:  candidateContent,
			Metadata: parsed.Metadata,
		},
		EvalReport: json.RawMessage(`{"score":0.82}`),
		Summary: CandidateSummary{
			DiffArtifactID:    "candidate-diff",
			PreferenceSummary: "Candidate is more actionable.",
		},
	}, "candidate.json")
	if err != nil {
		t.Fatalf("ImportCandidatePackage returned error: %v", err)
	}

	if version.State != "pending" || version.TemplateID != "planner" {
		t.Fatalf("candidate version = %+v", version)
	}
	after, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate after import returned error: %v", err)
	}
	if after.VersionID != current.VersionID || after.Content != current.Content {
		t.Fatalf("current template changed: before=%+v after=%+v", current, after)
	}
	latest, err := store.GetAgentTemplateReference(ctx, "planner@latest")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference latest returned error: %v", err)
	}
	if latest.VersionID != version.ID || latest.Content != candidateContent {
		t.Fatalf("latest template = %+v", latest)
	}
}

func testTemplate(id string, body string) db.AgentTemplate {
	content := testTemplateContent(id, body)
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

func testTemplateContent(id string, body string) string {
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
