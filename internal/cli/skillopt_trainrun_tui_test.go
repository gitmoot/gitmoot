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
