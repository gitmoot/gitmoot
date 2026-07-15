package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/github"
)

// githubRemoteWriteClient is the repository/file write surface shared by
// GitHub-backed artifact publishers.
type githubRemoteWriteClient interface {
	UpsertFile(ctx context.Context, input github.UpsertFileInput) (github.RepositoryFile, error)
	RepositoryExists(ctx context.Context, repo github.Repository) (bool, error)
	CreateRepository(ctx context.Context, repo github.Repository, private bool) error
}

type githubRemoteLoader func(config.Paths) (config.GitHubRemotePolicy, error)

// configuredGitHubRemote returns the configured policy best-effort. A missing
// or invalid home remains equivalent to an unconfigured optional remote.
func configuredGitHubRemote(home string, load githubRemoteLoader) (config.GitHubRemotePolicy, bool) {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return config.GitHubRemotePolicy{}, false
	}
	remote, err := load(paths)
	if err != nil || !remote.Configured() {
		return config.GitHubRemotePolicy{}, false
	}
	return remote, true
}

// resolveGitHubRemote applies the shared precedence rule: explicit flags win,
// then configured repo/ref/path, then the policy's built-in path default.
func resolveGitHubRemote(paths config.Paths, repoFlag, refFlag, pathFlag, defaultPath, missingMessage string, load githubRemoteLoader) (repo, ref, path string, err error) {
	remote, err := load(paths)
	if err != nil {
		return "", "", "", err
	}
	repo = strings.TrimSpace(repoFlag)
	if repo == "" {
		repo = strings.TrimSpace(remote.Repo)
	}
	if repo == "" {
		return "", "", "", errors.New(missingMessage)
	}
	if _, parseErr := daemon.ParseRepository(repo); parseErr != nil {
		return "", "", "", parseErr
	}
	ref = strings.TrimSpace(refFlag)
	if ref == "" {
		ref = strings.TrimSpace(remote.Ref)
	}
	path = strings.Trim(strings.TrimSpace(pathFlag), "/")
	if path == "" {
		path = strings.Trim(strings.TrimSpace(remote.Path), "/")
	}
	if path == "" {
		path = remote.ResolvedPath(defaultPath)
	}
	return repo, ref, path, nil
}

type githubRemoteCommandOptions struct {
	Command     string
	Noun        string
	Section     string
	DefaultRef  string
	DefaultPath string
	RefHelp     string
	PathHelp    string
	Load        githubRemoteLoader
	Ensure      func(config.Paths) error
}

func runGitHubRemoteSet(args []string, stdout, stderr io.Writer, opts githubRemoteCommandOptions) int {
	fs := flag.NewFlagSet(opts.Command+" set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	refHelp := opts.RefHelp
	if refHelp == "" {
		refHelp = "default git ref for publish/pull"
	}
	pathHelp := opts.PathHelp
	if pathHelp == "" {
		pathHelp = "default subdir holding the published files"
	}
	ref := fs.String("ref", "", refHelp)
	path := fs.String("path", "", pathHelp)
	repoArg, flagArgs := leadingID(args)
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if repoArg == "" {
		if fs.NArg() == 1 {
			repoArg = fs.Arg(0)
		} else {
			fmt.Fprintf(stderr, "%s set requires <owner/repo>\n", opts.Command)
			return 2
		}
	} else if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s set requires exactly one <owner/repo>\n", opts.Command)
		return 2
	}
	repository, err := daemon.ParseRepository(repoArg)
	if err != nil {
		fmt.Fprintf(stderr, "set %s remote: %v\n", opts.Noun, err)
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "set %s remote: %v\n", opts.Noun, err)
		return 1
	}
	if err := config.Initialize(paths); err != nil {
		fmt.Fprintf(stderr, "set %s remote: %v\n", opts.Noun, err)
		return 1
	}
	if err := opts.Ensure(paths); err != nil {
		fmt.Fprintf(stderr, "set %s remote: %v\n", opts.Noun, err)
		return 1
	}
	edits := []struct {
		key   string
		value string
		set   bool
	}{
		{"repo", repository.FullName(), true},
		{"ref", strings.TrimSpace(*ref), strings.TrimSpace(*ref) != ""},
		{"path", strings.Trim(strings.TrimSpace(*path), "/"), strings.TrimSpace(*path) != ""},
	}
	for _, edit := range edits {
		if !edit.set {
			continue
		}
		if err := config.SetConfigScalar(paths, []string{opts.Section, edit.key}, config.StringScalar(edit.value)); err != nil {
			fmt.Fprintf(stderr, "set %s remote: %v\n", opts.Noun, err)
			return 1
		}
	}
	writeLine(stdout, "set default %s remote to %s", opts.Noun, repository.FullName())
	return 0
}

func runGitHubRemoteShow(args []string, stdout, stderr io.Writer, opts githubRemoteCommandOptions) int {
	fs := flag.NewFlagSet(opts.Command+" show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s show does not accept positional arguments\n", opts.Command)
		return 2
	}
	remote, ok := configuredGitHubRemote(*home, opts.Load)
	if !ok {
		fmt.Fprintf(stdout, "no default %s remote configured\n", opts.Noun)
		return 0
	}
	writeLine(stdout, "repo: %s", remote.Repo)
	writeLine(stdout, "ref: %s", remote.ResolvedRef(opts.DefaultRef))
	writeLine(stdout, "path: %s", remote.ResolvedPath(opts.DefaultPath))
	return 0
}
