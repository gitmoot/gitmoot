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
	"github.com/jerryfane/gitmoot/internal/github"
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
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	baselineBlob, err := blobStore.Put([]byte("baseline"))
	if err != nil {
		t.Fatalf("Put baseline returned error: %v", err)
	}
	candidateBlob, err := blobStore.Put([]byte("candidate"))
	if err != nil {
		t.Fatalf("Put candidate returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "baseline",
		Hash:      baselineBlob.Hash,
		MediaType: "text/markdown",
		SizeBytes: baselineBlob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "candidate",
		Hash:      candidateBlob.Hash,
		MediaType: "text/markdown",
		SizeBytes: candidateBlob.Size,
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
		RunID:               "run-1",
		ItemID:              "item-001",
		BaselineArtifactID:  "baseline",
		CandidateArtifactID: "candidate",
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
	if training.Template.VersionID != installed.VersionID || len(training.Items) != 1 || len(training.Artifacts) != 2 {
		t.Fatalf("training package = %+v", training)
	}
	packetDir := filepath.Join(t.TempDir(), "packet")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "markdown", "export", "--home", home, "--run", "run-1", "--output", packetDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt feedback markdown export exit code = %d, stderr=%s", code, stderr.String())
	}
	feedbackYAML := `run_id: run-1
reviewer: jerry
items:
  - item_id: item-001
    choice: a
    reasoning: Clearer.
`
	if err := os.WriteFile(filepath.Join(packetDir, "feedback.yml"), []byte(feedbackYAML), 0o644); err != nil {
		t.Fatalf("write feedback.yml: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "markdown", "import", "--home", home, "--packet", packetDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt feedback markdown import exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported 1 feedback events") {
		t.Fatalf("feedback import stdout = %q", stdout.String())
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
		EvalReport: json.RawMessage(`{"score":0.91}`),
		Summary: skillopt.CandidateSummary{
			Score:             floatPtr(0.91),
			PreferenceSummary: "Candidate is more specific.",
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
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "list", "--home", home, "--template", "planner"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate list exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "planner@v2") || !strings.Contains(stdout.String(), "Candidate is more specific.") {
		t.Fatalf("candidate list stdout = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "show", "--home", home, "planner@v2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate show exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state: pending") || !strings.Contains(stdout.String(), "eval_report:") || !strings.Contains(stdout.String(), "content_diff:") {
		t.Fatalf("candidate show stdout = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "promote", "--home", home, "planner@v2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate promote exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "promoted candidate planner@v2") {
		t.Fatalf("candidate promote stdout = %q", stdout.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after import returned error: %v", err)
	}
	current, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate current returned error: %v", err)
	}
	if current.VersionID != "planner@v2" {
		t.Fatalf("current version = %q, want planner@v2", current.VersionID)
	}
	latest, err := store.GetAgentTemplateReference(context.Background(), "planner@latest")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference latest returned error: %v", err)
	}
	if latest.VersionID != "planner@v2" || latest.Content != candidateContent {
		t.Fatalf("latest = %+v", latest)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after promote returned error: %v", err)
	}
	rejectedContent := cliSkillOptTemplateContent("planner", "Plan the work and include every possible detail.")
	rejectedParsed, err := agenttemplate.ParseTemplateContent(rejectedContent)
	if err != nil {
		t.Fatalf("ParseTemplateContent rejected returned error: %v", err)
	}
	candidate.Candidate.Content = rejectedContent
	candidate.Candidate.Metadata = rejectedParsed.Metadata
	encodedCandidate, err = json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal rejected candidate: %v", err)
	}
	if err := os.WriteFile(candidatePath, encodedCandidate, 0o644); err != nil {
		t.Fatalf("write rejected candidate: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "import", "--home", home, "--file", candidatePath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt import rejected exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported pending candidate planner@v3") {
		t.Fatalf("second import stdout = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "reject", "--home", home, "planner@v3", "--reason", "too verbose"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate reject exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "rejected candidate planner@v3") {
		t.Fatalf("candidate reject stdout = %q", stdout.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after reject returned error: %v", err)
	}
	defer store.Close()
	rejected, err := store.GetAgentTemplateVersionByID(context.Background(), "planner@v3")
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID rejected returned error: %v", err)
	}
	if rejected.State != "rejected" {
		t.Fatalf("rejected = %+v", rejected)
	}
	rejectedReview, err := store.GetAgentTemplateCandidateReview(context.Background(), "planner@v3")
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview rejected returned error: %v", err)
	}
	if rejectedReview.DecisionReason != "too verbose" {
		t.Fatalf("rejected review = %+v", rejectedReview)
	}
	latest, err = store.GetAgentTemplateReference(context.Background(), "planner@latest")
	if err != nil {
		t.Fatalf("GetAgentTemplateReference latest after reject returned error: %v", err)
	}
	if latest.VersionID != "planner@v2" {
		t.Fatalf("latest after reject = %+v", latest)
	}
	events, err := store.ListFeedbackEvents(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].Choice != "a" {
		t.Fatalf("feedback events = %+v", events)
	}
}

func TestSkillOptImportCandidateArtifacts(t *testing.T) {
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
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	artifactDir := t.TempDir()
	diffContent := []byte("candidate diff\n")
	diffHash := artifact.ContentHash(diffContent)
	if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), diffContent, 0o644); err != nil {
		t.Fatalf("write diff artifact: %v", err)
	}
	diffSize := int64(len(diffContent))
	candidate := cliSkillOptCandidatePackage(t, "planner", installed.VersionID, "Plan with a concise risk section.")
	candidate.Summary.DiffArtifactID = "candidate-diff"
	candidate.Artifacts = []skillopt.CandidateArtifactRef{{
		ID:        "candidate-diff",
		Path:      "candidate.diff.md",
		Hash:      diffHash,
		MediaType: "text/markdown",
		Driver:    "text",
		SizeBytes: &diffSize,
	}}
	candidatePath := filepath.Join(t.TempDir(), "candidate.json")
	encodedCandidate, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	if err := os.WriteFile(candidatePath, encodedCandidate, 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "import", "--home", home, "--file", candidatePath, "--artifact-dir", artifactDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt import exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported pending candidate planner@v2") {
		t.Fatalf("import stdout = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "show", "--home", home, "planner@v2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate show exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "diff_artifact: candidate-diff") {
		t.Fatalf("candidate show stdout = %q", stdout.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after import returned error: %v", err)
	}
	defer store.Close()
	stored, err := store.GetEvalArtifact(context.Background(), "candidate-diff")
	if err != nil {
		t.Fatalf("GetEvalArtifact returned error: %v", err)
	}
	if stored.Hash != diffHash || stored.SizeBytes != diffSize || stored.MediaType != "text/markdown" {
		t.Fatalf("stored artifact = %+v", stored)
	}
	blobContent, err := artifact.NewStore(paths.ArtifactBlobs).Read(diffHash)
	if err != nil {
		t.Fatalf("Read stored artifact returned error: %v", err)
	}
	if string(blobContent) != string(diffContent) {
		t.Fatalf("stored artifact content = %q", string(blobContent))
	}
}

func TestSkillOptImportCandidateArtifactFailuresDoNotCreatePendingCandidate(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		hash        string
		artifactDir bool
		writeFile   bool
		wantErr     string
	}{
		{
			name:        "missing artifact dir",
			path:        "candidate.diff.md",
			hash:        artifact.ContentHash([]byte("candidate diff\n")),
			artifactDir: false,
			writeFile:   false,
			wantErr:     "candidate artifacts require --artifact-dir",
		},
		{
			name:        "invalid hash",
			path:        "candidate.diff.md",
			hash:        artifact.ContentHash([]byte("other")),
			artifactDir: true,
			writeFile:   true,
			wantErr:     "hash is",
		},
		{
			name:        "path traversal",
			path:        "../candidate.diff.md",
			hash:        artifact.ContentHash([]byte("candidate diff\n")),
			artifactDir: true,
			writeFile:   false,
			wantErr:     "relative path inside artifact-dir",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			paths := config.PathsForHome(home)
			if err := config.Initialize(paths); err != nil {
				t.Fatalf("Initialize returned error: %v", err)
			}
			store, err := db.Open(paths.Database)
			if err != nil {
				t.Fatalf("Open returned error: %v", err)
			}
			if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
				t.Fatalf("UpsertAgentTemplate returned error: %v", err)
			}
			installed, err := store.GetAgentTemplate(context.Background(), "planner")
			if err != nil {
				t.Fatalf("GetAgentTemplate returned error: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("Close returned error: %v", err)
			}
			artifactDir := ""
			if tt.artifactDir {
				artifactDir = t.TempDir()
				if tt.writeFile {
					if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), []byte("candidate diff\n"), 0o644); err != nil {
						t.Fatalf("write diff artifact: %v", err)
					}
				}
			}
			candidate := cliSkillOptCandidatePackage(t, "planner", installed.VersionID, "Plan with a concise risk section.")
			candidate.Summary.DiffArtifactID = "candidate-diff"
			candidate.Artifacts = []skillopt.CandidateArtifactRef{{
				ID:        "candidate-diff",
				Path:      tt.path,
				Hash:      tt.hash,
				MediaType: "text/markdown",
				Driver:    "text",
			}}
			candidatePath := filepath.Join(t.TempDir(), "candidate.json")
			encodedCandidate, err := json.Marshal(candidate)
			if err != nil {
				t.Fatalf("marshal candidate: %v", err)
			}
			if err := os.WriteFile(candidatePath, encodedCandidate, 0o644); err != nil {
				t.Fatalf("write candidate: %v", err)
			}
			args := []string{"skillopt", "import", "--home", home, "--file", candidatePath}
			if artifactDir != "" {
				args = append(args, "--artifact-dir", artifactDir)
			}
			var stdout, stderr bytes.Buffer
			code := Run(args, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("skillopt import exit code = 0, stdout=%s", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), tt.wantErr)
			}
			store, err = db.Open(paths.Database)
			if err != nil {
				t.Fatalf("Open after failed import returned error: %v", err)
			}
			defer store.Close()
			pending, err := store.ListPendingAgentTemplateVersions(context.Background(), "planner")
			if err != nil {
				t.Fatalf("ListPendingAgentTemplateVersions returned error: %v", err)
			}
			if len(pending) != 0 {
				t.Fatalf("pending versions = %+v, want none", pending)
			}
			if _, err := store.GetEvalArtifact(context.Background(), "candidate-diff"); err == nil {
				t.Fatalf("candidate artifact was registered despite failed import")
			}
		})
	}
}

func floatPtr(value float64) *float64 {
	return &value
}

func TestSkillOptFeedbackRejectsIncompleteCommands(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantErr      string
		wantStdout   string
		wantExitCode int
		wantNoStderr bool
		wantNoStdout bool
	}{
		{
			name:         "feedback help",
			args:         []string{"skillopt", "feedback", "--help"},
			wantStdout:   "gitmoot skillopt feedback github publish",
			wantExitCode: 0,
			wantNoStderr: true,
		},
		{
			name:         "unknown collector",
			args:         []string{"skillopt", "feedback", "json"},
			wantErr:      `unknown skillopt feedback collector "json"`,
			wantExitCode: 2,
			wantNoStdout: true,
		},
		{
			name:         "missing markdown subcommand",
			args:         []string{"skillopt", "feedback", "markdown"},
			wantErr:      "skillopt feedback markdown requires a subcommand",
			wantExitCode: 2,
			wantNoStdout: true,
		},
		{
			name:         "missing github subcommand",
			args:         []string{"skillopt", "feedback", "github"},
			wantErr:      "skillopt feedback github requires a subcommand",
			wantExitCode: 2,
			wantNoStdout: true,
		},
		{
			name:         "missing github sync target",
			args:         []string{"skillopt", "feedback", "github", "sync", "--run", "run-1"},
			wantErr:      "skillopt feedback github sync requires --issue or --pr",
			wantExitCode: 2,
			wantNoStdout: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(tt.args, &stdout, &stderr)
			if code != tt.wantExitCode {
				t.Fatalf("exit code = %d, want %d; stdout=%s stderr=%s", code, tt.wantExitCode, stdout.String(), stderr.String())
			}
			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Fatalf("stdout = %q, want substring %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), tt.wantErr)
			}
			if tt.wantNoStdout && stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			if tt.wantNoStderr && stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestSkillOptFeedbackGitHubCommands(t *testing.T) {
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	baselineBlob, err := blobStore.Put([]byte("baseline"))
	if err != nil {
		t.Fatalf("Put baseline returned error: %v", err)
	}
	candidateBlob, err := blobStore.Put([]byte("candidate"))
	if err != nil {
		t.Fatalf("Put candidate returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{ID: "baseline", Hash: baselineBlob.Hash, MediaType: "text/markdown", SizeBytes: baselineBlob.Size, Driver: "text"}); err != nil {
		t.Fatalf("UpsertEvalArtifact baseline returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{ID: "candidate", Hash: candidateBlob.Hash, MediaType: "text/markdown", SizeBytes: candidateBlob.Size, Driver: "text"}); err != nil {
		t.Fatalf("UpsertEvalArtifact candidate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{ID: "run-1", TargetRepo: "owner/repo", State: "review"}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:               "run-1",
		ItemID:              "item-001",
		BaselineArtifactID:  "baseline",
		CandidateArtifactID: "candidate",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			8: {
				{ID: 100, Body: "run_id: run-1\nitem-001: b - More concrete.", URL: "https://github.com/owner/repo/issues/8#issuecomment-100", Author: "alice", CreatedAt: "2026-05-31T10:00:00Z"},
			},
		},
	}
	oldClient := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return fake }
	t.Cleanup(func() {
		newSkillOptGitHubClient = oldClient
	})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "feedback", "github", "publish", "--home", home, "--run", "run-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("github publish exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "published github feedback issue for run-1 to owner/repo#8") {
		t.Fatalf("publish stdout = %q", stdout.String())
	}
	if fake.createdIssue.Repo.FullName() != "owner/repo" || !strings.Contains(fake.createdIssue.Body, "Copy-Paste YAML Reply") {
		t.Fatalf("created issue = %+v", fake.createdIssue)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "github", "sync", "--home", home, "--run", "run-1", "--issue", "8"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("github sync exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported 1 github feedback events") {
		t.Fatalf("sync stdout = %q", stdout.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after sync returned error: %v", err)
	}
	defer store.Close()
	events, err := store.ListFeedbackEvents(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ListFeedbackEvents returned error: %v", err)
	}
	if len(events) != 1 || events[0].Reviewer != "alice" || events[0].Source != "github" {
		t.Fatalf("events = %+v", events)
	}
}

func TestSkillOptReviewCreateAndStatus(t *testing.T) {
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
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt help exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gitmoot skillopt review create") || !strings.Contains(stdout.String(), "gitmoot skillopt review status") {
		t.Fatalf("skillopt help missing review commands:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "review", "create",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/repo",
		"--run", "planner-ab-1",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review create exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "created review planner-ab-1 for "+installed.VersionID) {
		t.Fatalf("review create stdout = %q", stdout.String())
	}

	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after create returned error: %v", err)
	}
	run, err := store.GetEvalRun(context.Background(), "planner-ab-1")
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	if run.TemplateID != "planner" || run.TemplateVersionID != installed.VersionID || run.TargetRepo != "owner/repo" || run.State != "review" {
		t.Fatalf("eval run = %+v", run)
	}
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	baselineBlob, err := blobStore.Put([]byte("baseline"))
	if err != nil {
		t.Fatalf("Put baseline returned error: %v", err)
	}
	candidateBlob, err := blobStore.Put([]byte("candidate"))
	if err != nil {
		t.Fatalf("Put candidate returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "baseline",
		Hash:      baselineBlob.Hash,
		MediaType: "text/markdown",
		SizeBytes: baselineBlob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact baseline returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "candidate",
		Hash:      candidateBlob.Hash,
		MediaType: "text/markdown",
		SizeBytes: candidateBlob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact candidate returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:               "planner-ab-1",
		ItemID:              "item-001",
		Title:               "README planning task",
		BaselineArtifactID:  "baseline",
		CandidateArtifactID: "candidate",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.UpsertFeedbackEvent(context.Background(), db.FeedbackEvent{
		RunID:     "planner-ab-1",
		ItemID:    "item-001",
		Choice:    "b",
		Reasoning: "More concrete.",
		Reviewer:  "jerry",
		Source:    "markdown",
		CreatedAt: "2026-05-31T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after seed returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "review", "status", "--home", home, "--run", "planner-ab-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"run: planner-ab-1",
		"template: planner",
		"template_version: " + installed.VersionID,
		"repo: owner/repo",
		"state: review",
		"items: 1",
		"feedback: 1",
		"packet_blockers: 0",
		"training_blockers: 0",
		"ready_for_packet: true",
		"ready_for_training: true",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSkillOptReviewItemAddStoresArtifacts(t *testing.T) {
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
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "review", "create",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/repo",
		"--run", "planner-ab-1",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review create exit code = %d, stderr=%s", code, stderr.String())
	}

	inputDir := t.TempDir()
	baselinePath := filepath.Join(inputDir, "baseline.md")
	candidatePath := filepath.Join(inputDir, "candidate.md")
	if err := os.WriteFile(baselinePath, []byte("# Baseline\n\nShort plan.\n"), 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	if err := os.WriteFile(candidatePath, []byte("# Candidate\n\nShort plan with risks.\n"), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "planner-ab-1",
		"--item", "item-001",
		"--title", "README planning task",
		"--baseline", baselinePath,
		"--candidate", candidatePath,
		"--metadata-json", `{"path":"README.md"}`,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review item add exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "added review item item-001 to planner-ab-1") {
		t.Fatalf("review item add stdout = %q", stdout.String())
	}

	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after item add returned error: %v", err)
	}
	items, err := store.ListEvalReviewItems(context.Background(), "planner-ab-1")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("review item count = %d, want 1", len(items))
	}
	item := items[0]
	if item.Title != "README planning task" || item.BaselineArtifactID != "planner-ab-1/item-001/baseline" || item.CandidateArtifactID != "planner-ab-1/item-001/candidate" || item.MetadataJSON != `{"path":"README.md"}` {
		t.Fatalf("review item = %+v", item)
	}
	baseline, err := store.GetEvalArtifact(context.Background(), item.BaselineArtifactID)
	if err != nil {
		t.Fatalf("GetEvalArtifact baseline returned error: %v", err)
	}
	candidate, err := store.GetEvalArtifact(context.Background(), item.CandidateArtifactID)
	if err != nil {
		t.Fatalf("GetEvalArtifact candidate returned error: %v", err)
	}
	if baseline.MediaType != "text/markdown" || candidate.MediaType != "text/markdown" || baseline.Driver != "text" || candidate.Driver != "text" {
		t.Fatalf("artifacts = %+v %+v", baseline, candidate)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after artifact check returned error: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "review", "status", "--home", home, "--run", "planner-ab-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"items: 1",
		"feedback: 0",
		"packet_blockers: 0",
		"training_blockers: 1",
		"ready_for_packet: true",
		"ready_for_training: false",
		"training_blocker: item item-001 has no imported feedback",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}

	packetDir := filepath.Join(t.TempDir(), "packet")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "markdown", "export", "--home", home, "--run", "planner-ab-1", "--output", packetDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt feedback markdown export exit code = %d, stderr=%s", code, stderr.String())
	}
	feedbackYAML, err := os.ReadFile(filepath.Join(packetDir, "feedback.yml"))
	if err != nil {
		t.Fatalf("read feedback.yml: %v", err)
	}
	if !strings.Contains(string(feedbackYAML), "item_id: item-001") {
		t.Fatalf("feedback.yml = %s", string(feedbackYAML))
	}
}

func TestSkillOptHumanFeedbackTrialSmoke(t *testing.T) {
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
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "review", "create",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/repo",
		"--run", "trial-smoke",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review create exit code = %d, stderr=%s", code, stderr.String())
	}

	inputDir := t.TempDir()
	baselinePath := filepath.Join(inputDir, "baseline.md")
	candidatePath := filepath.Join(inputDir, "candidate.md")
	if err := os.WriteFile(baselinePath, []byte("# Baseline\n\nPlan only the edit.\n"), 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	if err := os.WriteFile(candidatePath, []byte("# Candidate\n\nPlan the edit, test, and rollback notes.\n"), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "trial-smoke",
		"--item", "item-001",
		"--title", "README planning task",
		"--baseline", baselinePath,
		"--candidate", candidatePath,
		"--metadata-json", `{"path":"README.md"}`,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review item add exit code = %d, stderr=%s", code, stderr.String())
	}

	packetDir := filepath.Join(t.TempDir(), "packet")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "markdown", "export", "--home", home, "--run", "trial-smoke", "--output", packetDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt feedback markdown export exit code = %d, stderr=%s", code, stderr.String())
	}
	feedbackYAML := `run_id: trial-smoke
reviewer: jerry
items:
  - item_id: item-001
    choice: a
    reasoning: More complete execution plan.
`
	if err := os.WriteFile(filepath.Join(packetDir, "feedback.yml"), []byte(feedbackYAML), 0o644); err != nil {
		t.Fatalf("write feedback.yml: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "markdown", "import", "--home", home, "--packet", packetDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt feedback markdown import exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported 1 feedback events") {
		t.Fatalf("feedback import stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "review", "status", "--home", home, "--run", "trial-smoke"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"items: 1",
		"feedback: 1",
		"ready_for_packet: true",
		"ready_for_training: true",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}

	trainingPath := filepath.Join(t.TempDir(), "training.json")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "export", "--home", home, "--run", "trial-smoke", "--output", trainingPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt export exit code = %d, stderr=%s", code, stderr.String())
	}
	trainingContent, err := os.ReadFile(trainingPath)
	if err != nil {
		t.Fatalf("read training package: %v", err)
	}
	var training skillopt.TrainingPackage
	if err := json.Unmarshal(trainingContent, &training); err != nil {
		t.Fatalf("decode training package: %v\n%s", err, string(trainingContent))
	}
	if training.EvalRun.ID != "trial-smoke" || len(training.Items) != 1 || len(training.Artifacts) != 2 || len(training.FeedbackEvents) != 1 {
		t.Fatalf("training package = %+v", training)
	}
	if training.FeedbackEvents[0].ItemID != "item-001" || training.FeedbackEvents[0].Choice != "b" || training.FeedbackEvents[0].Reasoning != "More complete execution plan." {
		t.Fatalf("training feedback = %+v", training.FeedbackEvents)
	}

	artifactDir := t.TempDir()
	diffContent := []byte("candidate diff\n")
	diffHash := artifact.ContentHash(diffContent)
	if err := os.WriteFile(filepath.Join(artifactDir, "candidate.diff.md"), diffContent, 0o644); err != nil {
		t.Fatalf("write diff artifact: %v", err)
	}
	diffSize := int64(len(diffContent))
	candidate := cliSkillOptCandidatePackage(t, "planner", installed.VersionID, "Plan with test and rollback notes.")
	candidate.Summary.DiffArtifactID = "candidate-diff"
	candidate.Artifacts = []skillopt.CandidateArtifactRef{{
		ID:        "candidate-diff",
		Path:      "candidate.diff.md",
		Hash:      diffHash,
		MediaType: "text/markdown",
		Driver:    "text",
		SizeBytes: &diffSize,
	}}
	candidatePackagePath := filepath.Join(t.TempDir(), "candidate.json")
	encodedCandidate, err := json.Marshal(candidate)
	if err != nil {
		t.Fatalf("marshal candidate package: %v", err)
	}
	if err := os.WriteFile(candidatePackagePath, encodedCandidate, 0o644); err != nil {
		t.Fatalf("write candidate package: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "import", "--home", home, "--file", candidatePackagePath, "--artifact-dir", artifactDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt import exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported pending candidate planner@v2") {
		t.Fatalf("candidate import stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "show", "--home", home, "planner@v2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate show exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state: pending") || !strings.Contains(stdout.String(), "diff_artifact: candidate-diff") {
		t.Fatalf("candidate show stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "reject", "--home", home, "planner@v2", "--reason", "trial smoke"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt candidate reject exit code = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "rejected candidate planner@v2") {
		t.Fatalf("candidate reject stdout = %q", stdout.String())
	}

	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after trial smoke returned error: %v", err)
	}
	defer store.Close()
	current, err := store.GetAgentTemplate(context.Background(), "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if current.VersionID != installed.VersionID {
		t.Fatalf("current version = %q, want %q", current.VersionID, installed.VersionID)
	}
	rejected, err := store.GetAgentTemplateVersionByID(context.Background(), "planner@v2")
	if err != nil {
		t.Fatalf("GetAgentTemplateVersionByID returned error: %v", err)
	}
	if rejected.State != "rejected" {
		t.Fatalf("rejected candidate = %+v", rejected)
	}
	review, err := store.GetAgentTemplateCandidateReview(context.Background(), "planner@v2")
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview returned error: %v", err)
	}
	if review.DecisionReason != "trial smoke" {
		t.Fatalf("candidate review = %+v", review)
	}
}

func TestSkillOptReviewItemAddRejectsInvalidInputs(t *testing.T) {
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
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "planner-ab-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		MetadataJSON:      `{"driver":"manual-review"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	inputDir := t.TempDir()
	baselinePath := filepath.Join(inputDir, "baseline.md")
	candidatePath := filepath.Join(inputDir, "candidate.md")
	binaryPath := filepath.Join(inputDir, "candidate.bin")
	if err := os.WriteFile(baselinePath, []byte("baseline\n"), 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	if err := os.WriteFile(candidatePath, []byte("candidate\n"), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if err := os.WriteFile(binaryPath, []byte{0xff, 0x00, 0x01}, 0o644); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name: "missing run",
			args: []string{
				"skillopt", "review", "item", "add",
				"--home", home,
				"--run", "missing-run",
				"--item", "item-001",
				"--baseline", baselinePath,
				"--candidate", candidatePath,
			},
			wantErr: "review run missing-run not found",
		},
		{
			name: "invalid metadata",
			args: []string{
				"skillopt", "review", "item", "add",
				"--home", home,
				"--run", "planner-ab-1",
				"--item", "item-001",
				"--baseline", baselinePath,
				"--candidate", candidatePath,
				"--metadata-json", "{not-json",
			},
			wantErr: "metadata-json:",
		},
		{
			name: "binary without media type",
			args: []string{
				"skillopt", "review", "item", "add",
				"--home", home,
				"--run", "planner-ab-1",
				"--item", "item-001",
				"--baseline", baselinePath,
				"--candidate", binaryPath,
			},
			wantErr: "binary content requires --media-type",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(tt.args, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("exit code = 0, stdout=%s", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestSkillOptReviewStatusRequiresExportableArtifacts(t *testing.T) {
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
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "planner-ab-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		MetadataJSON:      `{"driver":"manual-review"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:               "planner-ab-1",
		ItemID:              "item-001",
		BaselineArtifactID:  "baseline",
		CandidateArtifactID: "candidate",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.UpsertFeedbackEvent(context.Background(), db.FeedbackEvent{
		RunID:     "planner-ab-1",
		ItemID:    "item-001",
		Choice:    "b",
		Reviewer:  "jerry",
		Source:    "markdown",
		CreatedAt: "2026-05-31T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "review", "status", "--home", home, "--run", "planner-ab-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"items: 1",
		"feedback: 1",
		"packet_blockers: 2",
		"training_blockers: 1",
		"ready_for_packet: false",
		"ready_for_training: false",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSkillOptReviewStatusRequiresExportableMetadata(t *testing.T) {
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
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	baselineBlob, err := blobStore.Put([]byte("baseline"))
	if err != nil {
		t.Fatalf("Put baseline returned error: %v", err)
	}
	candidateBlob, err := blobStore.Put([]byte("candidate"))
	if err != nil {
		t.Fatalf("Put candidate returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "baseline",
		Hash:      baselineBlob.Hash,
		MediaType: "text/markdown",
		SizeBytes: baselineBlob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact baseline returned error: %v", err)
	}
	if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
		ID:        "candidate",
		Hash:      candidateBlob.Hash,
		MediaType: "text/markdown",
		SizeBytes: candidateBlob.Size,
		Driver:    "text",
	}); err != nil {
		t.Fatalf("UpsertEvalArtifact candidate returned error: %v", err)
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "planner-ab-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		MetadataJSON:      `{"driver":"manual-review"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:               "planner-ab-1",
		ItemID:              "item-001",
		BaselineArtifactID:  "baseline",
		CandidateArtifactID: "candidate",
		MetadataJSON:        `{not-json`,
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	if err := store.UpsertFeedbackEvent(context.Background(), db.FeedbackEvent{
		RunID:     "planner-ab-1",
		ItemID:    "item-001",
		Choice:    "b",
		Reviewer:  "jerry",
		Source:    "markdown",
		CreatedAt: "2026-05-31T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "review", "status", "--home", home, "--run", "planner-ab-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"packet_blockers: 0",
		"training_blockers: 1",
		"ready_for_packet: true",
		"ready_for_training: false",
		"training_blocker: training export failed: eval item item-001 metadata_json:",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSkillOptReviewStatusRequiresFeedbackForEveryItem(t *testing.T) {
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
	blobStore := artifact.NewStore(paths.ArtifactBlobs)
	for _, fixture := range []struct {
		id      string
		content string
	}{
		{id: "item-001-baseline", content: "item 1 baseline"},
		{id: "item-001-candidate", content: "item 1 candidate"},
		{id: "item-002-baseline", content: "item 2 baseline"},
		{id: "item-002-candidate", content: "item 2 candidate"},
	} {
		blob, err := blobStore.Put([]byte(fixture.content))
		if err != nil {
			t.Fatalf("Put %s returned error: %v", fixture.id, err)
		}
		if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
			ID:        fixture.id,
			Hash:      blob.Hash,
			MediaType: "text/markdown",
			SizeBytes: blob.Size,
			Driver:    "text",
		}); err != nil {
			t.Fatalf("UpsertEvalArtifact %s returned error: %v", fixture.id, err)
		}
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "planner-ab-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		MetadataJSON:      `{"driver":"manual-review"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	for _, item := range []db.EvalReviewItem{
		{RunID: "planner-ab-1", ItemID: "item-001", BaselineArtifactID: "item-001-baseline", CandidateArtifactID: "item-001-candidate"},
		{RunID: "planner-ab-1", ItemID: "item-002", BaselineArtifactID: "item-002-baseline", CandidateArtifactID: "item-002-candidate"},
	} {
		if err := store.UpsertEvalReviewItem(context.Background(), item); err != nil {
			t.Fatalf("UpsertEvalReviewItem %s returned error: %v", item.ItemID, err)
		}
	}
	if err := store.UpsertFeedbackEvent(context.Background(), db.FeedbackEvent{
		RunID:     "planner-ab-1",
		ItemID:    "item-001",
		Choice:    "b",
		Reviewer:  "jerry",
		Source:    "markdown",
		CreatedAt: "2026-05-31T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertFeedbackEvent returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "review", "status", "--home", home, "--run", "planner-ab-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"items: 2",
		"feedback: 1",
		"packet_blockers: 0",
		"training_blockers: 1",
		"ready_for_packet: true",
		"ready_for_training: false",
		"training_blocker: item item-002 has no imported feedback",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestSkillOptReviewCreateRejectsUnknownTemplate(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"skillopt", "review", "create",
		"--home", home,
		"--template", "missing-template",
		"--repo", "owner/repo",
		"--run", "planner-ab-1",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("skillopt review create exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "agent template missing-template is not installed") {
		t.Fatalf("review create stderr = %q", stderr.String())
	}
}

func TestSkillOptReviewStatusRejectsMissingRun(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "review", "status", "--home", home, "--run", "missing-run"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("skillopt review status exit code = %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "review run missing-run not found") {
		t.Fatalf("review status stderr = %q", stderr.String())
	}
}

type skillOptFakeGitHub struct {
	github.NoopClient

	createdIssue github.CreateIssueInput
	comments     map[int64][]github.IssueComment
}

func (f *skillOptFakeGitHub) CreateIssue(_ context.Context, input github.CreateIssueInput) (github.Issue, error) {
	f.createdIssue = input
	return github.Issue{Number: 8, URL: "https://github.com/" + input.Repo.FullName() + "/issues/8"}, nil
}

func (f *skillOptFakeGitHub) ListIssueComments(_ context.Context, _ github.Repository, issueNumber int64) ([]github.IssueComment, error) {
	return append([]github.IssueComment(nil), f.comments[issueNumber]...), nil
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

func cliSkillOptCandidatePackage(t *testing.T, templateID string, baseVersionID string, body string) skillopt.CandidatePackage {
	t.Helper()
	candidateContent := cliSkillOptTemplateContent(templateID, body)
	parsed, err := agenttemplate.ParseTemplateContent(candidateContent)
	if err != nil {
		t.Fatalf("ParseTemplateContent returned error: %v", err)
	}
	return skillopt.CandidatePackage{
		Kind:            skillopt.CandidatePackageKind,
		ContractVersion: skillopt.ContractVersion,
		TemplateID:      templateID,
		BaseVersionID:   baseVersionID,
		Candidate: skillopt.CandidateTemplate{
			Content:  candidateContent,
			Metadata: parsed.Metadata,
		},
		EvalReport: json.RawMessage(`{"score":0.82}`),
		Summary: skillopt.CandidateSummary{
			PreferenceSummary: "Candidate is more actionable.",
		},
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
