package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
)

func routerTestHome(t *testing.T) (string, *db.Store) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return home, store
}

func seedRouterRows(t *testing.T, store *db.Store) {
	t.Helper()
	ctx := context.Background()
	rows := []db.RoutingTelemetry{
		{JobID: "j1", Repo: "acme/widget", Action: "implement", Runtime: "codex", Model: "gpt-5.5", TemplateID: "impl", JobState: "succeeded", Decision: "implemented", Approved: true, DurationMS: 100, InputTokens: 10, OutputTokens: 2},
		{JobID: "j2", Repo: "acme/widget", Action: "implement", Runtime: "codex", Model: "gpt-5.5", TemplateID: "impl", JobState: "failed", Decision: "failed", DurationMS: 300},
		{JobID: "j3", Repo: "acme/widget", Action: "review", Runtime: "claude", Model: "opus", TemplateID: "rev", JobState: "succeeded", Decision: "approved", Approved: true, DurationMS: 200},
		{JobID: "j4", Repo: "other/repo", Action: "review", Runtime: "kimi", JobState: "blocked", Decision: "blocked", DurationMS: 50},
	}
	for _, r := range rows {
		if err := store.InsertRoutingTelemetry(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.JobID, err)
		}
	}
}

// TestRouterSummaryJSON proves the read-only summary aggregates seeded rows into
// per-(action,runtime,model,template) groups with the right counts/rates and the
// mandatory advisory disclaimer.
func TestRouterSummaryJSON(t *testing.T) {
	home, store := routerTestHome(t)
	seedRouterRows(t, store)

	var stdout, stderr bytes.Buffer
	if code := runRouter([]string{"summary", "--home", home, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("router summary exit %d: %s", code, stderr.String())
	}
	var out routerSummaryJSON
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("parse json: %v (%s)", err, stdout.String())
	}
	if !strings.Contains(out.Note, "not a benchmark") {
		t.Fatalf("note missing disclaimer: %q", out.Note)
	}
	if out.Total != 4 {
		t.Fatalf("total = %d, want 4", out.Total)
	}
	if len(out.Groups) != 3 {
		t.Fatalf("groups = %d, want 3: %+v", len(out.Groups), out.Groups)
	}
	// Leading group is codex/implement (2 observations): 1 success of 2.
	g := out.Groups[0]
	if g.Action != "implement" || g.Runtime != "codex" || g.Count != 2 || g.SuccessCount != 1 || g.FailedCount != 1 {
		t.Fatalf("leading group mismatch: %+v", g)
	}
	if g.SuccessRate != 0.5 {
		t.Fatalf("success rate = %v, want 0.5", g.SuccessRate)
	}
	if g.MedianDurationMS != 200 {
		t.Fatalf("median = %d, want 200 (avg of 100,300)", g.MedianDurationMS)
	}
}

// TestRouterSummaryRepoFilter proves the --repo filter narrows to one repo.
func TestRouterSummaryRepoFilter(t *testing.T) {
	home, store := routerTestHome(t)
	seedRouterRows(t, store)

	var stdout, stderr bytes.Buffer
	if code := runRouter([]string{"summary", "--home", home, "--repo", "other/repo", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("router summary exit %d: %s", code, stderr.String())
	}
	var out routerSummaryJSON
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("parse json: %v", err)
	}
	if out.Total != 1 || len(out.Groups) != 1 || out.Groups[0].Runtime != "kimi" {
		t.Fatalf("repo filter mismatch: %+v", out)
	}
}

// TestRouterSummaryActionFilterText proves the --action filter and that the text
// output carries the advisory disclaimer.
func TestRouterSummaryActionFilterText(t *testing.T) {
	home, store := routerTestHome(t)
	seedRouterRows(t, store)

	var stdout, stderr bytes.Buffer
	if code := runRouter([]string{"summary", "--home", home, "--action", "review"}, &stdout, &stderr); code != 0 {
		t.Fatalf("router summary exit %d: %s", code, stderr.String())
	}
	text := stdout.String()
	if !strings.Contains(text, "not a benchmark") {
		t.Fatalf("text output missing disclaimer:\n%s", text)
	}
	if !strings.Contains(text, "claude") || !strings.Contains(text, "kimi") {
		t.Fatalf("expected both review runtimes in text:\n%s", text)
	}
	if strings.Contains(text, "codex") {
		t.Fatalf("action=review output should not include implement/codex rows:\n%s", text)
	}
}

// TestRouterSummaryEmpty proves a fresh home reports zero observations cleanly.
func TestRouterSummaryEmpty(t *testing.T) {
	home, _ := routerTestHome(t)
	var stdout, stderr bytes.Buffer
	if code := runRouter([]string{"summary", "--home", home}, &stdout, &stderr); code != 0 {
		t.Fatalf("router summary exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no routing telemetry recorded yet") {
		t.Fatalf("expected empty message, got:\n%s", stdout.String())
	}
}

// TestRouterContextEnabledResolves proves the [router] context_enabled config knob
// resolves through routerContextEnabled (off by default, on when set).
func TestRouterContextEnabledResolves(t *testing.T) {
	home, _ := routerTestHome(t)
	if routerContextEnabled(home) {
		t.Fatalf("expected context injection off by default")
	}
	paths := config.PathsForHome(home)
	existing, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, append(existing, []byte("\n[router]\ncontext_enabled = true\n")...), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if !routerContextEnabled(home) {
		t.Fatalf("expected context injection on after config set")
	}
	settings, err := config.LoadRouterSettings(paths)
	if err != nil {
		t.Fatalf("LoadRouterSettings: %v", err)
	}
	if !settings.ContextEnabled {
		t.Fatalf("LoadRouterSettings did not read context_enabled")
	}
}
