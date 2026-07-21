package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/memory"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

func TestSkillOptCandidatePromoteStagesSuccessObservation(t *testing.T) {
	home, versionID := seedSkillOptPromotionCandidate(t, true)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "candidate", "promote", "--home", home, versionID}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("candidate promote exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	store := openCLIJobStore(t, home)
	obs, err := store.ListMemoryObservations(context.Background(), "builder", "owner/repo")
	if err != nil {
		t.Fatalf("ListMemoryObservations returned error: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("promote should stage one observation, got %+v", obs)
	}
	got := obs[0]
	if got.Owner.Ref != "builder" || got.Repo != "owner/repo" || got.Scope != memory.ScopeRepo {
		t.Fatalf("observation owner/repo/scope = %+v", got)
	}
	if !strings.HasPrefix(got.Key, "skillopt:planner@v2-promoted:") {
		t.Fatalf("observation key = %q", got.Key)
	}
	if got.TrustMark != memory.TrustLow || got.Provenance != "skillopt-promotion:planner@v2" {
		t.Fatalf("observation trust/provenance = %q/%q", got.TrustMark, got.Provenance)
	}
	for _, want := range []string{
		"SkillOpt promoted planner@v2 over planner@v1",
		"review score 0.87",
		"replay gate accepted with candidate mean 0.91 over champion mean 0.72",
		"tighten result JSON",
		"avoid generic output",
	} {
		if !strings.Contains(got.Content, want) {
			t.Fatalf("observation content missing %q:\n%s", want, got.Content)
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"skillopt", "candidate", "promote", "--home", home, versionID}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("second promote unexpectedly succeeded; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	obs, err = store.ListMemoryObservations(context.Background(), "builder", "owner/repo")
	if err != nil {
		t.Fatalf("ListMemoryObservations after repeat returned error: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("repeat promote must not duplicate observations, got %+v", obs)
	}
}

func TestSkillOptCandidatePromoteDistillSuccessesDefaultOff(t *testing.T) {
	home, versionID := seedSkillOptPromotionCandidate(t, false)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "candidate", "promote", "--home", home, versionID}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("candidate promote exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	store := openCLIJobStore(t, home)
	obs, err := store.ListMemoryObservations(context.Background(), "builder", "owner/repo")
	if err != nil {
		t.Fatalf("ListMemoryObservations returned error: %v", err)
	}
	if len(obs) != 0 {
		t.Fatalf("distill_successes default-off should write no observations, got %+v", obs)
	}
}

func seedSkillOptPromotionCandidate(t *testing.T, distillSuccesses bool) (string, string) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if distillSuccesses {
		if err := os.WriteFile(paths.ConfigFile, []byte(config.DefaultConfig(paths)+`
[memory]
distill_successes = true
`), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	template := cliSkillOptTemplate("planner", "Plan the work.")
	template.SourceRepo = "owner/repo"
	if err := store.UpsertAgentTemplate(ctx, template); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	if err := store.UpsertAgent(ctx, db.Agent{
		Name:           "builder",
		Runtime:        runtime.ShellRuntime,
		RuntimeRef:     "true",
		RepoScope:      "owner/repo",
		TemplateID:     "planner",
		Capabilities:   []string{"ask"},
		AutonomyPolicy: runtime.AutonomyPolicyAuto,
	}); err != nil {
		t.Fatalf("UpsertAgent returned error: %v", err)
	}
	installed, err := store.GetAgentTemplate(ctx, "planner")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	candidate := cliSkillOptCandidatePackage(t, "planner", installed.VersionID, "Plan with stronger guidance.")
	version, err := skillopt.ImportCandidatePackageWithOptions(ctx, store, candidate, skillopt.CandidateImportOptions{SourcePath: "candidate.json"})
	if err != nil {
		t.Fatalf("ImportCandidatePackageWithOptions returned error: %v", err)
	}
	review, err := store.GetAgentTemplateCandidateReview(ctx, version.ID)
	if err != nil {
		t.Fatalf("GetAgentTemplateCandidateReview returned error: %v", err)
	}
	score := 0.87
	review.Score = &score
	review.SummaryMetadataJSON = `{"rejected_traits":{"Option A":["avoid generic output"]},"required_improvements":["tighten result JSON"]}`
	if err := store.UpsertAgentTemplateCandidateReview(ctx, review); err != nil {
		t.Fatalf("UpsertAgentTemplateCandidateReview returned error: %v", err)
	}
	if err := store.InsertSkillOptGateRun(ctx, db.SkillOptGateRun{
		ID:                 "gate-1",
		TemplateID:         "planner",
		CandidateVersionID: version.ID,
		ChampionVersionID:  installed.VersionID,
		Accepted:           true,
		ChampionMean:       0.72,
		CandidateMean:      0.91,
		CorpusItems:        2,
		Attempts:           1,
	}); err != nil {
		t.Fatalf("InsertSkillOptGateRun returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	return home, version.ID
}
