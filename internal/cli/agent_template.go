package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gitmoot/gitmoot/internal/agenttemplate"
	"github.com/gitmoot/gitmoot/internal/db"
)

// runAgentTemplate is the READ-ONLY agent-template surface. Authoring and
// distribution (add, export, publish, pull, remote, draft, diff, revert,
// update, validate) were removed in #2204: templates are installed data, and
// the only thing gitmoot does with them is read a row's content into a job
// payload so it reaches the agent's prompt. A template that needs changing is
// edited or re-seeded in the store by hand.
func runAgentTemplate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printAgentTemplateUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runAgentTemplateList(args[1:], stdout, stderr)
	case "show":
		return runAgentTemplateShow(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown agent template command %q\n", args[0])
		printAgentTemplateUsage(stderr)
		return 2
	}
}

func printAgentTemplateUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot agent template list [--capability <cap>] [--runtime <runtime>] [--tag <tag>] [--output <output>]")
	fmt.Fprintln(w, "  gitmoot agent template show thermo-nuclear-code-quality-review")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Agent templates are read-only installed data: gitmoot inspects and reads")
	fmt.Fprintln(w, "them, and cannot author, update, or distribute them. Seed or edit a row")
	fmt.Fprintln(w, "directly in the store to change a template.")
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
	var capabilityFilters repeatedFlag
	var runtimeFilters repeatedFlag
	var tagFilters repeatedFlag
	var outputFilters repeatedFlag
	fs.Var(&capabilityFilters, "capability", "filter by template capability")
	fs.Var(&runtimeFilters, "runtime", "filter by compatible runtime")
	fs.Var(&tagFilters, "tag", "filter by template tag")
	fs.Var(&outputFilters, "output", "filter by template output")
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
	filters := templateListFilters{
		Capabilities: compactValues(capabilityFilters),
		Runtimes:     compactValues(runtimeFilters),
		Tags:         compactValues(tagFilters),
		Outputs:      compactValues(outputFilters),
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
			metadata := metadataForTemplateDefinition(definition, installed[definition.ID])
			if !filters.Match(metadata) {
				continue
			}
			fmt.Fprintf(stdout, "%-36s %-18s %s\n", definition.ID, status, definition.SourceRepo+"/"+definition.SourcePath)
		}
		for _, cached := range cachedTemplates {
			if _, ok := agenttemplate.Lookup(cached.ID); ok {
				continue
			}
			if agenttemplate.IsRetired(cached.ID) {
				continue
			}
			metadata, ok := metadataForCachedTemplate(cached)
			if !filters.MatchOptional(metadata, ok) {
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
		if agenttemplate.IsRetired(id) {
			return retiredAgentTemplateError(id)
		}
		if definition, ok := agenttemplate.Lookup(id); ok {
			cached, err := store.GetAgentTemplate(context.Background(), definition.ID)
			installed := true
			if errors.Is(err, sql.ErrNoRows) {
				installed = false
			} else if err != nil {
				return err
			}
			writeTemplateDefinition(stdout, definition)
			writeTemplateMetadata(stdout, metadataForTemplateDefinition(definition, cached))
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

func retiredAgentTemplateError(id string) error {
	// #2203 added a SECOND retired id (decompose-and-verify -> verifier), so the
	// successor is per-id and agenttemplate.RetiredReplacement is the map that
	// knows it. Pointing every refusal at the planner told a decompose-and-verify
	// caller to use a template that does not do what it asked for.
	if replacement, ok := agenttemplate.RetiredReplacement(id); ok {
		return fmt.Errorf("agent template %s is retired; use %s", id, replacement)
	}
	return fmt.Errorf("agent template %s is retired", id)
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
	if metadata, ok := metadataForCachedTemplate(cached); ok {
		writeTemplateMetadata(w, metadata)
	}
	writeInstalledTemplate(w, cached)
}

func writeTemplateMetadata(w io.Writer, metadata agenttemplate.Metadata) {
	fmt.Fprintln(w, "metadata:")
	fmt.Fprintf(w, "  kind: %s\n", metadata.Kind)
	fmt.Fprintf(w, "  version: %d\n", metadata.Version)
	fmt.Fprintf(w, "  capabilities: %s\n", strings.Join(metadata.Capabilities, ","))
	fmt.Fprintf(w, "  runtime compatibility: %s\n", strings.Join(metadata.RuntimeCompatibility, ","))
	fmt.Fprintf(w, "  tags: %s\n", strings.Join(metadata.Tags, ","))
	fmt.Fprintf(w, "  inputs: %s\n", strings.Join(metadata.Inputs, ","))
	fmt.Fprintf(w, "  outputs: %s\n", strings.Join(metadata.Outputs, ","))
	if len(metadata.Evaluation) > 0 {
		keys := make([]string, 0, len(metadata.Evaluation))
		for key := range metadata.Evaluation {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fmt.Fprintln(w, "  evaluation:")
		for _, key := range keys {
			fmt.Fprintf(w, "    %s: %s\n", key, metadata.Evaluation[key])
		}
	}
}

func writeInstalledTemplate(w io.Writer, cached db.AgentTemplate) {
	fmt.Fprintln(w, "installed: yes")
	if cached.VersionID != "" {
		fmt.Fprintf(w, "version: v%d\n", cached.VersionNumber)
		fmt.Fprintf(w, "version id: %s\n", cached.VersionID)
		fmt.Fprintf(w, "promotion state: %s\n", cached.VersionState)
	}
	if cached.ContentHash != "" {
		fmt.Fprintf(w, "content hash: %s\n", cached.ContentHash)
	}
	fmt.Fprintf(w, "resolved commit: %s\n", cached.ResolvedCommit)
	fmt.Fprintf(w, "updated: %s\n", cached.UpdatedAt)
	fmt.Fprintln(w, "content:")
	fmt.Fprintln(w, strings.TrimRight(cached.Content, "\n"))
}

type templateListFilters struct {
	Capabilities []string
	Runtimes     []string
	Tags         []string
	Outputs      []string
}

func (f templateListFilters) Empty() bool {
	return len(f.Capabilities) == 0 && len(f.Runtimes) == 0 && len(f.Tags) == 0 && len(f.Outputs) == 0
}

func (f templateListFilters) MatchOptional(metadata agenttemplate.Metadata, ok bool) bool {
	if !ok {
		return f.Empty()
	}
	return f.Match(metadata)
}

func (f templateListFilters) Match(metadata agenttemplate.Metadata) bool {
	return containsAll(metadata.Capabilities, f.Capabilities) &&
		containsAll(metadata.RuntimeCompatibility, f.Runtimes) &&
		containsAll(metadata.Tags, f.Tags) &&
		containsAll(metadata.Outputs, f.Outputs)
}

func containsAll(values []string, filters []string) bool {
	for _, filter := range filters {
		if !containsValue(values, filter) {
			return false
		}
	}
	return true
}

func metadataForTemplateDefinition(definition agenttemplate.Definition, cached db.AgentTemplate) agenttemplate.Metadata {
	if metadata, ok := metadataForCachedTemplate(cached); ok {
		return metadata
	}
	return agenttemplate.MetadataForDefinition(definition)
}

func metadataForCachedTemplate(cached db.AgentTemplate) (agenttemplate.Metadata, bool) {
	if strings.TrimSpace(cached.MetadataJSON) != "" {
		metadata, err := agenttemplate.UnmarshalMetadata(cached.MetadataJSON)
		if err == nil {
			return metadata, true
		}
	}
	parsed, err := agenttemplate.ParseTemplateContent(cached.Content)
	if err != nil {
		return agenttemplate.Metadata{}, false
	}
	return parsed.Metadata, true
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
