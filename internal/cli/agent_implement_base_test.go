package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
)

type implementBaseFixture struct {
	home       string
	checkout   string
	initialSHA string
	remoteSHA  string
}

func newImplementBaseFixture(t *testing.T) implementBaseFixture {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	checkout := filepath.Join(root, "checkout")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "clone", remote, checkout)
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot Test")
	writeFile(t, filepath.Join(checkout, "README.md"), "initial\n")
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "push", "-u", "origin", "main")
	runGit(t, root, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")
	initialSHA := strings.TrimSpace(runGitOutput(t, checkout, "rev-parse", "HEAD"))
	runGit(t, checkout, "switch", "-c", "feature/stale")

	updater := filepath.Join(root, "updater")
	runGit(t, root, "clone", remote, updater)
	runGit(t, updater, "config", "user.email", "gitmoot@example.com")
	runGit(t, updater, "config", "user.name", "Gitmoot Test")
	writeFile(t, filepath.Join(updater, "remote.txt"), "new remote commit\n")
	runGit(t, updater, "add", "remote.txt")
	runGit(t, updater, "commit", "-m", "advance main")
	runGit(t, updater, "push", "origin", "main")
	remoteSHA := strings.TrimSpace(runGitOutput(t, updater, "rev-parse", "HEAD"))
	return implementBaseFixture{home: filepath.Join(root, "home"), checkout: checkout, initialSHA: initialSHA, remoteSHA: remoteSHA}
}

func prepareImplementBaseFixture(t *testing.T, fixture implementBaseFixture, request localAgentDispatchRequest) (db.Task, localAgentDispatchRequest, error) {
	t.Helper()
	store := openCLIJobStore(t, fixture.home)
	defer store.Close()
	request.Home = fixture.home
	request.Agent = "builder"
	request.Action = "implement"
	request.Instructions = "Implement the requested change."
	record := db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: fixture.checkout}
	return prepareLocalImplementDispatchRequest(context.Background(), store, record, github.Repository{Owner: "owner", Name: "repo"}, request)
}

func TestAgentImplementBaseOriginMainFetchesAndBasesWorktree(t *testing.T) {
	fixture := newImplementBaseFixture(t)
	var stderr bytes.Buffer
	options, ok := parseAgentRunOptions("implement", []string{"builder", "Implement the requested change.", "--base", "origin/main"}, &stderr)
	if !ok {
		t.Fatalf("parseAgentRunOptions failed: %s", stderr.String())
	}
	if err := normalizeAgentImplementBase(&options, "implement"); err != nil {
		t.Fatalf("normalizeAgentImplementBase: %v", err)
	}
	task, _, err := prepareImplementBaseFixture(t, fixture, localAgentDispatchRequest{Branch: "test-origin-main", ImplementBase: options.base})
	if err != nil {
		t.Fatalf("prepare implement: %v", err)
	}
	head := strings.TrimSpace(runGitOutput(t, task.WorktreePath, "rev-parse", "HEAD"))
	if head != fixture.remoteSHA {
		t.Fatalf("worktree head = %s, want freshly fetched origin/main %s", head, fixture.remoteSHA)
	}
}

func TestAgentImplementBaseHEADUsesCheckoutHead(t *testing.T) {
	fixture := newImplementBaseFixture(t)
	task, _, err := prepareImplementBaseFixture(t, fixture, localAgentDispatchRequest{Branch: "test-head", ImplementBase: "HEAD"})
	if err != nil {
		t.Fatalf("prepare implement: %v", err)
	}
	head := strings.TrimSpace(runGitOutput(t, task.WorktreePath, "rev-parse", "HEAD"))
	if head != fixture.initialSHA {
		t.Fatalf("worktree head = %s, want checkout HEAD %s", head, fixture.initialSHA)
	}
}

func TestAgentImplementBaseUsesWorkflowConfigDefault(t *testing.T) {
	fixture := newImplementBaseFixture(t)
	paths := config.PathsForHome(fixture.home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	content := config.DefaultConfig(paths) + "\n[workflow]\nimplement_base = \"origin/main\"\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	task, _, err := prepareImplementBaseFixture(t, fixture, localAgentDispatchRequest{Branch: "test-config"})
	if err != nil {
		t.Fatalf("prepare implement: %v", err)
	}
	head := strings.TrimSpace(runGitOutput(t, task.WorktreePath, "rev-parse", "HEAD"))
	if head != fixture.remoteSHA {
		t.Fatalf("worktree head = %s, want configured origin/main %s", head, fixture.remoteSHA)
	}
}

func TestAgentImplementBaseConfigHEADOptsIntoCheckoutFollowing(t *testing.T) {
	fixture := newImplementBaseFixture(t)
	paths := config.PathsForHome(fixture.home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	content := config.DefaultConfig(paths) + "\n[workflow]\nimplement_base = \"HEAD\"\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	task, _, err := prepareImplementBaseFixture(t, fixture, localAgentDispatchRequest{Branch: "test-config-head"})
	if err != nil {
		t.Fatalf("prepare implement: %v", err)
	}
	head := strings.TrimSpace(runGitOutput(t, task.WorktreePath, "rev-parse", "HEAD"))
	if head != fixture.initialSHA {
		t.Fatalf("worktree head = %s, want configured HEAD %s", head, fixture.initialSHA)
	}
}

func TestAgentImplementHeadSHAIsBaseAliasAndRejectsConflict(t *testing.T) {
	fixture := newImplementBaseFixture(t)
	var stderr bytes.Buffer
	options, ok := parseAgentRunOptions("implement", []string{"builder", "Implement the requested change.", "--head-sha", fixture.initialSHA}, &stderr)
	if !ok {
		t.Fatalf("parseAgentRunOptions failed: %s", stderr.String())
	}
	if err := normalizeAgentImplementBase(&options, "implement"); err != nil {
		t.Fatalf("normalize alias: %v", err)
	}
	if options.base != fixture.initialSHA || options.headSHA != "" {
		t.Fatalf("normalized options = %+v, want base %s and empty head SHA", options, fixture.initialSHA)
	}
	task, _, err := prepareImplementBaseFixture(t, fixture, localAgentDispatchRequest{Branch: "test-head-sha", ImplementBase: options.base})
	if err != nil {
		t.Fatalf("prepare implement from --head-sha: %v", err)
	}
	head := strings.TrimSpace(runGitOutput(t, task.WorktreePath, "rev-parse", "HEAD"))
	if head != fixture.initialSHA {
		t.Fatalf("worktree head = %s, want --head-sha base %s", head, fixture.initialSHA)
	}

	sha := strings.Repeat("a", 40)
	options = agentRunOptions{base: sha, headSHA: sha}
	if err := normalizeAgentImplementBase(&options, "implement"); err != nil {
		t.Fatalf("normalize equal values: %v", err)
	}
	options = agentRunOptions{base: "origin/main", headSHA: sha}
	if err := normalizeAgentImplementBase(&options, "implement"); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestAgentRunImplementRoutingCarriesBase(t *testing.T) {
	var stderr bytes.Buffer
	options, ok := parseAgentRunOptions("run", []string{"builder", "update docs and add tests", "--base=HEAD"}, &stderr)
	if !ok {
		t.Fatalf("parseAgentRunOptions failed: %s", stderr.String())
	}
	action, _ := selectAgentRunAction(options)
	if action != "implement" {
		t.Fatalf("action = %q, want implement", action)
	}
	if err := normalizeAgentImplementBase(&options, action); err != nil {
		t.Fatalf("normalizeAgentImplementBase: %v", err)
	}
	if options.base != "HEAD" {
		t.Fatalf("base = %q, want HEAD", options.base)
	}
}

func TestImplicitImplementBaseGuardMatrix(t *testing.T) {
	t.Run("default branch passes", func(t *testing.T) {
		fixture := newImplementBaseFixture(t)
		runGit(t, fixture.checkout, "switch", "main")
		task, _, err := prepareImplementBaseFixture(t, fixture, localAgentDispatchRequest{Branch: "test-default-pass"})
		if err != nil {
			t.Fatalf("prepare implement: %v", err)
		}
		head := strings.TrimSpace(runGitOutput(t, task.WorktreePath, "rev-parse", "HEAD"))
		if head != fixture.initialSHA {
			t.Fatalf("worktree head = %s, want checkout HEAD %s", head, fixture.initialSHA)
		}
	})

	t.Run("feature branch behind refuses", func(t *testing.T) {
		fixture := newImplementBaseFixture(t)
		_, _, err := prepareImplementBaseFixture(t, fixture, localAgentDispatchRequest{Branch: "test-behind-refuse"})
		if err == nil {
			t.Fatal("prepare implement succeeded from a stale feature checkout")
		}
		for _, want := range []string{"feature/stale", "1 behind origin/main", "--base origin/main", "--base HEAD"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("guard error %q missing %q", err, want)
			}
		}
	})

	t.Run("feature branch not behind passes", func(t *testing.T) {
		fixture := newImplementBaseFixture(t)
		runGit(t, fixture.checkout, "fetch", "origin")
		runGit(t, fixture.checkout, "switch", "-C", "feature/fresh", "origin/main")
		writeFile(t, filepath.Join(fixture.checkout, "feature.txt"), "feature\n")
		runGit(t, fixture.checkout, "add", "feature.txt")
		runGit(t, fixture.checkout, "-c", "user.name=Gitmoot Test", "-c", "user.email=gitmoot@example.com", "commit", "-m", "feature")
		featureSHA := strings.TrimSpace(runGitOutput(t, fixture.checkout, "rev-parse", "HEAD"))
		task, _, err := prepareImplementBaseFixture(t, fixture, localAgentDispatchRequest{Branch: "test-feature-pass"})
		if err != nil {
			t.Fatalf("prepare implement: %v", err)
		}
		head := strings.TrimSpace(runGitOutput(t, task.WorktreePath, "rev-parse", "HEAD"))
		if head != featureSHA {
			t.Fatalf("worktree head = %s, want feature HEAD %s", head, featureSHA)
		}
	})
}

func TestAgentImplementUnknownBaseRejectedBeforeTaskOrJob(t *testing.T) {
	fixture := newImplementBaseFixture(t)
	runGit(t, fixture.checkout, "switch", "main")
	store := openCLIJobStore(t, fixture.home)
	defer store.Close()
	record := db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: fixture.checkout}
	_, _, err := prepareLocalImplementDispatchRequest(context.Background(), store, record, github.Repository{Owner: "owner", Name: "repo"}, localAgentDispatchRequest{
		Home:          fixture.home,
		Agent:         "builder",
		Action:        "implement",
		Instructions:  "Implement the requested change.",
		Branch:        "test-unknown",
		ImplementBase: "origin/does-not-exist",
	})
	if err == nil || !strings.Contains(err.Error(), `unknown implement base ref "origin/does-not-exist"`) {
		t.Fatalf("unknown base error = %v", err)
	}
	if _, err := store.GetTaskByRepoBranch(context.Background(), "owner/repo", "test-unknown"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown base created a task, err=%v", err)
	}
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("unknown base enqueued jobs: %+v", jobs)
	}
}

func TestResolveLocalAgentRepoPreservesRegisteredDefaultBranch(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "config", "user.email", "gitmoot@example.com")
	runGit(t, checkout, "config", "user.name", "Gitmoot Test")
	writeFile(t, filepath.Join(checkout, "README.md"), "main\n")
	runGit(t, checkout, "add", "README.md")
	runGit(t, checkout, "commit", "-m", "initial")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "switch", "-c", "feature/stale")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepo(ctx, db.Repo{Owner: "owner", Name: "repo", DefaultBranch: "main", CheckoutPath: checkout, PollInterval: "30s"}); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	_, record, err := resolveLocalAgentRepo(ctx, store, "owner/repo")
	if err != nil {
		t.Fatalf("resolveLocalAgentRepo: %v", err)
	}
	if record.DefaultBranch != "main" {
		t.Fatalf("default branch = %q, want registered main", record.DefaultBranch)
	}
	if branch, err := (gitutil.Client{Dir: checkout}).CurrentBranch(ctx); err != nil || branch != "feature/stale" {
		t.Fatalf("checkout branch = %q err=%v, want feature/stale", branch, err)
	}
}

func TestResolveLocalAgentRepoSelfHealsDanglingCheckoutForImplementBase(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	primary, linked := setupLinkedWorktreeRepo(t)
	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepoForce(ctx, db.Repo{
		Owner: "owner", Name: "repo", DefaultBranch: "main",
		CheckoutPath: linked, PrimaryCheckoutPath: primary,
	}); err != nil {
		t.Fatalf("UpsertRepoForce: %v", err)
	}
	runGit(t, primary, "worktree", "remove", "--force", linked)

	_, record, err := resolveLocalAgentRepo(ctx, store, "owner/repo")
	if err != nil {
		t.Fatalf("resolveLocalAgentRepo: %v", err)
	}
	if record.CheckoutPath != primary || record.PrimaryCheckoutPath != primary {
		t.Fatalf("resolved record = %+v, want repaired primary %s", record, primary)
	}
	wantSHA := strings.TrimSpace(runGitOutput(t, primary, "rev-parse", "HEAD"))
	gotSHA, err := resolveLocalImplementBase(ctx, config.PathsForHome(home), record, "HEAD")
	if err != nil || gotSHA != wantSHA {
		t.Fatalf("resolveLocalImplementBase = %q, err=%v, want %q", gotSHA, err, wantSHA)
	}
	stored, err := store.GetRepo(ctx, "owner/repo")
	if err != nil || stored.CheckoutPath != primary || stored.PrimaryCheckoutPath != primary {
		t.Fatalf("stored record = %+v, err=%v", stored, err)
	}
}

func TestResolveLocalAgentRepoSelfHealsNonGitCheckout(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	primary, linked := setupLinkedWorktreeRepo(t)
	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepoForce(ctx, db.Repo{
		Owner: "owner", Name: "repo", DefaultBranch: "main",
		CheckoutPath: linked, PrimaryCheckoutPath: primary,
	}); err != nil {
		t.Fatalf("UpsertRepoForce: %v", err)
	}
	runGit(t, primary, "worktree", "remove", "--force", linked)
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatalf("recreate non-git checkout directory: %v", err)
	}

	_, record, err := resolveLocalAgentRepo(ctx, store, "owner/repo")
	if err != nil {
		t.Fatalf("resolveLocalAgentRepo: %v", err)
	}
	if record.CheckoutPath != primary || record.PrimaryCheckoutPath != primary {
		t.Fatalf("resolved record = %+v, want repaired primary %s", record, primary)
	}
	stored, err := store.GetRepo(ctx, "owner/repo")
	if err != nil || stored.CheckoutPath != primary || stored.PrimaryCheckoutPath != primary {
		t.Fatalf("stored record = %+v, err=%v", stored, err)
	}
}

func TestResolveLocalAgentRepoLeavesValidLinkedCheckoutRegistered(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	primary, linked := setupLinkedWorktreeRepo(t)
	store := openCLIJobStore(t, home)
	defer store.Close()
	if err := store.UpsertRepoForce(ctx, db.Repo{
		Owner: "owner", Name: "repo", DefaultBranch: "main",
		CheckoutPath: linked, PrimaryCheckoutPath: primary,
	}); err != nil {
		t.Fatalf("UpsertRepoForce: %v", err)
	}

	_, record, err := resolveLocalAgentRepo(ctx, store, "owner/repo")
	if err != nil {
		t.Fatalf("resolveLocalAgentRepo: %v", err)
	}
	if record.CheckoutPath != linked || record.PrimaryCheckoutPath != primary {
		t.Fatalf("resolved record = %+v, want valid linked checkout %s", record, linked)
	}
	stored, err := store.GetRepo(ctx, "owner/repo")
	if err != nil || stored.CheckoutPath != linked {
		t.Fatalf("stored record = %+v, err=%v", stored, err)
	}
}

func TestResolveRepoRecordPinsImplicitLinkedCheckoutToPrimary(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	primary, initialLinked := setupLinkedWorktreeRepo(t)
	runGit(t, primary, "worktree", "remove", "--force", initialLinked)
	linked := filepath.Join(home, ".gitmoot", "worktrees", "owner--repo", "adhoc-root-prevention")
	if err := os.MkdirAll(filepath.Dir(linked), 0o755); err != nil {
		t.Fatalf("mkdir managed worktree parent: %v", err)
	}
	runGit(t, primary, "worktree", "add", "-b", "adhoc-root-prevention", linked)
	store := openCLIJobStore(t, home)
	defer store.Close()

	record, err := resolveRepoRecord(ctx, store, github.Repository{Owner: "owner", Name: "repo"}, linked)
	if err != nil {
		t.Fatalf("resolveRepoRecord: %v", err)
	}
	if record.CheckoutPath != primary || record.PrimaryCheckoutPath != primary {
		t.Fatalf("implicit linked record = %+v, want primary %s", record, primary)
	}
	if err := store.UpsertRepo(ctx, record); err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	stored, err := store.GetRepo(ctx, "owner/repo")
	if err != nil || stored.CheckoutPath != primary {
		t.Fatalf("stored record = %+v, err=%v", stored, err)
	}
}
