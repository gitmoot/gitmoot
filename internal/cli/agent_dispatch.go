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

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

type localAgentDispatchRequest struct {
	RepoFlag     string
	Agent        string
	Action       string
	Instructions string
	Background   bool
	Type         string
	Home         string
}

type localAgentJobOutput struct {
	JobID          string                `json:"job_id"`
	State          string                `json:"state"`
	Repo           string                `json:"repo"`
	Agent          string                `json:"agent"`
	Action         string                `json:"action"`
	Result         *workflow.AgentResult `json:"result,omitempty"`
	RawOutputCount int                   `json:"raw_output_count"`
	WatchCommand   string                `json:"watch_command,omitempty"`
	DaemonRunning  bool                  `json:"daemon_running,omitempty"`
}

func dispatchLocalAgentJob(ctx context.Context, store *db.Store, request localAgentDispatchRequest) (localAgentJobOutput, error) {
	repo, record, err := resolveLocalAgentRepo(ctx, store, request.RepoFlag)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	if err := store.UpsertRepo(ctx, record); err != nil {
		return localAgentJobOutput{}, err
	}
	agent, releaseAgentReservation, err := resolveLocalDispatchAgent(ctx, store, request, repo.FullName(), record)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	defer func() {
		_ = releaseAgentReservation(context.Background())
	}()
	if err := ensureLocalAgentAccess(ctx, store, agent, repo.FullName(), request.Action); err != nil {
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
	if request.Background {
		return localAgentJobOutput{
			JobID:          job.ID,
			State:          job.State,
			Repo:           repo.FullName(),
			Agent:          job.Agent,
			Action:         job.Type,
			RawOutputCount: 0,
		}, nil
	}
	adapter, err := runtimeStartAdapter(newRuntimeFactory(), agent.Runtime, record.CheckoutPath)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	releaseLock, acquired, lockKey, err := acquireRuntimeSessionLock(ctx, store, job.ID, runtimeAgent(agent), time.Now().UTC(), daemonRunningJobStaleAfter)
	if err != nil {
		return localAgentJobOutput{}, err
	}
	if !acquired {
		message := fmt.Sprintf("runtime session %s is busy; synchronous ask was not run", lockKey)
		_, _ = store.TransitionJobStateWithEvent(ctx, job.ID, string(workflow.JobQueued), string(workflow.JobCancelled), db.JobEvent{
			JobID:   job.ID,
			Kind:    string(workflow.JobCancelled),
			Message: message,
		})
		_ = store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "runtime_lock_wait", Message: message})
		return localAgentJobOutput{}, fmt.Errorf("runtime session %s is busy; queued job %s was not run", lockKey, job.ID)
	}
	defer func() {
		_ = releaseLock(context.Background())
	}()
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

func resolveLocalDispatchAgent(ctx context.Context, store *db.Store, request localAgentDispatchRequest, repo string, record db.Repo) (db.Agent, func(context.Context) error, error) {
	forceType := strings.TrimSpace(request.Type)
	if forceType == "" {
		agent, err := store.GetAgent(ctx, request.Agent)
		if err == nil {
			return agent, noopAgentReservationRelease, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return db.Agent{}, noopAgentReservationRelease, err
		}
	}
	if !request.Background {
		return db.Agent{}, noopAgentReservationRelease, fmt.Errorf("agent %q not found", request.Agent)
	}
	typeName := firstNonEmpty(forceType, request.Agent)
	return ensureManagedAgentInstance(ctx, store, request.Home, typeName, repo, record)
}

func noopAgentReservationRelease(context.Context) error {
	return nil
}

func ensureManagedAgentInstance(ctx context.Context, store *db.Store, home string, typeName string, repo string, record db.Repo) (db.Agent, func(context.Context) error, error) {
	types, err := loadAgentTypeConfig(home)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	agentType, ok := types[typeName]
	if !ok {
		return db.Agent{}, noopAgentReservationRelease, fmt.Errorf("agent %q not found", typeName)
	}
	idleTimeout, err := time.ParseDuration(agentType.IdleTimeout)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, fmt.Errorf("agent type %s idle_timeout: %w", typeName, err)
	}
	now := time.Now().UTC()
	releaseTypeLock, acquiredTypeLock, typeLockKey, err := acquireManagedAgentTypeLock(ctx, store, typeName, now, daemonRunningJobStaleAfter)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	if !acquiredTypeLock {
		return db.Agent{}, noopAgentReservationRelease, fmt.Errorf("managed agent type %s is busy reserving %s", typeName, typeLockKey)
	}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			_ = releaseTypeLock(context.Background())
		}
	}()
	if instance, ok, err := store.FindReusableAgentInstance(ctx, typeName, repo, now); err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	} else if ok {
		if err := store.TouchAgentInstance(ctx, instance.Name, now, idleTimeout); err != nil {
			return db.Agent{}, noopAgentReservationRelease, err
		}
		agent, err := store.GetAgent(ctx, instance.Name)
		if err != nil {
			return db.Agent{}, noopAgentReservationRelease, err
		}
		releaseOnError = false
		return agent, releaseTypeLock, nil
	}
	count, err := store.CountActiveAgentInstances(ctx, typeName, now)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	if count >= agentType.MaxBackground {
		instance, ok, err := store.FindActiveAgentInstance(ctx, typeName, repo, now)
		if err != nil {
			return db.Agent{}, noopAgentReservationRelease, err
		}
		if ok {
			agent, err := store.GetAgent(ctx, instance.Name)
			if err != nil {
				return db.Agent{}, noopAgentReservationRelease, err
			}
			releaseOnError = false
			return agent, releaseTypeLock, nil
		}
		return db.Agent{}, noopAgentReservationRelease, fmt.Errorf("managed agent type %s reached max_background but no active instance is available", typeName)
	}
	instanceAgent := runtimeAgentFromType(agentType, repo, managedAgentInstanceName(typeName))
	var cachedPreset db.Preset
	if instanceAgent.PresetID != "" {
		var err error
		cachedPreset, err = store.GetPreset(ctx, instanceAgent.PresetID)
		if err != nil {
			return db.Agent{}, noopAgentReservationRelease, err
		}
	}
	adapter, err := runtimeStartAdapter(newRuntimeFactory(), instanceAgent.Runtime, record.CheckoutPath)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	started, err := adapter.Start(ctx, runtime.StartRequest{Agent: instanceAgent, Prompt: agentStartupPrompt(instanceAgent, cachedPreset)})
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	instanceAgent.RuntimeRef = strings.TrimSpace(started.RuntimeRef)
	if err := runtime.ValidateAgent(instanceAgent); err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	instance := db.AgentInstance{
		Name:         instanceAgent.Name,
		Type:         agentType.Name,
		Runtime:      instanceAgent.Runtime,
		RuntimeRef:   instanceAgent.RuntimeRef,
		RepoFullName: repo,
		Role:         instanceAgent.Role,
		PresetID:     instanceAgent.PresetID,
		Capabilities: instanceAgent.Capabilities,
		State:        "idle",
		CreatedAt:    formatManagedAgentTime(now),
		LastUsedAt:   formatManagedAgentTime(now),
		ExpiresAt:    formatManagedAgentTime(now.Add(idleTimeout)),
	}
	if err := store.UpsertAgentInstance(ctx, instance); err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	agent, err := store.GetAgent(ctx, instance.Name)
	if err != nil {
		return db.Agent{}, noopAgentReservationRelease, err
	}
	releaseOnError = false
	return agent, releaseTypeLock, nil
}

func acquireManagedAgentTypeLock(ctx context.Context, store *db.Store, typeName string, now time.Time, ttl time.Duration) (func(context.Context) error, bool, string, error) {
	if ttl <= 0 {
		return nil, false, "", fmt.Errorf("managed agent type lock ttl must be positive")
	}
	key := "agent-type:" + typeName
	ownerToken, err := newRuntimeLockOwnerToken()
	if err != nil {
		return nil, false, key, err
	}
	owner := "agent-type:" + typeName
	acquired, err := store.AcquireResourceLock(ctx, db.ResourceLock{
		ResourceKey: key,
		OwnerJobID:  owner,
		OwnerToken:  ownerToken,
		ExpiresAt:   now.UTC().Add(ttl).Format(time.RFC3339Nano),
	}, now)
	if err != nil || !acquired {
		return func(context.Context) error { return nil }, acquired, key, err
	}
	return func(releaseCtx context.Context) error {
		_, err := store.ReleaseResourceLock(releaseCtx, key, owner, ownerToken)
		return err
	}, true, key, nil
}

func runtimeAgentFromType(agentType config.AgentType, repo string, name string) runtime.Agent {
	return runtime.Agent{
		Name:           name,
		Role:           agentType.Role,
		Runtime:        agentType.Runtime,
		RepoScope:      repo,
		PresetID:       agentType.Preset,
		Capabilities:   agentType.Capabilities,
		AutonomyPolicy: "auto",
		HealthStatus:   "idle",
	}
}

func managedAgentInstanceName(typeName string) string {
	return fmt.Sprintf("%s-bg-%x", typeName, time.Now().UTC().UnixNano())
}

func formatManagedAgentTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
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
	if output.WatchCommand != "" {
		writeLine(stdout, "next: %s", output.WatchCommand)
	}
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
