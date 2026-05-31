package skillopt

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	if err := store.UpsertFeedbackEvent(ctx, db.FeedbackEvent{
		RunID:     "run-1",
		ItemID:    "item-001",
		Choice:    "b",
		Reasoning: "More specific.",
		Reviewer:  "jerry",
		Source:    "markdown",
		CreatedAt: "2026-05-31T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
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
	if len(pkg.FeedbackEvents) != 1 || pkg.FeedbackEvents[0].Choice != "b" {
		t.Fatalf("feedback events = %+v", pkg.FeedbackEvents)
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
		BaseVersionID:   "planner@latest",
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
	review, err := store.GetAgentTemplateCandidateReview(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview returned error: %v", err)
	}
	if review.BaseVersionID != current.VersionID || review.DiffArtifactID != "candidate-diff" || review.PreferenceSummary != "Candidate is more actionable." || review.EvalReportJSON != `{"score":0.82}` {
		t.Fatalf("candidate review = %+v", review)
	}
}

func TestImportCandidatePackageWithArtifacts(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(ctx, testTemplate("planner", "Plan carefully.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	artifactDir := t.TempDir()
	diffContent := []byte("candidate diff\n")
	diffHash := artifact.ContentHash(diffContent)
	if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), diffContent, 0o644); err != nil {
		t.Fatalf("write diff artifact: %v", err)
	}
	blobStore := artifact.NewStore(filepath.Join(t.TempDir(), "blobs"))
	candidate := testCandidatePackage(t, "planner", current.VersionID, "Plan carefully with artifact-backed evidence.")
	diffSize := int64(len(diffContent))
	candidate.Summary.DiffArtifactID = "candidate-diff"
	candidate.Artifacts = []CandidateArtifactRef{{
		ID:        "candidate-diff",
		Path:      "candidate.diff.md",
		Hash:      diffHash,
		MediaType: "text/markdown",
		Driver:    "text",
		SizeBytes: &diffSize,
	}}

	version, err := ImportCandidatePackageWithOptions(ctx, store, candidate, CandidateImportOptions{
		SourcePath:  "candidate.json",
		ArtifactDir: artifactDir,
		BlobStore:   blobStore,
	})
	if err != nil {
		t.Fatalf("ImportCandidatePackageWithOptions returned error: %v", err)
	}
	review, err := store.GetAgentTemplateCandidateReview(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview returned error: %v", err)
	}
	if review.DiffArtifactID != "candidate-diff" {
		t.Fatalf("review diff artifact id = %q", review.DiffArtifactID)
	}
	stored, err := store.GetEvalArtifact(ctx, "candidate-diff")
	if err != nil {
		t.Fatalf("GetEvalArtifact returned error: %v", err)
	}
	if stored.Hash != diffHash || stored.SizeBytes != diffSize || stored.MediaType != "text/markdown" || stored.Driver != "text" {
		t.Fatalf("stored artifact = %+v", stored)
	}
	storedContent, err := blobStore.Read(diffHash)
	if err != nil {
		t.Fatalf("Read stored blob returned error: %v", err)
	}
	if string(storedContent) != string(diffContent) {
		t.Fatalf("stored blob content = %q", string(storedContent))
	}
}

func TestImportCandidatePackageArtifactValidationFailsBeforeCandidateState(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		hash        string
		artifactDir string
		writeFile   bool
		wantErr     string
	}{
		{
			name:        "missing artifact dir",
			path:        "candidate.diff.md",
			hash:        artifact.ContentHash([]byte("candidate diff\n")),
			artifactDir: "",
			writeFile:   true,
			wantErr:     "candidate artifacts require --artifact-dir",
		},
		{
			name:        "invalid hash",
			path:        "candidate.diff.md",
			hash:        artifact.ContentHash([]byte("other")),
			artifactDir: "set",
			writeFile:   true,
			wantErr:     "hash is",
		},
		{
			name:        "path traversal",
			path:        "../candidate.diff.md",
			hash:        artifact.ContentHash([]byte("candidate diff\n")),
			artifactDir: "set",
			writeFile:   false,
			wantErr:     "relative path inside artifact-dir",
		},
		{
			name:        "absolute path",
			path:        "/tmp/candidate.diff.md",
			hash:        artifact.ContentHash([]byte("candidate diff\n")),
			artifactDir: "set",
			writeFile:   false,
			wantErr:     "relative path inside artifact-dir",
		},
		{
			name:        "missing file",
			path:        "missing.diff.md",
			hash:        artifact.ContentHash([]byte("candidate diff\n")),
			artifactDir: "set",
			writeFile:   false,
			wantErr:     "no such file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
			if err != nil {
				t.Fatalf("Open returned error: %v", err)
			}
			defer store.Close()
			if err := store.UpsertAgentTemplate(ctx, testTemplate("planner", "Plan carefully.")); err != nil {
				t.Fatalf("UpsertAgentTemplate returned error: %v", err)
			}
			current, err := store.GetAgentTemplate(ctx, "planner")
			if err != nil {
				t.Fatalf("GetAgentTemplate returned error: %v", err)
			}
			artifactDir := ""
			if tt.artifactDir == "set" {
				artifactDir = t.TempDir()
			}
			if tt.writeFile && artifactDir != "" {
				if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), []byte("candidate diff\n"), 0o644); err != nil {
					t.Fatalf("write diff artifact: %v", err)
				}
			}
			candidate := testCandidatePackage(t, "planner", current.VersionID, "Plan carefully with artifact-backed evidence.")
			candidate.Summary.DiffArtifactID = "candidate-diff"
			candidate.Artifacts = []CandidateArtifactRef{{
				ID:        "candidate-diff",
				Path:      tt.path,
				Hash:      tt.hash,
				MediaType: "text/markdown",
				Driver:    "text",
			}}

			_, err = ImportCandidatePackageWithOptions(ctx, store, candidate, CandidateImportOptions{
				SourcePath:  "candidate.json",
				ArtifactDir: artifactDir,
				BlobStore:   artifact.NewStore(filepath.Join(t.TempDir(), "blobs")),
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ImportCandidatePackageWithOptions error = %v, want substring %q", err, tt.wantErr)
			}
			pending, err := store.ListPendingAgentTemplateVersions(ctx, "planner")
			if err != nil {
				t.Fatalf("ListPendingAgentTemplateVersions returned error: %v", err)
			}
			if len(pending) != 0 {
				t.Fatalf("pending versions = %+v, want none", pending)
			}
			if _, err := store.GetEvalArtifact(ctx, "candidate-diff"); err == nil {
				t.Fatalf("candidate artifact was registered despite failed import")
			}
		})
	}
}

func TestImportCandidatePackageRejectsDuplicateCandidateArtifactIDs(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(ctx, testTemplate("planner", "Plan carefully.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	artifactDir := t.TempDir()
	content := []byte("candidate diff\n")
	hash := artifact.ContentHash(content)
	if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), content, 0o644); err != nil {
		t.Fatalf("write diff artifact: %v", err)
	}
	candidate := testCandidatePackage(t, "planner", current.VersionID, "Plan carefully with artifact-backed evidence.")
	candidate.Summary.DiffArtifactID = "candidate-diff"
	candidate.Artifacts = []CandidateArtifactRef{
		{ID: "candidate-diff", Path: "candidate.diff.md", Hash: hash, MediaType: "text/markdown", Driver: "text"},
		{ID: "candidate-diff", Path: "candidate.diff.md", Hash: hash, MediaType: "text/markdown", Driver: "text"},
	}

	_, err = ImportCandidatePackageWithOptions(ctx, store, candidate, CandidateImportOptions{
		SourcePath:  "candidate.json",
		ArtifactDir: artifactDir,
		BlobStore:   artifact.NewStore(filepath.Join(t.TempDir(), "blobs")),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("ImportCandidatePackageWithOptions error = %v, want duplicate id", err)
	}
	pending, err := store.ListPendingAgentTemplateVersions(ctx, "planner")
	if err != nil {
		t.Fatalf("ListPendingAgentTemplateVersions returned error: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending versions = %+v, want none", pending)
	}
}

func TestImportCandidatePackageRejectsExistingCandidateArtifactID(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAgentTemplate(ctx, testTemplate("planner", "Plan carefully.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	originalHash := artifact.ContentHash([]byte("old diff\n"))
	if err := store.UpsertEvalArtifact(ctx, db.EvalArtifact{
		ID:        "candidate-diff",
		Hash:      originalHash,
		MediaType: "text/markdown",
		SizeBytes: int64(len("old diff\n")),
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact returned error: %v", err)
	}
	current, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	artifactDir := t.TempDir()
	content := []byte("new diff\n")
	hash := artifact.ContentHash(content)
	if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), content, 0o644); err != nil {
		t.Fatalf("write diff artifact: %v", err)
	}
	candidate := testCandidatePackage(t, "planner", current.VersionID, "Plan carefully with artifact-backed evidence.")
	candidate.Summary.DiffArtifactID = "candidate-diff"
	candidate.Artifacts = []CandidateArtifactRef{{
		ID:        "candidate-diff",
		Path:      "candidate.diff.md",
		Hash:      hash,
		MediaType: "text/markdown",
		Driver:    "text",
	}}

	_, err = ImportCandidatePackageWithOptions(ctx, store, candidate, CandidateImportOptions{
		SourcePath:  "candidate.json",
		ArtifactDir: artifactDir,
		BlobStore:   artifact.NewStore(filepath.Join(t.TempDir(), "blobs")),
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("ImportCandidatePackageWithOptions error = %v, want existing artifact rejection", err)
	}
	stored, err := store.GetEvalArtifact(ctx, "candidate-diff")
	if err != nil {
		t.Fatalf("GetEvalArtifact returned error: %v", err)
	}
	if stored.Hash != originalHash {
		t.Fatalf("stored artifact hash = %q, want original %q", stored.Hash, originalHash)
	}
	pending, err := store.ListPendingAgentTemplateVersions(ctx, "planner")
	if err != nil {
		t.Fatalf("ListPendingAgentTemplateVersions returned error: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending versions = %+v, want none", pending)
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

func testCandidatePackage(t *testing.T, templateID string, baseVersionID string, body string) CandidatePackage {
	t.Helper()
	candidateContent := testTemplateContent(templateID, body)
	parsed, err := agenttemplate.ParseTemplateContent(candidateContent)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}
	return CandidatePackage{
		Kind:            CandidatePackageKind,
		ContractVersion: ContractVersion,
		TemplateID:      templateID,
		BaseVersionID:   baseVersionID,
		Candidate: CandidateTemplate{
			Content:  candidateContent,
			Metadata: parsed.Metadata,
		},
		EvalReport: json.RawMessage(`{"score":0.82}`),
		Summary: CandidateSummary{
			PreferenceSummary: "Candidate is more actionable.",
		},
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
