package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

type localAgentDispatchRequest struct {
	RepoFlag     string
	Agent        string
	Action       string
	Instructions string
}

type localAgentJobOutput struct {
	JobID          string                `json:"job_id"`
	State          string                `json:"state"`
	Repo           string                `json:"repo"`
	Agent          string                `json:"agent"`
	Action         string                `json:"action"`
	Result         *workflow.AgentResult `json:"result,omitempty"`
	RawOutputCount int                   `json:"raw_output_count"`
}

func dispatchLocalAgentJob(ctx context.Context, store *db.Store, request localAgentDispatchRequest) (localAgentJobOutput, error) {
	repo, record, err := resolveLocalAgentRepo(ctx, store, request.RepoFlag)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	if err := store.UpsertRepo(ctx, record); err != nil {
		return localAgentJobOutput{}, err
	}
	agent, err := store.GetAgent(ctx, request.Agent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return localAgentJobOutput{}, fmt.Errorf("agent %q not found", request.Agent)
		}
		return localAgentJobOutput{}, err
	}
	if err := ensureLocalAgentAccess(ctx, store, agent, repo.FullName(), request.Action); err != nil {
		return localAgentJobOutput{}, err
	}
	adapter, err := runtimeStartAdapter(newRuntimeFactory(), agent.Runtime, record.CheckoutPath)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	job, err := (workflow.Mailbox{Store: store}).Enqueue(ctx, workflow.JobRequest{
		ID:           localAgentJobID(request.Action, agent.Name),
		Agent:        agent.Name,
		Action:       request.Action,
		Repo:         repo.FullName(),
		Branch:       record.DefaultBranch,
		Sender:       "local",
		Instructions: request.Instructions,
	})
	if err != nil {
		return localAgentJobOutput{}, err
	}
	if _, err := (workflow.Mailbox{Store: store}).Run(ctx, job.ID, runtimeAgent(agent), adapter); err != nil {
		return localAgentJobOutput{}, err
	}
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_completed", Message: "workflow advancement completed"}); err != nil {
		return localAgentJobOutput{}, err
	}
	latest, err := store.GetJob(ctx, job.ID)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	payload, err := daemonJobPayload(latest)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	return localAgentJobOutput{
		JobID:          latest.ID,
		State:          latest.State,
		Repo:           payload.Repo,
		Agent:          latest.Agent,
		Action:         latest.Type,
		Result:         payload.Result,
		RawOutputCount: len(payload.RawOutputs),
	}, nil
}

func resolveLocalAgentRepo(ctx context.Context, store *db.Store, repoFlag string) (github.Repository, db.Repo, error) {
	repo, err := localAgentTargetRepo(ctx, repoFlag)
	if err != nil {
		return github.Repository{}, db.Repo{}, err
	}
	if strings.TrimSpace(repoFlag) == "" {
		record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: "."})
		if err != nil {
			return github.Repository{}, db.Repo{}, err
		}
		return repo, record, nil
	}
	if existing, err := store.GetRepo(ctx, repo.FullName()); err == nil && strings.TrimSpace(existing.CheckoutPath) != "" {
		record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: existing.CheckoutPath})
		if err != nil {
			return github.Repository{}, db.Repo{}, err
		}
		record.PollInterval = existing.PollInterval
		return repo, record, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return github.Repository{}, db.Repo{}, err
	}
	record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: "."})
	if err != nil {
		return github.Repository{}, db.Repo{}, err
	}
	return repo, record, nil
}

func localAgentTargetRepo(ctx context.Context, repoFlag string) (github.Repository, error) {
	if strings.TrimSpace(repoFlag) != "" {
		return daemon.ParseRepository(repoFlag)
	}
	remote, err := (gitutil.Client{Dir: "."}).OriginRemote(ctx)
	if err != nil {
		return github.Repository{}, fmt.Errorf("infer repo from current checkout: %w", err)
	}
	parsed, err := gitutil.ParseGitHubRemote(remote)
	if err != nil {
		return github.Repository{}, err
	}
	return github.Repository{Owner: parsed.Owner, Name: parsed.Name}, nil
}

func ensureLocalAgentAccess(ctx context.Context, store *db.Store, agent db.Agent, repo string, action string) error {
	allowed, err := store.AgentCanAccessRepo(ctx, agent.Name, repo)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("agent %q is not allowed on %q", agent.Name, repo)
	}
	if !agentHasCapability(agent.Capabilities, action) {
		return fmt.Errorf("agent %q lacks %s capability", agent.Name, action)
	}
	return nil
}

func localAgentJobID(action string, agent string) string {
	return fmt.Sprintf("local-%s-%s-%x", action, agent, time.Now().UTC().UnixNano())
}

func printLocalAgentJobOutput(stdout io.Writer, output localAgentJobOutput) {
	writeLine(stdout, "job: %s", output.JobID)
	writeLine(stdout, "state: %s", output.State)
	writeLine(stdout, "repo: %s", output.Repo)
	writeLine(stdout, "agent: %s", output.Agent)
	writeLine(stdout, "action: %s", output.Action)
	if output.Result == nil {
		return
	}
	writeLine(stdout, "decision: %s", output.Result.Decision)
	writeLine(stdout, "summary: %s", output.Result.Summary)
	printRawMessages(stdout, "findings", output.Result.Findings)
	printStringList(stdout, "needs", output.Result.Needs)
	printStringList(stdout, "tests_run", output.Result.TestsRun)
	printStringList(stdout, "next_agents", output.Result.NextAgents)
}

func printRawMessages(stdout io.Writer, label string, values []json.RawMessage) {
	if len(values) == 0 {
		return
	}
	writeLine(stdout, "%s:", label)
	for _, value := range values {
		writeLine(stdout, "- %s", strings.TrimSpace(string(value)))
	}
}

func printStringList(stdout io.Writer, label string, values []string) {
	if len(values) == 0 {
		return
	}
	writeLine(stdout, "%s:", label)
	for _, value := range values {
		writeLine(stdout, "- %s", value)
	}
}
