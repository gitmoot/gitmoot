package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// sessionJobActions are the job types a session ("here"/prompt-import) job may be
// opened as — the same action set an engine-run job uses.
var sessionJobActions = []string{"ask", "review", "implement"}

// jobSessionOutput is the shared JSON/text shape printed by `job open`, `job
// close`, and `job record` (#657). Decision/summary/PR fields are omitted when
// empty so an `open` result (no decision yet) is clean.
type jobSessionOutput struct {
	JobID            string `json:"job_id"`
	State            string `json:"state"`
	Agent            string `json:"agent"`
	Type             string `json:"type"`
	Repo             string `json:"repo"`
	ExternallyDriven bool   `json:"externally_driven"`
	Decision         string `json:"decision,omitempty"`
	Summary          string `json:"summary,omitempty"`
	PullRequest      int    `json:"pull_request,omitempty"`
}

func runJobOpen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job open", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	agent := fs.String("agent", "", "agent that performs the session work (must exist)")
	repo := fs.String("repo", "", "repo scope as owner/repo (must be tracked)")
	typeName := fs.String("type", "", "job type: ask|review|implement")
	title := fs.String("title", "", "optional human title for the job")
	task := fs.String("task", "", "optional task id to associate")
	pr := fs.Int("pr", 0, "optional pull request number")
	jsonOutput := fs.Bool("json", false, "print the created job as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "job open does not accept positional arguments")
		return 2
	}
	action, ok := validateSessionAction(*typeName, stderr)
	if !ok {
		return 2
	}
	if strings.TrimSpace(*agent) == "" || strings.TrimSpace(*repo) == "" {
		fmt.Fprintln(stderr, "job open requires --agent and --repo")
		return 2
	}

	var out jobSessionOutput
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		fullName, err := validateSessionAgentRepo(context.Background(), store, *agent, *repo)
		if err != nil {
			return err
		}
		engine := sessionWorkflowEngine(store, paths.Home)
		job, err := engine.OpenExternalJob(context.Background(), workflow.JobRequest{
			ID:          sessionJobID(action, *agent),
			Agent:       *agent,
			Action:      action,
			Repo:        fullName,
			TaskID:      strings.TrimSpace(*task),
			TaskTitle:   strings.TrimSpace(*title),
			PullRequest: *pr,
			Sender:      "session",
		})
		if err != nil {
			return err
		}
		out = jobSessionOutput{
			JobID:            job.ID,
			State:            job.State,
			Agent:            job.Agent,
			Type:             job.Type,
			Repo:             fullName,
			ExternallyDriven: true,
			PullRequest:      *pr,
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "job open: %v\n", err)
		return 1
	}
	printJobSessionOutput(stdout, out, *jsonOutput)
	return 0
}

func runJobClose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job close", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	decision := fs.String("decision", "", "result decision: "+strings.Join(workflow.ResultDecisions, "|"))
	summary := fs.String("summary", "", "optional result summary")
	pr := fs.Int("pr", 0, "optional pull request number to record")
	branch := fs.String("branch", "", "optional branch to record")
	jsonOutput := fs.Bool("json", false, "print the closed job as JSON")
	// The job id is positional and precedes the flags (`job close <id> --decision
	// …`), so pull it off args[0] before flag.Parse (which stops at the first
	// non-flag token).
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		if len(args) == 0 {
			fmt.Fprintln(stderr, "job close requires a job id")
			return 2
		}
		return 0
	}
	jobID := strings.TrimSpace(args[0])
	if jobID == "" || strings.HasPrefix(jobID, "-") {
		fmt.Fprintln(stderr, "job close requires a job id as the first argument")
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "job close accepts exactly one job id")
		return 2
	}
	if !validateSessionDecision(*decision, stderr) {
		return 2
	}

	var out jobSessionOutput
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		engine := sessionWorkflowEngine(store, paths.Home)
		job, err := engine.CloseExternalJob(context.Background(), jobID, workflow.AgentResult{
			Decision: *decision,
			Summary:  strings.TrimSpace(*summary),
		}, *pr, *branch)
		if err != nil {
			return err
		}
		payload, _ := workflow.ParseJobPayload(job.Payload)
		out = jobSessionOutput{
			JobID:            job.ID,
			State:            job.State,
			Agent:            job.Agent,
			Type:             job.Type,
			Repo:             payload.Repo,
			ExternallyDriven: job.ExternallyDriven,
			Decision:         *decision,
			Summary:          strings.TrimSpace(*summary),
			PullRequest:      payload.PullRequest,
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "job close: %v\n", err)
		return 1
	}
	printJobSessionOutput(stdout, out, *jsonOutput)
	return 0
}

func runJobRecord(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	agent := fs.String("agent", "", "agent that performed the session work (must exist)")
	repo := fs.String("repo", "", "repo scope as owner/repo (must be tracked)")
	typeName := fs.String("type", "", "job type: ask|review|implement")
	decision := fs.String("decision", "", "result decision: "+strings.Join(workflow.ResultDecisions, "|"))
	title := fs.String("title", "", "optional human title for the job")
	summary := fs.String("summary", "", "optional result summary")
	task := fs.String("task", "", "optional task id to associate")
	pr := fs.Int("pr", 0, "optional pull request number")
	branch := fs.String("branch", "", "optional branch to record")
	jsonOutput := fs.Bool("json", false, "print the recorded job as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "job record does not accept positional arguments")
		return 2
	}
	action, ok := validateSessionAction(*typeName, stderr)
	if !ok {
		return 2
	}
	if strings.TrimSpace(*agent) == "" || strings.TrimSpace(*repo) == "" {
		fmt.Fprintln(stderr, "job record requires --agent and --repo")
		return 2
	}
	if !validateSessionDecision(*decision, stderr) {
		return 2
	}

	var out jobSessionOutput
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		fullName, err := validateSessionAgentRepo(context.Background(), store, *agent, *repo)
		if err != nil {
			return err
		}
		engine := sessionWorkflowEngine(store, paths.Home)
		opened, err := engine.OpenExternalJob(context.Background(), workflow.JobRequest{
			ID:          sessionJobID(action, *agent),
			Agent:       *agent,
			Action:      action,
			Repo:        fullName,
			TaskID:      strings.TrimSpace(*task),
			TaskTitle:   strings.TrimSpace(*title),
			PullRequest: *pr,
			Sender:      "session",
		})
		if err != nil {
			return err
		}
		job, err := engine.CloseExternalJob(context.Background(), opened.ID, workflow.AgentResult{
			Decision: *decision,
			Summary:  strings.TrimSpace(*summary),
		}, *pr, *branch)
		if err != nil {
			return err
		}
		payload, _ := workflow.ParseJobPayload(job.Payload)
		out = jobSessionOutput{
			JobID:            job.ID,
			State:            job.State,
			Agent:            job.Agent,
			Type:             job.Type,
			Repo:             fullName,
			ExternallyDriven: job.ExternallyDriven,
			Decision:         *decision,
			Summary:          strings.TrimSpace(*summary),
			PullRequest:      payload.PullRequest,
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "job record: %v\n", err)
		return 1
	}
	printJobSessionOutput(stdout, out, *jsonOutput)
	return 0
}

// sessionWorkflowEngine builds the workflow engine used by the session-job
// commands. It needs no checkout or GitHub work (a session job never dispatches a
// runtime), so it passes an empty checkout; the only engine seam it exercises is
// the wired EventSink, which lets CloseExternalJob emit the outbound
// job.finished/failed/blocked event exactly as an engine-run job does.
func sessionWorkflowEngine(store *db.Store, home string) workflow.Engine {
	return daemonWorkflowEngine(store, github.NewClient(""), "", home)
}

// validateSessionAction validates a `--type` flag against the allowed job actions.
func validateSessionAction(value string, stderr io.Writer) (string, bool) {
	action := strings.TrimSpace(value)
	for _, a := range sessionJobActions {
		if action == a {
			return action, true
		}
	}
	fmt.Fprintf(stderr, "invalid --type %q; want one of %s\n", value, strings.Join(sessionJobActions, ", "))
	return "", false
}

// validateSessionDecision validates a `--decision` flag against ResultDecisions.
func validateSessionDecision(value string, stderr io.Writer) bool {
	decision := strings.TrimSpace(value)
	for _, d := range workflow.ResultDecisions {
		if decision == d {
			return true
		}
	}
	fmt.Fprintf(stderr, "invalid --decision %q; want one of %s\n", value, strings.Join(workflow.ResultDecisions, ", "))
	return false
}

// validateSessionAgentRepo confirms the agent and repo exist, returning the
// canonical owner/repo full name. The agent must be a registered agent (or a
// managed instance); the repo must be a well-formed owner/repo that gitmoot
// tracks. It does NOT run capability/access checks: a session job is a
// record-keeping surface for work the caller already did, not a dispatch.
func validateSessionAgentRepo(ctx context.Context, store *db.Store, agentName, repoFlag string) (string, error) {
	if _, err := store.GetAgent(ctx, strings.TrimSpace(agentName)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("agent %q not found", agentName)
		}
		return "", err
	}
	repo, err := daemon.ParseRepository(repoFlag)
	if err != nil {
		return "", err
	}
	fullName := repo.FullName()
	if _, err := store.GetRepo(ctx, fullName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("repo %q is not tracked; add it with `gitmoot repo add %s` first", fullName, fullName)
		}
		return "", err
	}
	return fullName, nil
}

// sessionJobID mints a stable, unique id for a session job, distinct from the
// engine's `local-*` foreground ids so a session-recorded job is recognizable.
func sessionJobID(action, agent string) string {
	return fmt.Sprintf("session-%s-%s-%x", action, agent, time.Now().UTC().UnixNano())
}

func printJobSessionOutput(stdout io.Writer, out jobSessionOutput, jsonOutput bool) {
	if jsonOutput {
		_ = writeJSON(stdout, out)
		return
	}
	writeLine(stdout, "job: %s", out.JobID)
	writeLine(stdout, "state: %s", out.State)
	writeLine(stdout, "agent: %s", out.Agent)
	writeLine(stdout, "type: %s", out.Type)
	writeLine(stdout, "repo: %s", out.Repo)
	if out.Decision != "" {
		writeLine(stdout, "decision: %s", out.Decision)
	}
	if out.Summary != "" {
		writeLine(stdout, "summary: %s", out.Summary)
	}
	if out.PullRequest > 0 {
		writeLine(stdout, "pull_request: %d", out.PullRequest)
	}
}
