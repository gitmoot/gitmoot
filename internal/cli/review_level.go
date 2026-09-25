package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/jev"
	"github.com/gitmoot/gitmoot/internal/pipeline"
	"github.com/gitmoot/gitmoot/internal/reviewlevel"
)

const (
	reviewLevelAPIKeyName    = "OPENROUTER_API_KEY"
	reviewLevelStatusContext = "gitmoot/review-level"
)

type reviewLevelGitHubClient interface {
	GetPullRequest(ctx context.Context, repo github.Repository, number int64) (github.PullRequest, error)
	ListPullRequestFiles(ctx context.Context, repo github.Repository, number int64) ([]github.PullRequestFile, error)
	CreateCommitStatus(ctx context.Context, input github.CommitStatusInput) (github.CommitStatus, error)
}

var newReviewLevelGitHubClient = func() reviewLevelGitHubClient {
	return github.NewClient("")
}

// newReviewLevelJudge returns nil when no key is available, which decides
// review before merge.
var newReviewLevelJudge = func(key string) reviewlevel.Judge {
	if key == "" {
		return nil
	}
	return &jev.Client{
		Endpoint:   jev.DefaultEndpoint,
		APIKey:     key,
		HTTP:       http.DefaultClient,
		Timeout:    jev.DefaultTimeout,
		MaxRetries: jev.DefaultMaxRetries,
	}
}

func runReviewLevel(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("review level", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoFlag := fs.String("repo", "", "OWNER/REPO")
	pr := fs.Int("pr", 0, "pull request number")
	asJSON := fs.Bool("json", false, "print the decision as JSON")
	home := fs.String("home", "", "gitmoot home")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	repo, err := github.ParseRepository(*repoFlag)
	if err != nil || *pr <= 0 || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "review level requires --repo OWNER/REPO and --pr NUMBER")
		return 2
	}
	ctx := context.Background()
	gh := newReviewLevelGitHubClient()
	pull, err := gh.GetPullRequest(ctx, repo, int64(*pr))
	if err != nil {
		fmt.Fprintf(stderr, "review level: %v\n", err)
		return 1
	}
	files, err := gh.ListPullRequestFiles(ctx, repo, int64(*pr))
	if err != nil {
		fmt.Fprintf(stderr, "review level: %v\n", err)
		return 1
	}
	input := reviewlevel.Input{Repo: repo.FullName(), PR: *pr, Title: pull.Title, HeadSHA: pull.HeadSHA}
	for _, file := range files {
		input.Files = append(input.Files, reviewlevel.File{Path: file.Filename, Patch: file.Patch})
	}

	decision := reviewlevel.Decide(ctx, newReviewLevelJudge(reviewLevelAPIKey(ctx, *home)), jev.DefaultModel, input)

	if _, err := gh.CreateCommitStatus(ctx, github.CommitStatusInput{
		Repo:        repo,
		SHA:         decision.HeadSHA,
		State:       "success",
		Context:     reviewLevelStatusContext,
		Description: reviewLevelStatusDescription(decision),
	}); err != nil {
		fmt.Fprintf(stderr, "warning: could not post %s status: %v\n", reviewLevelStatusContext, err)
	}

	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(decision); err != nil {
			fmt.Fprintf(stderr, "review level: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "level: %s\n", decision.Level)
	fmt.Fprintf(stdout, "head: %s\n", decision.HeadSHA)
	fmt.Fprintf(stdout, "reason: %s: %s\n", decision.Source, decision.Reason)
	if decision.Level == reviewlevel.LevelBackground {
		fmt.Fprintf(stdout, "after merging: gitmoot review request --repo %s --pr %d --head %s --role <your role> --post-merge\n",
			repo.FullName(), *pr, decision.HeadSHA)
	}
	return 0
}

// reviewLevelAPIKey prefers the process environment, then the Gitmoot keychain.
// Any keychain failure yields "" (review before merge), never an error.
func reviewLevelAPIKey(ctx context.Context, home string) string {
	if key := strings.TrimSpace(os.Getenv(reviewLevelAPIKeyName)); key != "" {
		return key
	}
	var key string
	_ = withStore(home, func(store *db.Store) error {
		_, values, err := pipeline.LoadValidatedKeychainFile(ctx, store, home)
		if err != nil {
			return err
		}
		key = strings.TrimSpace(values[reviewLevelAPIKeyName])
		return nil
	})
	return key
}

func reviewLevelStatusDescription(decision reviewlevel.Decision) string {
	var text string
	switch decision.Level {
	case reviewlevel.LevelNoReview:
		text = "L1 no review: merge after checks pass"
	case reviewlevel.LevelBackground:
		text = "L2 background review: merge after checks pass, then run gitmoot review request --post-merge"
	default:
		text = "L3 review before merge (" + decision.Source + ")"
	}
	if len(text) > 140 {
		text = text[:140]
	}
	return text
}
