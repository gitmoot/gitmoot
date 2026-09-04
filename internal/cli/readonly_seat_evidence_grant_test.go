package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// seedVerdict writes one prior review verdict for repo into a REAL database,
// left in WAL mode - the state that made an earlier form of this fix inert.
func seedVerdict(t *testing.T, paths config.Paths, repo, jobID, summary string) {
	t.Helper()
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()
	payload, err := json.Marshal(workflow.JobPayload{
		Repo:        repo,
		PullRequest: 1845,
		HeadSHA:     "cdda6b319e9c24945d40cd44bc10d843c60dd93a",
		Result: &workflow.AgentResult{
			Decision: "changes_requested",
			Severity: "P2",
			Evidence: workflow.EvidenceStaticOnly,
			Summary:  summary,
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID: jobID, Agent: "gm-review-opus", Type: "review",
		State: string(workflow.JobSucceeded), Payload: string(payload),
	}, db.JobEvent{Kind: string(workflow.JobSucceeded), Message: "verdict"}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
}

// TestPriorVerdictsArtifactIsReadableAndScoped is the SUFFICIENCY test.
//
// An earlier version asserted that a path appeared in a grant slice, which
// could not notice that a read-only seat cannot open the live WAL store at all.
// Path membership is not readability, so this reads the artifact the seat is
// actually given and checks what is in it - and what is NOT.
func TestPriorVerdictsArtifactIsReadableAndScoped(t *testing.T) {
	live := config.PathsForHome(t.TempDir())
	seedVerdict(t, live, "gitmoot/gitmoot", "mine-1", "a verdict on the repo under review")
	seedVerdict(t, live, "other/repo", "theirs-1", "a verdict on a DIFFERENT repo")

	file, diagnostic := stagePriorVerdicts(context.Background(), live, t.TempDir(), "gitmoot/gitmoot")
	if diagnostic != "" {
		t.Fatalf("staging reported %q", diagnostic)
	}
	if file == "" {
		t.Fatal("no artifact staged from a store that exists: the seat cannot enumerate prior verdicts, which is the #1839 defect")
	}

	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read the artifact the seat is given: %v", err)
	}
	var list priorVerdictList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("the artifact is not readable as JSON: %v", err)
	}
	if list.Repo != "gitmoot/gitmoot" || strings.TrimSpace(list.AsOf) == "" {
		t.Errorf("artifact does not state its own scope and as-of time: repo=%q as_of=%q", list.Repo, list.AsOf)
	}
	if len(list.Verdicts) != 1 {
		t.Fatalf("got %d verdicts, want exactly the one in scope: %+v", len(list.Verdicts), list.Verdicts)
	}
	got := list.Verdicts[0]
	if got.JobID != "mine-1" || got.Decision != "changes_requested" || got.Severity != "P2" {
		t.Errorf("verdict does not carry what a reviewer needs: %+v", got)
	}
	if got.Evidence != workflow.EvidenceStaticOnly {
		t.Errorf("evidence = %q, want %q: the executed/static-only distinction must survive into what the seat reads", got.Evidence, workflow.EvidenceStaticOnly)
	}

	// CROSS-REPO DISCLOSURE is the property that matters most here: an earlier
	// form copied the whole database, so every other repo's jobs, prompts and
	// results travelled to an untrusted runtime.
	if strings.Contains(string(body), "other/repo") || strings.Contains(string(body), "theirs-1") {
		t.Error("the artifact contains another repo's verdicts: cross-repo disclosure")
	}
	// And the fields a database copy would have carried are structurally
	// absent, not merely unmentioned.
	for _, forbidden := range []string{"owner_token", "task_state_claims", "confirmed_memories", "prompt"} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("the artifact carries %q, which a rendered verdict list must not", forbidden)
		}
	}
}

// TestPriorVerdictsRefusesAnUnscopedSeat pins the fail-closed direction: with
// no single repo to scope to, staging must decline and SAY SO rather than fall
// back to everything.
func TestPriorVerdictsRefusesAnUnscopedSeat(t *testing.T) {
	live := config.PathsForHome(t.TempDir())
	seedVerdict(t, live, "gitmoot/gitmoot", "mine-1", "verdict")

	for _, scope := range []string{"", "gitmoot/*", "   "} {
		file, diagnostic := stagePriorVerdicts(context.Background(), live, t.TempDir(), scope)
		if file != "" {
			t.Errorf("scope %q staged %q; an unscoped copy is the defect this replaces", scope, file)
		}
		if strings.TrimSpace(diagnostic) == "" {
			t.Errorf("scope %q declined SILENTLY; a seat losing its evidence must be told why", scope)
		}
	}
}

// TestPriorVerdictsSurvivesAnUncleanedPredecessor pins the defect where a
// leftover directory from a crashed job made the NEXT seat evidence-less.
//
// The staging destination lives in a root the daemon owns and has just
// re-created, so a leftover must be cleared rather than treated as an error.
func TestPriorVerdictsSurvivesAnUncleanedPredecessor(t *testing.T) {
	live := config.PathsForHome(t.TempDir())
	seedVerdict(t, live, "gitmoot/gitmoot", "mine-1", "verdict")
	cacheRoot := t.TempDir()

	// A predecessor's artifact, plus junk beside it, left behind by a job whose
	// cleanup never ran.
	stale := filepath.Join(cacheRoot, "evidence")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "prior-verdicts.json"), []byte("STALE"), 0o600); err != nil {
		t.Fatal(err)
	}

	file, diagnostic := stagePriorVerdicts(context.Background(), live, cacheRoot, "gitmoot/gitmoot")
	if file == "" {
		t.Fatalf("a leftover directory made this seat evidence-less (%s): that is the silent degradation this change removes", diagnostic)
	}
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "STALE") {
		t.Error("the seat was handed the PREDECESSOR's artifact")
	}
}

// TestReadOnlyGrantsStagePriorVerdictsThroughProduction drives the real grant
// builder rather than the helper - the "test pins a helper production never
// reaches" trap, which a mutant deleting the wiring survived once already.
func TestReadOnlyGrantsStagePriorVerdictsThroughProduction(t *testing.T) {
	home := t.TempDir()
	live := config.PathsForHome(home)
	seedVerdict(t, live, "gitmoot/gitmoot", "mine-1", "prior verdict via production")

	checkout := t.TempDir()
	runGit(t, checkout, "init", "-b", "main")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot")
	runGit(t, checkout, "commit", "--allow-empty", "-m", "init")

	agent := runtime.Agent{
		Name: "seat", Runtime: runtime.CodexRuntime, ReadOnlySeat: true,
		RepoScope: "gitmoot/gitmoot",
	}
	grants, err := readOnlyRuntimeSandboxGrants(home, agent, checkout, true)
	if err != nil {
		t.Fatalf("readOnlyRuntimeSandboxGrants: %v", err)
	}
	if grants.evidenceFile == "" {
		t.Fatal("production staged no verdict list: a seat cannot enumerate prior verdicts, the #1839 defect")
	}
	if !strings.HasPrefix(grants.evidenceFile, grants.cacheRoot) {
		t.Errorf("artifact %q is outside the seat's own cache root %q, so it would need a grant of its own", grants.evidenceFile, grants.cacheRoot)
	}
	if want := "GITMOOT_PRIOR_VERDICTS=" + grants.evidenceFile; !containsString(grants.env, want) {
		t.Errorf("env %v does not export %q, so nothing can find the artifact", grants.env, want)
	}
	// The live store is never granted, which is what retires the
	// credential-adjacency question rather than mitigating it.
	for _, granted := range grants.readFiles {
		if granted == live.Database || strings.HasPrefix(granted, live.Home+string(filepath.Separator)) {
			t.Errorf("the LIVE store or a file beside it is granted (%q); a rendered artifact exists so nothing there needs to be", granted)
		}
	}
	for _, root := range grants.reads {
		if root == live.Home {
			t.Errorf("the gitmoot home %q is granted as a read root, exposing everything beside the store", root)
		}
	}
	body, err := os.ReadFile(grants.evidenceFile)
	if err != nil {
		t.Fatalf("read the production artifact: %v", err)
	}
	if !strings.Contains(string(body), "mine-1") {
		t.Errorf("the production artifact holds no verdicts: %s", body)
	}
}
