package daemon

import (
	"context"
	"database/sql"
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

	pulls, err := d.GitHub.ListPullRequests(ctx, d.Repo, "open")
	if err != nil {
		return err
	}
	for _, pull := range pulls {
		if err := d.recordPullRequest(ctx, pull); err != nil {
			return err
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
	return nil
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

func (d Daemon) recordPullRequest(ctx context.Context, pull github.PullRequest) error {
	return d.Store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: d.Repo.FullName(),
		Number:       pull.Number,
		URL:          pull.URL,
		HeadBranch:   pull.HeadRef,
		BaseBranch:   pull.BaseRef,
		State:        pull.State,
	})
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

	job, created, err := d.enqueueJob(ctx, workflow.JobRequest{
		ID:           jobID(d.Repo, pull.Number, comment.ID, sequence, command.Agent, command.Action),
		Agent:        agent.Name,
		Action:       command.Action,
		Repo:         d.Repo.FullName(),
		Branch:       pull.HeadRef,
		PullRequest:  int(pull.Number),
		TaskID:       fmt.Sprintf("pr-%d-comment-%d", pull.Number, comment.ID),
		TaskTitle:    pull.Title,
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
