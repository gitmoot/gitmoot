package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
)

// templateRemoteClient keeps the existing template-specific test seam while
// sharing the write contract with other GitHub-backed publishers.
type templateRemoteClient = githubRemoteWriteClient

// newTemplateRemoteClient builds the GitHub client `publish` uses. It is a var so
// tests can replace it with a stubbed-Runner GhClient.
var newTemplateRemoteClient = func() templateRemoteClient {
	return github.NewClient("")
}

// newAgentTemplateRemoteSource builds the RemoteSource bulk `pull` uses (resolve
// ref + fetch file + list dir). It is a var so tests can replace it with a fake.
var newAgentTemplateRemoteSource = func() agenttemplate.RemoteSource {
	return agenttemplate.GHFetcher{}
}

// configuredTemplateRemote returns the configured default template remote and
// whether one is set. It is best-effort: any path/parse error (e.g. an
// uninitialized home) reports "not configured" so callers fall back to their
// existing flag-required behavior (off by default).
func configuredTemplateRemote(home string) (config.TemplateRemotePolicy, bool) {
	remote, ok := configuredGitHubRemote(home, loadTemplateGitHubRemote)
	return config.TemplateRemotePolicy(remote), ok
}

func loadTemplateGitHubRemote(paths config.Paths) (config.GitHubRemotePolicy, error) {
	remote, err := config.LoadTemplateRemote(paths)
	return config.GitHubRemotePolicy(remote), err
}

// resolveTemplateRemote resolves the effective owner/repo, ref, and subdir for a
// publish/pull invocation: an explicit flag wins, else the configured
// [template_remote] default. The repo must resolve to a valid owner/repo or it
// is an error. ref may be empty (the publish branch then defaults to the repo
// default branch; pull defaults it to main). path falls back to the built-in
// templates subdir.
func resolveTemplateRemote(paths config.Paths, repoFlag, refFlag, pathFlag string) (repo, ref, path string, err error) {
	return resolveGitHubRemote(paths, repoFlag, refFlag, pathFlag, config.DefaultTemplateRemotePath,
		"no template repo: pass --repo <owner/repo> or set a default with `gitmoot agent template remote set <owner/repo>`",
		loadTemplateGitHubRemote)
}

// runAgentTemplatePublish pushes each selected custom template's rebuilt .md to
// <repo>/<path>/<id>.md via the existing github.Client.UpsertFile (one GET-sha-
// then-PUT call per file). It reports per-file success/failure so a partial batch
// is clear, and only touches the network when invoked (--dry-run is network-free).
func runAgentTemplatePublish(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent template publish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	all := fs.Bool("all", false, "publish all custom templates (skips built-ins)")
	repo := fs.String("repo", "", "GitHub owner/repo to publish to (default: configured remote)")
	path := fs.String("path", "", "subdir within the repo to publish into (default: configured remote or templates)")
	ref := fs.String("ref", "", "branch to commit to (default: configured remote or the repo default branch)")
	message := fs.String("message", "", "commit message for each pushed file")
	create := fs.Bool("create", false, "create the repo (private) if it does not exist")
	dryRun := fs.Bool("dry-run", false, "list what would be published without touching the network")
	ids, flagArgs := leadingIDs(args)
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	ids = append(ids, fs.Args()...)
	ids = compactValues(ids)
	if len(ids) == 0 && !*all {
		fmt.Fprintln(stderr, "agent template publish requires one or more template ids or --all")
		return 2
	}
	if len(ids) > 0 && *all {
		fmt.Fprintln(stderr, "agent template publish accepts either template ids or --all, not both")
		return 2
	}
	return withStoreAndPathsExit(*home, stderr, "publish agent template", func(paths config.Paths, store *db.Store) error {
		repoName, branch, subdir, err := resolveTemplateRemote(paths, *repo, *ref, *path)
		if err != nil {
			return err
		}
		targets, err := exportTargets(context.Background(), store, ids, *all, stderr)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			fmt.Fprintln(stderr, "no custom templates to publish")
			return nil
		}
		repository, err := daemon.ParseRepository(repoName)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "note: publishing %d template(s) to %s; prompts are pushed verbatim — only publish private prompts to a private repo\n", len(targets), repoName)
		if *dryRun {
			for _, target := range targets {
				writeLine(stdout, "would publish %s -> %s/%s/%s.md", target.ID, repoName, subdir, target.ID)
			}
			return nil
		}
		client := newTemplateRemoteClient()
		if *create {
			exists, err := client.RepositoryExists(context.Background(), repository)
			if err != nil {
				return err
			}
			if !exists {
				if err := client.CreateRepository(context.Background(), repository, true); err != nil {
					return err
				}
				writeLine(stdout, "created %s (private)", repoName)
			}
		}
		message := strings.TrimSpace(*message)
		failures := 0
		for _, target := range targets {
			content, err := agenttemplate.Export(target)
			if err != nil {
				fmt.Fprintf(stderr, "publish %s: export failed: %v\n", target.ID, err)
				failures++
				continue
			}
			commitMessage := message
			if commitMessage == "" {
				commitMessage = "Publish agent template " + target.ID
			}
			file, err := client.UpsertFile(context.Background(), github.UpsertFileInput{
				Repo:    repository,
				Path:    subdir + "/" + target.ID + ".md",
				Content: []byte(content),
				Message: commitMessage,
				Branch:  branch,
			})
			if err != nil {
				fmt.Fprintf(stderr, "publish %s: %v\n", target.ID, err)
				failures++
				continue
			}
			writeLine(stdout, "published %s -> %s (%s)", target.ID, file.Path, shortCommit(file.SHA))
		}
		if failures > 0 {
			return fmt.Errorf("%d of %d template(s) failed to publish", failures, len(targets))
		}
		return nil
	})
}

// runAgentTemplatePull bulk-installs templates from a remote subdir via the
// existing AddRemote/update fetch core and auto-versioning UpsertAgentTemplate.
// Conflicts become a new version; identical content is a no-op; built-in and
// retired ids are skipped; a per-file failure does not abort the batch.
func runAgentTemplatePull(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent template pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	all := fs.Bool("all", false, "pull every template found under the remote subdir")
	repo := fs.String("repo", "", "GitHub owner/repo to pull from (default: configured remote)")
	ref := fs.String("ref", "", "git ref to pull from (default: configured remote or main)")
	path := fs.String("path", "", "subdir within the repo to pull from (default: configured remote or templates)")
	dryRun := fs.Bool("dry-run", false, "list what would be pulled without writing")
	ids, flagArgs := leadingIDs(args)
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	ids = append(ids, fs.Args()...)
	ids = compactValues(ids)
	if len(ids) == 0 && !*all {
		fmt.Fprintln(stderr, "agent template pull requires one or more template ids or --all")
		return 2
	}
	if len(ids) > 0 && *all {
		fmt.Fprintln(stderr, "agent template pull accepts either template ids or --all, not both")
		return 2
	}
	return withStoreAndPathsExit(*home, stderr, "pull agent template", func(paths config.Paths, store *db.Store) error {
		repoName, gitRef, subdir, err := resolveTemplateRemote(paths, *repo, *ref, *path)
		if err != nil {
			return err
		}
		fmt.Fprintln(stderr, "note: pulled templates are stored verbatim; only pull from repos you trust")
		results, err := agenttemplate.Pull(context.Background(), store, newAgentTemplateRemoteSource(), repoName, gitRef, subdir, ids, *dryRun)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			fmt.Fprintln(stderr, "no templates to pull")
			return nil
		}
		failures := 0
		for _, result := range results {
			switch result.Outcome {
			case agenttemplate.PullFailed:
				fmt.Fprintf(stderr, "pull %s: %s\n", result.ID, result.Detail)
				failures++
			case agenttemplate.PullSkipped:
				writeLine(stdout, "skipped %s (%s)", result.ID, result.Detail)
			case agenttemplate.PullUnchanged:
				writeLine(stdout, "unchanged %s at %s", result.ID, result.Commit)
			default:
				verb := string(result.Outcome)
				if *dryRun {
					verb = "would " + verb
				}
				writeLine(stdout, "%s %s at %s", verb, result.ID, result.Commit)
			}
		}
		if failures > 0 {
			return fmt.Errorf("%d of %d template(s) failed to pull", failures, len(results))
		}
		return nil
	})
}

func runAgentTemplateRemote(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printAgentTemplateRemoteUsage(stdout)
		return 0
	}
	switch args[0] {
	case "set":
		return runAgentTemplateRemoteSet(args[1:], stdout, stderr)
	case "show":
		return runAgentTemplateRemoteShow(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown agent template remote command %q\n\n", args[0])
		printAgentTemplateRemoteUsage(stderr)
		return 2
	}
}

func printAgentTemplateRemoteUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot agent template remote set <owner/repo> [--ref <ref>] [--path <subdir>]")
	fmt.Fprintln(w, "  gitmoot agent template remote show")
}

func runAgentTemplateRemoteSet(args []string, stdout, stderr io.Writer) int {
	return runGitHubRemoteSet(args, stdout, stderr, githubRemoteCommandOptions{
		Command: "agent template remote", Noun: "template", Section: "template_remote",
		DefaultRef: config.DefaultTemplateRemoteRef, DefaultPath: config.DefaultTemplateRemotePath,
		RefHelp: "default git ref for publish/pull/add", PathHelp: "default subdir holding the template .md files",
		Load: loadTemplateGitHubRemote, Ensure: config.EnsureTemplateRemoteSection,
	})
}

func runAgentTemplateRemoteShow(args []string, stdout, stderr io.Writer) int {
	return runGitHubRemoteShow(args, stdout, stderr, githubRemoteCommandOptions{
		Command: "agent template remote", Noun: "template", Section: "template_remote",
		DefaultRef: config.DefaultTemplateRemoteRef, DefaultPath: config.DefaultTemplateRemotePath,
		Load: loadTemplateGitHubRemote, Ensure: config.EnsureTemplateRemoteSection,
	})
}
