package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/jev"
	"github.com/gitmoot/gitmoot/internal/reviewlevel"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// lowRiskClassifierRetryAfter is how long a classifier failure recorded for a
// head is reused before the low-risk gate asks the classifier again. Until
// then the head stays level 3 (leave open) without a new model call per poll.
const lowRiskClassifierRetryAfter = time.Hour

// lowRiskPostMergeReviewEventKind marks the task once its level-2 post-merge
// review was requested, so daemon retries and restarts never request it twice.
const lowRiskPostMergeReviewEventKind = "low_risk_post_merge_review_requested"

// lowRiskPostMergeReviewOwedEventKind records a level-2 post-merge review that
// could not be requested automatically and is owed by a seat.
const lowRiskPostMergeReviewOwedEventKind = "low_risk_post_merge_review_owed"

// daemonLowRiskReviewLevel binds lowRiskReviewLevel to the daemon's GitHub
// client, the configured JEV key and the wall clock.
func daemonLowRiskReviewLevel(resolvedHome string, gh reviewLevelGitHubClient) func(ctx context.Context, repo string, pr int, headSHA string) (reviewlevel.Decision, error) {
	home := rawHomeFromResolved(resolvedHome)
	return func(ctx context.Context, repo string, pr int, headSHA string) (reviewlevel.Decision, error) {
		judge := func() reviewlevel.Judge { return newReviewLevelJudge(reviewLevelAPIKey(ctx, home)) }
		return lowRiskReviewLevel(home, gh, judge, time.Now)(ctx, repo, pr, headSHA)
	}
}

// rawHomeFromResolved turns the daemon gate's RESOLVED <home>/.gitmoot root
// (the #459 convention for daemonMergeGate.Home) back into the --home value
// that pathsFromFlag, withStore and requestReview expect, so the low-risk paths
// read and write the real review-levels.jsonl, keychain and org config.
func rawHomeFromResolved(home string) string {
	home = strings.TrimSpace(home)
	if home != "" && filepath.Base(filepath.Clean(home)) == config.DirName {
		return filepath.Dir(filepath.Clean(home))
	}
	return home
}

// lowRiskReviewLevel is the PolicyMergeGate.ReviewLevel hook for repos with
// auto_merge = "low_risk". It reuses the newest decision recorded for the exact
// head in review-levels.jsonl and otherwise classifies that head exactly as
// `gitmoot review level` does, recording the result in the same log and status.
func lowRiskReviewLevel(home string, gh reviewLevelGitHubClient, judge func() reviewlevel.Judge, now func() time.Time) func(ctx context.Context, repo string, pr int, headSHA string) (reviewlevel.Decision, error) {
	return func(ctx context.Context, repoName string, pr int, headSHA string) (reviewlevel.Decision, error) {
		headSHA = strings.ToLower(strings.TrimSpace(headSHA))
		entry, found, err := latestReviewLevelEntry(home, repoName, pr, headSHA)
		if err != nil {
			return reviewlevel.Decision{}, err
		}
		if found && !(classifierFailed(entry.Source) && reviewLevelEntryAge(entry, now()) >= lowRiskClassifierRetryAfter) {
			return entry.Decision, nil
		}
		repo, err := github.ParseRepository(repoName)
		if err != nil {
			return reviewlevel.Decision{}, err
		}
		pull, err := gh.GetPullRequest(ctx, repo, int64(pr))
		if err != nil {
			return reviewlevel.Decision{}, err
		}
		if !strings.EqualFold(strings.TrimSpace(pull.HeadSHA), headSHA) {
			return reviewlevel.Decision{}, fmt.Errorf("head moved to %s before classification", strings.TrimSpace(pull.HeadSHA))
		}
		files, err := gh.ListPullRequestFiles(ctx, repo, int64(pr))
		if err != nil {
			return reviewlevel.Decision{}, err
		}
		// The file list has no head of its own: re-read the head so the diff
		// judged is the diff of headSHA.
		after, err := gh.GetPullRequest(ctx, repo, int64(pr))
		if err != nil {
			return reviewlevel.Decision{}, err
		}
		if !strings.EqualFold(strings.TrimSpace(after.HeadSHA), headSHA) {
			return reviewlevel.Decision{}, fmt.Errorf("head moved to %s during classification", strings.TrimSpace(after.HeadSHA))
		}
		input := reviewlevel.Input{Repo: repo.FullName(), PR: pr, Title: pull.Title, HeadSHA: headSHA}
		for _, file := range files {
			input.Files = append(input.Files, reviewlevel.File{Path: file.Filename, Patch: file.Patch})
		}
		decision := reviewlevel.Decide(ctx, judge(), jev.DefaultModel, input)
		if _, err := gh.CreateCommitStatus(ctx, github.CommitStatusInput{
			Repo: repo, SHA: decision.HeadSHA, State: "success",
			Context: reviewLevelStatusContext, Description: reviewLevelStatusDescription(decision),
		}); err != nil {
			log.Printf("low-risk merge gate: could not post %s status for %s#%d: %v", reviewLevelStatusContext, repo.FullName(), pr, err)
		}
		if err := appendReviewLevelLog(home, repo.FullName(), pr, decision, now()); err != nil {
			log.Printf("low-risk merge gate: could not save review level for %s#%d: %v", repo.FullName(), pr, err)
		}
		return decision, nil
	}
}

func classifierFailed(source string) bool {
	switch strings.TrimSpace(source) {
	case "classifier_error", "classifier_unavailable":
		return true
	default:
		return false
	}
}

func reviewLevelEntryAge(entry reviewLevelLogEntry, now time.Time) time.Duration {
	at, err := time.Parse(time.RFC3339, entry.Time)
	if err != nil {
		return lowRiskClassifierRetryAfter
	}
	return now.Sub(at)
}

// latestReviewLevelEntry returns the newest review-levels.jsonl decision for
// repo/pr at exactly headSHA.
func latestReviewLevelEntry(home, repo string, pr int, headSHA string) (reviewLevelLogEntry, bool, error) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return reviewLevelLogEntry{}, false, err
	}
	file, err := os.Open(filepath.Join(paths.Home, reviewLevelLogName))
	if errors.Is(err, os.ErrNotExist) {
		return reviewLevelLogEntry{}, false, nil
	}
	if err != nil {
		return reviewLevelLogEntry{}, false, err
	}
	defer file.Close()
	var latest reviewLevelLogEntry
	found := false
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var entry reviewLevelLogEntry
			if json.Unmarshal(line, &entry) == nil && entry.PR == pr &&
				strings.EqualFold(strings.TrimSpace(entry.Repo), strings.TrimSpace(repo)) &&
				strings.EqualFold(strings.TrimSpace(entry.HeadSHA), headSHA) {
				latest, found = entry, true
			}
		}
		if readErr == io.EOF {
			return latest, found, nil
		}
		if readErr != nil {
			return reviewLevelLogEntry{}, false, readErr
		}
	}
}

// requestLowRiskPostMergeReview requests the AGENTS.md level-2 background
// review after a low-risk auto-merge. It is idempotent: the task event marks a
// completed request, and the review-request claim (repo, PR, head, post-merge
// purpose) attaches a repeated request to the existing job. When no role can be
// resolved, or the request fails, the obligation is recorded on the task.
func (g daemonMergeGate) requestLowRiskPostMergeReview(ctx context.Context, request workflow.MergeRequest, decision workflow.MergeDecision) {
	if decision.ReviewLevel != reviewlevel.LevelBackground || g.Store == nil {
		return
	}
	// The head that actually merged, not the task payload's recorded head.
	head := strings.ToLower(strings.TrimSpace(decision.MergedHeadSHA))
	if head == "" {
		head = strings.ToLower(strings.TrimSpace(request.HeadSHA))
	}
	taskID := strings.TrimSpace(request.TaskID)
	if taskID != "" {
		events, err := g.Store.ListTaskEvents(ctx, taskID)
		if err != nil {
			log.Printf("low-risk merge gate: read task %s events: %v", taskID, err)
			return
		}
		for _, event := range events {
			if event.Kind == lowRiskPostMergeReviewEventKind || event.Kind == lowRiskPostMergeReviewOwedEventKind {
				return
			}
		}
	}
	role, err := approvingReviewRole(ctx, g.Store, request.Repo, request.PullRequest, head)
	if err == nil && role == "" {
		err = errors.New("no acting organization role on an exact-head review job")
	}
	if err == nil {
		_, err = requestReview(ctx, g.Store, reviewRequestOptions{
			home: rawHomeFromResolved(g.Home), repo: request.Repo, pr: request.PullRequest, head: head,
			purpose: "code", role: role, ttl: defaultReviewRequestTTL, postMerge: true,
		}, io.Discard)
	}
	kind, note := lowRiskPostMergeReviewEventKind, fmt.Sprintf("level 2 post-merge review requested for %s#%d at %s for role %s", request.Repo, request.PullRequest, head, role)
	if err != nil {
		kind = lowRiskPostMergeReviewOwedEventKind
		note = fmt.Sprintf("level 2 post-merge review is owed for %s#%d at %s: gitmoot review request --repo %s --pr %d --head %s --role <role> --post-merge (automatic request failed: %v)",
			request.Repo, request.PullRequest, head, request.Repo, request.PullRequest, head, err)
	}
	log.Printf("low-risk merge gate: %s", note)
	if taskID != "" {
		if err := g.Store.AddTaskEvent(ctx, db.TaskEvent{TaskID: taskID, Kind: kind, Reason: note}); err != nil {
			log.Printf("low-risk merge gate: record %s for task %s: %v", kind, taskID, err)
		}
	}
}

// approvingReviewRole is the acting organization role of the newest
// pre-merge review job at the exact head: the seat the verdict reaches.
func approvingReviewRole(ctx context.Context, store *db.Store, repo string, pr int, head string) (string, error) {
	jobs, err := store.ListReviewJobsForPullRequest(ctx, repo, pr)
	if err != nil {
		return "", err
	}
	for _, job := range jobs {
		payload, err := workflow.ParseJobPayload(job.Payload)
		if err != nil || payload.PostMergeReview || !strings.EqualFold(strings.TrimSpace(payload.HeadSHA), head) {
			continue
		}
		if role := workflow.NormalizeActingOrgRole(payload.ActingOrgRole); role != "" {
			return role, nil
		}
	}
	return "", nil
}
