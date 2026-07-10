package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

type agentPromptOutput struct {
	Kind           string   `json:"kind"`
	Name           string   `json:"name"`
	TemplateID     string   `json:"template_id"`
	Runtime        string   `json:"runtime,omitempty"`
	Role           string   `json:"role,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	ResolvedCommit string   `json:"resolved_commit"`
	Content        string   `json:"content"`
	// JobID is set only when the prompt is imported with --record: it carries the
	// session job opened for this import so the JSON consumer can close it later.
	// Without --record it stays empty and is omitted, keeping the plain-inspection
	// output byte-identical to the historical behavior.
	JobID string `json:"job_id,omitempty"`
}

func runAgentPrompt(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent prompt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print prompt metadata and content as JSON")
	record := fs.Bool("record", false, "open a session job for this import so the here-method work is tracked (#657); print the job id in the header and close it with `gitmoot job close`")
	repo := fs.String("repo", "", "repo scope as owner/repo for the recorded session job (default: the agent's repo_scope); only used with --record")
	typeName := fs.String("type", "implement", "session job type when recording: ask|review|implement; only used with --record")
	id, flagArgs := leadingID(args)
	if len(args) == 0 || containsHelpFlag(args) {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "agent prompt requires exactly one agent or template id")
			return 2
		}
		return 0
	}
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if id == "" {
		if fs.NArg() == 1 {
			id = fs.Arg(0)
		} else {
			fmt.Fprintln(stderr, "agent prompt requires exactly one agent or template id")
			return 2
		}
	} else if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent prompt requires exactly one agent or template id")
		return 2
	}
	if *record {
		return runAgentPromptRecord(*home, id, *repo, *typeName, *jsonOutput, stdout, stderr)
	}

	var output agentPromptOutput
	if err := withReadOnlyStore(*home, func(store *db.Store) error {
		resolved, err := resolveAgentPrompt(context.Background(), store, id)
		if err != nil {
			return err
		}
		output = resolved
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "agent prompt: %v\n", promptReadError(err))
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "agent prompt: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, strings.TrimRight(output.Content, "\n"))
	return 0
}

// sessionCloseHint is the exact header line printed above a --record'd prompt so
// the importing agent knows the session job id it must close when the here-method
// work is done (#657). The decision list mirrors workflow.ResultDecisions.
func sessionCloseHint(jobID string) string {
	return fmt.Sprintf("[gitmoot session job %s \u2014 when this work is complete, run: gitmoot job close %s --decision <approved|changes_requested|implemented|blocked|failed|skipped> --summary \"...\"]", jobID, jobID)
}

// runAgentPromptRecord implements `agent prompt <id> --record`: it opens a session
// job (the same OpenExternalJob/Mailbox clock-in `job open` uses) for the resolved
// identity + repo and returns the prompt with a header that names the job id so the
// importing session tracks the here-method work by default. Unlike the plain prompt
// path this needs a writable store. The id may resolve two ways:
//   - a registered agent: the repo falls back to the agent's repo_scope when --repo
//     is omitted, and the session job records the agent name as its identity.
//   - a bare template (no agent registered): --repo owner/repo is REQUIRED (a
//     template has no repo_scope to fall back on), and the session job records the
//     template id as its identity (#673).
func runAgentPromptRecord(home, id, repoFlag, typeName string, jsonOutput bool, stdout, stderr io.Writer) int {
	action, ok := validateSessionAction(typeName, stderr)
	if !ok {
		return 2
	}
	ctx := context.Background()
	var (
		output agentPromptOutput
		jobID  string
	)
	if err := withStoreAndPaths(home, func(paths config.Paths, store *db.Store) error {
		id := strings.TrimSpace(id)
		agent, err := store.GetAgent(ctx, id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		var (
			resolved agentPromptOutput
			identity string
			repoName string
		)
		if err == nil {
			// Registered-agent path (unchanged): repo falls back to repo_scope.
			resolved, err = promptForAgent(ctx, store, agent)
			if err != nil {
				return err
			}
			repoScope := strings.TrimSpace(repoFlag)
			if repoScope == "" {
				repoScope = strings.TrimSpace(agent.RepoScope)
			}
			if repoScope == "" {
				return fmt.Errorf("no repo to record against: pass --repo owner/repo or set agent %q's repo_scope", agent.Name)
			}
			fullName, verr := validateSessionAgentRepo(ctx, store, agent.Name, repoScope)
			if verr != nil {
				return verr
			}
			identity = agent.Name
			repoName = fullName
		} else {
			// Bare-template path (#673): no agent to fall back on, so --repo is
			// required and the template id is the recorded identity.
			template, terr := loadInstalledTemplate(ctx, store, id)
			if terr != nil {
				return terr
			}
			repoScope := strings.TrimSpace(repoFlag)
			if repoScope == "" {
				return fmt.Errorf("--repo owner/repo is required to record %q: it is a template, not a registered agent, so there is no repo scope to record against", id)
			}
			fullName, verr := validateSessionRepo(ctx, store, repoScope)
			if verr != nil {
				return verr
			}
			resolved = promptOutputForTemplate("template", template.ID, template)
			identity = template.ID
			repoName = fullName
		}

		engine := sessionWorkflowEngine(store, paths.Home)
		job, err := engine.OpenExternalJob(ctx, workflow.JobRequest{
			ID:     sessionJobID(action, identity),
			Agent:  identity,
			Action: action,
			Repo:   repoName,
			Sender: "session",
		})
		if err != nil {
			return err
		}
		jobID = job.ID
		resolved.JobID = job.ID
		output = resolved
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "agent prompt --record: %v\n", promptReadError(err))
		return 1
	}
	if jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "agent prompt --record: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(stdout, sessionCloseHint(jobID))
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, strings.TrimRight(output.Content, "\n"))
	return 0
}

func withReadOnlyStore(home string, fn func(*db.Store) error) error {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return err
	}
	if _, err := os.Stat(paths.Database); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("Gitmoot state is not initialized at %s; run gitmoot init first", paths.Home)
		}
		return fmt.Errorf("stat database: %w", err)
	}
	store, err := db.OpenReadOnly(paths.Database)
	if err != nil {
		return fmt.Errorf("open database read-only: %w", err)
	}
	defer store.Close()
	return fn(store)
}

func promptReadError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if strings.Contains(message, "no such table") || strings.Contains(message, "no such column") {
		return fmt.Errorf("Gitmoot state is outdated for prompt import; run gitmoot init or another Gitmoot command that migrates local state first: %w", err)
	}
	return err
}

func resolveAgentPrompt(ctx context.Context, store *db.Store, id string) (agentPromptOutput, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return agentPromptOutput{}, errors.New("agent or template id is required")
	}
	agent, err := store.GetAgent(ctx, id)
	if err == nil {
		return promptForAgent(ctx, store, agent)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return agentPromptOutput{}, err
	}
	template, err := loadInstalledTemplate(ctx, store, id)
	if err != nil {
		return agentPromptOutput{}, err
	}
	return promptOutputForTemplate("template", template.ID, template), nil
}

func promptForAgent(ctx context.Context, store *db.Store, agent db.Agent) (agentPromptOutput, error) {
	if strings.TrimSpace(agent.TemplateID) == "" {
		return agentPromptOutput{}, fmt.Errorf("agent %s has no agent template to import; start or subscribe it with --template <template-id>", agent.Name)
	}
	template, err := loadInstalledTemplate(ctx, store, agent.TemplateID)
	if err != nil {
		return agentPromptOutput{}, err
	}
	output := promptOutputForTemplate("agent", agent.Name, template)
	output.TemplateID = agent.TemplateID
	output.Runtime = agent.Runtime
	output.Role = agent.Role
	output.Capabilities = append([]string{}, agent.Capabilities...)
	return output, nil
}

func promptOutputForTemplate(kind string, name string, template db.AgentTemplate) agentPromptOutput {
	return agentPromptOutput{
		Kind:           kind,
		Name:           name,
		TemplateID:     template.ID,
		ResolvedCommit: template.ResolvedCommit,
		Content:        agenttemplate.InstructionsForContent(template.Content),
	}
}
