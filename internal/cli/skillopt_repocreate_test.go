package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
	fake := &repoCreateFakeGitHub{existing: map[string]bool{"o/exists": true}}
	restore := replaceSkillOptGitHubClient(fake)
	defer restore()

	var out bytes.Buffer
	// Existing repo: no create, no output.
	if err := ensureSkillOptTrainRepo("o/exists", &out); err != nil {
		t.Fatalf("ensure existing: %v", err)
	}
	if len(fake.created) != 0 || out.Len() != 0 {
		t.Fatalf("existing repo should be untouched: created=%v out=%q", fake.created, out.String())
	}

	// Missing repo: created + a created_repo line.
	if err := ensureSkillOptTrainRepo("o/missing", &out); err != nil {
		t.Fatalf("ensure missing: %v", err)
	}
	if len(fake.created) != 1 || fake.created[0] != "o/missing" {
		t.Fatalf("created = %v, want [o/missing]", fake.created)
	}
	if !strings.Contains(out.String(), "created_repo: o/missing") {
		t.Fatalf("expected created_repo line: %q", out.String())
	}
}

func TestEnsureSkillOptTrainRepoSkipsOnAmbiguousError(t *testing.T) {
	fake := &repoCreateFakeGitHub{existErr: context.DeadlineExceeded}
	restore := replaceSkillOptGitHubClient(fake)
	defer restore()

	var out bytes.Buffer
	if err := ensureSkillOptTrainRepo("o/repo", &out); err != nil {
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
