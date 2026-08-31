package cli

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/github"
)

type fakeRepoCollisionClient struct {
	pulls          []github.PullRequest
	files          map[int64][]github.PullRequestFile
	requestedState string
	fileCalls      []int64
}

func (f *fakeRepoCollisionClient) ListPullRequests(_ context.Context, _ github.Repository, state string) ([]github.PullRequest, error) {
	f.requestedState = state
	return append([]github.PullRequest(nil), f.pulls...), nil
}

func (f *fakeRepoCollisionClient) ListPullRequestFiles(_ context.Context, _ github.Repository, number int64) ([]github.PullRequestFile, error) {
	f.fileCalls = append(f.fileCalls, number)
	return append([]github.PullRequestFile(nil), f.files[number]...), nil
}

func TestDetectOpenPRFileCollisionsUsesDistinctPRIntersections(t *testing.T) {
	client := &fakeRepoCollisionClient{
		pulls: []github.PullRequest{{Number: 30}, {Number: 10}, {Number: 20}},
		files: map[int64][]github.PullRequestFile{
			10: {{Filename: "internal/cli/repo.go"}, {Filename: "README.md"}},
			20: {{Filename: "docs/events.md"}},
			30: {{Filename: "README.md"}, {Filename: "README.md"}, {Filename: "internal/workflow/engine.go"}},
		},
	}
	got, err := detectOpenPRFileCollisions(context.Background(), client, github.Repository{Owner: "gitmoot", Name: "gitmoot"}, repoCollisionDefaultLimit)
	if err != nil {
		t.Fatal(err)
	}
	want := []repoPRCollision{{FirstPR: 10, SecondPR: 30, Files: []string{"README.md"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collisions=%+v, want %+v", got, want)
	}
	if client.requestedState != "open" {
		t.Fatalf("requested pull request state=%q, want open", client.requestedState)
	}
	if !reflect.DeepEqual(client.fileCalls, []int64{10, 20, 30}) {
		t.Fatalf("file calls=%v, want sorted distinct PRs", client.fileCalls)
	}
}

func TestDetectOpenPRFileCollisionsLimitsNewestPullRequests(t *testing.T) {
	client := &fakeRepoCollisionClient{
		pulls: []github.PullRequest{{Number: 1}, {Number: 4}, {Number: 2}, {Number: 3}},
		files: map[int64][]github.PullRequestFile{
			1: {{Filename: "old-one.txt"}},
			2: {{Filename: "old-two.txt"}},
			3: {{Filename: "shared.txt"}},
			4: {{Filename: "shared.txt"}},
		},
	}
	got, err := detectOpenPRFileCollisions(
		context.Background(),
		client,
		github.Repository{Owner: "gitmoot", Name: "gitmoot"},
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []repoPRCollision{{FirstPR: 3, SecondPR: 4, Files: []string{"shared.txt"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collisions=%+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(client.fileCalls, []int64{3, 4}) {
		t.Fatalf("file calls=%v, want only two newest pull requests", client.fileCalls)
	}
}

func TestRunRepoCollisionsWarnsWithPairAndFiles(t *testing.T) {
	client := &fakeRepoCollisionClient{
		pulls: []github.PullRequest{{Number: 8}, {Number: 4}},
		files: map[int64][]github.PullRequestFile{
			4: {{Filename: "internal/cli/org.go"}, {Filename: "internal/cli/repo.go"}},
			8: {{Filename: "internal/cli/repo.go"}, {Filename: "internal/cli/org.go"}},
		},
	}
	previous := newRepoCollisionClient
	newRepoCollisionClient = func() repoCollisionClient { return client }
	t.Cleanup(func() { newRepoCollisionClient = previous })

	var stdout, stderr bytes.Buffer
	code := runRepo([]string{"collisions", "gitmoot/gitmoot"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("collision code=%d out=%q err=%q, want warning exit 1", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"WARN: PR #4 and PR #8", "2 shared file(s)", "internal/cli/org.go, internal/cli/repo.go"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("collision output=%q, want %q", stdout.String(), want)
		}
	}
}

func TestRunRepoCollisionsReportsCleanDistinctFileSets(t *testing.T) {
	client := &fakeRepoCollisionClient{
		pulls: []github.PullRequest{{Number: 4}, {Number: 8}},
		files: map[int64][]github.PullRequestFile{
			4: {{Filename: "internal/cli/org.go"}},
			8: {{Filename: "internal/cli/repo.go"}},
		},
	}
	previous := newRepoCollisionClient
	newRepoCollisionClient = func() repoCollisionClient { return client }
	t.Cleanup(func() { newRepoCollisionClient = previous })

	var stdout, stderr bytes.Buffer
	code := runRepo([]string{"collisions", "gitmoot/gitmoot"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "no open PR file collisions") {
		t.Fatalf("clean code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunRepoCollisionsJSONReturnsEmptyArray(t *testing.T) {
	client := &fakeRepoCollisionClient{}
	previous := newRepoCollisionClient
	newRepoCollisionClient = func() repoCollisionClient { return client }
	t.Cleanup(func() { newRepoCollisionClient = previous })

	var stdout, stderr bytes.Buffer
	code := runRepo([]string{"collisions", "gitmoot/gitmoot", "--json"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "[]\n" {
		t.Fatalf("clean JSON code=%d out=%q err=%q, want []", code, stdout.String(), stderr.String())
	}
}

func TestRunRepoCollisionsRejectsUnboundedLimits(t *testing.T) {
	for _, limit := range []string{"0", "101"} {
		t.Run(limit, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runRepo([]string{"collisions", "gitmoot/gitmoot", "--limit", limit}, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), "--limit must be between 1 and 100") {
				t.Fatalf("limit=%s code=%d out=%q err=%q", limit, code, stdout.String(), stderr.String())
			}
		})
	}
}
