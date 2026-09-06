package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	if engine.RiskTiersEnabled {
		t.Fatal("applyReviewPolicy must leave risk tiers OFF when [review] is absent")
	}
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
	body := "[review]\nnative_fanout_enabled = true\nblocking_severity = \"P2\"\nrisk_tiers_enabled = true\nhigh_risk_paths = [\"cmd/**\"]\nrisk_label_high = \"sev:1\"\n[repos.\"owner/off\".review]\nnative_fanout_enabled = false\nblocking_severity = \"P1\"\n"
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
	body := "[review]\nblocking_severity = \"critical\"\nnative_fanout_enabled = true\nrisk_tiers_enabled = true\nhigh_risk_paths = [\"cmd/**\"]\n"
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var engine workflow.Engine
	applyReviewPolicy(&engine, root)

	if !engine.RiskTiersEnabled || len(engine.HighRiskPaths) != 1 || engine.HighRiskPaths[0] != "cmd/**" {
		t.Fatalf("invalid threshold discarded valid high-risk policy: enabled=%v paths=%v", engine.RiskTiersEnabled, engine.HighRiskPaths)
	}
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
	body := "[review]\nnative_fanout_enabled = true\nblocking_severity = \"P1\"\nrisk_tiers_enabled = maybe\n"
	if err := os.WriteFile(filepath.Join(root, config.ConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var engine workflow.Engine
	applyReviewPolicy(&engine, root)

	if engine.RiskTiersEnabled {
		t.Fatal("malformed review config must leave risk tiers off")
	}
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
	if engine.RiskTiersEnabled {
		t.Fatal("empty home must resolve to risk tiers OFF")
	}
	if engine.NativeReviewFanoutEnabled == nil || engine.NativeReviewFanoutEnabled("owner/repo") {
		t.Fatal("empty home must resolve native fanout OFF")
	}
	if engine.ReviewBlockingSeverity == nil || engine.ReviewBlockingSeverity("owner/repo") != reviewseverity.DefaultBlocking {
		t.Fatal("empty home must resolve blocking severity to P3")
	}
}

type reviewCompareClient struct {
	githubtest.NoopClient
	base   string
	head   string
	calls  int
	result github.CompareResult
}

func (c *reviewCompareClient) CompareCommits(_ context.Context, _ github.Repository, base string, head string) (github.CompareResult, error) {
	c.base, c.head = base, head
	c.calls++
	return c.result, nil
}

func TestWireReviewChangedFilesUsesExactReviewerHead(t *testing.T) {
	client := &reviewCompareClient{result: github.CompareResult{Status: "ahead", Files: []github.PullRequestFile{
		{Filename: "internal/z.go"},
		{Filename: "internal/a.go"},
		{Filename: "internal/a.go"},
	}}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client, "", nil)
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

// reviewScopeCheckout builds a real two-commit repository whose follow-up range
// changes `changed` files, and returns the checkout plus the two head SHAs.
func reviewScopeCheckout(t *testing.T, changed int) (checkout string, base string, head string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	writeFile(t, filepath.Join(dir, "README.md"), "base\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "base")
	base = reviewScopeHead(t, dir)

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

	paths, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, base, head)
	if err != nil {
		t.Fatalf("a %d-file follow-up range must scope from the local checkout: %v", changed, err)
	}
	if client.calls != 0 {
		t.Fatalf("compare API calls = %d, want 0: local git already proved the range complete", client.calls)
	}
	if len(paths) != changed {
		t.Fatalf("changed files = %d, want all %d paths past the 300-file compare cap", len(paths), changed)
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("changed files are not sorted: %v...", paths[:3])
	}
	if paths[0] != "internal/pkg0001/file.go" || paths[len(paths)-1] != fmt.Sprintf("internal/pkg%04d/file.go", changed) {
		t.Fatalf("changed files bounds = %q..%q", paths[0], paths[len(paths)-1])
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
	paths, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, base, head)
	if err != nil {
		t.Fatalf("the daemon engine must scope a %d-file range from its own checkout: %v", changed, err)
	}
	if client.calls != 0 {
		t.Fatalf("compare API calls = %d, want 0: the daemon checkout was in scope at the wiring site", client.calls)
	}
	if len(paths) != changed {
		t.Fatalf("changed files = %d, want all %d paths — the wiring site dropped the checkout", len(paths), changed)
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

	paths, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "reviewer-head", "current-head")
	var unavailable workflow.ReviewScopeUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("ReviewChangedFiles = (%d paths, %v), want ReviewScopeUnavailableError for a capped compare with no local proof", len(paths), err)
	}
	if paths != nil {
		t.Fatalf("a failed scope must return no paths; got %d", len(paths))
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

	_, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, sibling, main)
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
	client := &reviewCompareClient{result: github.CompareResult{Status: "ahead", Files: []github.PullRequestFile{
		{Filename: "internal/b.go"},
		{Filename: "internal/a.go"},
	}}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client, checkout, subprocess.ExecRunner{})

	paths, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17,
		"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222")
	if err != nil {
		t.Fatalf("ReviewChangedFiles must fall back to the API when the objects are absent: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("compare API calls = %d, want 1", client.calls)
	}
	if len(paths) != 2 || paths[0] != "internal/a.go" || paths[1] != "internal/b.go" {
		t.Fatalf("changed files = %#v, want the sorted API list", paths)
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

	_, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "reviewer-head", "current-head")
	var unavailable workflow.ReviewScopeUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("ReviewChangedFiles error = %v, want ReviewScopeUnavailableError", err)
	}
}

func TestWireReviewChangedFilesMarksDivergedRangeUnscopable(t *testing.T) {
	client := &reviewCompareClient{result: github.CompareResult{Status: "diverged"}}
	var engine workflow.Engine
	wireReviewChangedFiles(&engine, client, "", nil)

	_, err := engine.ReviewChangedFiles(context.Background(), "owner/repo", 17, "reviewer-head", "current-head")
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
		fact, err := store.SubscribeAwaitedFact(ctx, db.AwaitedFactSubscription{
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
