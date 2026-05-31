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
