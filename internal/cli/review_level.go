package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/hubclient"
	"github.com/gitmoot/gitmoot/internal/jev"
	"github.com/gitmoot/gitmoot/internal/keychain"
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
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	repo, err := github.ParseRepository(*repoFlag)
	if err != nil || *pr <= 0 || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "review level requires --repo OWNER/REPO and --pr NUMBER")
		return 2
	}
	var decision reviewlevel.Decision
	handled, code := withHubClient(*home, "", "review level", stderr, func(ctx context.Context, client *hubclient.Client, role string) error {
		var answer struct {
			Decision reviewlevel.Decision `json:"decision"`
			Warnings []string             `json:"warnings"`
		}
		if err := client.Do(ctx, http.MethodPost, "/v1/reviews/level", role, map[string]any{"repo": repo.FullName(), "pr": *pr}, &answer); err != nil {
			return err
		}
		for _, warning := range answer.Warnings {
			fmt.Fprintln(stderr, warning)
		}
		decision = answer.Decision
		return nil
	})
	if handled && code != 0 {
		return code
	}
	if !handled {
		var err error
		decision, err = decideReviewLevel(context.Background(), *home, repo, *pr, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "review level: %v\n", err)
			return 1
		}
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

// decideReviewLevel classifies a pull request, saves the decision and posts
// the commit status. `review level` and the hub API both call it; warnings
// about the log and the status go to warn and do not fail the decision.
func decideReviewLevel(ctx context.Context, home string, repo github.Repository, pr int, warn io.Writer) (reviewlevel.Decision, error) {
	gh := newReviewLevelGitHubClient()
	pull, err := gh.GetPullRequest(ctx, repo, int64(pr))
	if err != nil {
		return reviewlevel.Decision{}, err
	}
	files, err := gh.ListPullRequestFiles(ctx, repo, int64(pr))
	if err != nil {
		return reviewlevel.Decision{}, err
	}
	input := reviewlevel.Input{Repo: repo.FullName(), PR: pr, Title: pull.Title, HeadSHA: pull.HeadSHA}
	for _, file := range files {
		input.Files = append(input.Files, reviewlevel.File{Path: file.Filename, Patch: file.Patch})
	}

	decision := reviewlevel.Decide(ctx, newReviewLevelJudge(reviewLevelAPIKey(ctx, home)), jev.DefaultModel, input)
	decision, recorded, err := settleReviewLevel(home, repo.FullName(), pr, decision, time.Now(), false)
	if err != nil {
		fmt.Fprintf(warn, "warning: could not save the decision to %s: %v\n", reviewLevelLogName, err)
	} else if !recorded {
		fmt.Fprintf(warn, "warning: the decision was not saved to %s: %s\n", reviewLevelLogName, decision.Reason)
	}

	if _, err := gh.CreateCommitStatus(ctx, github.CommitStatusInput{
		Repo:        repo,
		SHA:         decision.HeadSHA,
		State:       "success",
		Context:     reviewLevelStatusContext,
		Description: reviewLevelStatusDescription(decision),
	}); err != nil {
		fmt.Fprintf(warn, "warning: could not post %s status: %v\n", reviewLevelStatusContext, err)
	}
	return decision, nil
}

// reviewLevelLogName is the file, in the Gitmoot home, that keeps every level
// decision. The commit status shows only the latest decision per head, and
// only on GitHub; without this record nobody could count how many changes
// skipped review, by which rule, or how often JEV was unavailable (#2265).
const reviewLevelLogName = "review-levels.jsonl"

type reviewLevelLogEntry struct {
	Time string `json:"time"`
	Repo string `json:"repo"`
	PR   int    `json:"pr"`
	reviewlevel.Decision
}

// reviewLevelHistory returns every recorded decision for repo/pr, oldest
// first. A missing log is no history and a malformed line is skipped; any
// other read failure is an error, which fails closed to level 3.
func reviewLevelHistory(home, repo string, pr int) ([]reviewLevelLogEntry, error) {
	var history []reviewLevelLogEntry
	err := scanReviewLevelLog(home, func(entry reviewLevelLogEntry) {
		if entry.PR == pr && strings.EqualFold(strings.TrimSpace(entry.Repo), strings.TrimSpace(repo)) {
			history = append(history, entry)
		}
	})
	if err != nil {
		return nil, err
	}
	return history, nil
}

// reviewLevelHistories is reviewLevelHistory for every pull request at once,
// keyed by lower-cased repo and then pull request, from ONE read of the log:
// the stalled-PR evaluator (#2677) needs the level of every open pull request.
func reviewLevelHistories(home string) (map[string]map[int][]reviewLevelLogEntry, error) {
	histories := map[string]map[int][]reviewLevelLogEntry{}
	err := scanReviewLevelLog(home, func(entry reviewLevelLogEntry) {
		repo := strings.ToLower(strings.TrimSpace(entry.Repo))
		if histories[repo] == nil {
			histories[repo] = map[int][]reviewLevelLogEntry{}
		}
		histories[repo][entry.PR] = append(histories[repo][entry.PR], entry)
	})
	if err != nil {
		return nil, err
	}
	return histories, nil
}

// scanReviewLevelLog calls visit for every well-formed entry, oldest first.
func scanReviewLevelLog(home string, visit func(reviewLevelLogEntry)) error {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(paths.Home, reviewLevelLogName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var entry reviewLevelLogEntry
			if json.Unmarshal(line, &entry) == nil {
				visit(entry)
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func reviewLevelHistoryUnreadable(fresh reviewlevel.Decision, err error) reviewlevel.Decision {
	return reviewlevel.HistoryUnavailable(fresh, reviewlevel.SourceHistoryUnreadable,
		fmt.Sprintf("earlier review levels of this pull request could not be read (%v)", err))
}

// reviewLevelLockWait bounds how long a writer waits for the record lock
// before failing closed.
var reviewLevelLockWait = 10 * time.Second

// lockReviewLevelLog takes the exclusive flock on review-levels.jsonl.lock,
// next to the log, waiting at most reviewLevelLockWait. The lock is on the
// open file, so separate goroutines and processes exclude each other; unlock
// closes it.
func lockReviewLevelLog(home string) (unlock func(), err error) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(paths.Home, reviewLevelLogName+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(reviewLevelLockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("still held by another writer after %s", reviewLevelLockWait)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// settleReviewLevel is the one critical section both deciders (`review
// level` and the low-risk merge gate) use to apply the pull request ratchet
// (#2674) and record the result: under the record lock it reads the
// pull request's history, raises fresh to the highest earlier real judgement,
// and appends the result, so no writer records a lower level after another
// recorded a higher one. Callers judge BEFORE calling it: the lock is never
// held across GitHub or JEV. With onlyIfRaised, nothing is appended unless the
// level was raised. A lock or history that cannot be read fails closed to
// level 3 and is not appended. recorded reports that the decision was
// appended (or the append attempted; err is its failure), so the caller
// posts the matching commit status.
func settleReviewLevel(home, repo string, pr int, fresh reviewlevel.Decision, now time.Time, onlyIfRaised bool) (decision reviewlevel.Decision, recorded bool, err error) {
	unlock, err := lockReviewLevelLog(home)
	if err != nil {
		return reviewlevel.HistoryUnavailable(fresh, reviewlevel.SourceHistoryLocked,
			fmt.Sprintf("the review-level record could not be locked (%v)", err)), false, nil
	}
	defer unlock()
	history, err := reviewLevelHistory(home, repo, pr)
	if err != nil {
		return reviewLevelHistoryUnreadable(fresh, err), false, nil
	}
	records := make([]reviewlevel.Record, len(history))
	for i, entry := range history {
		records[i] = reviewlevel.Record{Time: entry.Time, Decision: entry.Decision}
	}
	decision = reviewlevel.Ratchet(fresh, records)
	if onlyIfRaised && decision.Level == fresh.Level {
		return decision, false, nil
	}
	return decision, true, appendReviewLevelLog(home, repo, pr, decision, now)
}

// appendReviewLevelLog adds one JSON line per decision. It never blocks the
// answer: a failure is a warning, since the decision itself is still sound.
// Deciders append through settleReviewLevel, under the record lock.
func appendReviewLevelLog(home, repo string, pr int, decision reviewlevel.Decision, now time.Time) error {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return err
	}
	line, err := json.Marshal(reviewLevelLogEntry{Time: now.UTC().Format(time.RFC3339), Repo: repo, PR: pr, Decision: decision})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(paths.Home, reviewLevelLogName), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	// One write of a short line with O_APPEND: concurrent runs do not interleave.
	_, err = f.Write(append(line, '\n'))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// reviewLevelAPIKey prefers the process environment, then the Gitmoot keychain.
// Any keychain failure yields "" (review before merge), never an error.
func reviewLevelAPIKey(ctx context.Context, home string) string {
	if key := strings.TrimSpace(os.Getenv(reviewLevelAPIKeyName)); key != "" {
		return key
	}
	var key string
	_ = withStore(home, func(store *db.Store) error {
		_, values, err := keychain.LoadValidated(ctx, store, home)
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
	if held := decision.HeldBy; decision.Source == reviewlevel.SourceRatchet && held != nil {
		suffix := "ratchet: held by " + held.Source + " at " + shortReviewLevelHead(held.HeadSHA)
		if decision.Level == reviewlevel.LevelRequired {
			text = "L3 review before merge (" + suffix + ")"
		} else {
			text += " (" + suffix + ")"
		}
	}
	if len(text) > 140 {
		text = text[:140]
	}
	return text
}

func shortReviewLevelHead(head string) string {
	head = strings.TrimSpace(head)
	if len(head) > 8 {
		return head[:8]
	}
	return head
}
