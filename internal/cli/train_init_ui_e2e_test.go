package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

// TestTrainInitUIEndToEnd exercises the two #196 UI surfaces together: the
// line-oriented wizard completing a scaffold from stdin, and the dashboard
// reporting pending prompts and SkillOpt train phase, including answering a
// prompt through the shared interactive mechanism.
func TestTrainInitUIEndToEnd(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	store, err := dbtest.Open(t, paths.Database)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertAgentTemplate(context.Background(), cliSkillOptTemplate("planner", "Plan the work.")); err != nil {
		t.Fatalf("UpsertAgentTemplate returned error: %v", err)
	}
	// Seed a train session so the dashboard's train view has something to show.
	if err := store.UpsertSkillOptTrainSession(context.Background(), db.SkillOptTrainSession{
		ID:         "e2e-train",
		TemplateID: "planner",
		TargetRepo: "owner/product",
		State:      skillopt.TrainStateItemsReady,
	}); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	// 1. Complete the wizard from stdin.
	restoreInteractive := replaceSkillOptTrainInitInteractive(true)
	defer restoreInteractive()
	restoreStdin := replaceSkillOptTrainInitStdin("e2e-flow\nplanner\nowner/repo\ntext\ntext-table\nImprove planner answers.\n")
	defer restoreStdin()
	var wizOut, wizErr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "init", "--home", home}, &wizOut, &wizErr); code != 0 {
		t.Fatalf("wizard init exit code = %d, stderr=%s", code, wizErr.String())
	}
	cfg, err := skillopt.LoadTrainInitConfig(filepath.Join(workspace, ".gitmoot", "skillopt", "e2e-flow", "config.toml"))
	if err != nil {
		t.Fatalf("LoadTrainInitConfig returned error: %v", err)
	}
	if cfg.Name != "e2e-flow" || cfg.Template != "planner" {
		t.Fatalf("wizard config = %+v", cfg)
	}

	// 2. The dashboard reports the seeded train session phase.
	var dashOut, dashErr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home, "--json"}, &dashOut, &dashErr); code != 0 {
		t.Fatalf("dashboard exit code = %d, stderr=%s", code, dashErr.String())
	}
	var snapshot dashboardSnapshot
	if err := json.Unmarshal(dashOut.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode dashboard snapshot: %v\n%s", err, dashOut.String())
	}
	foundTrain := false
	for _, train := range snapshot.TrainSessions {
		if train.ID == "e2e-train" {
			foundTrain = true
			if train.Phase == "" {
				t.Fatalf("train session phase is empty: %+v", train)
			}
		}
	}
	if !foundTrain {
		t.Fatalf("dashboard did not report the train session: %+v", snapshot.TrainSessions)
	}

	// 3. Create a pending prompt with --prompts and confirm the dashboard shows
	//    it, then answer it through the dashboard's shared mechanism.
	var promptOut, promptErr bytes.Buffer
	if code := Run([]string{"skillopt", "train", "init", "--home", home, "--prompts", "--name", "e2e-prompted"}, &promptOut, &promptErr); code != 0 {
		t.Fatalf("train init --prompts exit code = %d, stderr=%s", code, promptErr.String())
	}
	pending := dashboardPendingPromptIDs(t, home)
	if len(pending) == 0 {
		t.Fatalf("expected pending prompts after --prompts")
	}
	// The dashboard's pending prompts match interactive list.
	if !sameStringSet(pending, interactiveListPromptIDs(t, home)) {
		t.Fatalf("dashboard pending prompts do not match interactive list")
	}
	// Answer the template prompt through the dashboard.
	templatePrompt := ""
	for _, id := range pending {
		if strings.HasSuffix(id, ".template") {
			templatePrompt = id
		}
	}
	if templatePrompt == "" {
		t.Fatalf("no template prompt among %v", pending)
	}
	var ansOut, ansErr bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home, "--answer", templatePrompt, "--value", "planner"}, &ansOut, &ansErr); code != 0 {
		t.Fatalf("dashboard --answer exit code = %d, stderr=%s", code, ansErr.String())
	}
	if contains(dashboardPendingPromptIDs(t, home), templatePrompt) {
		t.Fatalf("answered prompt %q should no longer be pending", templatePrompt)
	}
}

func dashboardPendingPromptIDs(t *testing.T, home string) []string {
	t.Helper()
	var out, errBuf bytes.Buffer
	if code := Run([]string{"dashboard", "--home", home, "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("dashboard exit code = %d, stderr=%s", code, errBuf.String())
	}
	var snapshot dashboardSnapshot
	if err := json.Unmarshal(out.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	ids := []string{}
	for _, prompt := range snapshot.PendingPrompts {
		ids = append(ids, prompt.ID)
	}
	return ids
}

func interactiveListPromptIDs(t *testing.T, home string) []string {
	t.Helper()
	var out, errBuf bytes.Buffer
	if code := Run([]string{"interactive", "list", "--home", home, "--state", "pending", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("interactive list exit code = %d, stderr=%s", code, errBuf.String())
	}
	var prompts []db.InteractivePrompt
	if err := json.Unmarshal(out.Bytes(), &prompts); err != nil {
		t.Fatalf("decode interactive list: %v", err)
	}
	ids := []string{}
	for _, prompt := range prompts {
		ids = append(ids, prompt.ID)
	}
	return ids
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[string]bool{}
	for _, value := range a {
		set[value] = true
	}
	for _, value := range b {
		if !set[value] {
			return false
		}
	}
	return true
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
