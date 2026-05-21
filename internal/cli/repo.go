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
	"time"

	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/pathutil"
)

func runRepo(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printRepoUsage(stdout)
		return 0
	}
	switch args[0] {
	case "add":
		return runRepoAdd(args[1:], stdout, stderr)
	case "list":
		return runRepoList(args[1:], stdout, stderr)
	case "remove":
		return runRepoRemove(args[1:], stdout, stderr)
	case "doctor":
		return runRepoDoctor(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown repo command %q\n\n", args[0])
		printRepoUsage(stderr)
		return 2
	}
}

func printRepoUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot repo add owner/repo --path <path> [--poll 30s]")
	fmt.Fprintln(w, "  gitmoot repo list")
	fmt.Fprintln(w, "  gitmoot repo remove owner/repo")
	fmt.Fprintln(w, "  gitmoot repo doctor owner/repo")
}

func runRepoAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	path := fs.String("path", ".", "local checkout path")
	poll := fs.Duration("poll", 30*time.Second, "poll interval")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "repo add requires owner/repo")
			return 2
		}
		return 0
	}
	repoArg := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "repo add requires exactly one owner/repo")
		return 2
	}
	if *poll <= 0 {
		fmt.Fprintln(stderr, "poll interval must be positive")
		return 2
	}
	repo, err := daemon.ParseRepository(repoArg)
	if err != nil {
		fmt.Fprintf(stderr, "invalid repo: %v\n", err)
		return 2
	}
	record, err := repoRecordFromPath(context.Background(), repo, *path)
	if err != nil {
		fmt.Fprintf(stderr, "repo add: %v\n", err)
		return 1
	}
	record.PollInterval = poll.String()
	if err := withStore(*home, func(store *db.Store) error {
		return store.UpsertRepo(context.Background(), record)
	}); err != nil {
		fmt.Fprintf(stderr, "repo add: %v\n", err)
		return 1
	}
	writeLine(stdout, "registered %s at %s", record.FullName(), record.CheckoutPath)
	return 0
}

func runRepoList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "repo list does not accept positional arguments")
		return 2
	}
	var repos []db.Repo
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		repos, err = store.ListRepos(context.Background())
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "repo list: %v\n", err)
		return 1
	}
	for _, repo := range repos {
		enabled := "disabled"
		if repo.Enabled {
			enabled = "enabled"
		}
		writeLine(stdout, "%-24s %-8s %-8s %s", repo.FullName(), enabled, repo.PollInterval, repo.CheckoutPath)
	}
	return 0
}

func runRepoRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "repo remove requires owner/repo")
			return 2
		}
		return 0
	}
	repoArg := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "repo remove requires exactly one owner/repo")
		return 2
	}
	repo, err := daemon.ParseRepository(repoArg)
	if err != nil {
		fmt.Fprintf(stderr, "invalid repo: %v\n", err)
		return 2
	}
	var removed bool
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		removed, err = store.RemoveRepo(context.Background(), repo.FullName())
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "repo remove: %v\n", err)
		return 1
	}
	if !removed {
		fmt.Fprintf(stderr, "repo %q not found\n", repo.FullName())
		return 1
	}
	writeLine(stdout, "removed %s", repo.FullName())
	return 0
}

func runRepoDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "repo doctor requires owner/repo")
			return 2
		}
		return 0
	}
	repoArg := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "repo doctor requires exactly one owner/repo")
		return 2
	}
	repo, err := daemon.ParseRepository(repoArg)
	if err != nil {
		fmt.Fprintf(stderr, "invalid repo: %v\n", err)
		return 2
	}
	var record db.Repo
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		record, err = store.GetRepo(context.Background(), repo.FullName())
		return err
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fmt.Fprintf(stderr, "repo %q not found\n", repo.FullName())
			return 1
		}
		fmt.Fprintf(stderr, "repo doctor: %v\n", err)
		return 1
	}
	validated, err := repoRecordFromPath(context.Background(), repo, record.CheckoutPath)
	if err != nil {
		fmt.Fprintf(stderr, "repo doctor: %v\n", err)
		return 1
	}
	writeLine(stdout, "repo: %s ok", repo.FullName())
	writeLine(stdout, "path: %s", validated.CheckoutPath)
	writeLine(stdout, "remote: %s", validated.RemoteURL)
	if validated.DefaultBranch != "" {
		writeLine(stdout, "branch: %s", validated.DefaultBranch)
	}
	return 0
}

func repoRecordFromPath(ctx context.Context, repo github.Repository, path string) (db.Repo, error) {
	checkout, err := cleanCheckoutPath(path)
	if err != nil {
		return db.Repo{}, err
	}
	return repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: checkout})
}

func cleanCheckoutPath(path string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	cleaned := pathutil.CleanExpandHome(path, home)
	absolute, err := filepath.Abs(cleaned)
	if err != nil {
		return "", err
	}
	return absolute, nil
}
