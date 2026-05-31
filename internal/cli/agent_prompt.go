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
	"github.com/jerryfane/gitmoot/internal/db"
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
}

func runAgentPrompt(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent prompt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print prompt metadata and content as JSON")
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
