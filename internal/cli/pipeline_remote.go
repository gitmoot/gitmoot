package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/github"
	yaml "gopkg.in/yaml.v3"
)

type pipelineRemoteClient interface {
	githubRemoteWriteClient
	DeleteFile(ctx context.Context, input github.DeleteFileInput) (github.RepositoryFile, error)
}

var newPipelineRemoteClient = func() pipelineRemoteClient {
	return github.NewClient("")
}

var newPipelineRemoteSource = func() agenttemplate.RemoteSource {
	return agenttemplate.GHFetcher{}
}

func configuredPipelineRemote(home string) (config.PipelineRemotePolicy, bool) {
	remote, ok := configuredGitHubRemote(home, loadPipelineGitHubRemote)
	return config.PipelineRemotePolicy(remote), ok
}

func loadPipelineGitHubRemote(paths config.Paths) (config.GitHubRemotePolicy, error) {
	remote, err := config.LoadPipelineRemote(paths)
	return config.GitHubRemotePolicy(remote), err
}

func resolvePipelineRemote(paths config.Paths, remoteFlag string) (repo, ref, path string, err error) {
	repo, ref, path, err = resolveGitHubRemote(paths, remoteFlag, "", "", config.DefaultPipelineRemotePath,
		"no pipeline remote: pass --remote <owner/repo> or configure one with `gitmoot pipeline remote set <owner/repo>`",
		loadPipelineGitHubRemote)
	if err != nil {
		return "", "", "", err
	}
	if strings.TrimSpace(ref) == "" {
		ref = config.DefaultPipelineRemoteRef
	}
	return repo, ref, path, nil
}

func pipelineRemoteBundlePath(root, name string) string {
	return strings.Trim(strings.TrimSpace(root), "/") + "/" + strings.TrimSpace(name)
}

func runPipelineRemote(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPipelineRemoteUsage(stdout)
		return 0
	}
	switch args[0] {
	case "set":
		return runGitHubRemoteSet(args[1:], stdout, stderr, githubRemoteCommandOptions{
			Command: "pipeline remote", Noun: "pipeline", Section: "pipeline_remote",
			DefaultRef: config.DefaultPipelineRemoteRef, DefaultPath: config.DefaultPipelineRemotePath,
			Load: loadPipelineGitHubRemote, Ensure: config.EnsurePipelineRemoteSection,
		})
	case "show":
		return runGitHubRemoteShow(args[1:], stdout, stderr, githubRemoteCommandOptions{
			Command: "pipeline remote", Noun: "pipeline", Section: "pipeline_remote",
			DefaultRef: config.DefaultPipelineRemoteRef, DefaultPath: config.DefaultPipelineRemotePath,
			Load: loadPipelineGitHubRemote, Ensure: config.EnsurePipelineRemoteSection,
		})
	default:
		fmt.Fprintf(stderr, "unknown pipeline remote command %q\n\n", args[0])
		printPipelineRemoteUsage(stderr)
		return 2
	}
}

func printPipelineRemoteUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot pipeline remote set <owner/repo> [--ref <ref>] [--path <subdir>]")
	fmt.Fprintln(w, "  gitmoot pipeline remote show")
}

func runPipelinePublish(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printPipelineUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "pipeline publish requires a pipeline name")
			return 2
		}
		return 0
	}
	name, flagArgs := leadingID(args)
	fs := flag.NewFlagSet("pipeline publish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	remoteFlag := fs.String("remote", "", "GitHub owner/repo to publish to (default: configured remote)")
	create := fs.Bool("create", false, "create the repo (private) if it does not exist")
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if name == "" && fs.NArg() == 1 {
		name = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pipeline publish accepts exactly one pipeline name")
		return 2
	}
	if name == "" || !pipelineBundleToken.MatchString(name) {
		fmt.Fprintln(stderr, "pipeline publish requires exactly one name-safe pipeline name")
		return 2
	}
	if err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		ctx := context.Background()
		repoName, ref, root, err := resolvePipelineRemote(paths, *remoteFlag)
		if err != nil {
			return err
		}
		repository, err := daemon.ParseRepository(repoName)
		if err != nil {
			return err
		}
		tempDir, err := os.MkdirTemp("", "gitmoot-pipeline-publish-")
		if err != nil {
			return fmt.Errorf("create pipeline publish temp directory: %w", err)
		}
		defer os.RemoveAll(tempDir)
		if err := exportPipelineBundle(ctx, store, name, tempDir, io.Discard, stderr); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "note: publishing pipeline %s to %s; prompts are pushed verbatim — only publish private prompts to a private repo\n", name, repoName)

		client := newPipelineRemoteClient()
		if *create {
			exists, err := client.RepositoryExists(ctx, repository)
			if err != nil {
				return err
			}
			if !exists {
				if err := client.CreateRepository(ctx, repository, true); err != nil {
					return err
				}
				writeLine(stdout, "created %s (private)", repoName)
			}
		}
		desired, err := readPipelinePublishFiles(tempDir, pipelineRemoteBundlePath(root, name))
		if err != nil {
			return err
		}
		changed, deleted, err := syncPipelineRemoteFiles(ctx, client, newPipelineRemoteSource(), repository, ref, pipelineRemoteBundlePath(root, name), desired, name)
		if err != nil {
			return err
		}
		writeLine(stdout, "published pipeline %s -> %s/%s (%d changed, %d deleted)", name, repoName, pipelineRemoteBundlePath(root, name), changed, deleted)
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "pipeline publish: %v\n", err)
		return 1
	}
	return 0
}

func readPipelinePublishFiles(bundleDir, remoteDir string) (map[string][]byte, error) {
	files := make(map[string][]byte)
	err := filepath.WalkDir(bundleDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("bundle contains non-regular file %s", path)
		}
		rel, err := filepath.Rel(bundleDir, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[strings.Trim(remoteDir, "/")+"/"+filepath.ToSlash(rel)] = content
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read exported pipeline bundle: %w", err)
	}
	return files, nil
}

func syncPipelineRemoteFiles(ctx context.Context, client pipelineRemoteClient, source agenttemplate.RemoteSource, repo github.Repository, ref, remoteDir string, desired map[string][]byte, name string) (changed, deleted int, err error) {
	commit, err := source.ResolveRef(ctx, repo.FullName(), ref)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve pipeline remote ref %s: %w", ref, err)
	}
	existing, err := listPipelineRemoteFiles(ctx, source, repo.FullName(), commit, remoteDir, true)
	if err != nil {
		return 0, 0, err
	}
	message := "Publish pipeline " + name
	for _, path := range existing {
		if _, keep := desired[path]; keep {
			continue
		}
		if _, err := client.DeleteFile(ctx, github.DeleteFileInput{Repo: repo, Path: path, Message: message, Branch: ref}); err != nil {
			return changed, deleted, fmt.Errorf("delete stale remote file %s: %w", path, err)
		}
		deleted++
	}
	paths := make([]string, 0, len(desired))
	for path := range desired {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	existingSet := make(map[string]struct{}, len(existing))
	for _, path := range existing {
		existingSet[path] = struct{}{}
	}
	for _, path := range paths {
		content := desired[path]
		if _, ok := existingSet[path]; ok {
			file, err := source.FetchFile(ctx, repo.FullName(), commit, path)
			if err != nil {
				return changed, deleted, fmt.Errorf("read remote file %s: %w", path, err)
			}
			if bytes.Equal([]byte(file.Content), content) {
				continue
			}
		}
		if _, err := client.UpsertFile(ctx, github.UpsertFileInput{Repo: repo, Path: path, Content: content, Message: message, Branch: ref}); err != nil {
			return changed, deleted, fmt.Errorf("publish %s: %w", path, err)
		}
		changed++
	}
	return changed, deleted, nil
}

func listPipelineRemoteFiles(ctx context.Context, source agenttemplate.RemoteSource, repo, ref, dir string, allowMissing bool) ([]string, error) {
	entries, err := source.ListDir(ctx, repo, ref, dir)
	if err != nil {
		if allowMissing && isGitHubRemoteNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list remote directory %s: %w", dir, err)
	}
	files := make([]string, 0)
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
			return nil, fmt.Errorf("remote directory %s contains unsafe entry %q", dir, entry.Name)
		}
		path := strings.Trim(dir, "/") + "/" + name
		switch entry.Type {
		case "file":
			files = append(files, path)
		case "dir":
			nested, err := listPipelineRemoteFiles(ctx, source, repo, ref, path, false)
			if err != nil {
				return nil, err
			}
			files = append(files, nested...)
		}
	}
	sort.Strings(files)
	return files, nil
}

func isGitHubRemoteNotFound(err error) bool {
	detail := strings.ToLower(err.Error())
	return strings.Contains(detail, "not found") || strings.Contains(detail, "404")
}

type pipelineRemoteListing struct {
	Name         string
	Description  string
	Requirements pipelineBundleRequirements
}

func runPipelinePull(args []string, stdout, stderr io.Writer) int {
	name, flagArgs := leadingID(args)
	fs := flag.NewFlagSet("pipeline pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	remoteFlag := fs.String("remote", "", "GitHub owner/repo to pull from (default: configured remote)")
	repoFlag := fs.String("repo", "", "target repository (owner/name)")
	nameFlag := fs.String("name", "", "import under a different pipeline name")
	list := fs.Bool("list", false, "list pipelines available from the remote")
	force := fs.Bool("force", false, "replace conflicting templates, agents, or pipeline")
	enable := fs.Bool("enable", false, "enable after import (also re-consents declared write authority)")
	var mapFlags repeatedFlag
	fs.Var(&mapFlags, "agent-map", "map an exported agent to a registered local agent (exported=local; repeatable)")
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if name == "" && fs.NArg() == 1 {
		name = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "pipeline pull accepts at most one pipeline name")
		return 2
	}
	if *list {
		if name != "" {
			fmt.Fprintln(stderr, "pipeline pull --list does not accept a pipeline name")
			return 2
		}
		if strings.TrimSpace(*repoFlag) != "" || strings.TrimSpace(*nameFlag) != "" || len(mapFlags) > 0 || *force || *enable {
			fmt.Fprintln(stderr, "pipeline pull --list does not accept import options")
			return 2
		}
		return runPipelinePullList(*home, *remoteFlag, stdout, stderr)
	}
	if name == "" || !pipelineBundleToken.MatchString(name) {
		fmt.Fprintln(stderr, "pipeline pull requires a name-safe pipeline name or --list")
		return 2
	}
	if _, err := daemon.ParseRepository(*repoFlag); err != nil {
		fmt.Fprintf(stderr, "pipeline pull: --repo is required and must be owner/name: %v\n", err)
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline pull: %v\n", err)
		return 1
	}
	if err := config.Initialize(paths); err != nil {
		fmt.Fprintf(stderr, "pipeline pull: %v\n", err)
		return 1
	}
	remoteRepo, ref, root, err := resolvePipelineRemote(paths, *remoteFlag)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline pull: %v\n", err)
		return 1
	}
	source := newPipelineRemoteSource()
	commit, listings, err := loadPipelineRemoteListings(context.Background(), source, remoteRepo, ref, root)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline pull: %v\n", err)
		return 1
	}
	if !pipelineRemoteHasName(listings, name) {
		fmt.Fprintf(stderr, "pipeline pull: unknown pipeline %q in %s/%s; available: %s\n", name, remoteRepo, root, pipelineRemoteAvailableNames(listings))
		return 1
	}
	tempDir, err := os.MkdirTemp("", "gitmoot-pipeline-pull-")
	if err != nil {
		fmt.Fprintf(stderr, "pipeline pull: create temp directory: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tempDir)
	remoteDir := pipelineRemoteBundlePath(root, name)
	if err := fetchPipelineRemoteDirectory(context.Background(), source, remoteRepo, commit, remoteDir, tempDir); err != nil {
		fmt.Fprintf(stderr, "pipeline pull: %v\n", err)
		return 1
	}
	importArgs := []string{tempDir, "--home", *home, "--repo", *repoFlag}
	if strings.TrimSpace(*nameFlag) != "" {
		importArgs = append(importArgs, "--name", *nameFlag)
	}
	for _, mapping := range mapFlags {
		importArgs = append(importArgs, "--agent-map", mapping)
	}
	if *force {
		importArgs = append(importArgs, "--force")
	}
	if *enable {
		importArgs = append(importArgs, "--enable")
	}
	return runPipelineImport(importArgs, stdout, stderr)
}

func runPipelinePullList(home, remoteFlag string, stdout, stderr io.Writer) int {
	paths, err := pathsFromFlag(home)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline pull --list: %v\n", err)
		return 1
	}
	if err := config.Initialize(paths); err != nil {
		fmt.Fprintf(stderr, "pipeline pull --list: %v\n", err)
		return 1
	}
	repo, ref, root, err := resolvePipelineRemote(paths, remoteFlag)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline pull --list: %v\n", err)
		return 1
	}
	_, listings, err := loadPipelineRemoteListings(context.Background(), newPipelineRemoteSource(), repo, ref, root)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline pull --list: %v\n", err)
		return 1
	}
	for _, listing := range listings {
		description := strings.TrimSpace(listing.Description)
		if description == "" {
			description = "-"
		}
		writeLine(stdout, "%s\t%s\t%s", listing.Name, description, pipelineRemoteRequirementsLine(listing.Requirements))
	}
	return 0
}

func loadPipelineRemoteListings(ctx context.Context, source agenttemplate.RemoteSource, repo, ref, root string) (string, []pipelineRemoteListing, error) {
	commit, err := source.ResolveRef(ctx, repo, ref)
	if err != nil {
		return "", nil, fmt.Errorf("resolve pipeline remote ref %s: %w", ref, err)
	}
	entries, err := source.ListDir(ctx, repo, commit, root)
	if err != nil {
		if isGitHubRemoteNotFound(err) {
			return commit, nil, nil
		}
		return "", nil, fmt.Errorf("list pipeline remote %s: %w", root, err)
	}
	listings := make([]pipelineRemoteListing, 0)
	for _, entry := range entries {
		if entry.Type != "dir" || !pipelineBundleToken.MatchString(entry.Name) {
			continue
		}
		path := pipelineRemoteBundlePath(root, entry.Name) + "/bundle.yaml"
		file, err := source.FetchFile(ctx, repo, commit, path)
		if err != nil {
			return "", nil, fmt.Errorf("read %s: %w", path, err)
		}
		listing, err := parsePipelineRemoteListing(entry.Name, []byte(file.Content))
		if err != nil {
			return "", nil, fmt.Errorf("read %s: %w", path, err)
		}
		listings = append(listings, listing)
	}
	sort.Slice(listings, func(i, j int) bool { return listings[i].Name < listings[j].Name })
	return commit, listings, nil
}

func parsePipelineRemoteListing(directoryName string, manifestRaw []byte) (pipelineRemoteListing, error) {
	var manifest pipelineBundleManifest
	if err := yaml.Unmarshal(manifestRaw, &manifest); err != nil {
		return pipelineRemoteListing{}, fmt.Errorf("parse bundle.yaml: %w", err)
	}
	if manifest.Pipeline == "" || manifest.Pipeline != directoryName {
		return pipelineRemoteListing{}, fmt.Errorf("manifest pipeline %q does not match directory %q", manifest.Pipeline, directoryName)
	}
	return pipelineRemoteListing{Name: manifest.Pipeline, Description: manifest.Description, Requirements: manifest.Requirements}, nil
}

func pipelineRemoteRequirementsLine(requirements pipelineBundleRequirements) string {
	runtimes := append([]string(nil), requirements.Runtimes...)
	sort.Strings(runtimes)
	connections := make([]string, 0, len(requirements.Connections))
	for _, connection := range requirements.Connections {
		connections = append(connections, connection.Kind+"/"+connection.Name)
	}
	sort.Strings(connections)
	upstreams := append([]string(nil), requirements.UpstreamPipelines...)
	sort.Strings(upstreams)
	return fmt.Sprintf("requirements: runtimes=%s; connections=%s; upstreams=%s", listOrNone(runtimes), listOrNone(connections), listOrNone(upstreams))
}

func listOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ",")
}

func pipelineRemoteHasName(listings []pipelineRemoteListing, name string) bool {
	for _, listing := range listings {
		if listing.Name == name {
			return true
		}
	}
	return false
}

func pipelineRemoteAvailableNames(listings []pipelineRemoteListing) string {
	names := make([]string, 0, len(listings))
	for _, listing := range listings {
		names = append(names, listing.Name)
	}
	return listOrNone(names)
}

func fetchPipelineRemoteDirectory(ctx context.Context, source agenttemplate.RemoteSource, repo, ref, remoteDir, targetDir string) error {
	files, err := listPipelineRemoteFiles(ctx, source, repo, ref, remoteDir, false)
	if err != nil {
		return err
	}
	for _, remotePath := range files {
		rel := strings.TrimPrefix(remotePath, strings.Trim(remoteDir, "/")+"/")
		if rel == remotePath || rel == "" {
			return fmt.Errorf("remote file %s is outside %s", remotePath, remoteDir)
		}
		localPath := filepath.Join(targetDir, filepath.FromSlash(rel))
		if relPath, err := filepath.Rel(targetDir, localPath); err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
			return fmt.Errorf("remote file %s has unsafe path", remotePath)
		}
		file, err := source.FetchFile(ctx, repo, ref, remotePath)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", remotePath, err)
		}
		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(localPath, []byte(file.Content), 0o600); err != nil {
			return err
		}
	}
	return nil
}
