package skillopt

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

func TestExportTrainingPackageIncludesBinaryVerdicts(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	template := testTemplate("planner", "Plan carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "run-b",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "ready",
	}); err != nil {
		t.Fatalf("UpsertEvalRun: %v", err)
	}
	if err := store.UpsertBinaryVerdict(ctx, db.BinaryVerdict{RunID: "run-b", QuestionID: "q1", Dimension: "correctness", Verdict: "yes", Explanation: "ok"}); err != nil {
		t.Fatalf("UpsertBinaryVerdict: %v", err)
	}
	if err := store.UpsertBinaryVerdict(ctx, db.BinaryVerdict{RunID: "run-b", QuestionID: "q2", Dimension: "style", Verdict: "no"}); err != nil {
		t.Fatalf("UpsertBinaryVerdict: %v", err)
	}

	pkg, err := ExportTrainingPackage(ctx, store, "run-b")
	if err != nil {
		t.Fatalf("ExportTrainingPackage: %v", err)
	}
	if len(pkg.BinaryVerdicts) != 2 {
		t.Fatalf("binary verdicts = %d, want 2", len(pkg.BinaryVerdicts))
	}
	if pkg.BinaryVerdicts[0].QuestionID != "q1" || pkg.BinaryVerdicts[0].Verdict != "yes" {
		t.Fatalf("first verdict = %+v", pkg.BinaryVerdicts[0])
	}

	// JSON round-trip preserves the section.
	raw, err := json.Marshal(pkg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"binary_verdicts"`) {
		t.Fatalf("marshaled packet missing binary_verdicts section: %s", raw)
	}
	var decoded TrainingPackage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.BinaryVerdicts) != 2 || decoded.BinaryVerdicts[1].Dimension != "style" {
		t.Fatalf("round-tripped verdicts = %+v", decoded.BinaryVerdicts)
	}
}

func TestExportTrainingPackageOmitsBinaryVerdictsWhenAbsent(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	template := testTemplate("planner", "Plan carefully.")
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate: %v", err)
	}
	if err := store.UpsertEvalRun(ctx, db.EvalRun{
		ID:                "run-empty",
		TemplateID:        "planner",
		TemplateVersionID: installed.VersionID,
		TargetRepo:        "owner/repo",
		State:             "ready",
	}); err != nil {
		t.Fatalf("UpsertEvalRun: %v", err)
	}

	pkg, err := ExportTrainingPackage(ctx, store, "run-empty")
	if err != nil {
		t.Fatalf("ExportTrainingPackage: %v", err)
	}
	if pkg.BinaryVerdicts != nil {
		t.Fatalf("binary verdicts = %+v, want nil", pkg.BinaryVerdicts)
	}
	// omitempty keeps a verdict-less packet byte-identical to the pre-#525 shape.
	raw, err := json.Marshal(pkg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "binary_verdicts") {
		t.Fatalf("verdict-less packet unexpectedly carries binary_verdicts: %s", raw)
	}
}
