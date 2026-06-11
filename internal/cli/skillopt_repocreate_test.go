package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
)

// repoCreateFakeGitHub reports configured repos as missing and records creates.
// CloneRepository lays down a real local git checkout (origin + an initial commit
// on a branch) so repoRecordForCheckout validates the provisioned repo.
type repoCreateFakeGitHub struct {
	github.NoopClient
	existing map[string]bool
	created  []string
	cloned   []string
	existErr error
}

func (f *repoCreateFakeGitHub) RepositoryExists(_ context.Context, repo github.Repository) (bool, error) {
	if f.existErr != nil {
		return false, f.existErr
	}
	return f.existing[repo.FullName()], nil
}

func (f *repoCreateFakeGitHub) CreateRepository(_ context.Context, repo github.Repository, _ bool) error {
	f.created = append(f.created, repo.FullName())
	if f.existing == nil {
		f.existing = map[string]bool{}
	}
	f.existing[repo.FullName()] = true
	return nil
}

func (f *repoCreateFakeGitHub) CloneRepository(_ context.Context, repo github.Repository, dir string) error {
	f.cloned = append(f.cloned, repo.FullName()+" -> "+dir)
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return nil
	}
	if err := run("init", "-b", "main", dir); err != nil {
		return err
	}
	if err := run("-C", dir, "remote", "add", "origin", "https://github.com/"+repo.FullName()+".git"); err != nil {
		return err
	}
	return run("-C", dir, "commit", "--allow-empty", "-m", "init")
}

func replaceSkillOptGitHubClient(client github.Client) func() {
	prev := newSkillOptGitHubClient
	newSkillOptGitHubClient = func() github.Client { return client }
	return func() { newSkillOptGitHubClient = prev }
}

func TestEnsureSkillOptTrainRepoCreatesMissing(t *testing.T) {
	home := t.TempDir()
	if err := config.Initialize(config.PathsForHome(home)); err != nil {
		t.Fatalf("init: %v", err)
	}
	fake := &repoCreateFakeGitHub{existing: map[string]bool{"o/exists": true}}
	restore := replaceSkillOptGitHubClient(fake)
	defer restore()

	var out bytes.Buffer
	// Existing repo: no create, no output, no record.
	if err := ensureSkillOptTrainRepo(home, "o/exists", "train", "sess-1", &out); err != nil {
		t.Fatalf("ensure existing: %v", err)
	}
	if len(fake.created) != 0 || out.Len() != 0 {
		t.Fatalf("existing repo should be untouched: created=%v out=%q", fake.created, out.String())
	}

	// Missing repo: created + a created_repo line + a created_repos record.
	if err := ensureSkillOptTrainRepo(home, "o/missing", "train", "sess-1", &out); err != nil {
		t.Fatalf("ensure missing: %v", err)
	}
	if len(fake.created) != 1 || fake.created[0] != "o/missing" {
		t.Fatalf("created = %v, want [o/missing]", fake.created)
	}
	if !strings.Contains(out.String(), "created_repo: o/missing") {
		t.Fatalf("expected created_repo line: %q", out.String())
	}
	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	records, err := store.ListCreatedReposForSession(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("list created repos: %v", err)
	}
	if len(records) != 1 || records[0].Repo != "o/missing" {
		t.Fatalf("created_repos records = %+v", records)
	}
}

func TestEnsureSkillOptTrainRepoSkipsOnAmbiguousError(t *testing.T) {
	home := t.TempDir()
	fake := &repoCreateFakeGitHub{existErr: context.DeadlineExceeded}
	restore := replaceSkillOptGitHubClient(fake)
	defer restore()

	var out bytes.Buffer
	if err := ensureSkillOptTrainRepo(home, "o/repo", "train", "", &out); err != nil {
		t.Fatalf("ensure should not error on ambiguous check: %v", err)
	}
	if len(fake.created) != 0 {
		t.Fatalf("ambiguous check must not create: %v", fake.created)
	}
}

func TestSkillOptTrainRepoCheckerAndCreator(t *testing.T) {
	fake := &repoCreateFakeGitHub{existing: map[string]bool{"o/here": true}}
	restore := replaceSkillOptGitHubClient(fake)
	defer restore()

	check := skillOptTrainRepoChecker()
	missing, err := check("o/here")
	if err != nil || missing {
		t.Fatalf("existing repo: (missing=%v, err=%v), want (false, nil)", missing, err)
	}
	missing, err = check("o/gone")
	if err != nil || !missing {
		t.Fatalf("absent repo: (missing=%v, err=%v), want (true, nil)", missing, err)
	}
	if _, err := check("not-a-repo-ref-without-slash"); err == nil {
		t.Fatal("unparseable repo should error")
	}

	home := t.TempDir()
	if err := config.Initialize(config.PathsForHome(home)); err != nil {
		t.Fatalf("init: %v", err)
	}
	create := skillOptTrainRepoCreator(home)
	if err := create("o/new"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(fake.created) != 1 || fake.created[0] != "o/new" {
		t.Fatalf("created = %v", fake.created)
	}
}

func TestProvisionTrainGenerationRepo(t *testing.T) {
	home := t.TempDir()
	if err := config.Initialize(config.PathsForHome(home)); err != nil {
		t.Fatalf("init: %v", err)
	}
	fake := &repoCreateFakeGitHub{}
	restore := replaceSkillOptGitHubClient(fake)
	defer restore()

	repo, err := daemon.ParseRepository("o/fresh")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := provisionTrainGenerationRepo(context.Background(), home, repo); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if len(fake.created) != 1 || len(fake.cloned) != 1 {
		t.Fatalf("expected one create + one clone, got created=%v cloned=%v", fake.created, fake.cloned)
	}

	store, err := db.Open(config.PathsForHome(home).Database)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	// Registered with a checkout under the gitmoot-managed checkouts dir.
	got, err := store.GetRepo(ctx, "o/fresh")
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	wantPrefix := filepath.Join(config.PathsForHome(home).Home, "checkouts", "o", "fresh")
	if got.CheckoutPath != wantPrefix {
		t.Fatalf("checkout path = %q, want %q", got.CheckoutPath, wantPrefix)
	}
	// Recorded for cleanup.
	records, err := store.ListCreatedReposForSession(ctx, "")
	if err != nil {
		t.Fatalf("list created: %v", err)
	}
	found := false
	for _, r := range records {
		if r.Repo == "o/fresh" {
			found = true
		}
	}
	if !found {
		t.Fatalf("o/fresh should be recorded as created: %+v", records)
	}

	// Idempotent: a second provision reuses the existing checkout (no second clone).
	if err := provisionTrainGenerationRepo(ctx, home, repo); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	if len(fake.cloned) != 1 {
		t.Fatalf("re-provision should not clone again: %v", fake.cloned)
	}
}
