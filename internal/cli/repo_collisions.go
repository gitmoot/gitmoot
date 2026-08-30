package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/github"
)

type repoCollisionClient interface {
	ListPullRequests(context.Context, github.Repository, string) ([]github.PullRequest, error)
	ListPullRequestFiles(context.Context, github.Repository, int64) ([]github.PullRequestFile, error)
}

var newRepoCollisionClient = func() repoCollisionClient {
	return github.NewClient("")
}

type repoPRCollision struct {
	FirstPR  int64    `json:"first_pr"`
	SecondPR int64    `json:"second_pr"`
	Files    []string `json:"files"`
}

func runRepoCollisions(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo collisions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "print collisions as JSON")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "repo collisions requires owner/repo")
			return 2
		}
		return 0
	}
	repoArg, code := parseRepoPositional(fs, "repo collisions", args, nil, map[string]struct{}{"json": {}}, stderr)
	if code >= 0 {
		return code
	}
	repo, err := daemon.ParseRepository(repoArg)
	if err != nil {
		fmt.Fprintf(stderr, "repo collisions: invalid repo: %v\n", err)
		return 2
	}
	collisions, err := detectOpenPRFileCollisions(context.Background(), newRepoCollisionClient(), repo)
	if err != nil {
		fmt.Fprintf(stderr, "repo collisions: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(collisions); err != nil {
			fmt.Fprintf(stderr, "repo collisions: %v\n", err)
			return 1
		}
		if len(collisions) > 0 {
			return 1
		}
		return 0
	}
	if len(collisions) == 0 {
		fmt.Fprintf(stdout, "repo: %s no open PR file collisions\n", repo.FullName())
		return 0
	}
	for _, collision := range collisions {
		fmt.Fprintf(stdout, "WARN: PR #%d and PR #%d touch %d shared file(s): %s\n",
			collision.FirstPR, collision.SecondPR, len(collision.Files), strings.Join(collision.Files, ", "))
	}
	return 1
}

func detectOpenPRFileCollisions(ctx context.Context, client repoCollisionClient, repo github.Repository) ([]repoPRCollision, error) {
	if client == nil {
		return nil, errors.New("GitHub client is required")
	}
	pulls, err := client.ListPullRequests(ctx, repo, "open")
	if err != nil {
		return nil, fmt.Errorf("list open pull requests for %s: %w", repo.FullName(), err)
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number < pulls[j].Number })
	fileSets := make(map[int64]map[string]struct{}, len(pulls))
	for _, pull := range pulls {
		files, err := client.ListPullRequestFiles(ctx, repo, pull.Number)
		if err != nil {
			return nil, fmt.Errorf("list files for PR #%d: %w", pull.Number, err)
		}
		set := make(map[string]struct{}, len(files))
		for _, file := range files {
			if name := strings.TrimSpace(file.Filename); name != "" {
				set[name] = struct{}{}
			}
		}
		fileSets[pull.Number] = set
	}

	collisions := make([]repoPRCollision, 0)
	for i := 0; i < len(pulls); i++ {
		for j := i + 1; j < len(pulls); j++ {
			first, second := pulls[i].Number, pulls[j].Number
			shared := intersectPRFileSets(fileSets[first], fileSets[second])
			if len(shared) == 0 {
				continue
			}
			collisions = append(collisions, repoPRCollision{FirstPR: first, SecondPR: second, Files: shared})
		}
	}
	return collisions, nil
}

func intersectPRFileSets(left, right map[string]struct{}) []string {
	if len(left) > len(right) {
		left, right = right, left
	}
	shared := make([]string, 0)
	for file := range left {
		if _, ok := right[file]; ok {
			shared = append(shared, file)
		}
	}
	sort.Strings(shared)
	return shared
}
