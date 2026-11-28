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
	"text/tabwriter"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/reviewseverity"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type orgSeatStatusGitHubClient interface {
	ListPullRequests(ctx context.Context, repo github.Repository, state string) ([]github.PullRequest, error)
	GetPullRequest(ctx context.Context, repo github.Repository, number int64) (github.PullRequest, error)
	ListPullRequestChecks(ctx context.Context, repo github.Repository, number int64) ([]github.PullRequestCheck, error)
	ListOpenIssuesWithLabels(ctx context.Context, repo github.Repository, labels []string) ([]github.Issue, error)
}

var newOrgSeatStatusGitHubClient = func() orgSeatStatusGitHubClient {
	return github.NewClient("")
}

var orgSeatStatusNow = func() time.Time {
	return time.Now().UTC()
}

type orgSeatStatusOutput struct {
	Role           string                      `json:"role"`
	GeneratedAt    string                      `json:"generated_at"`
	PullRequests   orgSeatStatusPRSection      `json:"pull_requests"`
	Reviews        orgSeatStatusReviewSection  `json:"reviews"`
	AwaitedFacts   orgSeatStatusAwaitSection   `json:"awaited_facts"`
	ReviewP2Issues orgSeatStatusP2IssueSection `json:"review_p2_issues"`
}

type orgSeatStatusPRSection struct {
	Items []orgSeatStatusPR `json:"items"`
	Error string            `json:"error,omitempty"`
}

type orgSeatStatusPR struct {
	Repo             string          `json:"repo"`
	PR               int             `json:"pr"`
	HeadSHA          string          `json:"head_sha"`
	ReviewLevel      string          `json:"review_level"`
	CI               orgSeatStatusCI `json:"ci"`
	MergeStateStatus string          `json:"merge_state_status"`
}

type orgSeatStatusCI struct {
	Passed  int `json:"passed"`
	Pending int `json:"pending"`
	Failed  int `json:"failed"`
}

type orgSeatStatusReviewSection struct {
	Items []orgSeatStatusReview `json:"items"`
	Error string                `json:"error,omitempty"`
}

type orgSeatStatusReview struct {
	RunID           string `json:"run_id"`
	Repo            string `json:"repo"`
	PR              int    `json:"pr"`
	HeadSHA         string `json:"head_sha"`
	State           string `json:"state"`
	Decision        string `json:"decision,omitempty"`
	HighestSeverity string `json:"highest_severity,omitempty"`
}

type orgSeatStatusAwaitSection struct {
	Items []db.AwaitedFact `json:"items"`
	Error string           `json:"error,omitempty"`
}

type orgSeatStatusP2IssueSection struct {
	Items []orgSeatStatusP2Issue `json:"items"`
	Error string                 `json:"error,omitempty"`
}

type orgSeatStatusP2Issue struct {
	Repo      string `json:"repo"`
	Number    int64  `json:"number"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at,omitempty"`
	Age       string `json:"age"`
}

func runOrgSeatStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org seat status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	roleFlag := fs.String("role", "", "organization role to inspect (defaults to GITMOOT_ORG_ROLE or bound Herdr pane)")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org seat status does not accept positional arguments")
		return 2
	}

	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org seat status: resolve paths: %v\n", err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org seat status: %v\n", err)
		return 1
	}

	ctx := context.Background()
	role, err := messageActingRole(ctx, cfg, *roleFlag)
	if err != nil {
		fmt.Fprintf(stderr, "org seat status: %v\n", err)
		return 2
	}
	roleCfg, _ := cfg.Role(role)

	store, err := openOrgDatabase(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org seat status: open store: %v\n", err)
		return 1
	}
	defer store.Close()

	now := orgSeatStatusNow().UTC()
	out, err := buildOrgSeatStatus(ctx, *home, store, roleCfg, newOrgSeatStatusGitHubClient(), now)
	if err != nil {
		fmt.Fprintf(stderr, "org seat status: %v\n", err)
		return 1
	}

	if *jsonOutput {
		if err := writeJSON(stdout, out); err != nil {
			fmt.Fprintf(stderr, "org seat status: %v\n", err)
			return 1
		}
		return 0
	}
	printOrgSeatStatus(stdout, out)
	return 0
}

func buildOrgSeatStatus(
	ctx context.Context,
	home string,
	store *db.Store,
	roleCfg config.OrgRole,
	gh orgSeatStatusGitHubClient,
	now time.Time,
) (orgSeatStatusOutput, error) {
	role := roleCfg.Name
	out := orgSeatStatusOutput{
		Role:        role,
		GeneratedAt: now.UTC().Format(time.RFC3339),
		PullRequests: orgSeatStatusPRSection{
			Items: []orgSeatStatusPR{},
		},
		Reviews: orgSeatStatusReviewSection{
			Items: []orgSeatStatusReview{},
		},
		AwaitedFacts: orgSeatStatusAwaitSection{
			Items: []db.AwaitedFact{},
		},
		ReviewP2Issues: orgSeatStatusP2IssueSection{
			Items: []orgSeatStatusP2Issue{},
		},
	}

	prRefs, err := store.ListPullRequestsByActingRole(ctx, role)
	if err != nil {
		return orgSeatStatusOutput{}, fmt.Errorf("list pull requests for role %q: %w", role, err)
	}

	levelHistories, levelErr := reviewLevelHistories(home)
	var prErrors []string
	if levelErr != nil {
		prErrors = append(prErrors, fmt.Sprintf("review-level log: %v", levelErr))
	}

	// A role's job history names every PR it ever touched (hundreds for a busy
	// seat), so list each repo's open PRs once and fetch details only for the
	// open ones instead of one GetPullRequest per historical PR.
	openByRepo := make(map[string]map[int64]bool)
	listFailed := make(map[string]bool)
	reviewTargets := make([]db.PullRequestRef, 0, len(prRefs))
	for _, ref := range prRefs {
		ghRepo, parseErr := github.ParseRepository(ref.Repo)
		if parseErr != nil {
			prErrors = append(prErrors, fmt.Sprintf("%s#%d: %v", ref.Repo, ref.Number, parseErr))
			reviewTargets = append(reviewTargets, ref)
			continue
		}
		repoKey := strings.ToLower(ghRepo.FullName())
		open, listed := openByRepo[repoKey]
		if !listed && !listFailed[repoKey] {
			pulls, err := gh.ListPullRequests(ctx, ghRepo, "open")
			if err != nil {
				listFailed[repoKey] = true
				prErrors = append(prErrors, fmt.Sprintf("%s open pull requests: %v", ghRepo.FullName(), err))
			} else {
				open = make(map[int64]bool, len(pulls))
				for _, pull := range pulls {
					open[pull.Number] = true
				}
				openByRepo[repoKey] = open
			}
		}
		if listFailed[repoKey] {
			// Unknown state: keep the PR's local review rows visible.
			reviewTargets = append(reviewTargets, ref)
			continue
		}
		if !open[ref.Number] {
			continue
		}
		pr, getErr := gh.GetPullRequest(ctx, ghRepo, ref.Number)
		if getErr != nil {
			prErrors = append(prErrors, fmt.Sprintf("%s#%d: %v", ref.Repo, ref.Number, getErr))
			reviewTargets = append(reviewTargets, ref)
			continue
		}
		if pr.Merged || !strings.EqualFold(strings.TrimSpace(pr.State), "open") {
			continue
		}
		reviewTargets = append(reviewTargets, ref)

		level := lastRecordedReviewLevel(levelHistories, ref.Repo, int(ref.Number))
		ci := orgSeatStatusCI{}
		checks, checksErr := gh.ListPullRequestChecks(ctx, ghRepo, ref.Number)
		if checksErr != nil {
			prErrors = append(prErrors, fmt.Sprintf("%s#%d checks: %v", ref.Repo, ref.Number, checksErr))
		} else {
			ci = summarizeSeatStatusChecks(checks)
		}

		mergeState := strings.ToUpper(strings.TrimSpace(pr.MergeableState))
		if mergeState == "" {
			mergeState = "UNKNOWN"
		}
		out.PullRequests.Items = append(out.PullRequests.Items, orgSeatStatusPR{
			Repo:             ghRepo.FullName(),
			PR:               int(ref.Number),
			HeadSHA:          strings.TrimSpace(pr.HeadSHA),
			ReviewLevel:      level,
			CI:               ci,
			MergeStateStatus: mergeState,
		})
	}
	if len(prErrors) > 0 {
		out.PullRequests.Error = strings.Join(prErrors, "; ")
	}

	cutoff := now.Add(-24 * time.Hour)
	for _, ref := range reviewTargets {
		jobs, err := store.ListReviewJobsForPullRequest(ctx, ref.Repo, int(ref.Number))
		if err != nil {
			return orgSeatStatusOutput{}, fmt.Errorf("list review runs for %s#%d: %w", ref.Repo, ref.Number, err)
		}
		for _, job := range jobs {
			if !includeReviewRunInSeatStatus(job, cutoff) {
				continue
			}
			var headSHA, decision, highestSeverity string
			if payload, decErr := workflow.DecodeJobPayload(job); decErr == nil {
				headSHA = strings.TrimSpace(payload.HeadSHA)
				if payload.Result != nil {
					decision = strings.TrimSpace(payload.Result.Decision)
					highestSeverity = highestUnansweredFindingSeverity(payload.Result)
				}
			}
			out.Reviews.Items = append(out.Reviews.Items, orgSeatStatusReview{
				RunID:           job.ID,
				Repo:            ref.Repo,
				PR:              int(ref.Number),
				HeadSHA:         headSHA,
				State:           job.State,
				Decision:        decision,
				HighestSeverity: highestSeverity,
			})
		}
	}

	facts, err := store.ListAwaitedFacts(ctx, role, db.AwaitedFactStateWaiting)
	if err != nil {
		return orgSeatStatusOutput{}, fmt.Errorf("list awaited facts for role %q: %w", role, err)
	}
	if facts != nil {
		out.AwaitedFacts.Items = facts
	}

	scopeRepos := seatStatusScopeRepos(roleCfg.Scope)
	p2Labels := []string{"review-p2", "agent:" + role}
	var issueErrors []string
	for _, ghRepo := range scopeRepos {
		issues, listErr := gh.ListOpenIssuesWithLabels(ctx, ghRepo, p2Labels)
		if listErr != nil {
			issueErrors = append(issueErrors, fmt.Sprintf("%s: %v", ghRepo.FullName(), listErr))
			continue
		}
		for _, issue := range issues {
			out.ReviewP2Issues.Items = append(out.ReviewP2Issues.Items, orgSeatStatusP2Issue{
				Repo:      ghRepo.FullName(),
				Number:    issue.Number,
				Title:     strings.TrimSpace(issue.Title),
				CreatedAt: strings.TrimSpace(issue.CreatedAt),
				Age:       formatSeatStatusAge(issue.CreatedAt, now),
			})
		}
	}
	sort.Slice(out.ReviewP2Issues.Items, func(i, j int) bool {
		a, b := out.ReviewP2Issues.Items[i], out.ReviewP2Issues.Items[j]
		if a.Repo != b.Repo {
			return strings.ToLower(a.Repo) < strings.ToLower(b.Repo)
		}
		return a.Number < b.Number
	})
	if len(issueErrors) > 0 {
		out.ReviewP2Issues.Error = strings.Join(issueErrors, "; ")
	}

	return out, nil
}

func lastRecordedReviewLevel(histories map[string]map[int][]reviewLevelLogEntry, repo string, pr int) string {
	byPR := histories[strings.ToLower(strings.TrimSpace(repo))]
	if len(byPR) == 0 {
		return "unrecorded"
	}
	entries := byPR[pr]
	if len(entries) == 0 {
		return "unrecorded"
	}
	if level := strings.TrimSpace(entries[len(entries)-1].Level); level != "" {
		return level
	}
	return "unrecorded"
}

func summarizeSeatStatusChecks(checks []github.PullRequestCheck) orgSeatStatusCI {
	var ci orgSeatStatusCI
	for _, check := range checks {
		bucket := strings.ToLower(strings.TrimSpace(check.Bucket))
		switch bucket {
		case "pass":
			ci.Passed++
		case "fail":
			ci.Failed++
		case "pending":
			ci.Pending++
		case "skipping":
			continue
		default:
			switch strings.ToLower(strings.TrimSpace(check.State)) {
			case "success", "neutral", "pass":
				ci.Passed++
			case "failure", "error", "cancelled", "timed_out", "action_required", "fail":
				ci.Failed++
			case "pending", "expected", "queued", "in_progress", "waiting":
				ci.Pending++
			}
		}
	}
	return ci
}

func includeReviewRunInSeatStatus(job db.Job, cutoff time.Time) bool {
	state := strings.TrimSpace(job.State)
	if state == string(workflow.JobQueued) || state == string(workflow.JobRunning) {
		return true
	}
	ts := parseSeatStatusTime(job.UpdatedAt)
	if ts.IsZero() {
		ts = parseSeatStatusTime(job.CreatedAt)
	}
	if ts.IsZero() {
		return false
	}
	return !ts.Before(cutoff)
}

func parseSeatStatusTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts.UTC()
		}
	}
	return time.Time{}
}

func highestUnansweredFindingSeverity(result *workflow.AgentResult) string {
	if result == nil || len(result.Findings) == 0 {
		return ""
	}
	best := ""
	bestRank := 0
	for _, raw := range result.Findings {
		var item struct {
			Severity    string `json:"severity"`
			State       string `json:"state"`
			Disposition string `json:"disposition"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(item.State), "answered") ||
			strings.EqualFold(strings.TrimSpace(item.Disposition), "answered") {
			continue
		}
		sev := strings.ToUpper(strings.TrimSpace(item.Severity))
		rank, ok := reviewseverity.Rank(sev)
		if !ok {
			continue
		}
		if best == "" || rank < bestRank {
			best = sev
			bestRank = rank
		}
	}
	return best
}

func seatStatusScopeRepos(scope []string) []github.Repository {
	seen := make(map[string]struct{}, len(scope))
	repos := make([]github.Repository, 0, len(scope))
	for _, raw := range scope {
		raw = strings.TrimSpace(raw)
		if raw == "" || raw == "*" || strings.Contains(raw, "*") {
			continue
		}
		repo, err := github.ParseRepository(raw)
		if err != nil {
			continue
		}
		key := strings.ToLower(repo.FullName())
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool {
		return strings.ToLower(repos[i].FullName()) < strings.ToLower(repos[j].FullName())
	})
	return repos
}

func formatSeatStatusAge(createdAt string, now time.Time) string {
	ts := parseSeatStatusTime(createdAt)
	if ts.IsZero() {
		return "-"
	}
	d := now.Sub(ts)
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return "<1m"
	}
}

func shortSeatStatusSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "-"
	}
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func seatStatusDash(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	return s
}

func printOrgSeatStatus(w io.Writer, out orgSeatStatusOutput) {
	fmt.Fprintf(w, "Seat status: %s (generated %s)\n", out.Role, out.GeneratedAt)

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Open Pull Requests")
	if out.PullRequests.Error != "" {
		fmt.Fprintf(w, "  error: %s\n", out.PullRequests.Error)
	}
	if len(out.PullRequests.Items) == 0 {
		if out.PullRequests.Error == "" {
			fmt.Fprintln(w, "  (none)")
		}
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  REPO\tPR\tHEAD\tREVIEW_LEVEL\tCI\tMERGE_STATE")
		for _, item := range out.PullRequests.Items {
			ci := fmt.Sprintf("%d pass/%d pend/%d fail", item.CI.Passed, item.CI.Pending, item.CI.Failed)
			fmt.Fprintf(tw, "  %s\t#%d\t%s\t%s\t%s\t%s\n",
				item.Repo, item.PR, shortSeatStatusSHA(item.HeadSHA), item.ReviewLevel, ci, item.MergeStateStatus)
		}
		_ = tw.Flush()
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Reviews (active or last 24h)")
	if out.Reviews.Error != "" {
		fmt.Fprintf(w, "  error: %s\n", out.Reviews.Error)
	}
	if len(out.Reviews.Items) == 0 {
		if out.Reviews.Error == "" {
			fmt.Fprintln(w, "  (none)")
		}
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  RUN_ID\tPR\tHEAD\tSTATE\tDECISION\tSEVERITY")
		for _, item := range out.Reviews.Items {
			fmt.Fprintf(tw, "  %s\t%s#%d\t%s\t%s\t%s\t%s\n",
				item.RunID, item.Repo, item.PR, shortSeatStatusSHA(item.HeadSHA),
				item.State, seatStatusDash(item.Decision), seatStatusDash(item.HighestSeverity))
		}
		_ = tw.Flush()
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Awaited Facts (waiting)")
	if out.AwaitedFacts.Error != "" {
		fmt.Fprintf(w, "  error: %s\n", out.AwaitedFacts.Error)
	}
	if len(out.AwaitedFacts.Items) == 0 {
		if out.AwaitedFacts.Error == "" {
			fmt.Fprintln(w, "  (none)")
		}
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ID\tSUBJECT\tDEADLINE")
		for _, fact := range out.AwaitedFacts.Items {
			fmt.Fprintf(tw, "  %d\t%s\t%s\n", fact.ID, fact.SubjectKey, fact.Deadline)
		}
		_ = tw.Flush()
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Open review-p2 Issues")
	if out.ReviewP2Issues.Error != "" {
		fmt.Fprintf(w, "  error: %s\n", out.ReviewP2Issues.Error)
	}
	if len(out.ReviewP2Issues.Items) == 0 {
		if out.ReviewP2Issues.Error == "" {
			fmt.Fprintln(w, "  (none)")
		}
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  REPO\tISSUE\tAGE\tTITLE")
		for _, issue := range out.ReviewP2Issues.Items {
			fmt.Fprintf(tw, "  %s\t#%d\t%s\t%s\n", issue.Repo, issue.Number, issue.Age, issue.Title)
		}
		_ = tw.Flush()
	}
}
