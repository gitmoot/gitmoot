package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/reviewlevel"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// The stalled-PR evaluator (#2677) joins what GitHub knows about an open pull
// request (head, draft, mergeable, CI at the head) with what gitmoot knows
// (review level at the head, the verdict or a running review at the head, the
// merge gate, holds) and names the pull requests stuck between two steps. One
// evaluator feeds `gitmoot pr stalled` and the daemon sweep.

const (
	prStallNoCIOnHead     = "no_ci_on_head"
	prStallCIRed          = "ci_red"
	prStallAwaitingReview = "awaiting_review"
	prStallGreenNotMerged = "green_not_merged"
	prStallDraftStale     = "draft_stale"
)

// prStalledGitHub is the GitHub surface the evaluator reads: one GraphQL query
// per repository, and the base branch's required checks (cached).
type prStalledGitHub interface {
	OpenPullRequestFacts(ctx context.Context, repo github.Repository) (github.OpenPullRequestFacts, error)
	RequiredStatusCheckContexts(ctx context.Context, repo github.Repository, branch string) ([]string, bool, error)
	// GetPullRequest reads the head `pr hold` binds the hold to.
	GetPullRequest(ctx context.Context, repo github.Repository, number int64) (github.PullRequest, error)
}

// prStalledGitHubClient builds the GitHub client; tests replace it.
var prStalledGitHubClient = func() prStalledGitHub { return github.NewClient("") }

// prStallFacts is everything the classifier decides from, for one pull request.
type prStallFacts struct {
	Repo string
	PR   github.PullRequestFacts
	// Required is the base branch's required checks; RequiredKnown=false
	// counts every check as required.
	Required      map[string]bool
	RequiredKnown bool
	// Level is the review level at the head ("" when none was recorded).
	Level string
	// ReviewRunning reports a pre-merge review queued, running or blocked at
	// the head; Verdict is the newest saved verdict at the head.
	ReviewRunning bool
	Verdict       string
	VerdictAt     time.Time
	MergeGate     *db.MergeGate
	Hold          *prHold
}

// prStall is one classified stalled pull request.
type prStall struct {
	State string
	Since time.Time
	Cause string
	Next  string
}

// classifyStalledPR returns the state a pull request is stalled in, or false.
// An active hold (bound to the current head) silences it.
func classifyStalledPR(f prStallFacts, policy config.PRStalledPolicy, now time.Time) (prStall, bool) {
	pr := f.PR
	if f.Hold != nil && strings.EqualFold(f.Hold.Head, pr.HeadSHA) {
		return prStall{}, false
	}
	ref := fmt.Sprintf("%d --repo %s", pr.Number, f.Repo)
	if len(pr.Checks) == 0 {
		since := firstNonZeroTime(pr.HeadCommittedAt, pr.CreatedAt)
		if now.Sub(since) < policy.NoCIAfter {
			return prStall{}, false
		}
		stall := prStall{State: prStallNoCIOnHead, Since: since}
		switch {
		case pr.Mergeable == "CONFLICTING":
			stall.Cause = "conflicting with " + firstNonEmpty(pr.BaseRef, "the base") + ", so pull_request CI cannot run"
			stall.Next = fmt.Sprintf("rebase onto %s and push: git fetch origin && git rebase origin/%s", firstNonEmpty(pr.BaseRef, "the base"), firstNonEmpty(pr.BaseRef, "main"))
		case pr.Draft:
			stall.Cause = "a draft, so CI did not run"
			stall.Next = "gh pr ready " + ref
		default:
			stall.Cause = "no check reported at the head"
			stall.Next = fmt.Sprintf("check the workflow triggers: gh run list --repo %s --commit %s", f.Repo, pr.HeadSHA)
		}
		return stall, true
	}
	if pr.Draft {
		since := laterTime(pr.CreatedAt, pr.HeadCommittedAt)
		if now.Sub(since) < policy.DraftStaleAfter {
			return prStall{}, false
		}
		return prStall{State: prStallDraftStale, Since: since, Cause: "a draft with no push since",
			Next: "gh pr ready " + ref + ", or close it"}, true
	}
	ci := summarizePRChecks(pr.Checks, f.Required, f.RequiredKnown)
	if ci.failed != "" {
		since := firstNonZeroTime(ci.redSince, pr.HeadCommittedAt)
		if now.Sub(since) < policy.CIRedAfter {
			return prStall{}, false
		}
		return prStall{State: prStallCIRed, Since: since, Cause: fmt.Sprintf("required check %q failed", ci.failed),
			Next: "gh pr checks " + ref + ", then fix the failure or re-run it"}, true
	}
	if ci.pending {
		return prStall{}, false
	}
	greenSince := firstNonZeroTime(ci.greenSince, pr.HeadCommittedAt)
	level := prStallLevelLabel(f.Level)
	if f.Level != reviewlevel.LevelNoReview && f.Level != reviewlevel.LevelBackground {
		// Level 3, or no level recorded at the head (which counts as level 3).
		if f.ReviewRunning || f.Verdict == "changes_requested" {
			return prStall{}, false
		}
		if f.Verdict == "" {
			if now.Sub(greenSince) < policy.AwaitingReviewAfter {
				return prStall{}, false
			}
			return prStall{State: prStallAwaitingReview, Since: greenSince,
				Cause: level + ", green, and no review requested or running at the head",
				Next:  fmt.Sprintf("gitmoot review request --repo %s --pr %d --head %s", f.Repo, pr.Number, pr.HeadSHA)}, true
		}
		greenSince = laterTime(greenSince, f.VerdictAt)
	}
	if now.Sub(greenSince) < policy.GreenNotMergedAfter || f.mergeGateProgressing(policy, now) {
		return prStall{}, false
	}
	cause := level + " and green, not merged"
	if f.Verdict == "approved" {
		cause = level + ", approved and green, not merged"
	}
	if f.MergeGate != nil && f.MergeGate.State == "blocked" {
		cause += "; merge gate: " + truncateForWake(f.MergeGate.Reason, 120)
	}
	return prStall{State: prStallGreenNotMerged, Since: greenSince, Cause: cause,
		Next: fmt.Sprintf("comment /gitmoot merge on %s#%d (gitmoot review status --repo %s --pr %d shows the gate)", f.Repo, pr.Number, f.Repo, pr.Number)}, true
}

// mergeGateProgressing reports an armed merge gate making progress: pending
// and evaluated again within the green_not_merged threshold.
func (f prStallFacts) mergeGateProgressing(policy config.PRStalledPolicy, now time.Time) bool {
	if f.MergeGate == nil || f.MergeGate.State != "pending" {
		return false
	}
	updated := parseTranscriptStoreTime(f.MergeGate.UpdatedAt)
	return !updated.IsZero() && now.Sub(updated) < policy.GreenNotMergedAfter
}

type prCheckSummary struct {
	failed     string // the first failing required check
	redSince   time.Time
	pending    bool
	greenSince time.Time
}

func summarizePRChecks(checks []github.PullRequestCheckFact, required map[string]bool, requiredKnown bool) prCheckSummary {
	var summary prCheckSummary
	for _, check := range checks {
		at := firstNonZeroTime(check.CompletedAt, check.StartedAt)
		switch check.Bucket {
		case "fail":
			if requiredKnown && !required[strings.TrimSpace(check.Name)] {
				continue
			}
			if summary.failed == "" || (!at.IsZero() && at.Before(summary.redSince)) {
				summary.failed, summary.redSince = check.Name, at
			}
		case "pass", "skipping":
			summary.greenSince = laterTime(summary.greenSince, at)
		default:
			summary.pending = true
		}
	}
	return summary
}

func prStallLevelLabel(level string) string {
	switch level {
	case reviewlevel.LevelNoReview:
		return "level 1"
	case reviewlevel.LevelBackground:
		return "level 2"
	case reviewlevel.LevelRequired:
		return "level 3"
	}
	return "level unknown"
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// prStalledRow is one flagged pull request as reported.
type prStalledRow struct {
	Repo        string    `json:"repo"`
	PullRequest int       `json:"pr"`
	Title       string    `json:"title"`
	Head        string    `json:"head_sha"`
	State       string    `json:"state"`
	Since       time.Time `json:"since"`
	Age         string    `json:"age"`
	Level       string    `json:"level"`
	Owner       string    `json:"owner_role"`
	OwnerSource string    `json:"owner_source"`
	Cause       string    `json:"cause"`
	Next        string    `json:"next"`
	// attributed is the role the PR's own records name, before any roster
	// check; the alert carries it and the sink validates it.
	attributed string
}

// prHeldRow is one pull request silenced by a hold at its current head.
type prHeldRow struct {
	Repo        string `json:"repo"`
	PullRequest int    `json:"pr"`
	Head        string `json:"head_sha"`
	By          string `json:"by"`
	Reason      string `json:"reason"`
	At          string `json:"at"`
}

type prStalledReport struct {
	Repo      string         `json:"repo"`
	Open      int            `json:"open"`
	Truncated bool           `json:"truncated,omitempty"`
	Cost      int            `json:"graphql_cost"`
	Stalled   []prStalledRow `json:"stalled"`
	Held      []prHeldRow    `json:"held"`
	// open maps every open pull request read to its head.
	open map[int]string
}

// prStalledEnv is what one evaluation shares across repositories.
type prStalledEnv struct {
	home     string
	gh       prStalledGitHub
	required *prRequiredCheckCache
	// levels loads the review-level log once per evaluation pass.
	levels func() (map[string]map[int][]reviewLevelLogEntry, error)
	org    config.OrgConfig
	orgOK  bool
	roster orgRoster
}

func newPRStalledEnv(ctx context.Context, store *db.Store, home string, gh prStalledGitHub, required *prRequiredCheckCache) *prStalledEnv {
	env := &prStalledEnv{home: home, gh: gh, required: required}
	var once sync.Once
	var histories map[string]map[int][]reviewLevelLogEntry
	var historyErr error
	env.levels = func() (map[string]map[int][]reviewLevelLogEntry, error) {
		once.Do(func() { histories, historyErr = reviewLevelHistories(rawHomeFromResolved(home)) })
		return histories, historyErr
	}
	if cfg, ok := loadMergeGateOrgConfig(home); ok && cfg.Enabled() {
		env.org, env.orgOK = cfg, true
		env.roster = loadOrgRoster(ctx, store, cfg)
	}
	return env
}

// evaluateStalledPullRequests evaluates every open pull request of repo: one
// GraphQL query, then store reads only.
func evaluateStalledPullRequests(ctx context.Context, store *db.Store, env *prStalledEnv, repo string, policy config.PRStalledPolicy, now time.Time) (prStalledReport, error) {
	report := prStalledReport{Repo: repo, Stalled: []prStalledRow{}, Held: []prHeldRow{}, open: map[int]string{}}
	ghRepo, err := github.ParseRepository(repo)
	if err != nil {
		return report, err
	}
	facts, err := env.gh.OpenPullRequestFacts(ctx, ghRepo)
	if err != nil {
		return report, fmt.Errorf("read open pull requests of %s: %w", repo, err)
	}
	report.Open, report.Cost = facts.Total, facts.Cost
	report.Truncated = facts.Total > len(facts.PullRequests)
	if len(facts.PullRequests) == 0 {
		return report, nil
	}
	numbers := make([]int, 0, len(facts.PullRequests))
	for _, pr := range facts.PullRequests {
		numbers = append(numbers, pr.Number)
		report.open[pr.Number] = pr.HeadSHA
	}
	reviews, err := prStalledReviewsAtHead(ctx, store, repo, numbers)
	if err != nil {
		return report, err
	}
	gates, err := store.ListMergeGatesByRepo(ctx, repo)
	if err != nil {
		return report, err
	}
	holds, err := listPRHolds(ctx, store, repo)
	if err != nil {
		return report, err
	}
	histories, historyErr := env.levels()
	levelsByPR := histories[strings.ToLower(repo)]
	for _, pr := range facts.PullRequests {
		f := prStallFacts{Repo: repo, PR: pr}
		f.Required, f.RequiredKnown = env.required.get(ctx, env.gh, ghRepo, pr.BaseRef, now)
		if historyErr == nil {
			f.Level = prLevelAtHead(levelsByPR[pr.Number], pr.HeadSHA)
		}
		review := reviews[prHeadKey(pr.Number, pr.HeadSHA)]
		f.ReviewRunning, f.Verdict, f.VerdictAt = review.running, review.verdict, review.verdictAt
		if gate, ok := gates[int64(pr.Number)]; ok {
			f.MergeGate = &gate
		}
		if hold, ok := holds[pr.Number]; ok {
			f.Hold = &hold
			if strings.EqualFold(hold.Head, pr.HeadSHA) {
				report.Held = append(report.Held, prHeldRow{Repo: repo, PullRequest: pr.Number, Head: pr.HeadSHA, By: hold.By, Reason: hold.Reason, At: hold.At})
				continue
			}
		}
		stall, ok := classifyStalledPR(f, policy, now)
		if !ok {
			continue
		}
		row := prStalledRow{Repo: repo, PullRequest: pr.Number, Title: pr.Title, Head: pr.HeadSHA, State: stall.State,
			Since: stall.Since, Age: formatStallAge(now.Sub(stall.Since)), Level: prStallLevelLabel(f.Level),
			Cause: stall.Cause, Next: stall.Next}
		attributed, err := pullRequestAttributedRole(ctx, store, repo, pr.Number)
		if err != nil {
			return report, err
		}
		row.attributed = attributed
		row.Owner, row.OwnerSource = env.resolveOwner(attributed, repo)
		report.Stalled = append(report.Stalled, row)
	}
	sort.SliceStable(report.Stalled, func(i, j int) bool { return report.Stalled[i].Since.Before(report.Stalled[j].Since) })
	return report, nil
}

// resolveOwner mirrors the blocked-event sink: the attributed role while it is
// a live roster member, else the repo's single owner, else unowned.
func (env *prStalledEnv) resolveOwner(attributed, repo string) (string, string) {
	if !env.orgOK {
		if attributed != "" {
			return attributed, "attributed"
		}
		return "", "unowned"
	}
	if orgRosterHasMember(env.roster, attributed) {
		return attributed, "attributed"
	}
	if owner := resolveRepoOrgOwner(env.roster, env.org, repo).Owner; owner != "" {
		return owner, "repo_owner"
	}
	return "", "unowned"
}

// pullRequestAttributedRole is #2671's blocked-task attribution keyed by pull
// request: the review requester, then the acting role of the newest implement
// run, then the pull request owner. "" means none was recorded.
func pullRequestAttributedRole(ctx context.Context, store *db.Store, repo string, pullRequest int) (string, error) {
	attribution, err := store.PullRequestJobAttribution(ctx, repo, pullRequest)
	if err != nil {
		return "", err
	}
	if role := workflow.NormalizeActingOrgRole(firstNonEmpty(attribution.Requester, attribution.ActingRole)); role != "" {
		return role, nil
	}
	owner, err := store.ResolvePullRequestOwner(ctx, repo, pullRequest, "")
	if err != nil {
		return "", err
	}
	return workflow.NormalizeActingOrgRole(owner), nil
}

// prLevelAtHead is the ratcheted review level recorded at head, or "" when no
// level was recorded at it.
func prLevelAtHead(history []reviewLevelLogEntry, head string) string {
	entry, ok := latestReviewLevelEntryAtHead(history, head)
	if !ok {
		return ""
	}
	records := make([]reviewlevel.Record, len(history))
	for i, item := range history {
		records[i] = reviewlevel.Record{Time: item.Time, Decision: item.Decision}
	}
	return reviewlevel.Ratchet(entry.Decision, records).Level
}

type prHeadReview struct {
	running   bool
	verdict   string
	verdictAt time.Time
}

func prHeadKey(pr int, head string) string {
	return strconv.Itoa(pr) + "@" + strings.ToLower(strings.TrimSpace(head))
}

// prStalledReviewsAtHead reads, per pull request and head, whether a pre-merge
// review is still running and the newest saved verdict.
func prStalledReviewsAtHead(ctx context.Context, store *db.Store, repo string, numbers []int) (map[string]prHeadReview, error) {
	jobs, err := store.ListReviewJobsForPullRequests(ctx, repo, numbers)
	if err != nil {
		return nil, err
	}
	out := map[string]prHeadReview{}
	for _, job := range jobs { // newest first
		payload, err := workflow.DecodeJobPayload(job)
		if err != nil || payload.PostMergeReview || strings.TrimSpace(payload.HeadSHA) == "" {
			continue
		}
		key := prHeadKey(payload.PullRequest, payload.HeadSHA)
		review := out[key]
		switch job.State {
		case string(workflow.JobQueued), string(workflow.JobRunning), string(workflow.JobBlocked):
			review.running = true
		}
		if review.verdict == "" {
			if verdict := reviewVerdictDecision(job, payload); verdict != "" {
				review.verdict, review.verdictAt = verdict, parseTranscriptStoreTime(job.UpdatedAt)
			}
		}
		out[key] = review
	}
	return out, nil
}

// prRequiredCheckCache caches each base branch's required checks: they change
// rarely, and each lookup is two REST calls.
type prRequiredCheckCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]prRequiredChecks
}

type prRequiredChecks struct {
	at       time.Time
	required map[string]bool
	known    bool
}

const prRequiredCheckTTL = 6 * time.Hour

// prStalledRequiredChecks is the daemon's process-wide cache.
var prStalledRequiredChecks = newPRRequiredCheckCache()

func newPRRequiredCheckCache() *prRequiredCheckCache {
	return &prRequiredCheckCache{ttl: prRequiredCheckTTL, entries: map[string]prRequiredChecks{}}
}

func (c *prRequiredCheckCache) get(ctx context.Context, gh prStalledGitHub, repo github.Repository, branch string, now time.Time) (map[string]bool, bool) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil, false
	}
	key := strings.ToLower(repo.FullName()) + "\x00" + branch
	c.mu.Lock()
	entry, ok := c.entries[key]
	c.mu.Unlock()
	if ok && now.Sub(entry.at) < c.ttl {
		return entry.required, entry.known
	}
	contexts, known, err := gh.RequiredStatusCheckContexts(ctx, repo, branch)
	entry = prRequiredChecks{at: now, known: err == nil && known}
	if entry.known {
		entry.required = make(map[string]bool, len(contexts))
		for _, name := range contexts {
			entry.required[strings.TrimSpace(name)] = true
		}
	}
	c.mu.Lock()
	c.entries[key] = entry
	c.mu.Unlock()
	return entry.required, entry.known
}

func formatStallAge(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	age = age.Round(time.Minute)
	hours := int(age / time.Hour)
	minutes := int((age % time.Hour) / time.Minute)
	switch {
	case hours >= 48:
		return fmt.Sprintf("%dd%dh", hours/24, hours%24)
	case hours > 0:
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
