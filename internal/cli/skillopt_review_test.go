package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/artifact"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

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
	if !strings.Contains(stdout.String(), "gitmoot skillopt train continue --session <id> [--backend codex]") {
		t.Fatalf("skillopt help missing train backend preset:\n%s", stdout.String())
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
	if !strings.Contains(string(feedbackYAML), "item_id: item-001") || !strings.Contains(string(feedbackYAML), "choice:") {
		t.Fatalf("feedback.yml = %s", string(feedbackYAML))
	}
}

func TestSkillOptRankedReviewItemAddAndMarkdownExport(t *testing.T) {
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
		"--run", "planner-explore-1",
		"--mode", "explore",
		"--exploration-level", "high",
		"--options", "4",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review create exit code = %d, stderr=%s", code, stderr.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open before preseeded review item returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:        "planner-explore-1",
		ItemID:       "item-001",
		Title:        "Preplanned landing page",
		MetadataJSON: `{"brief":"Preserve this brief","output_type":"vue"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem preseed returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after preseeded review item returned error: %v", err)
	}

	inputDir := t.TempDir()
	optionPaths := map[string]string{}
	for _, label := range []string{"a", "b", "c", "d"} {
		path := filepath.Join(inputDir, label+".md")
		if err := os.WriteFile(path, []byte("# Option "+strings.ToUpper(label)+"\n\nReview content.\n"), 0o644); err != nil {
			t.Fatalf("write option %s: %v", label, err)
		}
		optionPaths[label] = path
	}
	missingOptionArgs := []string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "planner-explore-1",
		"--item", "item-001",
		"--title", "Landing page",
		"--option", "a=" + optionPaths["a"],
		"--option", "b=" + optionPaths["b"],
		"--option", "c=" + optionPaths["c"],
		"--option", "d=" + filepath.Join(inputDir, "missing.md"),
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(missingOptionArgs, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "missing.md") {
		t.Fatalf("ranked item add with missing option: code=%d stderr=%s", code, stderr.String())
	}
	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after failed ranked item add returned error: %v", err)
	}
	options, err := store.ListEvalReviewOptions(context.Background(), "planner-explore-1", "item-001")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions after failed add returned error: %v", err)
	}
	if len(options) != 0 {
		t.Fatalf("failed ranked item add persisted partial options: %+v", options)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after failed ranked item add returned error: %v", err)
	}
	rankedItemArgs := []string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "planner-explore-1",
		"--item", "item-001",
		"--title", "Landing page",
		"--option", "a=" + optionPaths["a"],
		"--option", "b=" + optionPaths["b"],
		"--option", "c=" + optionPaths["c"],
		"--option", "d=" + optionPaths["d"],
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(rankedItemArgs, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review item add exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(rankedItemArgs, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review item add retry exit code = %d, stderr=%s", code, stderr.String())
	}
	replacementPath := filepath.Join(inputDir, "e.md")
	if err := os.WriteFile(replacementPath, []byte("# Option E\n\nReplacement content.\n"), 0o644); err != nil {
		t.Fatalf("write replacement option: %v", err)
	}
	rankedReplacementArgs := []string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "planner-explore-1",
		"--item", "item-001",
		"--title", "Landing page",
		"--option", "a=" + optionPaths["a"],
		"--option", "b=" + optionPaths["b"],
		"--option", "c=" + optionPaths["c"],
		"--option", "e=" + replacementPath,
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(rankedReplacementArgs, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review item add replacement exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "planner-explore-1",
		"--item", "item-ab",
		"--baseline", optionPaths["a"],
		"--candidate", optionPaths["b"],
	}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "is ranked mode; use repeated --option") {
		t.Fatalf("ranked run accepted A/B artifacts: code=%d stderr=%s", code, stderr.String())
	}

	store, err = db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open after ranked item add returned error: %v", err)
	}
	run, err := store.GetEvalRun(context.Background(), "planner-explore-1")
	if err != nil {
		t.Fatalf("GetEvalRun returned error: %v", err)
	}
	if run.Mode != db.EvalRunModeExplore || run.ExplorationLevel != db.ExplorationLevelHigh || run.OptionsCount != 4 {
		t.Fatalf("run = %+v", run)
	}
	options, err = store.ListEvalReviewOptions(context.Background(), "planner-explore-1", "item-001")
	if err != nil {
		t.Fatalf("ListEvalReviewOptions returned error: %v", err)
	}
	if len(options) != 4 || options[0].Label != "a" || options[3].Label != "e" || !strings.Contains(options[0].MetadataJSON, optionPaths["a"]) {
		t.Fatalf("options = %+v", options)
	}
	items, err := store.ListEvalReviewItems(context.Background(), "planner-explore-1")
	if err != nil {
		t.Fatalf("ListEvalReviewItems returned error: %v", err)
	}
	if len(items) != 1 || items[0].Title != "Landing page" || !strings.Contains(items[0].MetadataJSON, "Preserve this brief") {
		t.Fatalf("item metadata was not preserved after option add: %+v", items)
	}
	for _, option := range options {
		if option.Label == "d" {
			t.Fatalf("replacement left stale option d: %+v", options)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close after ranked checks returned error: %v", err)
	}

	packetDir := filepath.Join(t.TempDir(), "packet")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "feedback", "markdown", "export", "--home", home, "--run", "planner-explore-1", "--output", packetDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt feedback markdown export exit code = %d, stderr=%s", code, stderr.String())
	}
	index, err := os.ReadFile(filepath.Join(packetDir, "index.md"))
	if err != nil {
		t.Fatalf("read index.md: %v", err)
	}
	if !strings.Contains(string(index), "ranking every option") || !strings.Contains(string(index), "A > B > C > E") {
		t.Fatalf("index.md = %s", string(index))
	}
	itemFiles, err := os.ReadDir(filepath.Join(packetDir, "items"))
	if err != nil {
		t.Fatalf("read items dir: %v", err)
	}
	if len(itemFiles) != 1 {
		t.Fatalf("item files = %d, want 1", len(itemFiles))
	}
	itemContent, err := os.ReadFile(filepath.Join(packetDir, "items", itemFiles[0].Name()))
	if err != nil {
		t.Fatalf("read item markdown: %v", err)
	}
	if !strings.Contains(string(itemContent), "| Option | Artifact | Reference |") || !strings.Contains(string(itemContent), "Option C") {
		t.Fatalf("item markdown = %s", string(itemContent))
	}
	feedbackYAML, err := os.ReadFile(filepath.Join(packetDir, "feedback.yml"))
	if err != nil {
		t.Fatalf("read feedback.yml: %v", err)
	}
	if !strings.Contains(string(feedbackYAML), "ranking:") ||
		!strings.Contains(string(feedbackYAML), "<replace with ranked option labels, best to worst>") ||
		strings.Contains(string(feedbackYAML), "- C") {
		t.Fatalf("feedback.yml = %s", string(feedbackYAML))
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "review", "create",
		"--home", home,
		"--template", "planner",
		"--repo", "owner/repo",
		"--run", "planner-validate-1",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("validate review create exit code = %d, stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{
		"skillopt", "review", "item", "add",
		"--home", home,
		"--run", "planner-validate-1",
		"--item", "item-ranked",
		"--option", "a=" + optionPaths["a"],
		"--option", "b=" + optionPaths["b"],
	}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "is validate/A/B mode; use --baseline and --candidate") {
		t.Fatalf("validate run accepted ranked options: code=%d stderr=%s", code, stderr.String())
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

func TestSkillOptReviewStatusShowsRankedPairwisePreferences(t *testing.T) {
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
	optionLabels := []string{"a", "b", "c", "d"}
	for _, label := range optionLabels {
		blob, err := blobStore.Put([]byte("option " + label))
		if err != nil {
			t.Fatalf("Put option %s returned error: %v", label, err)
		}
		artifactID := "option-" + label
		if err := store.UpsertEvalArtifact(context.Background(), db.EvalArtifact{
			ID:        artifactID,
			Hash:      blob.Hash,
			MediaType: "text/markdown",
			SizeBytes: blob.Size,
			Driver:    "text",
		}); err != nil {
			t.Fatalf("UpsertEvalArtifact %s returned error: %v", artifactID, err)
		}
	}
	if err := store.UpsertEvalRun(context.Background(), db.EvalRun{
		ID:                "planner-ranked-1",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "review",
		Mode:              db.EvalRunModeExplore,
		ExplorationLevel:  db.ExplorationLevelHigh,
		OptionsCount:      4,
		MetadataJSON:      `{"driver":"manual-review"}`,
	}); err != nil {
		t.Fatalf("UpsertEvalRun returned error: %v", err)
	}
	if err := store.UpsertEvalReviewItem(context.Background(), db.EvalReviewItem{
		RunID:  "planner-ranked-1",
		ItemID: "item-001",
		Title:  "Landing page",
	}); err != nil {
		t.Fatalf("UpsertEvalReviewItem returned error: %v", err)
	}
	for _, label := range optionLabels {
		if err := store.UpsertEvalReviewOption(context.Background(), db.EvalReviewOption{
			RunID:      "planner-ranked-1",
			ItemID:     "item-001",
			Label:      label,
			ArtifactID: "option-" + label,
		}); err != nil {
			t.Fatalf("UpsertEvalReviewOption %s returned error: %v", label, err)
		}
	}
	ranking, err := json.Marshal([]string{"c", "a", "d", "b"})
	if err != nil {
		t.Fatalf("marshal ranking: %v", err)
	}
	if err := store.UpsertRankedFeedbackEvent(context.Background(), db.RankedFeedbackEvent{
		RunID:       "planner-ranked-1",
		ItemID:      "item-001",
		RankingJSON: string(ranking),
		Winner:      "c",
		Reasoning:   "C explains the product most clearly.",
		Reviewer:    "jerry",
		Source:      "github",
		SourceURL:   "https://github.com/owner/repo/issues/1#issuecomment-1",
		CreatedAt:   "2026-06-02T10:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRankedFeedbackEvent returned error: %v", err)
	}
	if err := store.UpsertRankedFeedbackEvent(context.Background(), db.RankedFeedbackEvent{
		RunID:       "planner-ranked-1",
		ItemID:      "item-001",
		RankingJSON: string(ranking),
		Winner:      "c",
		Reasoning:   "C is still strongest.",
		Reviewer:    "jerry",
		Source:      "github",
		SourceURL:   "https://github.com/owner/repo/issues/1#issuecomment-2",
		CreatedAt:   "2026-06-02T11:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRankedFeedbackEvent second returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "review", "status", "--home", home, "--run", "planner-ranked-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("skillopt review status exit code = %d, stderr=%s", code, stderr.String())
	}
	for _, want := range []string{
		"items: 1",
		"feedback: 2",
		"pairwise_preferences: 12",
		"mode: explore",
		"exploration_level: high",
		"ranking_stability: c 2/2",
		"recommended_next_mode: refine",
		"recommendation: recommend refine",
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
