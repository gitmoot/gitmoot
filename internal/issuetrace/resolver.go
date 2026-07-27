package issuetrace

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

const defaultCacheTTL = 2 * time.Minute

type LocalReader interface {
	ListTasksByRepo(ctx context.Context, repo string) ([]db.Task, error)
	ListPullRequests(ctx context.Context, repo string) ([]db.PullRequest, error)
	ListJobs(ctx context.Context) ([]db.Job, error)
}

type Resolver struct {
	Remote Reader
	Local  LocalReader
	TTL    time.Duration
	Now    func() time.Time

	mu      sync.Mutex
	cache   map[string]cacheEntry
	flights map[string]*flight
}

type cacheEntry struct {
	trace     IssueTrace
	expiresAt time.Time
}

type flight struct {
	done  chan struct{}
	trace IssueTrace
	err   error
}

type localSnapshot struct {
	tasks []db.Task
	prs   []db.PullRequest
	jobs  []db.Job
}

type attemptBuilder struct {
	pr       github.TracePullRequest
	evidence []Evidence
}

var supersedeTitlePattern = regexp.MustCompile(`(?i)^\s*SUPERSEDE\s+PR\s+#([0-9]+)\b`)
var trailingIssueTitlePattern = regexp.MustCompile(`\(#([0-9]+)\)\s*$`)

func (r *Resolver) TraceIssue(ctx context.Context, repo github.Repository, number int64) (IssueTrace, error) {
	if repo.FullName() == "" || number <= 0 {
		return IssueTrace{}, fmt.Errorf("repository and positive issue number are required")
	}
	key := strings.ToLower(repo.FullName()) + "#" + strconv.FormatInt(number, 10)
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	ttl := r.TTL
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}

	r.mu.Lock()
	if entry, ok := r.cache[key]; ok && now().Before(entry.expiresAt) {
		r.mu.Unlock()
		return entry.trace, nil
	}
	if active, ok := r.flights[key]; ok {
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return IssueTrace{}, ctx.Err()
		case <-active.done:
			return active.trace, active.err
		}
	}
	if r.cache == nil {
		r.cache = map[string]cacheEntry{}
	}
	if r.flights == nil {
		r.flights = map[string]*flight{}
	}
	active := &flight{done: make(chan struct{})}
	r.flights[key] = active
	r.mu.Unlock()

	trace, err := r.resolveIssue(ctx, repo, number)

	r.mu.Lock()
	active.trace, active.err = trace, err
	if err == nil {
		r.cache[key] = cacheEntry{trace: trace, expiresAt: now().Add(ttl)}
	}
	delete(r.flights, key)
	close(active.done)
	r.mu.Unlock()
	return trace, err
}

func (r *Resolver) TracePullRequest(ctx context.Context, repo github.Repository, number int64) (IssueTrace, error) {
	if repo.FullName() == "" || number <= 0 {
		return IssueTrace{}, fmt.Errorf("repository and positive pull request number are required")
	}
	snapshot, err := r.loadLocal(ctx, repo.FullName())
	if err != nil {
		return IssueTrace{}, err
	}
	trace := newTrace("pr", repo, 0)
	if r.Remote == nil {
		r.markRemoteError(&trace, fmt.Errorf("GitHub trace reader is unavailable"))
		r.addLocalPRFallback(&trace, snapshot, number)
		return trace, nil
	}
	pr, err := r.Remote.GetTracePullRequest(ctx, repo, number)
	if err != nil {
		r.markRemoteError(&trace, err)
		r.addLocalPRFallback(&trace, snapshot, number)
		return trace, nil
	}
	titleIssues := titleIssueNumbers(pr.Title)
	declaredIssues := declaredIssueNumbers(pr.Body, repo.FullName())
	timelineIssues := map[int64]struct{}{}
	if refs, refErr := r.Remote.ListTraceCrossReferences(ctx, repo, number); refErr != nil {
		r.markRemoteError(&trace, refErr)
	} else {
		for _, ref := range refs {
			if ref.IsPullRequest() || !sameRepositoryURL(ref.Source.Issue.RepositoryURL, repo.FullName()) {
				continue
			}
			timelineIssues[ref.Source.Issue.Number] = struct{}{}
		}
	}
	issues := titleIssues
	basis := "trailing title reference"
	if len(issues) == 0 {
		issues = declaredIssues
		basis = "closing keyword"
	}
	if len(issues) == 0 {
		issues = timelineIssues
		basis = "timeline cross-reference"
	}
	numbers := sortedNumbers(issues)
	if len(numbers) == 0 {
		trace.Attempts = []PullRequestAttempt{attemptFromGitHub(pr, []Evidence{{
			Kind: "github_pr", Basis: "pull_request_state", Certainty: CertaintyObserved,
			Detail: fmt.Sprintf("GitHub reports PR #%d as %s", pr.Number, pr.State), URL: pr.URL,
		}})}
		r.joinLocal(&trace, snapshot)
		trace.Warnings = append(trace.Warnings, Warning{
			Code: "issue_unknown", Detail: "no same-repository issue reference was found for this pull request",
		})
		return trace, nil
	}
	resolved, err := r.TraceIssue(ctx, repo, numbers[0])
	if err != nil {
		return IssueTrace{}, err
	}
	resolved.Selector = "pr"
	if len(numbers) > 1 {
		resolved.Warnings = append(resolved.Warnings, Warning{
			Code: "ambiguous_issue", Detail: fmt.Sprintf("PR #%d has multiple %s issue references %v; trace is rooted at #%d", number, basis, numbers, numbers[0]),
		})
	}
	return resolved, nil
}

func (r *Resolver) resolveIssue(ctx context.Context, repo github.Repository, number int64) (IssueTrace, error) {
	trace := newTrace("issue", repo, number)
	snapshot, err := r.loadLocal(ctx, repo.FullName())
	if err != nil {
		return IssueTrace{}, err
	}
	if r.Remote == nil {
		r.markRemoteError(&trace, fmt.Errorf("GitHub trace reader is unavailable"))
		addUnlinkedLocal(&trace, snapshot)
		return trace, nil
	}

	issue, err := r.Remote.GetTraceIssue(ctx, repo, number)
	if err != nil {
		r.markRemoteError(&trace, err)
		addUnlinkedLocal(&trace, snapshot)
		return trace, nil
	}
	trace.Issue = Issue{
		Number: issue.Number, Title: issue.Title, State: issue.State, URL: issue.URL,
		Evidence: []Evidence{{
			Kind: "github_issue", Basis: "issue_state", Certainty: CertaintyObserved,
			Detail: fmt.Sprintf("GitHub reports issue #%d as %s", issue.Number, issue.State), URL: issue.URL,
		}},
	}

	builders := map[int64]*attemptBuilder{}
	add := func(pr github.TracePullRequest, evidence Evidence) {
		builder := builders[pr.Number]
		if builder == nil {
			builder = &attemptBuilder{pr: pr}
			builders[pr.Number] = builder
		} else {
			mergeTracePR(&builder.pr, pr)
		}
		builder.evidence = appendEvidence(builder.evidence, evidence)
	}

	if refs, refErr := r.Remote.ListTraceCrossReferences(ctx, repo, number); refErr != nil {
		r.markRemoteError(&trace, refErr)
	} else {
		for _, ref := range refs {
			if !ref.IsPullRequest() || !sameRepositoryURL(ref.Source.Issue.RepositoryURL, repo.FullName()) {
				continue
			}
			source := ref.Source.Issue
			add(github.TracePullRequest{
				Number: source.Number, Title: source.Title, Body: source.Body, URL: source.URL,
			}, Evidence{
				Kind: "issue_pr_link", Basis: "github_timeline_cross_reference", Certainty: CertaintyObserved,
				Detail: fmt.Sprintf("issue #%d timeline cross-references PR #%d", number, source.Number), URL: source.URL,
			})
		}
	}
	if candidates, searchErr := r.Remote.SearchTracePullRequests(ctx, repo, fmt.Sprintf("%q", "#"+strconv.FormatInt(number, 10))); searchErr != nil {
		r.markRemoteError(&trace, searchErr)
	} else {
		for _, pr := range candidates {
			if _, ok := declaredIssueNumbers(pr.Body, repo.FullName())[number]; !ok {
				continue
			}
			add(pr, Evidence{
				Kind: "issue_pr_link", Basis: "closing_keyword", Certainty: CertaintyDeclared,
				Detail: fmt.Sprintf("PR #%d declares that it closes issue #%d", pr.Number, number), URL: pr.URL,
			})
		}
	}

	queue := sortedBuilderNumbers(builders)
	expanded := map[int64]bool{}
	for len(queue) > 0 {
		prNumber := queue[0]
		queue = queue[1:]
		if expanded[prNumber] {
			continue
		}
		expanded[prNumber] = true
		builder := builders[prNumber]
		if detail, detailErr := r.Remote.GetTracePullRequest(ctx, repo, prNumber); detailErr != nil {
			r.markRemoteError(&trace, detailErr)
		} else {
			mergeTracePR(&builder.pr, detail)
			builder.evidence = appendEvidence(builder.evidence, Evidence{
				Kind: "github_pr", Basis: "pull_request_state", Certainty: CertaintyObserved,
				Detail: fmt.Sprintf("GitHub reports PR #%d as %s", detail.Number, detail.State), URL: detail.URL,
			})
			if _, ok := declaredIssueNumbers(detail.Body, repo.FullName())[number]; ok {
				builder.evidence = appendEvidence(builder.evidence, Evidence{
					Kind: "issue_pr_link", Basis: "closing_keyword", Certainty: CertaintyDeclared,
					Detail: fmt.Sprintf("PR #%d declares that it closes issue #%d", detail.Number, number), URL: detail.URL,
				})
			}
		}

		successors, successorErr := r.successorCandidates(ctx, repo, builder.pr)
		if successorErr != nil {
			r.markRemoteError(&trace, successorErr)
		}
		for _, successor := range successors {
			add(successor, Evidence{
				Kind: "successor", Basis: "supersede_title", Certainty: CertaintyDeclared,
				Detail: fmt.Sprintf("PR #%d title declares it supersedes PR #%d", successor.Number, prNumber), URL: successor.URL,
			})
			trace.Successors = appendSuccessor(trace.Successors, SuccessorEdge{
				From: prNumber, To: successor.Number, Basis: "supersede_title", Certainty: CertaintyDeclared,
			})
			if !expanded[successor.Number] {
				queue = append(queue, successor.Number)
			}
		}
		sort.Slice(queue, func(i, j int) bool { return queue[i] < queue[j] })
	}

	r.addGitHubAttempts(ctx, &trace, repo, builders)
	r.joinLocal(&trace, snapshot)
	addGraphWarnings(&trace)
	return trace, nil
}

func (r *Resolver) successorCandidates(ctx context.Context, repo github.Repository, predecessor github.TracePullRequest) ([]github.TracePullRequest, error) {
	found := map[int64]github.TracePullRequest{}
	var firstErr error
	if refs, err := r.Remote.ListTraceCrossReferences(ctx, repo, predecessor.Number); err != nil {
		firstErr = err
	} else {
		for _, ref := range refs {
			source := ref.Source.Issue
			if !ref.IsPullRequest() || !sameRepositoryURL(source.RepositoryURL, repo.FullName()) {
				continue
			}
			if supersededPR(source.Title) != predecessor.Number {
				continue
			}
			found[source.Number] = github.TracePullRequest{
				Number: source.Number, Title: source.Title, Body: source.Body, URL: source.URL,
			}
		}
	}
	candidates, err := r.Remote.SearchTracePullRequests(ctx, repo, fmt.Sprintf("%q", "SUPERSEDE PR #"+strconv.FormatInt(predecessor.Number, 10)))
	if err != nil && firstErr == nil {
		firstErr = err
	}
	for _, candidate := range candidates {
		if supersededPR(candidate.Title) == predecessor.Number {
			found[candidate.Number] = candidate
		}
	}
	out := make([]github.TracePullRequest, 0, len(found))
	for _, pr := range found {
		out = append(out, pr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].Number < out[j].Number
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out, firstErr
}

func (r *Resolver) addGitHubAttempts(ctx context.Context, trace *IssueTrace, repo github.Repository, builders map[int64]*attemptBuilder) {
	defaultBranch := ""
	if metadata, err := r.Remote.GetTraceRepository(ctx, repo); err != nil {
		r.markRemoteError(trace, err)
	} else {
		defaultBranch = strings.TrimSpace(metadata.DefaultBranch)
	}
	for _, number := range sortedBuilderNumbers(builders) {
		builder := builders[number]
		attempt := attemptFromGitHub(builder.pr, builder.evidence)
		if attempt.MergeSHA != "" {
			attempt.Evidence = appendEvidence(attempt.Evidence, Evidence{
				Kind: "merge", Basis: "github_merge_sha", Certainty: CertaintyObserved,
				Detail: fmt.Sprintf("GitHub records PR #%d merged as commit %s", number, attempt.MergeSHA), URL: attempt.URL,
			})
			if defaultBranch != "" {
				comparison, err := r.Remote.CompareTraceCommit(ctx, repo, attempt.MergeSHA, defaultBranch)
				if err != nil {
					r.markRemoteError(trace, err)
				} else {
					onDefault := comparison.Status == "ahead" || comparison.Status == "identical"
					attempt.OnDefault = &onDefault
					attempt.Evidence = appendEvidence(attempt.Evidence, Evidence{
						Kind: "default_branch_ancestry", Basis: "github_compare", Certainty: CertaintyObserved,
						Detail: fmt.Sprintf("commit %s ancestor of %s at observation time: %t", attempt.MergeSHA, defaultBranch, onDefault),
					})
				}
			}
		}
		trace.Attempts = append(trace.Attempts, attempt)
	}
	sort.Slice(trace.Attempts, func(i, j int) bool {
		if trace.Attempts[i].CreatedAt == trace.Attempts[j].CreatedAt {
			return trace.Attempts[i].Number < trace.Attempts[j].Number
		}
		return trace.Attempts[i].CreatedAt < trace.Attempts[j].CreatedAt
	})
}

func (r *Resolver) loadLocal(ctx context.Context, repo string) (localSnapshot, error) {
	if r.Local == nil {
		return localSnapshot{}, nil
	}
	tasks, err := r.Local.ListTasksByRepo(ctx, repo)
	if err != nil {
		return localSnapshot{}, fmt.Errorf("list local tasks: %w", err)
	}
	prs, err := r.Local.ListPullRequests(ctx, repo)
	if err != nil {
		return localSnapshot{}, fmt.Errorf("list local pull requests: %w", err)
	}
	jobs, err := r.Local.ListJobs(ctx)
	if err != nil {
		return localSnapshot{}, fmt.Errorf("list local jobs: %w", err)
	}
	return localSnapshot{tasks: tasks, prs: prs, jobs: jobs}, nil
}

func (r *Resolver) joinLocal(trace *IssueTrace, snapshot localSnapshot) {
	attemptByNumber := map[int64]*PullRequestAttempt{}
	for i := range trace.Attempts {
		attemptByNumber[trace.Attempts[i].Number] = &trace.Attempts[i]
	}
	taskByID := map[string]*Task{}
	for _, localPR := range snapshot.prs {
		attempt := attemptByNumber[localPR.Number]
		if attempt == nil {
			continue
		}
		if attempt.HeadBranch == "" {
			attempt.HeadBranch = localPR.HeadBranch
		}
		if attempt.HeadSHA == "" {
			attempt.HeadSHA = localPR.HeadSHA
		}
		if attempt.MergeSHA == "" {
			attempt.MergeSHA = observedMergeSHA(localPR.State, "", localPR.MergeCommitSHA)
		}
		attempt.Evidence = appendEvidence(attempt.Evidence, Evidence{
			Kind: "local_pr", Basis: "local_pull_request_row", Certainty: CertaintyObserved,
			Detail: fmt.Sprintf("local PR row records #%d on branch %s in state %s", localPR.Number, localPR.HeadBranch, localPR.State),
			URL:    localPR.URL,
		})
		for _, localTask := range snapshot.tasks {
			if !strings.EqualFold(strings.TrimSpace(localTask.RepoFullName), trace.Repository) ||
				strings.TrimSpace(localTask.Branch) == "" || localTask.Branch != localPR.HeadBranch {
				continue
			}
			attempt.LinkedTaskIDs = appendUniqueString(attempt.LinkedTaskIDs, localTask.ID)
			if taskByID[localTask.ID] == nil {
				task := Task{
					ID: localTask.ID, Title: localTask.Title, State: localTask.State, Branch: localTask.Branch,
					Basis: Evidence{
						Kind: "task_pr_link", Basis: "exact_repo_head_branch", Certainty: CertaintyInferred,
						Detail: fmt.Sprintf("task %s and PR #%d share exact repo and head branch %s", localTask.ID, localPR.Number, localPR.HeadBranch),
					},
				}
				taskByID[localTask.ID] = &task
			}
		}
	}
	for _, job := range snapshot.jobs {
		var payload struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal([]byte(job.Payload), &payload) != nil {
			continue
		}
		task := taskByID[strings.TrimSpace(payload.TaskID)]
		if task == nil {
			continue
		}
		task.Jobs = append(task.Jobs, LocalJob{
			ID: job.ID, Type: job.Type, State: job.State, Agent: job.Agent, Created: job.CreatedAt,
			Basis: "local_job_row", Certainty: CertaintyObserved,
		})
	}
	for _, task := range taskByID {
		sort.Slice(task.Jobs, func(i, j int) bool {
			if task.Jobs[i].Created == task.Jobs[j].Created {
				return task.Jobs[i].ID < task.Jobs[j].ID
			}
			return task.Jobs[i].Created < task.Jobs[j].Created
		})
		trace.Tasks = append(trace.Tasks, *task)
	}
	sort.Slice(trace.Tasks, func(i, j int) bool { return trace.Tasks[i].ID < trace.Tasks[j].ID })
}

func (r *Resolver) addLocalPRFallback(trace *IssueTrace, snapshot localSnapshot, number int64) {
	for _, pr := range snapshot.prs {
		if pr.Number != number {
			continue
		}
		trace.Attempts = append(trace.Attempts, PullRequestAttempt{
			Number: pr.Number, State: pr.State, URL: pr.URL, HeadBranch: pr.HeadBranch,
			HeadSHA: pr.HeadSHA, MergeSHA: observedMergeSHA(pr.State, "", pr.MergeCommitSHA),
			Evidence: []Evidence{{
				Kind: "local_pr", Basis: "local_pull_request_row", Certainty: CertaintyObserved,
				Detail: fmt.Sprintf("local PR row records #%d on branch %s in state %s", pr.Number, pr.HeadBranch, pr.State), URL: pr.URL,
			}},
		})
		break
	}
	r.joinLocal(trace, snapshot)
}

func addUnlinkedLocal(trace *IssueTrace, snapshot localSnapshot) {
	for _, pr := range snapshot.prs {
		trace.UnlinkedLocal = append(trace.UnlinkedLocal, Evidence{
			Kind: "local_pr", Basis: "local_pull_request_row", Certainty: CertaintyObserved,
			Detail: fmt.Sprintf("unlinked local PR #%d branch=%s state=%s", pr.Number, pr.HeadBranch, pr.State),
			URL:    pr.URL,
		})
	}
	for _, task := range snapshot.tasks {
		if !strings.EqualFold(strings.TrimSpace(task.RepoFullName), trace.Repository) {
			continue
		}
		trace.UnlinkedLocal = append(trace.UnlinkedLocal, Evidence{
			Kind: "local_task", Basis: "local_task_row", Certainty: CertaintyObserved,
			Detail: fmt.Sprintf("unlinked local task %s branch=%s state=%s", task.ID, task.Branch, task.State),
		})
	}
	if len(trace.UnlinkedLocal) > 0 {
		trace.Warnings = append(trace.Warnings, Warning{
			Code:   "local_evidence_unlinked",
			Detail: "remote issue-to-PR evidence was unavailable; local rows are shown but are not attributed to this issue",
		})
	}
}

func (r *Resolver) markRemoteError(trace *IssueTrace, err error) {
	if err == nil {
		return
	}
	trace.RemoteUnavailable = true
	if trace.RefreshError == "" {
		trace.RefreshError = err.Error()
	}
}

func newTrace(selector string, repo github.Repository, issue int64) IssueTrace {
	return IssueTrace{
		SchemaVersion: SchemaVersion,
		Selector:      selector,
		Repository:    repo.FullName(),
		Issue:         Issue{Number: issue},
		Attempts:      []PullRequestAttempt{},
		Successors:    []SuccessorEdge{},
		Assessment:    "partial_evidence",
		Deployment: Deployment{
			Status: "unknown", Certainty: CertaintyUnknowable,
			Reason: "deployment is not derivable from issue, pull request, commit, or local workflow records",
		},
	}
}

func attemptFromGitHub(pr github.TracePullRequest, evidence []Evidence) PullRequestAttempt {
	return PullRequestAttempt{
		Number: pr.Number, Title: pr.Title, State: pr.State, URL: pr.URL,
		CreatedAt: pr.CreatedAt, HeadBranch: pr.Head.Ref, HeadSHA: pr.Head.SHA,
		MergedAt: pr.MergedAt, MergeSHA: observedMergeSHA(pr.State, pr.MergedAt, pr.MergeSHA), Evidence: evidence,
	}
}

func observedMergeSHA(state, mergedAt, mergeSHA string) string {
	// GitHub populates merge_commit_sha on open PRs with a synthetic test merge
	// commit. Remote and local evidence must both prove the PR merged before
	// surfacing that SHA.
	if !strings.EqualFold(strings.TrimSpace(state), "merged") && strings.TrimSpace(mergedAt) == "" {
		return ""
	}
	return strings.TrimSpace(mergeSHA)
}

func mergeTracePR(dst *github.TracePullRequest, src github.TracePullRequest) {
	if src.Number != 0 {
		dst.Number = src.Number
	}
	if src.Title != "" {
		dst.Title = src.Title
	}
	if src.State != "" {
		dst.State = src.State
	}
	if src.URL != "" {
		dst.URL = src.URL
	}
	if src.Body != "" {
		dst.Body = src.Body
	}
	if src.CreatedAt != "" {
		dst.CreatedAt = src.CreatedAt
	}
	if src.MergedAt != "" {
		dst.MergedAt = src.MergedAt
	}
	if src.MergeSHA != "" {
		dst.MergeSHA = src.MergeSHA
	}
	if src.Head.Ref != "" {
		dst.Head.Ref = src.Head.Ref
	}
	if src.Head.SHA != "" {
		dst.Head.SHA = src.Head.SHA
	}
	if src.Base.Ref != "" {
		dst.Base.Ref = src.Base.Ref
	}
}

func declaredIssueNumbers(body, repo string) map[int64]struct{} {
	pattern := regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+(?:` +
		regexp.QuoteMeta(repo) + `)?#([0-9]+)\b`)
	matches := pattern.FindAllStringSubmatch(body, -1)
	out := make(map[int64]struct{}, len(matches))
	for _, match := range matches {
		number, err := strconv.ParseInt(match[1], 10, 64)
		if err == nil && number > 0 {
			out[number] = struct{}{}
		}
	}
	return out
}

func supersededPR(title string) int64 {
	match := supersedeTitlePattern.FindStringSubmatch(title)
	if len(match) != 2 {
		return 0
	}
	number, _ := strconv.ParseInt(match[1], 10, 64)
	return number
}

func titleIssueNumbers(title string) map[int64]struct{} {
	match := trailingIssueTitlePattern.FindStringSubmatch(strings.TrimSpace(title))
	out := map[int64]struct{}{}
	if len(match) != 2 {
		return out
	}
	number, err := strconv.ParseInt(match[1], 10, 64)
	if err == nil && number > 0 {
		out[number] = struct{}{}
	}
	return out
}

func sameRepositoryURL(repositoryURL, repo string) bool {
	value := strings.TrimRight(strings.TrimSpace(repositoryURL), "/")
	return strings.EqualFold(value, "https://api.github.com/repos/"+strings.TrimSpace(repo))
}

func sortedNumbers(values map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(values))
	for number := range values {
		out = append(out, number)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedBuilderNumbers(values map[int64]*attemptBuilder) []int64 {
	out := make([]int64, 0, len(values))
	for number := range values {
		out = append(out, number)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := values[out[i]].pr, values[out[j]].pr
		if a.CreatedAt == b.CreatedAt {
			return a.Number < b.Number
		}
		return a.CreatedAt < b.CreatedAt
	})
	return out
}

func appendEvidence(values []Evidence, candidate Evidence) []Evidence {
	for _, value := range values {
		if value.Kind == candidate.Kind && value.Basis == candidate.Basis && value.Detail == candidate.Detail {
			return values
		}
	}
	return append(values, candidate)
}

func appendSuccessor(values []SuccessorEdge, candidate SuccessorEdge) []SuccessorEdge {
	for _, value := range values {
		if value.From == candidate.From && value.To == candidate.To {
			return values
		}
	}
	return append(values, candidate)
}

func appendUniqueString(values []string, candidate string) []string {
	for _, value := range values {
		if value == candidate {
			return values
		}
	}
	return append(values, candidate)
}

func addGraphWarnings(trace *IssueTrace) {
	outgoing := map[int64][]int64{}
	for _, edge := range trace.Successors {
		outgoing[edge.From] = append(outgoing[edge.From], edge.To)
	}
	for from, targets := range outgoing {
		if len(targets) > 1 {
			sort.Slice(targets, func(i, j int) bool { return targets[i] < targets[j] })
			trace.Warnings = append(trace.Warnings, Warning{
				Code: "successor_fork", Detail: fmt.Sprintf("PR #%d has multiple declared successors %v", from, targets),
			})
		}
	}
	seen, stack := map[int64]bool{}, map[int64]bool{}
	var visit func(int64) bool
	visit = func(node int64) bool {
		if stack[node] {
			return true
		}
		if seen[node] {
			return false
		}
		seen[node], stack[node] = true, true
		for _, next := range outgoing[node] {
			if visit(next) {
				return true
			}
		}
		stack[node] = false
		return false
	}
	for node := range outgoing {
		if visit(node) {
			trace.Warnings = append(trace.Warnings, Warning{
				Code: "successor_cycle", Detail: "the declared PR successor graph contains a cycle",
			})
			break
		}
	}
	sort.Slice(trace.Successors, func(i, j int) bool {
		if trace.Successors[i].From == trace.Successors[j].From {
			return trace.Successors[i].To < trace.Successors[j].To
		}
		return trace.Successors[i].From < trace.Successors[j].From
	})
	sort.Slice(trace.Warnings, func(i, j int) bool {
		if trace.Warnings[i].Code == trace.Warnings[j].Code {
			return trace.Warnings[i].Detail < trace.Warnings[j].Detail
		}
		return trace.Warnings[i].Code < trace.Warnings[j].Code
	})
}
