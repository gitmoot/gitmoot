package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

type Engine struct {
	Store             *db.Store
	RequiredReviewers []string
	MergeGate         MergeGate
	JobID             func(JobRequest) string
}

type PullRequestEvent struct {
	Repo              string
	Branch            string
	PullRequest       int
	HeadSHA           string
	GoalID            string
	TaskID            string
	TaskTitle         string
	LeadAgent         string
	Sender            string
	RequiredReviewers []string
}

type MergeRequest struct {
	Repo        string
	Branch      string
	PullRequest int
	TaskID      string
	Reviewer    string
}

type MergeDecision struct {
	Ready  bool
	Reason string
}

type MergeGate interface {
	Evaluate(ctx context.Context, request MergeRequest) (MergeDecision, error)
}

type BlockedError struct {
	Reason string
}

func (e BlockedError) Error() string {
	return "workflow blocked: " + e.Reason
}

func (e Engine) HandlePullRequestOpened(ctx context.Context, event PullRequestEvent) error {
	if err := e.validate(); err != nil {
		return err
	}
	if err := validatePullRequestEvent(event); err != nil {
		return err
	}
	ref := taskRefFromPullRequest(event)
	if err := e.setTaskState(ctx, ref, TaskPullRequestOpen); err != nil {
		return err
	}
	if err := e.ensureAgentAllowed(ctx, JobRequest{
		Agent:  event.LeadAgent,
		Action: "implement",
		Repo:   event.Repo,
		Branch: event.Branch,
	}, ref); err != nil {
		return err
	}
	reviewers := compactStrings(append([]string{}, event.RequiredReviewers...))
	if len(reviewers) == 0 {
		reviewers = compactStrings(append([]string{}, e.RequiredReviewers...))
	}
	if len(reviewers) == 0 {
		if err := e.runMergeGate(ctx, "", JobPayload{
			Repo:        event.Repo,
			Branch:      event.Branch,
			PullRequest: event.PullRequest,
			GoalID:      event.GoalID,
			TaskID:      event.TaskID,
			TaskTitle:   event.TaskTitle,
			LeadAgent:   event.LeadAgent,
		}, ref); err != nil {
			return err
		}
		return e.recordPullRequestBaseline(ctx, event)
	}
	reviewRound, err := e.nextReviewRound(ctx, event)
	if err != nil {
		return err
	}
	requests := make([]JobRequest, 0, len(reviewers))
	for _, reviewer := range reviewers {
		request := JobRequest{
			Agent:       reviewer,
			Action:      "review",
			Repo:        event.Repo,
			Branch:      event.Branch,
			PullRequest: event.PullRequest,
			GoalID:      event.GoalID,
			TaskID:      event.TaskID,
			TaskTitle:   event.TaskTitle,
			LeadAgent:   event.LeadAgent,
			Reviewers:   reviewers,
			ReviewRound: reviewRound,
			Sender:      event.Sender,
			Instructions: fmt.Sprintf(
				"Review pull request #%d for task %s.",
				event.PullRequest,
				taskLabel(event.TaskID, event.TaskTitle),
			),
		}
		requests = append(requests, request)
	}
	for _, request := range requests {
		if err := e.ensureAgentAllowed(ctx, request, ref); err != nil {
			return err
		}
	}
	for _, request := range requests {
		if err := e.enqueue(ctx, request); err != nil {
			return err
		}
	}
	if err := e.setTaskState(ctx, ref, TaskReviewing); err != nil {
		return err
	}
	return e.recordPullRequestBaseline(ctx, event)
}

func (e Engine) RunJob(ctx context.Context, jobID string, agent runtime.Agent, adapter DeliveryAdapter) (AgentResult, error) {
	if err := e.validate(); err != nil {
		return AgentResult{}, err
	}
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return AgentResult{}, err
	}
	if err := e.ensureAgentAllowed(ctx, JobRequest{
		Agent:  job.Agent,
		Action: job.Type,
		Repo:   payload.Repo,
		Branch: payload.Branch,
	}, taskRefFromPayload(payload)); err != nil {
		return AgentResult{}, err
	}
	result, err := (Mailbox{Store: e.Store}).Run(ctx, jobID, agent, adapter)
	if err != nil {
		return result, err
	}
	if err := e.AdvanceJob(ctx, jobID); err != nil {
		return result, err
	}
	return result, nil
}

func (e Engine) AdvanceJob(ctx context.Context, jobID string) error {
	if err := e.validate(); err != nil {
		return err
	}
	job, payload, err := e.jobPayload(ctx, jobID)
	if err != nil {
		return err
	}
	if payload.Result == nil {
		return fmt.Errorf("job %q has no agent result", jobID)
	}
	ref := taskRefFromPayload(payload)

	if job.Type == "review" {
		latest, err := e.latestReviewRound(ctx, payload)
		if err != nil {
			return err
		}
		if latest != "" && strings.TrimSpace(payload.ReviewRound) != latest {
			return nil
		}
	}
	if payload.Result.Decision == "blocked" || payload.Result.Decision == "failed" {
		return e.block(ctx, ref, payload.Result.Summary)
	}
	if err := e.dispatchNextAgents(ctx, payload, *payload.Result, ref); err != nil {
		return err
	}

	switch job.Type {
	case "implement":
		if payload.Result.Decision != "implemented" {
			return nil
		}
		leadAgent := strings.TrimSpace(payload.LeadAgent)
		if leadAgent == "" {
			leadAgent = job.Agent
		}
		event := PullRequestEvent{
			Repo:              payload.Repo,
			Branch:            payload.Branch,
			PullRequest:       payload.PullRequest,
			GoalID:            payload.GoalID,
			TaskID:            payload.TaskID,
			TaskTitle:         payload.TaskTitle,
			LeadAgent:         leadAgent,
			Sender:            job.Agent,
			RequiredReviewers: e.requiredReviewers(payload),
		}
		return e.HandlePullRequestOpened(ctx, event)
	case "review":
		switch payload.Result.Decision {
		case "changes_requested":
			if err := e.setTaskState(ctx, ref, TaskChangesRequested); err != nil {
				return err
			}
			return e.dispatchFix(ctx, job.Agent, payload, *payload.Result, ref)
		case "approved":
			ready, err := e.allRequiredReviewersApproved(ctx, job.Agent, payload)
			if err != nil {
				return err
			}
			if !ready {
				return e.setReviewingIfNotChangesRequested(ctx, ref)
			}
			return e.runMergeGate(ctx, job.Agent, payload, ref)
		}
	}
	return nil
}

func (e Engine) jobPayload(ctx context.Context, jobID string) (db.Job, JobPayload, error) {
	job, err := e.Store.GetJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.Job{}, JobPayload{}, fmt.Errorf("job %q not found", jobID)
		}
		return db.Job{}, JobPayload{}, err
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		return db.Job{}, JobPayload{}, err
	}
	return job, payload, nil
}

func (e Engine) validate() error {
	if e.Store == nil {
		return errors.New("workflow engine store is required")
	}
	return nil
}

func validatePullRequestEvent(event PullRequestEvent) error {
	switch {
	case strings.TrimSpace(event.Repo) == "":
		return errors.New("pull request repo is required")
	case strings.TrimSpace(event.Branch) == "":
		return errors.New("pull request branch is required")
	case event.PullRequest <= 0:
		return errors.New("pull request number is required")
	case strings.TrimSpace(event.TaskID) == "":
		return errors.New("pull request task id is required")
	case strings.TrimSpace(event.LeadAgent) == "":
		return errors.New("pull request lead agent is required")
	}
	return nil
}

func (e Engine) dispatchFix(ctx context.Context, reviewer string, payload JobPayload, result AgentResult, ref taskRef) error {
	leadAgent, err := e.leadAgent(ctx, payload)
	if err != nil {
		return err
	}
	request := JobRequest{
		Agent:       leadAgent,
		Action:      "implement",
		Repo:        payload.Repo,
		Branch:      payload.Branch,
		PullRequest: payload.PullRequest,
		GoalID:      payload.GoalID,
		TaskID:      payload.TaskID,
		TaskTitle:   payload.TaskTitle,
		LeadAgent:   leadAgent,
		Reviewers:   e.requiredReviewers(payload),
		ReviewRound: payload.ReviewRound,
		Sender:      reviewer,
		Instructions: fmt.Sprintf(
			"Address requested changes from %s: %s",
			reviewer,
			result.Summary,
		),
	}
	return e.dispatch(ctx, request, ref)
}

func (e Engine) leadAgent(ctx context.Context, payload JobPayload) (string, error) {
	leadAgent := strings.TrimSpace(payload.LeadAgent)
	if leadAgent != "" {
		return leadAgent, nil
	}
	lock, err := e.Store.GetBranchLock(ctx, payload.Repo, payload.Branch)
	if err == nil {
		return lock.Owner, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("lead agent is required")
	}
	return "", err
}

func (e Engine) dispatchNextAgents(ctx context.Context, payload JobPayload, result AgentResult, ref taskRef) error {
	requests := []JobRequest{}
	for _, agent := range compactStrings(result.NextAgents) {
		request := JobRequest{
			Agent:        agent,
			Action:       "ask",
			Repo:         payload.Repo,
			Branch:       payload.Branch,
			PullRequest:  payload.PullRequest,
			GoalID:       payload.GoalID,
			TaskID:       payload.TaskID,
			TaskTitle:    payload.TaskTitle,
			LeadAgent:    payload.LeadAgent,
			Reviewers:    e.requiredReviewers(payload),
			ReviewRound:  payload.ReviewRound,
			Sender:       payload.Sender,
			Instructions: "Another agent requested your input: " + result.Summary,
		}
		requests = append(requests, request)
	}
	for _, request := range requests {
		if err := e.ensureAgentAllowed(ctx, request, ref); err != nil {
			return err
		}
	}
	for _, request := range requests {
		if err := e.enqueue(ctx, request); err != nil {
			return err
		}
	}
	return nil
}

func (e Engine) allRequiredReviewersApproved(ctx context.Context, currentReviewer string, payload JobPayload) (bool, error) {
	required := e.requiredReviewers(payload)
	if len(required) == 0 {
		return true, nil
	}

	approved := map[string]bool{}
	if currentReviewer != "" {
		approved[currentReviewer] = true
	}

	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return false, err
	}
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		jobPayload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return false, err
		}
		if !sameTask(payload, jobPayload) || !sameReviewRound(payload, jobPayload) || jobPayload.Result == nil {
			continue
		}
		if jobPayload.Result.Decision == "approved" {
			approved[job.Agent] = true
		}
	}

	for _, reviewer := range required {
		if !approved[reviewer] {
			return false, nil
		}
	}
	return true, nil
}

func (e Engine) setReviewingIfNotChangesRequested(ctx context.Context, ref taskRef) error {
	if strings.TrimSpace(ref.ID) == "" {
		return nil
	}
	task, err := e.Store.GetTask(ctx, ref.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && task.State == string(TaskChangesRequested) {
		return nil
	}
	return e.setTaskState(ctx, ref, TaskReviewing)
}

func (e Engine) latestReviewRound(ctx context.Context, current JobPayload) (string, error) {
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return "", err
	}
	latestRound := ""
	latestNumber := 0
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return "", err
		}
		if !sameTask(current, payload) {
			continue
		}
		round := strings.TrimSpace(payload.ReviewRound)
		if round == "" {
			continue
		}
		number, ok := reviewRoundNumber(round)
		if ok && number > latestNumber {
			latestRound = round
			latestNumber = number
			continue
		}
		if !ok && latestNumber == 0 && round > latestRound {
			latestRound = round
		}
	}
	return latestRound, nil
}

func reviewRoundNumber(round string) (int, bool) {
	value, ok := strings.CutPrefix(round, "review-")
	if !ok {
		return 0, false
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return number, true
}

func (e Engine) requiredReviewers(payload JobPayload) []string {
	reviewers := compactStrings(append([]string{}, payload.Reviewers...))
	if len(reviewers) == 0 {
		reviewers = compactStrings(append([]string{}, e.RequiredReviewers...))
	}
	return reviewers
}

func sameTask(left JobPayload, right JobPayload) bool {
	if left.Repo != "" && right.Repo != "" && left.Repo != right.Repo {
		return false
	}
	if left.PullRequest > 0 && right.PullRequest > 0 && left.PullRequest != right.PullRequest {
		return false
	}
	if left.TaskID != "" || right.TaskID != "" {
		return left.TaskID != "" && left.TaskID == right.TaskID
	}
	return left.Repo == right.Repo && left.PullRequest == right.PullRequest
}

func sameReviewRound(left JobPayload, right JobPayload) bool {
	leftRound := strings.TrimSpace(left.ReviewRound)
	rightRound := strings.TrimSpace(right.ReviewRound)
	if leftRound == "" {
		return rightRound == ""
	}
	return leftRound == rightRound
}

func (e Engine) dispatch(ctx context.Context, request JobRequest, ref taskRef) error {
	if request.ID == "" {
		request.ID = e.jobID(request)
	}
	if err := e.ensureAgentAllowed(ctx, request, ref); err != nil {
		return err
	}
	return e.enqueue(ctx, request)
}

func (e Engine) enqueue(ctx context.Context, request JobRequest) error {
	if request.ID == "" {
		request.ID = e.jobID(request)
	}
	_, err := (Mailbox{Store: e.Store}).Enqueue(ctx, request)
	return err
}

func (e Engine) ensureAgentAllowed(ctx context.Context, request JobRequest, ref taskRef) error {
	agent, err := e.Store.GetAgent(ctx, request.Agent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return e.block(ctx, ref, fmt.Sprintf("agent %q is not subscribed", request.Agent))
		}
		return err
	}
	if agent.RepoScope != request.Repo {
		return e.block(ctx, ref, fmt.Sprintf("agent %q is scoped to %q, not %q", agent.Name, agent.RepoScope, request.Repo))
	}
	if !contains(agent.Capabilities, request.Action) {
		return e.block(ctx, ref, fmt.Sprintf("agent %q lacks %q capability", agent.Name, request.Action))
	}
	if request.Action == "implement" {
		if err := e.ensureBranchLock(ctx, request.Repo, request.Branch, request.Agent, ref); err != nil {
			return err
		}
	}
	return nil
}

func (e Engine) ensureBranchLock(ctx context.Context, repo string, branch string, owner string, ref taskRef) error {
	if strings.TrimSpace(branch) == "" {
		return e.block(ctx, ref, "branch lock rejected action: branch is required")
	}
	acquired, err := e.Store.AcquireLock(ctx, db.BranchLock{RepoFullName: repo, Branch: branch, Owner: owner})
	if err != nil {
		return err
	}
	if !acquired {
		return e.block(ctx, ref, fmt.Sprintf("branch lock rejected action for %s", branch))
	}
	return nil
}

func (e Engine) runMergeGate(ctx context.Context, reviewer string, payload JobPayload, ref taskRef) error {
	if e.MergeGate == nil {
		return e.setTaskState(ctx, ref, TaskReadyToMerge)
	}
	decision, err := e.MergeGate.Evaluate(ctx, MergeRequest{
		Repo:        payload.Repo,
		Branch:      payload.Branch,
		PullRequest: payload.PullRequest,
		TaskID:      payload.TaskID,
		Reviewer:    reviewer,
	})
	if err != nil {
		return err
	}
	if !decision.Ready {
		reason := strings.TrimSpace(decision.Reason)
		if reason == "" {
			reason = "merge gate rejected action"
		}
		return e.block(ctx, ref, reason)
	}
	return e.setTaskState(ctx, ref, TaskReadyToMerge)
}

func (e Engine) recordPullRequestBaseline(ctx context.Context, event PullRequestEvent) error {
	if event.PullRequest <= 0 {
		return nil
	}
	return e.Store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: event.Repo,
		Number:       int64(event.PullRequest),
		HeadBranch:   event.Branch,
		HeadSHA:      event.HeadSHA,
		State:        "open",
	})
}

func (e Engine) nextReviewRound(ctx context.Context, event PullRequestEvent) (string, error) {
	jobs, err := e.Store.ListJobs(ctx)
	if err != nil {
		return "", err
	}
	current := JobPayload{Repo: event.Repo, PullRequest: event.PullRequest, TaskID: event.TaskID}
	rounds := map[string]bool{}
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			return "", err
		}
		if !sameTask(current, payload) {
			continue
		}
		round := strings.TrimSpace(payload.ReviewRound)
		if round == "" {
			round = job.ID
		}
		rounds[round] = true
	}
	return "review-" + strconv.Itoa(len(rounds)+1), nil
}

func (e Engine) block(ctx context.Context, ref taskRef, reason string) error {
	if err := e.setTaskState(ctx, ref, TaskBlocked); err != nil {
		return err
	}
	return BlockedError{Reason: reason}
}

func (e Engine) setTaskState(ctx context.Context, ref taskRef, state TaskState) error {
	if strings.TrimSpace(ref.ID) == "" {
		return nil
	}
	task := db.Task{
		ID:           ref.ID,
		RepoFullName: ref.Repo,
		GoalID:       ref.GoalID,
		Title:        ref.Title,
		State:        string(state),
		Branch:       ref.Branch,
	}
	existing, err := e.Store.GetTask(ctx, ref.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if task.GoalID == "" {
			task.GoalID = existing.GoalID
		}
		if task.RepoFullName == "" {
			task.RepoFullName = existing.RepoFullName
		}
		if task.Title == "" {
			task.Title = existing.Title
		}
		if task.Branch == "" {
			task.Branch = existing.Branch
		}
	}
	return e.Store.UpsertTask(ctx, task)
}

func (e Engine) jobID(request JobRequest) string {
	if e.JobID != nil {
		return e.JobID(request)
	}
	hash := fnv.New64a()
	for _, value := range []string{
		request.Repo,
		request.Branch,
		strconv.Itoa(request.PullRequest),
		request.TaskID,
		request.Agent,
		request.Action,
		request.ReviewRound,
		request.Instructions,
	} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return "workflow-" + strconv.FormatUint(hash.Sum64(), 36)
}

type taskRef struct {
	ID     string
	Repo   string
	GoalID string
	Title  string
	Branch string
}

func taskRefFromPullRequest(event PullRequestEvent) taskRef {
	return taskRef{
		ID:     event.TaskID,
		Repo:   event.Repo,
		GoalID: event.GoalID,
		Title:  event.TaskTitle,
		Branch: event.Branch,
	}
}

func taskRefFromPayload(payload JobPayload) taskRef {
	return taskRef{
		ID:     payload.TaskID,
		Repo:   payload.Repo,
		GoalID: payload.GoalID,
		Title:  payload.TaskTitle,
		Branch: payload.Branch,
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
