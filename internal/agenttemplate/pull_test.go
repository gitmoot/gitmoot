package agenttemplate

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

type fakeRemoteSource struct {
	commit  string
	entries []DirEntry
	files   map[string]string
}

func (f fakeRemoteSource) ResolveRef(context.Context, string, string) (string, error) {
	return f.commit, nil
}

func (f fakeRemoteSource) FetchFile(_ context.Context, _ string, _ string, path string) (File, error) {
	content, ok := f.files[path]
	if !ok {
		return File{}, fmt.Errorf("no file at %s", path)
	}
	return File{Content: content}, nil
}

func (f fakeRemoteSource) ListDir(context.Context, string, string, string) ([]DirEntry, error) {
	return f.entries, nil
}

func outcomeByID(results []PullResult) map[string]PullResult {
	out := make(map[string]PullResult, len(results))
	for _, result := range results {
		out[result.ID] = result
	}
	return out
}

func TestPullListsInstallsSkipsAndNoOps(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	frontend := testTemplateContent("frontend-reviewer", "Review UI changes.\n")
	api := testTemplateContent("api-reviewer", "Review API changes.\n")
	source := fakeRemoteSource{
		commit: "sha-1",
		entries: []DirEntry{
			{Name: "frontend-reviewer.md", Path: "templates/frontend-reviewer.md", Type: "file"},
			{Name: "api-reviewer.md", Path: "templates/api-reviewer.md", Type: "file"},
			// A built-in id present in the repo must be discovered then SKIPPED.
			{Name: PlannerTemplateID + ".md", Path: "templates/" + PlannerTemplateID + ".md", Type: "file"},
			// Non-template entries are filtered out by discovery.
			{Name: "README.md", Path: "templates/README.md", Type: "file"},
			{Name: "nested", Path: "templates/nested", Type: "dir"},
		},
		files: map[string]string{
			"templates/frontend-reviewer.md":         frontend,
			"templates/api-reviewer.md":              api,
			"templates/" + PlannerTemplateID + ".md": testTemplateContent(PlannerTemplateID, "Plan.\n"),
		},
	}

	// First pull with --all (empty ids) installs the two custom templates and
	// skips the built-in.
	results, err := Pull(ctx, store, source, "jerry/templates", "main", "templates", nil, false)
	if err != nil {
		t.Fatalf("Pull returned error: %v", err)
	}
	got := outcomeByID(results)
	if got["frontend-reviewer"].Outcome != PullInstalled || got["api-reviewer"].Outcome != PullInstalled {
		t.Fatalf("expected both custom templates installed, got %+v", results)
	}
	if got[PlannerTemplateID].Outcome != PullSkipped {
		t.Fatalf("expected built-in %s skipped, got %+v", PlannerTemplateID, results)
	}
	if _, ok := got["README"]; ok {
		t.Fatalf("README must not be discovered as a template: %+v", results)
	}

	// Second identical pull is a no-op for both.
	results, err = Pull(ctx, store, source, "jerry/templates", "main", "templates", nil, false)
	if err != nil {
		t.Fatalf("second Pull returned error: %v", err)
	}
	got = outcomeByID(results)
	if got["frontend-reviewer"].Outcome != PullUnchanged || got["api-reviewer"].Outcome != PullUnchanged {
		t.Fatalf("expected unchanged no-op on identical pull, got %+v", results)
	}

	// Change one template upstream: it re-fetches as a new version (conflict-as-
	// new-version), the other stays unchanged.
	source.commit = "sha-2"
	source.files["templates/frontend-reviewer.md"] = testTemplateContent("frontend-reviewer", "Review UI changes deeply.\n")
	results, err = Pull(ctx, store, source, "jerry/templates", "main", "templates", []string{"frontend-reviewer", "api-reviewer"}, false)
	if err != nil {
		t.Fatalf("third Pull returned error: %v", err)
	}
	got = outcomeByID(results)
	if got["frontend-reviewer"].Outcome != PullUpdated {
		t.Fatalf("expected frontend-reviewer updated, got %+v", results)
	}
	if got["api-reviewer"].Outcome != PullUnchanged {
		t.Fatalf("expected api-reviewer unchanged, got %+v", results)
	}
	stored, err := store.GetAgentTemplate(ctx, "frontend-reviewer")
	if err != nil {
		t.Fatalf("GetAgentTemplate returned error: %v", err)
	}
	if stored.VersionNumber != 2 {
		t.Fatalf("expected frontend-reviewer at v2 after conflict, got v%d", stored.VersionNumber)
	}
}

func TestPullDryRunWritesNothing(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	source := fakeRemoteSource{
		commit: "sha-1",
		files: map[string]string{
			"templates/frontend-reviewer.md": testTemplateContent("frontend-reviewer", "Review UI changes.\n"),
		},
	}
	results, err := Pull(ctx, store, source, "jerry/templates", "main", "templates", []string{"frontend-reviewer"}, true)
	if err != nil {
		t.Fatalf("Pull dry-run returned error: %v", err)
	}
	if len(results) != 1 || results[0].Outcome != PullInstalled {
		t.Fatalf("dry-run should report would-install, got %+v", results)
	}
	if _, err := store.GetAgentTemplate(ctx, "frontend-reviewer"); err == nil {
		t.Fatalf("dry-run must not write the template to the store")
	}
}

func TestPullReportsPerFileFailureAndContinues(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	source := fakeRemoteSource{
		commit: "sha-1",
		files: map[string]string{
			"templates/frontend-reviewer.md": testTemplateContent("frontend-reviewer", "Review UI changes.\n"),
			// api-reviewer.md is deliberately absent -> fetch fails.
		},
	}
	results, err := Pull(ctx, store, source, "jerry/templates", "main", "templates", []string{"api-reviewer", "frontend-reviewer"}, false)
	if err != nil {
		t.Fatalf("Pull returned error: %v", err)
	}
	got := outcomeByID(results)
	if got["api-reviewer"].Outcome != PullFailed || got["api-reviewer"].Detail == "" {
		t.Fatalf("expected api-reviewer failed with detail, got %+v", results)
	}
	// The batch continued and still installed the healthy one.
	if got["frontend-reviewer"].Outcome != PullInstalled {
		t.Fatalf("expected frontend-reviewer installed despite sibling failure, got %+v", results)
	}
}

func TestPullRejectsMalformedRepo(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	if _, err := Pull(context.Background(), store, fakeRemoteSource{}, "not-owner-repo", "main", "templates", []string{"x"}, false); err == nil {
		t.Fatalf("Pull accepted a malformed repo")
	}
}
