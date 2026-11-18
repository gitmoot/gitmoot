package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/github/githubtest"
	"github.com/gitmoot/gitmoot/internal/reviewseverity"
	"github.com/gitmoot/gitmoot/internal/subprocess"
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
	if engine.NativeReviewFanoutEnabled == nil || engine.NativeReviewFanoutEnabled("owner/repo") {
		t.Fatal("applyReviewPolicy must install native fanout OFF when [review] is absent")
	}
	if engine.ReviewBlockingSeverity == nil || engine.ReviewBlockingSeverity("owner/repo") != reviewseverity.DefaultBlocking {
		t.Fatal("applyReviewPolicy must install block-all review severity when [review] is absent")
	}
}

func TestApplyReviewPolicyEnabledFromConfig(t *testing.T) {
	home := t.TempDir()
	root := config.PathsForHome(home).Home
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[review]\nnative_fanout_enabled = true\nblocking_severity = \"P2\"\n[repos.\"owner/off\".review]\nnative_fanout_enabled = false\nblocking_severity = \"P1\"\n"
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var engine workflow.Engine
	applyReviewPolicy(&engine, root)
	if engine.NativeReviewFanoutEnabled == nil || !engine.NativeReviewFanoutEnabled("owner/on") {
		t.Fatal("global native_fanout_enabled = true was not applied")
	}
	if engine.NativeReviewFanoutEnabled("owner/off") {
		t.Fatal("repository native_fanout_enabled = false override was not applied")
	}
	if engine.ReviewBlockingSeverity == nil || engine.ReviewBlockingSeverity("owner/on") != reviewseverity.P2 {
		t.Fatal("global blocking_severity = P2 was not applied")
	}
	if engine.ReviewBlockingSeverity("owner/off") != reviewseverity.P1 {
		t.Fatal("repository blocking_severity = P1 override was not applied")
	}
}

func TestApplyReviewPolicyInvalidSeverityRetainsValidSafetyPolicy(t *testing.T) {
	home := t.TempDir()
	root := config.PathsForHome(home).Home
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[review]\nblocking_severity = \"critical\"\nnative_fanout_enabled = true\n"
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var engine workflow.Engine
	applyReviewPolicy(&engine, root)

	if engine.NativeReviewFanoutEnabled == nil || !engine.NativeReviewFanoutEnabled("owner/repo") {
		t.Fatal("invalid threshold discarded valid native fanout policy")
	}
	if engine.ReviewBlockingSeverity == nil || engine.ReviewBlockingSeverity("owner/repo") != reviewseverity.P3 {
		t.Fatal("invalid threshold must fail closed to P3")
	}
}

func TestApplyReviewPolicyMalformedNonSeverityFieldFailsClosed(t *testing.T) {
	home := t.TempDir()
	root := config.PathsForHome(home).Home
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[review]\nnative_fanout_enabled = true\nblocking_severity = \"P1\"\nhigh_risk_paths = maybe\n"
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var engine workflow.Engine
	applyReviewPolicy(&engine, root)

	if engine.NativeReviewFanoutEnabled == nil || engine.NativeReviewFanoutEnabled("owner/repo") {
		t.Fatal("malformed review config must leave native fanout off")
	}
	if engine.ReviewBlockingSeverity == nil ||
		engine.ReviewBlockingSeverity("owner/repo") != reviewseverity.P3 {
		t.Fatal("malformed review config must restore fail-closed P3")
	}
}

func TestApplyReviewPolicyInvalidRepoSeverityOverridesPermissiveGlobal(t *testing.T) {
	home := t.TempDir()
	root := config.PathsForHome(home).Home
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[review]\nblocking_severity = \"P1\"\n[repos.\"owner/sensitive\".review]\nblocking_severity = \"critical\"\nnative_fanout_enabled = true\n"
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var engine workflow.Engine
	applyReviewPolicy(&engine, root)

	if got := engine.ReviewBlockingSeverity("owner/ordinary"); got != reviewseverity.P1 {
		t.Fatalf("ordinary repository blocking severity = %q, want global P1", got)
	}
	if got := engine.ReviewBlockingSeverity("owner/sensitive"); got != reviewseverity.P3 {
		t.Fatalf("invalid sensitive repository threshold = %q, want fail-closed P3", got)
	}
	if !engine.NativeReviewFanoutEnabled("owner/sensitive") {
		t.Fatal("invalid threshold discarded a valid repository override")
	}
}

func TestApplyReviewPolicyEmptyHomeIsOff(t *testing.T) {
	// applyReviewPolicy("") resolves config.DefaultPaths() from $HOME, so without
	// this the assertions below read the operator's live ~/.gitmoot/config.toml
	// and a host with [review] set turns the whole package red (#1924).
	t.Setenv("HOME", t.TempDir())
	var engine workflow.Engine
	applyReviewPolicy(&engine, "")
	if engine.NativeReviewFanoutEnabled == nil || engine.NativeReviewFanoutEnabled("owner/repo") {
		t.Fatal("empty home must resolve native fanout OFF")
	}
	if engine.ReviewBlockingSeverity == nil || engine.ReviewBlockingSeverity("owner/repo") != reviewseverity.DefaultBlocking {
		t.Fatal("empty home must resolve blocking severity to P3")
	}
}

// reviewCompareClient answers every compare with result unless byRange names
// the exact `base...head`, and records every range it was asked for.
type reviewCompareClient struct {
	githubtest.NoopClient
	ranges  []string
	calls   int
	result  github.CompareResult
	byRange map[string]github.CompareResult
	pull    *github.PullRequest
}

func (c *reviewCompareClient) CompareCommits(_ context.Context, _ github.Repository, base string, head string) (github.CompareResult, error) {
	c.ranges = append(c.ranges, base+"..."+head)
	c.calls++
	if result, ok := c.byRange[base+"..."+head]; ok {
		return result, nil
	}
	return c.result, nil
}

func (c *reviewCompareClient) GetPullRequest(ctx context.Context, repo github.Repository, number int64) (github.PullRequest, error) {
	if c.pull == nil {
		return c.NoopClient.GetPullRequest(ctx, repo, number)
	}
	return *c.pull, nil
}

// compareAt is a compare answer whose three-dot merge base is mergeBase.
func compareAt(mergeBase string, files ...github.PullRequestFile) github.CompareResult {
	result := github.CompareResult{Status: "ahead", Files: files}
	result.MergeBaseCommit.SHA = mergeBase
	return result
}

func TestWireReviewChangedFilesUsesExactReviewerHead(t *testing.T) {
	client := &reviewCompareClient{result: compareAt("fork-point",
		github.PullRequestFile{Filename: "internal/z.go"},
		github.PullRequestFile{Filename: "internal/a.go"},
		github.PullRequestFile{Filename: "internal/a.go"},
	)}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client, "", nil)
	if engine.ReviewChangedFiles == nil {
		t.Fatal("ReviewChangedFiles was not wired")
	}
	diff, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "main", "reviewer-head", "current-head")
	if err != nil {
		t.Fatalf("ReviewChangedFiles: %v", err)
	}
	if len(client.ranges) == 0 || client.ranges[0] != "reviewer-head...current-head" {
		t.Fatalf("compare ranges = %v, want reviewer-head...current-head first", client.ranges)
	}
	if files := diff.ChangedFiles; len(files) != 2 || files[0] != "internal/a.go" || files[1] != "internal/z.go" {
		t.Fatalf("changed files = %#v, want sorted exact-head paths", files)
	}
	if diff.MergeBase != "fork-point" {
		t.Fatalf("merge base = %q, want the compare's merge base", diff.MergeBase)
	}
}

// reviewScopeCheckout builds a real checkout cloned from a local origin whose
// main holds one base commit; the checkout's follow-up commit changes `changed`
// files. It returns the checkout plus the two head SHAs.
func reviewScopeCheckout(t *testing.T, changed int) (checkout string, base string, head string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	origin := t.TempDir()
	runGit(t, origin, "init", "-b", "main")
	runGit(t, origin, "config", "user.email", "gitmoot@example.com")
	runGit(t, origin, "config", "user.name", "Gitmoot")
	writeFile(t, filepath.Join(origin, "README.md"), "base\n")
	runGit(t, origin, "add", "-A")
	runGit(t, origin, "commit", "-m", "base")
	base = reviewScopeHead(t, origin)

	dir := t.TempDir()
	runGit(t, dir, "clone", "-q", origin, dir)
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	for i := range changed {
		path := filepath.Join(dir, fmt.Sprintf("internal/pkg%04d/file.go", i+1))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll returned error: %v", err)
		}
		writeFile(t, path, "package p\n")
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "follow-up")
	return dir, base, reviewScopeHead(t, dir)
}

func reviewScopeHead(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
}

// The acceptance case for the whole design: a follow-up range LARGER than
// GitHub's 300-file compare cap must scope COMPLETELY. No compare response can
// prove that (the JSON page stops at 300 and the diff media type truncates
// silently at HTTP 200), so the completeness proof comes from the daemon's own
// checkout. The API client here would fail the scope closed, which is what makes
// this test attribute the complete list to local git and nothing else.
func TestWireReviewChangedFilesScopesCompleteRangeBeyondCompareCapViaLocalGit(t *testing.T) {
	const changed = 350
	checkout, base, head := reviewScopeCheckout(t, changed)
	capped := make([]github.PullRequestFile, 300)
	for i := range capped {
		capped[i].Filename = fmt.Sprintf("internal/pkg%04d/file.go", i+1)
	}
	client := &reviewCompareClient{result: github.CompareResult{Status: "ahead", Files: capped, Truncated: true}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client, checkout, subprocess.ExecRunner{})

	diff, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "main", base, head)
	if err != nil {
		t.Fatalf("a %d-file follow-up range must scope from the local checkout: %v", changed, err)
	}
	if client.calls != 0 {
		t.Fatalf("compare API calls = %d, want 0: local git already proved the range complete", client.calls)
	}
	paths := diff.ChangedFiles
	if len(paths) != changed {
		t.Fatalf("changed files = %d, want all %d paths past the 300-file compare cap", len(paths), changed)
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("changed files are not sorted: %v...", paths[:3])
	}
	if paths[0] != "internal/pkg0001/file.go" || paths[len(paths)-1] != fmt.Sprintf("internal/pkg%04d/file.go", changed) {
		t.Fatalf("changed files bounds = %q..%q", paths[0], paths[len(paths)-1])
	}
	if diff.MergeBase != base {
		t.Fatalf("merge base = %q, want origin/main %q", diff.MergeBase, base)
	}
}

// The test above enters through wireReviewChangedFiles, which is a HELPER: a
// wiring mutant that stops handing the seam the daemon's checkout leaves it
// green while production silently loses the only instrument that can prove a
// range complete. So pin the PATH too — build the engine the daemon actually
// builds and scope the same >300-file range through it.
func TestDaemonWorkflowEngineScopesReviewFromTheDaemonCheckout(t *testing.T) {
	const changed = 350
	checkout, base, head := reviewScopeCheckout(t, changed)
	home := config.PathsForHome(t.TempDir()).Home
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	capped := make([]github.PullRequestFile, 300)
	for i := range capped {
		capped[i].Filename = fmt.Sprintf("internal/pkg%04d/file.go", i+1)
	}
	client := &reviewCompareClient{result: github.CompareResult{Status: "ahead", Files: capped, Truncated: true}}

	engine := daemonWorkflowEngineForRunner(daemonWorkerStore(t), client, checkout, home, subprocess.ExecRunner{}, nil)
	if engine.ReviewChangedFiles == nil {
		t.Fatal("the daemon engine did not wire ReviewChangedFiles")
	}
	diff, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "main", base, head)
	if err != nil {
		t.Fatalf("the daemon engine must scope a %d-file range from its own checkout: %v", changed, err)
	}
	if client.calls != 0 {
		t.Fatalf("compare API calls = %d, want 0: the daemon checkout was in scope at the wiring site", client.calls)
	}
	if len(diff.ChangedFiles) != changed {
		t.Fatalf("changed files = %d, want all %d paths — the wiring site dropped the checkout", len(diff.ChangedFiles), changed)
	}
}

// reviewScopeGhRunner answers every `gh` invocation with one canned payload so a
// test can drive the REAL github.GhClient. That matters here: the assertion is
// about the cap -> Truncated -> fail-closed chain, and a hand-set Truncated flag
// would not prove CompareCommits still sets it.
type reviewScopeGhRunner struct {
	stdout string
	calls  [][]string
}

func (r *reviewScopeGhRunner) Run(_ context.Context, _ string, command string, args ...string) (subprocess.Result, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	return subprocess.Result{Command: command, Args: args, Stdout: r.stdout}, nil
}

func (r *reviewScopeGhRunner) LookPath(file string) (string, error) { return file, nil }

// The other acceptance case: with NO local checkout and a compare page sitting
// on the 300-file cap, the scope is UNKNOWN. It must fail closed to
// ReviewScopeUnavailableError — which HandlePullRequestOpened degrades to an
// unscoped full review at the same head — never to a silently incomplete list.
func TestWireReviewChangedFilesRejectsCappedCompareWithoutLocalCheckout(t *testing.T) {
	files := make([]github.PullRequestFile, 300)
	for i := range files {
		files[i] = github.PullRequestFile{Filename: fmt.Sprintf("internal/pkg%04d/file.go", i+1), Status: "modified"}
	}
	payload, err := json.Marshal(github.CompareResult{Status: "ahead", AheadBy: 4, Files: files})
	if err != nil {
		t.Fatalf("marshal compare payload: %v", err)
	}
	runner := &reviewScopeGhRunner{stdout: string(payload)}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, github.NewClientWithRunner(t.TempDir(), runner), "", nil)

	diff, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "main", "reviewer-head", "current-head")
	var unavailable workflow.ReviewScopeUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("ReviewChangedFiles = (%d paths, %v), want ReviewScopeUnavailableError for a capped compare with no local proof", len(diff.ChangedFiles), err)
	}
	if diff.ChangedFiles != nil {
		t.Fatalf("a failed scope must return no paths; got %d", len(diff.ChangedFiles))
	}
	if len(runner.calls) != 1 {
		t.Fatalf("gh calls = %v, want exactly the compare request", runner.calls)
	}
}

// Local ancestry is a PROOF that a range is not a direct follow-up, so a
// diverged range must fail closed without spending a compare call that could
// only agree.
func TestWireReviewChangedFilesProvesDivergedRangeLocally(t *testing.T) {
	checkout, base, _ := reviewScopeCheckout(t, 2)
	runGit(t, checkout, "switch", "-c", "sibling", base)
	writeFile(t, filepath.Join(checkout, "sibling.go"), "package p\n")
	runGit(t, checkout, "add", "-A")
	runGit(t, checkout, "commit", "-m", "sibling")
	sibling := reviewScopeHead(t, checkout)
	runGit(t, checkout, "switch", "main")
	main := reviewScopeHead(t, checkout)

	client := &reviewCompareClient{result: github.CompareResult{Status: "ahead", Files: []github.PullRequestFile{{Filename: "wrong.go"}}}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client, checkout, subprocess.ExecRunner{})

	_, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "main", sibling, main)
	var unavailable workflow.ReviewScopeUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("ReviewChangedFiles error = %v, want ReviewScopeUnavailableError for a locally diverged range", err)
	}
	if client.calls != 0 {
		t.Fatalf("compare API calls = %d, want 0: the local objects already disproved the range", client.calls)
	}
}

// A checkout that cannot supply the range (cold clone, no fetchable remote) is
// an instrument that could not RUN, not a proof of anything, so the seam must
// fall back to the API rather than report an empty or partial scope.
func TestWireReviewChangedFilesFallsBackToAPIWhenLocalObjectsAreMissing(t *testing.T) {
	checkout, _, _ := reviewScopeCheckout(t, 1)
	client := &reviewCompareClient{result: compareAt("fork-point",
		github.PullRequestFile{Filename: "internal/b.go"},
		github.PullRequestFile{Filename: "internal/a.go"},
	)}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client, checkout, subprocess.ExecRunner{})

	diff, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "main",
		"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222")
	if err != nil {
		t.Fatalf("ReviewChangedFiles must fall back to the API when the objects are absent: %v", err)
	}
	// The round itself, then the pull request's own change at each end.
	if client.calls != 3 {
		t.Fatalf("compare API calls = %v, want the round and both own-change compares", client.ranges)
	}
	if paths := diff.ChangedFiles; len(paths) != 2 || paths[0] != "internal/a.go" || paths[1] != "internal/b.go" {
		t.Fatalf("changed files = %#v, want the sorted API list", paths)
	}
}

// #2682 through the API fallback: the round's raw range carries the base file
// X the pull request merged in, but only Y's own patch moved, so the scope is
// [Y] read from the new merge base.
func TestForgeReviewScopeExcludesFilesAMergedBaseBroughtIn(t *testing.T) {
	const prev, head = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	y1 := github.PullRequestFile{Filename: "y.go", Status: "modified", Patch: "@@ -1,2 +1,2 @@\n a\n-b\n+own one\n"}
	y2 := github.PullRequestFile{Filename: "y.go", Status: "modified", Patch: "@@ -1,2 +1,2 @@\n a\n-b\n+own two\n"}
	z := github.PullRequestFile{Filename: "z.go", Status: "added", Patch: "@@ -0,0 +1 @@\n+z\n"}
	zShifted := github.PullRequestFile{Filename: "z.go", Status: "added", Patch: "@@ -0,0 +1 @@\n+z\n"}
	round := compareAt(prev, github.PullRequestFile{Filename: "x.go"}, y2, zShifted)
	client := &reviewCompareClient{byRange: map[string]github.CompareResult{
		prev + "..." + head: round,
		"main..." + prev:    compareAt("mb-one", y1, z),
		"main..." + head:    compareAt("mb-two", y2, zShifted),
		// The base's own movement between the merge bases: it added x.go only.
		"mb-one...mb-two": compareAt("mb-one", github.PullRequestFile{Filename: "x.go", Status: "added", Patch: "@@ -0,0 +1 @@\n+package x\n"}),
	}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client, "", nil)
	diff, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "main", prev, head)
	if err != nil {
		t.Fatalf("ReviewChangedFiles: %v", err)
	}
	if !slices.Equal(diff.ChangedFiles, []string{"y.go"}) || diff.MergeBase != "mb-two" {
		t.Fatalf("scope = %v from %q, want [y.go] from mb-two", diff.ChangedFiles, diff.MergeBase)
	}
}

// A merged head is inside its base branch, so the compare endpoint's own change
// is empty; the fallback cannot measure it and must not return an empty scope.
func TestForgeReviewScopeRefusesAHeadInsideItsBase(t *testing.T) {
	const prev, head = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	client := &reviewCompareClient{byRange: map[string]github.CompareResult{
		prev + "..." + head: compareAt(prev, github.PullRequestFile{Filename: "y.go"}),
		"main..." + prev:    compareAt(prev),
		"main..." + head:    compareAt(head),
	}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client, "", nil)
	_, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "main", prev, head)
	var unavailable workflow.ReviewScopeUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("ReviewChangedFiles error = %v, want ReviewScopeUnavailableError", err)
	}
}

func TestWireReviewChangedFilesRejectsTruncatedCompare(t *testing.T) {
	files := make([]github.PullRequestFile, 300)
	for i := range files {
		files[i].Filename = fmt.Sprintf("internal/pkg%04d/file.go", i)
	}
	client := &reviewCompareClient{result: github.CompareResult{Status: "ahead", Files: files, Truncated: true}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client, "", nil)

	_, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "main", "reviewer-head", "current-head")
	var unavailable workflow.ReviewScopeUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("ReviewChangedFiles error = %v, want ReviewScopeUnavailableError", err)
	}
}

func TestWireReviewChangedFilesMarksDivergedRangeUnscopable(t *testing.T) {
	client := &reviewCompareClient{result: github.CompareResult{Status: "diverged"}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client, "", nil)

	_, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "main", "reviewer-head", "current-head")
	var unavailable workflow.ReviewScopeUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("ReviewChangedFiles error = %v, want ReviewScopeUnavailableError", err)
	}
}

// The awaited review-verdict fact is satisfied inside the job state-transition
// transaction, so its wake detail is rendered by the STORE, not the engine. Every
// gitmoot command — the daemon included — takes its store from withStoreAndPaths,
// so this pins the wiring: without it the fix is inert and that one wake channel
// silently reports a raw verdict the engine has already folded.
func TestWithStoreInstallsReviewBlockingSeverity(t *testing.T) {
	home := t.TempDir()
	root := config.PathsForHome(home).Home
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, config.ConfigName),
		[]byte("[review]\nblocking_severity = \"P1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := func(decision string) string {
		t.Helper()
		encoded, err := json.Marshal(workflow.JobPayload{
			Repo: "acme/widget", PullRequest: 46, HeadSHA: "head-notes", TaskID: "task-46",
			Result: &workflow.AgentResult{Decision: decision, Severity: reviewseverity.P2, Summary: "polish"},
		})
		if err != nil {
			t.Fatalf("Marshal payload: %v", err)
		}
		return string(encoded)
	}
	if err := withStore(home, func(store *db.Store) error {
		if err := store.CreateJob(ctx, db.Job{
			ID: "review-notes", Agent: "audit", Type: "review", State: "running", Payload: payload(""),
		}); err != nil {
			return err
		}
		key, err := db.ReviewVerdictSubjectKey("acme/widget", 46, "head-notes")
		if err != nil {
			return err
		}
		fact, _, err := store.SubscribeAwaitedFact(ctx, db.AwaitedFactSubscription{
			WaiterRole: "lane", SubjectKind: db.AwaitedFactSubjectReviewVerdict,
			SubjectKey: key, Deadline: time.Now().UTC().Add(time.Hour),
		})
		if err != nil {
			return err
		}
		if _, err := store.TransitionJobStatePayloadWithEvent(ctx, "review-notes", "running", "succeeded",
			payload("changes_requested"), db.JobEvent{Kind: "succeeded", Message: "verdict"}); err != nil {
			return err
		}
		satisfied, err := store.GetAwaitedFact(ctx, fact.ID)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(satisfied.ResolutionDetail, "review verdict approved") {
			t.Fatalf("resolution detail = %q, want the effective approved verdict under blocking_severity P1",
				satisfied.ResolutionDetail)
		}
		return nil
	}); err != nil {
		t.Fatalf("withStore returned error: %v", err)
	}
}
