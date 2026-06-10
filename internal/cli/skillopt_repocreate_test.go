package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
)

// repoCreateFakeGitHub reports configured repos as missing and records creates.
type repoCreateFakeGitHub struct {
	github.NoopClient
	existing map[string]bool
	created  []string
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

	create := skillOptTrainRepoCreator()
	if err := create("o/new"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(fake.created) != 1 || fake.created[0] != "o/new" {
		t.Fatalf("created = %v", fake.created)
	}
}
