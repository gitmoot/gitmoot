package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/db"
)

var newAgentTemplateFetcher = func() agenttemplate.Fetcher {
	return agenttemplate.GHFetcher{}
}

func runAgentTemplate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printAgentTemplateUsage(stdout)
		return 0
	}
	switch args[0] {
	case "add":
		return runAgentTemplateAdd(args[1:], stdout, stderr)
	case "list":
		return runAgentTemplateList(args[1:], stdout, stderr)
	case "show":
		return runAgentTemplateShow(args[1:], stdout, stderr)
	case "update":
		return runAgentTemplateUpdate(args[1:], stdout, stderr)
	case "diff":
		return runAgentTemplateDiff(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown agent template command %q\n\n", args[0])
		printAgentTemplateUsage(stderr)
		return 2
	}
}

func printAgentTemplateUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot agent template add <template-id> --file ./agents/<template-id>.md [--name <name>] [--description <text>]")
	fmt.Fprintln(w, "  gitmoot agent template list")
	fmt.Fprintln(w, "  gitmoot agent template show thermo-nuclear-code-quality-review")
	fmt.Fprintln(w, "  gitmoot agent template update thermo-nuclear-code-quality-review")
	fmt.Fprintln(w, "  gitmoot agent template diff thermo-nuclear-code-quality-review")
}

func runAgentTemplateAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent template add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	file := fs.String("file", "", "local prompt file to install")
	name := fs.String("name", "", "agent template display name")
	description := fs.String("description", "", "agent template description")
	id, flagArgs := leadingID(args)
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
			fmt.Fprintln(stderr, "agent template add requires exactly one template id")
			return 2
		}
	} else if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent template add requires exactly one template id")
		return 2
	}
	if strings.TrimSpace(*file) == "" {
		fmt.Fprintln(stderr, "agent template add requires --file")
		return 2
	}
	return withStoreExit(*home, stderr, "add agent template", func(store *db.Store) error {
		added, err := agenttemplate.AddLocal(context.Background(), store, id, *file, *name, *description)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "added %s at %s\n", added.ID, added.ResolvedCommit)
		return nil
	})
}

func leadingID(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

func runAgentTemplateList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent template list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent template list does not accept positional arguments")
		return 2
	}
	return withStoreExit(*home, stderr, "list agent templates", func(store *db.Store) error {
		cachedTemplates, err := store.ListAgentTemplates(context.Background())
		if err != nil {
			return err
		}
		installed := installedTemplateMap(cachedTemplates)
		for _, definition := range agenttemplate.Builtins() {
			status := "available"
			if installedTemplate, ok := installed[definition.ID]; ok {
				status = "installed@" + shortCommit(installedTemplate.ResolvedCommit)
			}
			fmt.Fprintf(stdout, "%-36s %-18s %s\n", definition.ID, status, definition.SourceRepo+"/"+definition.SourcePath)
		}
		for _, cached := range cachedTemplates {
			if _, ok := agenttemplate.Lookup(cached.ID); ok {
				continue
			}
			status := "installed@" + shortCommit(cached.ResolvedCommit)
			fmt.Fprintf(stdout, "%-36s %-18s %s:%s\n", cached.ID, status, cached.SourceRepo, cached.SourcePath)
		}
		return nil
	})
}

func runAgentTemplateShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent template show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "agent template show requires exactly one template id")
		return 2
	}
	id := fs.Arg(0)
	return withStoreExit(*home, stderr, "show agent template", func(store *db.Store) error {
		if definition, ok := agenttemplate.Lookup(id); ok {
			cached, err := store.GetAgentTemplate(context.Background(), definition.ID)
			installed := true
			if errors.Is(err, sql.ErrNoRows) {
				installed = false
			} else if err != nil {
				return err
			}
			writeTemplateDefinition(stdout, definition)
			if !installed {
				fmt.Fprintln(stdout, "installed: no")
				return nil
			}
			writeInstalledTemplate(stdout, cached)
			return nil
		}
		cached, err := store.GetAgentTemplate(context.Background(), id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("unknown agent template %q", id)
		}
		if err != nil {
			return err
		}
		writeCustomTemplate(stdout, cached)
		return nil
	})
}

func runAgentTemplateUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent template update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "agent template update requires exactly one template id")
		return 2
	}
	id := fs.Arg(0)
	return withStoreExit(*home, stderr, "update agent template", func(store *db.Store) error {
		updated, err := updateTemplateByID(context.Background(), store, id)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "updated %s at %s\n", updated.ID, updated.ResolvedCommit)
		return nil
	})
}

func runAgentTemplateDiff(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent template diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "agent template diff requires exactly one template id")
		return 2
	}
	id := fs.Arg(0)
	return withStoreExit(*home, stderr, "diff agent template", func(store *db.Store) error {
		cached, err := store.GetAgentTemplate(context.Background(), id)
		if errors.Is(err, sql.ErrNoRows) {
			if _, ok := agenttemplate.Lookup(id); ok {
				return fmt.Errorf("agent template %s is not installed; run gitmoot agent template update %s", id, id)
			}
			return fmt.Errorf("unknown agent template %q", id)
		}
		if err != nil {
			return err
		}
		if agenttemplate.IsLocal(cached) {
			file, hash, err := agenttemplate.ReadLocalForDiff(cached.SourcePath)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "cached:   %s\n", cached.ResolvedCommit)
			fmt.Fprintf(stdout, "upstream: %s\n", hash)
			fmt.Fprint(stdout, agenttemplate.DiffExact(cached.Content, file.Content))
			return nil
		}
		definition, ok := agenttemplate.Lookup(id)
		if !ok {
			return fmt.Errorf("agent template %s is not a local custom template and has no built-in source", id)
		}
		fetcher := newAgentTemplateFetcher()
		resolvedCommit, err := fetcher.ResolveRef(context.Background(), definition.SourceRepo, definition.SourceRef)
		if err != nil {
			return err
		}
		upstream, err := fetcher.FetchFile(context.Background(), definition.SourceRepo, resolvedCommit, definition.SourcePath)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "cached:   %s\n", cached.ResolvedCommit)
		fmt.Fprintf(stdout, "upstream: %s\n", resolvedCommit)
		fmt.Fprint(stdout, agenttemplate.Diff(cached.Content, upstream.Content))
		return nil
	})
}

func updateTemplateByID(ctx context.Context, store *db.Store, id string) (db.AgentTemplate, error) {
	if _, ok := agenttemplate.Lookup(id); ok {
		return agenttemplate.Update(ctx, store, newAgentTemplateFetcher(), id)
	}
	cached, err := store.GetAgentTemplate(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return db.AgentTemplate{}, fmt.Errorf("unknown agent template %q; run gitmoot agent template add %s --file <path>", id, id)
	}
	if err != nil {
		return db.AgentTemplate{}, err
	}
	if !agenttemplate.IsLocal(cached) {
		return db.AgentTemplate{}, fmt.Errorf("agent template %s is not a local custom template and has no built-in source", id)
	}
	return agenttemplate.UpdateLocal(ctx, store, cached)
}

func installedTemplateMap(templates []db.AgentTemplate) map[string]db.AgentTemplate {
	installed := make(map[string]db.AgentTemplate, len(templates))
	for _, cached := range templates {
		installed[cached.ID] = cached
	}
	return installed
}

func writeTemplateDefinition(w io.Writer, definition agenttemplate.Definition) {
	fmt.Fprintf(w, "id: %s\n", definition.ID)
	fmt.Fprintf(w, "name: %s\n", definition.Name)
	fmt.Fprintf(w, "description: %s\n", definition.Description)
	fmt.Fprintf(w, "source: %s@%s:%s\n", definition.SourceRepo, definition.SourceRef, definition.SourcePath)
	fmt.Fprintf(w, "default role: %s\n", definition.DefaultRole)
	fmt.Fprintf(w, "default capabilities: %s\n", strings.Join(definition.DefaultCapabilities, ","))
	fmt.Fprintf(w, "mutation: %t\n", definition.Mutation)
}

func writeCustomTemplate(w io.Writer, cached db.AgentTemplate) {
	fmt.Fprintf(w, "id: %s\n", cached.ID)
	fmt.Fprintf(w, "name: %s\n", cached.Name)
	fmt.Fprintf(w, "description: %s\n", cached.Description)
	fmt.Fprintf(w, "source: %s@%s:%s\n", cached.SourceRepo, cached.SourceRef, cached.SourcePath)
	fmt.Fprintln(w, "default role: ")
	fmt.Fprintln(w, "default capabilities: ")
	fmt.Fprintln(w, "mutation: false")
	writeInstalledTemplate(w, cached)
}

func writeInstalledTemplate(w io.Writer, cached db.AgentTemplate) {
	fmt.Fprintln(w, "installed: yes")
	fmt.Fprintf(w, "resolved commit: %s\n", cached.ResolvedCommit)
	fmt.Fprintf(w, "updated: %s\n", cached.UpdatedAt)
	fmt.Fprintln(w, "content:")
	fmt.Fprintln(w, strings.TrimRight(cached.Content, "\n"))
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}

func withStoreExit(home string, stderr io.Writer, label string, fn func(*db.Store) error) int {
	if err := withStore(home, fn); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", label, err)
		return 1
	}
	return 0
}
