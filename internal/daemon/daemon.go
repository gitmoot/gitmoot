package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

const defaultPollInterval = 30 * time.Second

type Daemon struct {
	Repo         github.Repository
	PollInterval time.Duration
	Store        *db.Store
	GitHub       github.Client
	Workflow     *workflow.Engine
	Sleep        func(context.Context, time.Duration) error
}

func (d Daemon) Run(ctx context.Context) error {
	interval := d.PollInterval
	if interval == 0 {
		interval = defaultPollInterval
	}
	if interval < 0 {
		return fmt.Errorf("poll interval must be positive")
	}
	if err := d.validate(); err != nil {
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_ = d.PollOnce(ctx)
		if err := d.sleep(ctx, interval); err != nil {
			return err
		}
	}
}

func (d Daemon) PollOnce(ctx context.Context) error {
	if err := d.validate(); err != nil {
		return err
	}

	var firstErr error
	pulls, err := d.GitHub.ListPullRequests(ctx, d.Repo, "open")
	if err != nil {
		return err
	}
	openBranches := map[string]struct{}{}
	for _, pull := range pulls {
		openBranches[pull.HeadRef] = struct{}{}
		changed, err := d.pullRequestChanged(ctx, pull)
		if err != nil {
			return err
		}
		if changed {
			if err := d.handlePullRequestWorkflow(ctx, pull); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				changed = false
			} else {
				merged, err := d.pullRequestStoredMerged(ctx, pull)
				if err != nil {
					return err
				}
				if merged {
					changed = false
				}
			}
		}
		if changed {
			if err := d.recordPullRequest(ctx, pull); err != nil {
				return err
			}
		} else {
			retry, err := d.pullRequestReadyToMerge(ctx, pull)
			if err != nil {
				return err
			}
			if retry {
				if err := d.handleReadyToMergeWorkflow(ctx, pull); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
		comments, err := d.GitHub.ListIssueComments(ctx, d.Repo, pull.Number)
		if err != nil {
			return err
		}
		for _, comment := range comments {
			if err := d.handleComment(ctx, pull, comment); err != nil {
				return err
			}
		}
	}
	if err := d.retryClosedReadyToMerge(ctx, openBranches); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (d Daemon) validate() error {
	if d.Store == nil {
		return errors.New("daemon store is required")
	}
	if d.GitHub == nil {
		return errors.New("daemon github client is required")
	}
	if d.Repo.FullName() == "" {
		return errors.New("daemon repo is required")
	}
	return nil
}

func (d Daemon) pullRequestChanged(ctx context.Context, pull github.PullRequest) (bool, error) {
	previous, err := d.Store.GetPullRequest(ctx, d.Repo.FullName(), pull.Number)
	switch {
	case err == nil:
		if previous.HeadSHA != pull.HeadSHA {
			return true, nil
		}
		routing, err := d.pullRequestWorkflowRouting(ctx, pull)
		if err != nil {
			return false, err
		}
		return routing.stale, nil
	case errors.Is(err, sql.ErrNoRows):
		return true, nil
	default:
		return false, err
	}
}

type pullRequestRouting struct {
	stale bool
}

func (d Daemon) pullRequestWorkflowRouting(ctx context.Context, pull github.PullRequest) (pullRequestRouting, error) {
	jobs, err := d.Store.ListJobs(ctx)
	if err != nil {
		return pullRequestRouting{}, err
	}
	routing := pullRequestRouting{}
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		var payload workflow.JobPayload
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			return pullRequestRouting{}, fmt.Errorf("parse job payload %q: %w", job.ID, err)
		}
		if payload.Repo == d.Repo.FullName() &&
			payload.PullRequest == int(pull.Number) &&
			payload.Branch == pull.HeadRef &&
			strings.TrimSpace(payload.LeadAgent) != "" &&
			strings.TrimSpace(payload.ReviewRound) != "" &&
			len(payload.Reviewers) > 0 {
			if strings.TrimSpace(payload.HeadSHA) == pull.HeadSHA {
				return pullRequestRouting{}, nil
			}
			routing.stale = true
		}
	}
	return routing, nil
}

func (d Daemon) recordPullRequest(ctx context.Context, pull github.PullRequest) error {
	return d.Store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: d.Repo.FullName(),
		Number:       pull.Number,
		URL:          pull.URL,
		HeadBranch:   pull.HeadRef,
		BaseBranch:   pull.BaseRef,
		HeadSHA:      pull.HeadSHA,
		State:        pull.State,
	})
}

func (d Daemon) pullRequestStoredMerged(ctx context.Context, pull github.PullRequest) (bool, error) {
	stored, err := d.Store.GetPullRequest(ctx, d.Repo.FullName(), pull.Number)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(stored.State) == "merged", nil
}

func (d Daemon) handlePullRequestWorkflow(ctx context.Context, pull github.PullRequest) error {
	if d.Workflow == nil {
		return nil
	}
	lock, err := d.Store.GetBranchLock(ctx, d.Repo.FullName(), pull.HeadRef)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	ref := workflowTaskRef{
		id:     pull.HeadRef,
		title:  pull.Title,
		branch: pull.HeadRef,
	}
	if task, err := d.lookupPullRequestTask(ctx, d.Repo.FullName(), pull.HeadRef); err == nil {
		ref.id = task.ID
		ref.goalID = task.GoalID
		ref.title = task.Title
		if task.Branch != "" {
			ref.branch = task.Branch
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	reviewers, err := d.workflowReviewers(ctx)
	if err != nil {
		return err
	}
	return d.Workflow.HandlePullRequestOpened(ctx, workflow.PullRequestEvent{
		Repo:              d.Repo.FullName(),
		Branch:            ref.branch,
		PullRequest:       int(pull.Number),
		HeadSHA:           pull.HeadSHA,
		GoalID:            ref.goalID,
		TaskID:            ref.id,
		TaskTitle:         ref.title,
		LeadAgent:         lock.Owner,
		Sender:            "github",
		RequiredReviewers: reviewers,
	})
}

func (d Daemon) pullRequestReadyToMerge(ctx context.Context, pull github.PullRequest) (bool, error) {
	task, err := d.lookupPullRequestTask(ctx, d.Repo.FullName(), pull.HeadRef)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return task.State == string(workflow.TaskReadyToMerge), nil
}

func (d Daemon) handleReadyToMergeWorkflow(ctx context.Context, pull github.PullRequest) error {
	if d.Workflow == nil {
		return nil
	}
	task, err := d.lookupPullRequestTask(ctx, d.Repo.FullName(), pull.HeadRef)
	if err != nil {
		return err
	}
	lock, err := d.Store.GetBranchLock(ctx, d.Repo.FullName(), pull.HeadRef)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	leadAgent := strings.TrimSpace(lock.Owner)
	if leadAgent == "" {
		leadAgent = "github"
	}
	branch := task.Branch
	if branch == "" {
		branch = pull.HeadRef
	}
	return d.Workflow.HandlePullRequestReadyToMerge(ctx, workflow.PullRequestEvent{
		Repo:        d.Repo.FullName(),
		Branch:      branch,
		PullRequest: int(pull.Number),
		HeadSHA:     pull.HeadSHA,
		GoalID:      task.GoalID,
		TaskID:      task.ID,
		TaskTitle:   task.Title,
		LeadAgent:   leadAgent,
		Sender:      "github",
	})
}

func (d Daemon) retryClosedReadyToMerge(ctx context.Context, openBranches map[string]struct{}) error {
	tasks, err := d.Store.ListTasksByRepoState(ctx, d.Repo.FullName(), string(workflow.TaskReadyToMerge))
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}
	type readyPullRequest struct {
		number  int64
		headSHA string
	}
	readyBranches := map[string]readyPullRequest{}
	for _, task := range tasks {
		if task.Branch == "" {
			continue
		}
		if _, open := openBranches[task.Branch]; open {
			continue
		}
		stored, err := d.Store.GetPullRequestByRepoBranch(ctx, d.Repo.FullName(), task.Branch)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return err
		}
		readyBranches[task.Branch] = readyPullRequest{number: stored.Number, headSHA: stored.HeadSHA}
	}
	if len(readyBranches) == 0 {
		return nil
	}
	closed, err := d.GitHub.ListPullRequests(ctx, d.Repo, "closed")
	if err != nil {
		return err
	}
	for _, pull := range closed {
		ready, ok := readyBranches[pull.HeadRef]
		if !ok {
			continue
		}
		if pull.Number != ready.number {
			continue
		}
		if ready.headSHA != "" && pull.HeadSHA != ready.headSHA {
			continue
		}
		if err := d.handleReadyToMergeWorkflow(ctx, pull); err != nil {
			return err
		}
		delete(readyBranches, pull.HeadRef)
	}
	return nil
}

func (d Daemon) lookupPullRequestTask(ctx context.Context, repoFullName string, branch string) (db.Task, error) {
	task, err := d.Store.GetTaskByRepoBranch(ctx, repoFullName, branch)
	if err == nil {
		return task, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return db.Task{}, err
	}
	task, err = d.Store.GetTask(ctx, branch)
	if err != nil {
		return db.Task{}, err
	}
	if task.RepoFullName != "" && task.RepoFullName != repoFullName {
		return db.Task{}, sql.ErrNoRows
	}
	return task, nil
}

func (d Daemon) workflowReviewers(ctx context.Context) ([]string, error) {
	if d.Workflow != nil && len(d.Workflow.RequiredReviewers) > 0 {
		return append([]string{}, d.Workflow.RequiredReviewers...), nil
	}
	agents, err := d.Store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	reviewers := []string{}
	for _, agent := range agents {
		if agent.RepoScope == d.Repo.FullName() && hasCapability(agent.Capabilities, "review") {
			reviewers = append(reviewers, agent.Name)
		}
	}
	return reviewers, nil
}

func (d Daemon) handleComment(ctx context.Context, pull github.PullRequest, comment github.IssueComment) error {
	commands := ParseCommands(comment.Body)
	if len(commands) == 0 {
		return nil
	}

	seen, err := d.Store.HasCommentSeen(ctx, d.Repo.FullName(), comment.ID)
	if err != nil {
		return err
	}
	if seen {
		return nil
	}

	authorized, err := d.authorizeCommenter(ctx, comment.Author)
	if err != nil {
		return err
	}
	if !authorized {
		if err := d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot ignored comment %d from `%s`: `/gitmoot` commands require write, maintain, or admin repository permission.", comment.ID, comment.Author)); err != nil {
			return err
		}
		return d.markCommentSeen(ctx, pull, comment)
	}

	for sequence, command := range commands {
		if err := d.handleCommand(ctx, pull, comment, sequence, command); err != nil {
			return err
		}
	}
	return d.markCommentSeen(ctx, pull, comment)
}

func (d Daemon) handleCommand(ctx context.Context, pull github.PullRequest, comment github.IssueComment, sequence int, command Command) error {
	if err := command.Validate(); err != nil {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not route comment %d: %v.", comment.ID, err))
	}
	if command.Action == "status" || command.Action == "merge" {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot recognized `/gitmoot %s`, but that command is not implemented in this task yet.", command.Action))
	}

	agent, err := d.Store.GetAgent(ctx, command.Agent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot could not find subscribed agent `%s` for this repository.", command.Agent))
		}
		return err
	}
	if agent.RepoScope != d.Repo.FullName() {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot agent `%s` is subscribed for `%s`, not `%s`.", agent.Name, agent.RepoScope, d.Repo.FullName()))
	}
	if !hasCapability(agent.Capabilities, command.Action) {
		return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot agent `%s` does not advertise `%s` capability.", agent.Name, command.Action))
	}
	if command.Action == "implement" {
		allowed, err := d.agentOwnsBranchLock(ctx, agent.Name, pull.HeadRef)
		if err != nil {
			return err
		}
		if !allowed {
			return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot agent `%s` cannot implement on `%s` without holding the branch lock.", agent.Name, pull.HeadRef))
		}
	}

	ref, err := d.commentTaskRef(ctx, pull, comment)
	if err != nil {
		return err
	}
	job, created, err := d.enqueueJob(ctx, workflow.JobRequest{
		ID:           jobID(d.Repo, pull.Number, comment.ID, sequence, command.Agent, command.Action),
		Agent:        agent.Name,
		Action:       command.Action,
		Repo:         d.Repo.FullName(),
		Branch:       pull.HeadRef,
		PullRequest:  int(pull.Number),
		HeadSHA:      pull.HeadSHA,
		GoalID:       ref.goalID,
		TaskID:       ref.id,
		TaskTitle:    ref.title,
		Sender:       comment.Author,
		Instructions: command.Instructions,
		Constraints: []string{
			"Respond using the gitmoot_result JSON contract.",
			"Keep the work scoped to the pull request and requested action.",
		},
	})
	if err != nil {
		return err
	}

	if created {
		if err := d.Store.AddJobEvent(ctx, db.JobEvent{
			JobID:   job.ID,
			Kind:    "routed",
			Message: fmt.Sprintf("routed from PR #%d comment %d by %s", pull.Number, comment.ID, comment.Author),
		}); err != nil {
			return err
		}
	}
	return d.ack(ctx, pull.Number, fmt.Sprintf("Gitmoot queued `%s` job `%s` for `%s`.", command.Action, job.ID, agent.Name))
}

func (d Daemon) commentTaskRef(ctx context.Context, pull github.PullRequest, comment github.IssueComment) (workflowTaskRef, error) {
	ref := workflowTaskRef{
		id:     fmt.Sprintf("pr-%d-comment-%d", pull.Number, comment.ID),
		title:  pull.Title,
		branch: pull.HeadRef,
	}
	task, err := d.lookupPullRequestTask(ctx, d.Repo.FullName(), pull.HeadRef)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ref, nil
		}
		return workflowTaskRef{}, err
	}
	ref.id = task.ID
	ref.goalID = task.GoalID
	ref.title = task.Title
	if task.Branch != "" {
		ref.branch = task.Branch
	}
	return ref, nil
}

func (d Daemon) agentOwnsBranchLock(ctx context.Context, agentName string, branch string) (bool, error) {
	lock, err := d.Store.GetBranchLock(ctx, d.Repo.FullName(), branch)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return lock.Owner == agentName, nil
}

func (d Daemon) authorizeCommenter(ctx context.Context, author string) (bool, error) {
	if strings.TrimSpace(author) == "" {
		return false, nil
	}
	permission, err := d.GitHub.GetUserPermission(ctx, d.Repo, author)
	if err != nil {
		return false, err
	}
	return hasWritePermission(permission.Permission), nil
}

func hasWritePermission(permission string) bool {
	switch permission {
	case "admin", "maintain", "write":
		return true
	default:
		return false
	}
}

func (d Daemon) enqueueJob(ctx context.Context, request workflow.JobRequest) (db.Job, bool, error) {
	existing, err := d.Store.GetJob(ctx, request.ID)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return db.Job{}, false, err
	}
	job, err := (workflow.Mailbox{Store: d.Store}).Enqueue(ctx, request)
	return job, true, err
}

func (d Daemon) markCommentSeen(ctx context.Context, pull github.PullRequest, comment github.IssueComment) error {
	_, err := d.Store.MarkCommentSeenIfNew(ctx, db.Comment{
		RepoFullName: d.Repo.FullName(),
		CommentID:    comment.ID,
		PullRequest:  pull.Number,
		Body:         comment.Body,
	})
	return err
}

func (d Daemon) ack(ctx context.Context, issueNumber int64, body string) error {
	_, err := d.GitHub.PostIssueComment(ctx, d.Repo, issueNumber, body)
	return err
}

func (d Daemon) sleep(ctx context.Context, duration time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type workflowTaskRef struct {
	id     string
	goalID string
	title  string
	branch string
}

func hasCapability(capabilities []string, target string) bool {
	for _, capability := range capabilities {
		if capability == target {
			return true
		}
	}
	return false
}

func jobID(repo github.Repository, pullNumber, commentID int64, sequence int, agent, action string) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(repo.FullName()))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strconv.FormatInt(pullNumber, 10)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strconv.FormatInt(commentID, 10)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strconv.Itoa(sequence)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(agent))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(action))
	return "pr-comment-" + strconv.FormatUint(hash.Sum64(), 36)
}

func ParseRepository(value string) (github.Repository, error) {
	parts := strings.Split(strings.TrimSpace(value), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return github.Repository{}, fmt.Errorf("repo must be owner/repo")
	}
	return github.Repository{Owner: parts[0], Name: parts[1]}, nil
}
