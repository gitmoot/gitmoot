package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

const (
	// designNotesSweepInterval: the sweep runs at most once an hour.
	designNotesSweepInterval = time.Hour
	// designNotesSweepTimeout bounds the GitHub work of one sweep, on top of
	// workflow.DesignNotesSweepLimit, so the supervisor tick it runs in moves on.
	designNotesSweepTimeout = 90 * time.Second
)

// designNotesGitHubClient is the GitHub surface the design-notes sweep needs.
type designNotesGitHubClient interface {
	GetPullRequest(ctx context.Context, repo github.Repository, number int64) (github.PullRequest, error)
	SearchIssuesByTitle(ctx context.Context, repo github.Repository, title, label string) ([]github.Issue, error)
	CreateIssue(ctx context.Context, input github.CreateIssueInput) (github.Issue, error)
	ListIssueComments(ctx context.Context, repo github.Repository, issueNumber int64) ([]github.IssueComment, error)
	PostIssueComment(ctx context.Context, repo github.Repository, issueNumber int64, body string) (github.IssueComment, error)
}

var newDesignNotesGitHubClient = func() designNotesGitHubClient {
	return github.NewClient("")
}

// designNotesSweepState is the supervisor's memory between sweeps: when the
// last one started, and the round-robin cursor (the last pull request the
// previous sweep attempted).
type designNotesSweepState struct {
	lastRun time.Time
	cursor  workflow.DesignNotesPullRequestKey
}

// runDesignNotesSweepOnce runs the weekly design-notes sweep (#2558) at most
// once per designNotesSweepInterval. The cadence advances on every attempt, so
// a failing GitHub is not retried every tick. Per-pull-request failures are
// logged here and recorded by the sweep as job events; only a ledger read
// failure is returned, for the caller to log.
func runDesignNotesSweepOnce(ctx context.Context, store *db.Store, now time.Time, state *designNotesSweepState, stdout io.Writer) error {
	if store == nil || state == nil {
		return fmt.Errorf("design notes sweep requires a store and state")
	}
	if !maintenancePassDue(state.lastRun, now, designNotesSweepInterval) {
		return nil
	}
	state.lastRun = now
	sweepCtx, cancel := context.WithTimeout(ctx, designNotesSweepTimeout)
	defer cancel()
	engine := workflow.Engine{Store: store, Now: func() time.Time { return now }}
	wireDesignNotes(&engine, newDesignNotesGitHubClient())
	result, err := engine.SweepDesignNotes(sweepCtx, workflow.DesignNotesSweepOptions{After: state.cursor})
	state.cursor = result.Next
	for _, message := range result.Errors {
		writeLine(stdout, "design notes: %s", message)
	}
	for _, url := range result.Created {
		writeLine(stdout, "design notes: opened %s", url)
	}
	if result.Filed+result.Skipped > 0 {
		writeLine(stdout, "design notes: checked %d pull request(s), filed %d finding(s) (%d repaired from posted comments), skipped %d, %d still open",
			result.Checked, result.Filed, result.Repaired, result.Skipped, result.Pending)
	}
	return err
}

// wireDesignNotes installs the engine seams of the design-notes sweep, the way
// wireReviewPostMergeIssues installs PostMergeFollowUpIssue.
func wireDesignNotes(engine *workflow.Engine, gh designNotesGitHubClient) {
	if engine == nil || gh == nil {
		return
	}
	engine.DesignNotesPullRequest = func(ctx context.Context, repo string, number int) (workflow.DesignNotesPullRequestState, error) {
		r, err := github.ParseRepository(repo)
		if err != nil {
			return workflow.DesignNotesPullRequestState{}, err
		}
		pr, err := gh.GetPullRequest(ctx, r, int64(number))
		if err != nil {
			return workflow.DesignNotesPullRequestState{}, err
		}
		merged := pr.Merged || strings.TrimSpace(pr.MergedAt) != ""
		return workflow.DesignNotesPullRequestState{
			Merged:  merged,
			Closed:  !merged && strings.EqualFold(strings.TrimSpace(pr.State), "closed"),
			HeadSHA: strings.TrimSpace(pr.HeadSHA),
		}, nil
	}
	engine.DesignNotesFindIssue = func(ctx context.Context, repo, title, label string) (workflow.DesignNotesIssue, bool, error) {
		r, err := github.ParseRepository(repo)
		if err != nil {
			return workflow.DesignNotesIssue{}, false, err
		}
		issues, err := gh.SearchIssuesByTitle(ctx, r, title, label)
		if err != nil {
			return workflow.DesignNotesIssue{}, false, err
		}
		for _, issue := range issues {
			if issue.Number > 0 {
				return workflow.DesignNotesIssue{Number: issue.Number, URL: issue.URL}, true, nil
			}
		}
		return workflow.DesignNotesIssue{}, false, nil
	}
	engine.DesignNotesCreateIssue = func(ctx context.Context, repo, title, body string, labels []string) (workflow.DesignNotesIssue, error) {
		r, err := github.ParseRepository(repo)
		if err != nil {
			return workflow.DesignNotesIssue{}, err
		}
		issue, err := gh.CreateIssue(ctx, github.CreateIssueInput{Repo: r, Title: title, Body: body, Labels: labels})
		if err != nil {
			return workflow.DesignNotesIssue{}, err
		}
		if issue.Number <= 0 || strings.TrimSpace(issue.URL) == "" {
			return workflow.DesignNotesIssue{}, fmt.Errorf("created issue %q has no number or url", title)
		}
		return workflow.DesignNotesIssue{Number: issue.Number, URL: issue.URL}, nil
	}
	engine.DesignNotesListComments = func(ctx context.Context, repo string, issue int64) ([]string, error) {
		r, err := github.ParseRepository(repo)
		if err != nil {
			return nil, err
		}
		comments, err := gh.ListIssueComments(ctx, r, issue)
		if err != nil {
			return nil, err
		}
		bodies := make([]string, 0, len(comments))
		for _, comment := range comments {
			bodies = append(bodies, comment.Body)
		}
		return bodies, nil
	}
	engine.DesignNotesComment = func(ctx context.Context, repo string, issue int64, body string) error {
		r, err := github.ParseRepository(repo)
		if err != nil {
			return err
		}
		_, err = gh.PostIssueComment(ctx, r, issue, body)
		return err
	}
}
