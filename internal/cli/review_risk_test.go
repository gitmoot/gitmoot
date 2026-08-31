package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestApplyReviewPolicyOffByDefault(t *testing.T) {
	home := t.TempDir()
	root := config.PathsForHome(home).Home
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// A config with no [review] section.
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte("[orchestrate]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var engine workflow.Engine
	applyReviewPolicy(&engine, root)
	if engine.RiskTiersEnabled {
		t.Fatal("applyReviewPolicy must leave risk tiers OFF when [review] is absent")
	}
	if engine.NativeReviewFanoutEnabled == nil || engine.NativeReviewFanoutEnabled("owner/repo") {
		t.Fatal("applyReviewPolicy must install native fanout OFF when [review] is absent")
	}
}

func TestApplyReviewPolicyEnabledFromConfig(t *testing.T) {
	home := t.TempDir()
	root := config.PathsForHome(home).Home
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[review]\nnative_fanout_enabled = true\nrisk_tiers_enabled = true\nhigh_risk_paths = [\"cmd/**\"]\nrisk_label_high = \"sev:1\"\n[repos.\"owner/off\".review]\nnative_fanout_enabled = false\n"
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var engine workflow.Engine
	applyReviewPolicy(&engine, root)
	if !engine.RiskTiersEnabled {
		t.Fatal("applyReviewPolicy must enable risk tiers from [review].risk_tiers_enabled")
	}
	if len(engine.HighRiskPaths) != 1 || engine.HighRiskPaths[0] != "cmd/**" {
		t.Fatalf("HighRiskPaths = %v", engine.HighRiskPaths)
	}
	if engine.RiskLabelHigh != "sev:1" {
		t.Fatalf("RiskLabelHigh = %q", engine.RiskLabelHigh)
	}
	if engine.NativeReviewFanoutEnabled == nil || !engine.NativeReviewFanoutEnabled("owner/on") {
		t.Fatal("global native_fanout_enabled = true was not applied")
	}
	if engine.NativeReviewFanoutEnabled("owner/off") {
		t.Fatal("repository native_fanout_enabled = false override was not applied")
	}
}

func TestApplyReviewPolicyEmptyHomeIsOff(t *testing.T) {
	var engine workflow.Engine
	applyReviewPolicy(&engine, "")
	if engine.RiskTiersEnabled {
		t.Fatal("empty home must resolve to risk tiers OFF")
	}
	if engine.NativeReviewFanoutEnabled == nil || engine.NativeReviewFanoutEnabled("owner/repo") {
		t.Fatal("empty home must resolve native fanout OFF")
	}
}

type reviewCompareClient struct {
	github.NoopClient
	base   string
	head   string
	result github.CompareResult
}

func (c *reviewCompareClient) CompareCommits(_ context.Context, _ github.Repository, base string, head string) (github.CompareResult, error) {
	c.base, c.head = base, head
	return c.result, nil
}

func TestWireReviewChangedFilesUsesExactReviewerHead(t *testing.T) {
	client := &reviewCompareClient{result: github.CompareResult{Status: "ahead", Files: []github.PullRequestFile{
		{Filename: "internal/z.go"},
		{Filename: "internal/a.go"},
		{Filename: "internal/a.go"},
	}}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client)
	if engine.ReviewChangedFiles == nil {
		t.Fatal("ReviewChangedFiles was not wired")
	}
	files, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "reviewer-head", "current-head")
	if err != nil {
		t.Fatalf("ReviewChangedFiles: %v", err)
	}
	if client.base != "reviewer-head" || client.head != "current-head" {
		t.Fatalf("compare range = %s...%s, want reviewer-head...current-head", client.base, client.head)
	}
	if len(files) != 2 || files[0] != "internal/a.go" || files[1] != "internal/z.go" {
		t.Fatalf("changed files = %#v, want sorted exact-head paths", files)
	}
}

// A follow-up range larger than GitHub's 300-file compare cap is scopable as
// long as CompareCommits recovered the whole list: gitmoot generates such ranges
// itself when the merge gate updates a PR branch, so rejecting them on size
// alone rejected its own input.
func TestWireReviewChangedFilesScopesCompleteCompareBeyondCompareCap(t *testing.T) {
	files := make([]github.PullRequestFile, 1200)
	for i := range files {
		files[i].Filename = fmt.Sprintf("internal/pkg%04d/file.go", len(files)-i)
	}
	client := &reviewCompareClient{result: github.CompareResult{Status: "ahead", Files: files}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client)

	paths, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "reviewer-head", "current-head")
	if err != nil {
		t.Fatalf("a complete %d-file compare must scope: %v", len(files), err)
	}
	if len(paths) != len(files) {
		t.Fatalf("changed files = %d, want all %d paths", len(paths), len(files))
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("changed files are not sorted: %v...", paths[:3])
	}
	if paths[0] != "internal/pkg0001/file.go" || paths[len(paths)-1] != "internal/pkg1200/file.go" {
		t.Fatalf("changed files bounds = %q..%q", paths[0], paths[len(paths)-1])
	}
}

func TestWireReviewChangedFilesRejectsTruncatedCompare(t *testing.T) {
	files := make([]github.PullRequestFile, 300)
	for i := range files {
		files[i].Filename = fmt.Sprintf("internal/pkg%04d/file.go", i)
	}
	client := &reviewCompareClient{result: github.CompareResult{Status: "ahead", Files: files, Truncated: true}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client)

	_, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "reviewer-head", "current-head")
	var unavailable workflow.ReviewScopeUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("ReviewChangedFiles error = %v, want ReviewScopeUnavailableError", err)
	}
}

func TestWireReviewChangedFilesMarksDivergedRangeUnscopable(t *testing.T) {
	client := &reviewCompareClient{result: github.CompareResult{Status: "diverged"}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client)

	_, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "reviewer-head", "current-head")
	var unavailable workflow.ReviewScopeUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("ReviewChangedFiles error = %v, want ReviewScopeUnavailableError", err)
	}
}
