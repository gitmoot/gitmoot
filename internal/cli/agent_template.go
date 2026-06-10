package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	case "draft":
		return runAgentTemplateDraft(args[1:], stdout, stderr)
	case "list":
		return runAgentTemplateList(args[1:], stdout, stderr)
	case "validate":
		return runAgentTemplateValidate(args[1:], stdout, stderr)
	case "show":
		return runAgentTemplateShow(args[1:], stdout, stderr)
	case "update":
		return runAgentTemplateUpdate(args[1:], stdout, stderr)
	case "diff":
		return runAgentTemplateDiff(args[1:], stdout, stderr)
	case "revert":
		return runAgentTemplateRevert(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown agent template command %q\n\n", args[0])
		printAgentTemplateUsage(stderr)
		return 2
	}
}

func printAgentTemplateUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot agent template add <template-id> --file ./agents/<template-id>.md [--name <name>] [--description <text>]")
	fmt.Fprintln(w, "  gitmoot agent template draft <template-id> [--output .gitmoot/templates/<template-id>.md] [--force]")
	fmt.Fprintln(w, "  gitmoot agent template validate <file>")
	fmt.Fprintln(w, "  gitmoot agent template list")
	fmt.Fprintln(w, "  gitmoot agent template show thermo-nuclear-code-quality-review")
	fmt.Fprintln(w, "  gitmoot agent template update thermo-nuclear-code-quality-review")
	fmt.Fprintln(w, "  gitmoot agent template diff thermo-nuclear-code-quality-review")
	fmt.Fprintln(w, "  gitmoot agent template revert <template-id> --version <version-id>")
}

// runAgentTemplateRevert rolls a template back to a previously current
// (superseded) version.
func runAgentTemplateRevert(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent template revert", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	versionRef := fs.String("version", "", "superseded version id to make current again")
	id, flagArgs := leadingID(args)
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(*versionRef) == "" {
		fmt.Fprintln(stderr, "agent template revert requires <template-id> and --version")
		return 2
	}
	var reverted db.AgentTemplateVersion
	if err := withStore(*home, func(store *db.Store) error {
		version, err := store.GetAgentTemplateVersion(context.Background(), strings.TrimSpace(id), strings.TrimSpace(*versionRef))
		if err != nil {
			return err
		}
		reverted, err = store.RevertAgentTemplateVersion(context.Background(), strings.TrimSpace(id), version.ID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "agent template revert: %v\n", err)
		return 1
	}
	writeLine(stdout, "reverted %s to %s", strings.TrimSpace(id), reverted.ID)
	return 0
}

func runAgentTemplateAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent template add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	file := fs.String("file", "", "local template file to install")
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

func runAgentTemplateDraft(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent template draft", flag.ContinueOnError)
	fs.SetOutput(stderr)
	_ = fs.String("home", "", "accepted for command consistency; not used")
	output := fs.String("output", "", "path to write the draft template")
	force := fs.Bool("force", false, "overwrite an existing draft file")
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
			fmt.Fprintln(stderr, "agent template draft requires exactly one template id")
			return 2
		}
	} else if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent template draft requires exactly one template id")
		return 2
	}
	content, err := agenttemplate.DraftCaptureTemplate(id)
	if err != nil {
		fmt.Fprintf(stderr, "draft agent template: %v\n", err)
		return 1
	}
	path := strings.TrimSpace(*output)
	if path == "" {
		path = filepath.Join(".gitmoot", "templates", id+".md")
	}
	if err := writeAgentTemplateDraft(path, content, *force); err != nil {
		fmt.Fprintf(stderr, "draft agent template: %v\n", err)
		return 1
	}
	writeLine(stdout, "drafted %s at %s", id, path)
	return 0
}

func writeAgentTemplateDraft(path string, content string, force bool) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("output path is required")
	}
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("template draft %s already exists; pass --force to overwrite", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect template draft %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create template draft directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write template draft %s: %w", path, err)
	}
	return nil
}

func runAgentTemplateValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent template validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	_ = fs.String("home", "", "accepted for command consistency; not used")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "agent template validate requires exactly one file path")
		return 2
	}
	path := fs.Arg(0)
	if err := agenttemplate.ValidateCaptureTemplateFile(path); err != nil {
		fmt.Fprintf(stderr, "validate agent template: %v\n", err)
		return 1
	}
	writeLine(stdout, "valid agent template: %s", path)
	return 0
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
		if agenttemplate.IsRetired(id) {
			return retiredAgentTemplateError(id)
		}
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
		upstreamContent, err := agenttemplate.ContentForDefinition(definition, upstream.Content)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "cached:   %s\n", cached.ResolvedCommit)
		fmt.Fprintf(stdout, "upstream: %s\n", resolvedCommit)
		fmt.Fprint(stdout, agenttemplate.Diff(cached.Content, upstreamContent))
		return nil
	})
}

func updateTemplateByID(ctx context.Context, store *db.Store, id string) (db.AgentTemplate, error) {
	if agenttemplate.IsRetired(id) {
		return db.AgentTemplate{}, retiredAgentTemplateError(id)
	}
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

func retiredAgentTemplateError(id string) error {
	return fmt.Errorf("agent template %s is retired; use %s", id, agenttemplate.PlannerTemplateID)
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
