package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

func replaceSkillOptTrainRunTUI(capable bool, run func(home, sessionID string, stdout, stderr io.Writer) int) func() {
	prevCapable := skillOptTrainRunTUICapable
	prevRun := runSkillOptTrainRunTUI
	skillOptTrainRunTUICapable = func() bool { return capable }
	if run != nil {
		runSkillOptTrainRunTUI = run
	}
	return func() {
		skillOptTrainRunTUICapable = prevCapable
		runSkillOptTrainRunTUI = prevRun
	}
}

func seedTrainSession(t *testing.T, home string, session db.SkillOptTrainSession) {
	t.Helper()
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if err := store.UpsertSkillOptTrainSession(context.Background(), session); err != nil {
		t.Fatalf("UpsertSkillOptTrainSession: %v", err)
	}
}

func TestResolveSkillOptTrainRunSession(t *testing.T) {
	home := t.TempDir()

	if id, err := resolveSkillOptTrainRunSession(home, "sess-direct", ""); err != nil || id != "sess-direct" {
		t.Fatalf("explicit session = (%q, %v), want (sess-direct, nil)", id, err)
	}
	if _, err := resolveSkillOptTrainRunSession(home, "", ""); err == nil {
		t.Fatal("expected error with neither --session nor --config")
	}
}

func TestResolveSkillOptTrainRunSessionFromConfigNewest(t *testing.T) {
	home := t.TempDir()
	if err := config.Initialize(config.PathsForHome(home)); err != nil {
		t.Fatalf("init: %v", err)
	}
	// Real session IDs embed a sortable timestamp; the resolver breaks the
	// coarse created_at tie by id descending, so the later-timestamped id wins.
	seedTrainSession(t, home, db.SkillOptTrainSession{ID: "train-planner-20260101-000000-1", TemplateID: "planner", TargetRepo: "o/r", State: "items_ready"})
	seedTrainSession(t, home, db.SkillOptTrainSession{ID: "train-planner-20260201-000000-1", TemplateID: "planner", TargetRepo: "o/r", State: "items_ready"})
	seedTrainSession(t, home, db.SkillOptTrainSession{ID: "train-writer-20260301-000000-1", TemplateID: "writer", TargetRepo: "o/r", State: "items_ready"})

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := "name = \"x\"\ntemplate = \"planner\"\ntemplate_version = \"planner@v1\"\nreview_repo = \"o/r\"\ntask_kind = \"custom\"\nartifact_kind = \"text\"\npreview = \"none\"\nmode = \"explore\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	id, err := resolveSkillOptTrainRunSession(home, "", cfgPath)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != "train-planner-20260201-000000-1" {
		t.Fatalf("resolved newest matching session = %q, want train-planner-20260201-000000-1", id)
	}
}

func TestSkillOptTrainRunDispatchLaunchesTUI(t *testing.T) {
	var gotSession string
	restore := replaceSkillOptTrainRunTUI(true, func(_, sessionID string, _, _ io.Writer) int {
		gotSession = sessionID
		return 0
	})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "run", "--session", "s1", "--home", t.TempDir()}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if gotSession != "s1" {
		t.Fatalf("TUI launched with session %q, want s1", gotSession)
	}
}

func TestSkillOptTrainRunDispatchPlainFallback(t *testing.T) {
	home := t.TempDir()
	if err := config.Initialize(config.PathsForHome(home)); err != nil {
		t.Fatalf("init: %v", err)
	}
	seedTrainSession(t, home, db.SkillOptTrainSession{ID: "s1", TemplateID: "planner", TargetRepo: "o/r", State: "items_ready", CreatedAt: "2026-01-01T00:00:00Z"})

	restore := replaceSkillOptTrainRunTUI(false, nil) // not capable → plain fallback
	defer restore()

	var stdout, stderr bytes.Buffer
	code := Run([]string{"skillopt", "train", "run", "--session", "s1", "--home", home}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "session: s1") || !strings.Contains(out, "next: gitmoot skillopt train continue --session s1") {
		t.Fatalf("plain fallback output unexpected:\n%s", out)
	}
}

func TestBuildSkillOptTrainRunPlan(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := "name = \"x\"\ntemplate = \"planner\"\ntemplate_version = \"planner@v1\"\nreview_repo = \"o/r\"\ntask_kind = \"custom\"\nartifact_kind = \"text\"\npreview = \"none\"\nmode = \"explore\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// No workspace repo → confirm screen must ask for it; template label not doubled.
	plan, err := buildSkillOptTrainRunPlan(cfgPath, "")
	if err != nil {
		t.Fatalf("buildSkillOptTrainRunPlan: %v", err)
	}
	if !plan.NeedWorkspaceRepo || plan.Template != "planner @v1" || plan.ReviewRepo != "o/r" {
		t.Fatalf("plan = %+v", plan)
	}
	// With a workspace repo → no prompt needed.
	plan, err = buildSkillOptTrainRunPlan(cfgPath, "o/ws")
	if err != nil {
		t.Fatalf("buildSkillOptTrainRunPlan: %v", err)
	}
	if plan.NeedWorkspaceRepo || plan.WorkspaceRepo != "o/ws" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestCreateSkillOptTrainRunSession(t *testing.T) {
	home := t.TempDir()
	workspace := chdirTemp(t)
	seedPlannerTemplate(t, home)
	// A scaffold config + items the start command can read.
	scaffold := filepath.Join(workspace, ".gitmoot", "skillopt", "runsess")
	if err := os.MkdirAll(scaffold, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(scaffold, "config.toml")
	cfg := "name = \"runsess\"\ntemplate = \"planner\"\ntemplate_version = \"planner@v1\"\nreview_repo = \"jerryfane/gitmoot\"\ntask_kind = \"custom\"\nartifact_kind = \"text\"\npreview = \"none\"\nmode = \"explore\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	itemsPath := filepath.Join(scaffold, "review-items.yml")
	if err := os.WriteFile(itemsPath, []byte("items:\n  - title: One\n    brief: First item.\n    output_type: markdown\n  - title: Two\n    brief: Second item.\n    output_type: markdown\n"), 0o644); err != nil {
		t.Fatalf("write items: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scaffold, "task.md"), []byte("Improve the planner summaries.\n"), 0o644); err != nil {
		t.Fatalf("write task.md: %v", err)
	}

	// Stub GitHub so --create-repos does not hit the network.
	restore := replaceSkillOptGitHubClient(&repoCreateFakeGitHub{existing: map[string]bool{"jerryfane/gitmoot": true, "jerryfane/gitmoot-ws": true}})
	defer restore()

	id, err := createSkillOptTrainRunSession(home, cfgPath, "jerryfane/gitmoot-ws", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("createSkillOptTrainRunSession: %v", err)
	}
	if id == "" {
		t.Fatal("expected a session id")
	}
	// The session exists and is loadable.
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if _, err := store.GetSkillOptTrainSession(context.Background(), id); err != nil {
		t.Fatalf("created session not found: %v", err)
	}

	// Missing workspace repo → error, no session.
	if _, err := createSkillOptTrainRunSession(home, cfgPath, "", &bytes.Buffer{}); err == nil {
		t.Fatal("empty workspace repo should error")
	}
}

func TestTailSkillOptTrainLog(t *testing.T) {
	home := t.TempDir()
	if err := config.Initialize(config.PathsForHome(home)); err != nil {
		t.Fatalf("init: %v", err)
	}
	logPath := filepath.Join(config.PathsForHome(home).Logs, "skillopt-train-sess.log")

	// Missing log → no lines, offset unchanged.
	lines, off, err := tailSkillOptTrainLog(home, "sess", 0)
	if err != nil || len(lines) != 0 || off != 0 {
		t.Fatalf("missing log: lines=%v off=%d err=%v", lines, off, err)
	}

	// Write two complete lines + a partial (no trailing newline).
	if err := os.WriteFile(logPath, []byte("first\nsecond\npartial"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	lines, off, err = tailSkillOptTrainLog(home, "sess", 0)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(lines) != 2 || lines[0] != "first" || lines[1] != "second" {
		t.Fatalf("complete lines = %v (partial should be withheld)", lines)
	}
	// Offset is past "first\nsecond\n" (13 bytes), before "partial".
	if off != 13 {
		t.Fatalf("offset = %d, want 13", off)
	}

	// From that offset, after the partial line is completed, it appears.
	if err := os.WriteFile(logPath, []byte("first\nsecond\npartial now done\n"), 0o644); err != nil {
		t.Fatalf("rewrite log: %v", err)
	}
	lines, off2, err := tailSkillOptTrainLog(home, "sess", off)
	if err != nil {
		t.Fatalf("tail2: %v", err)
	}
	if len(lines) != 1 || lines[0] != "partial now done" || off2 <= off {
		t.Fatalf("second tail = %v off=%d", lines, off2)
	}

	// Empty session id → no-op.
	if l, _, err := tailSkillOptTrainLog(home, "", 5); err != nil || len(l) != 0 {
		t.Fatalf("empty session: lines=%v err=%v", l, err)
	}
}

func TestToTrainRunSnapshot(t *testing.T) {
	snap := skillOptTrainStatusSnapshot{
		SessionID:       "s1",
		IterationID:     "s1-001",
		TemplateID:      "smithyx",
		TemplateVersion: "smithyx@v9",
		TargetRepo:      "o/r",
		StatusPhase:     "review_published",
		CurrentPhase:    "review_published",
		IssueURL:        "https://github.com/o/r/issues/7",
		Counts:          skillOptTrainStatusCountsJSON{ReviewItems: 2, FeedbackEvents: 1, RankedFeedbackEvents: 2},
		Progress:        skillOptTrainStatusProgress{GeneratedOptions: 4, ETA: "41s"},
		Verbose:         &skillOptTrainStatusVerbose{Elapsed: "2m", Jobs: skillOptTrainStatusJobs{Running: 1, Succeeded: 3, Failed: 0}},
	}
	out := toTrainRunSnapshot(snap)
	if out.Template != "smithyx @v9" {
		t.Fatalf("template label = %q, want \"smithyx @v9\" (no doubled id)", out.Template)
	}
	if out.Phase != "review_published" || out.IssueURL != "https://github.com/o/r/issues/7" {
		t.Fatalf("phase/issue mapping wrong: %+v", out)
	}
	if out.FeedbackCount != 3 || out.ReviewItems != 2 || out.GeneratedOptions != 4 {
		t.Fatalf("counts mapping wrong: %+v", out)
	}
	if out.JobsRunning != 1 || out.JobsSucceeded != 3 || out.Elapsed != "2m" {
		t.Fatalf("verbose mapping wrong: %+v", out)
	}
}
