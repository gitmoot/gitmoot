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
	"strings"
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
	case "set-interval":
		return runRepoSetInterval(args[1:], stdout, stderr)
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

// parseRepoPositional reorders args so the owner/repo positional may appear
// before or after flags, parses them into fs, and returns the single
// positional. The returned code is -1 when parsing succeeded (use repoArg) or a
// process exit code the caller should return immediately (0 for --help, 2 for a
// parse or arity error). stringFlags lists fs flags that consume a value.
func parseRepoPositional(fs *flag.FlagSet, command string, args []string, stringFlags map[string]struct{}, stderr io.Writer) (string, int) {
	parsedArgs, err := reorderFlagArgs(args, stringFlags, nil)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return "", 2
	}
	if err := fs.Parse(parsedArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", 0
		}
		return "", 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "%s requires exactly one owner/repo\n", command)
		return "", 2
	}
	return fs.Arg(0), -1
}

func printRepoUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot repo add owner/repo --path <path> [--poll <duration>] [--force]")
	fmt.Fprintln(w, "  gitmoot repo list")
	fmt.Fprintln(w, "  gitmoot repo set-interval owner/repo (<duration>|default)")
	fmt.Fprintln(w, "  gitmoot repo set-interval --all (<duration>|default)")
	fmt.Fprintln(w, "  gitmoot repo remove owner/repo")
	fmt.Fprintln(w, "  gitmoot repo doctor owner/repo")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "owner/repo may be given before or after the flags.")
}

func runRepoAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	path := fs.String("path", ".", "local checkout path")
	poll := fs.Duration("poll", 30*time.Second, "poll interval")
	force := fs.Bool("force", false, "allow a linked worktree to replace the registered checkout")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "repo add requires owner/repo")
			return 2
		}
		return 0
	}
	repoArg, code := parseRepoPositional(fs, "repo add", args, map[string]struct{}{"home": {}, "path": {}, "poll": {}}, stderr)
	if code >= 0 {
		return code
	}
	if *poll <= 0 {
		fmt.Fprintln(stderr, "poll interval must be positive")
		return 2
	}
	pollExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "poll" {
			pollExplicit = true
		}
	})
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
	client := gitutil.Client{Dir: record.CheckoutPath}
	primary, err := client.PrimaryWorktree(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "repo add: resolve primary worktree: %v\n", err)
		return 1
	}
	record.PrimaryCheckoutPath = primary
	linked, err := client.IsLinkedWorktree(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "repo add: detect linked worktree: %v\n", err)
		return 1
	}
	if linked && !*force {
		fmt.Fprintf(stderr, "repo add: %s is a linked worktree; register the primary checkout at %s instead (use --force to override)\n", record.CheckoutPath, primary)
		return 1
	}
	if pollExplicit {
		record.PollInterval = poll.String()
	}
	if err := withStore(*home, func(store *db.Store) error {
		if *force {
			return store.UpsertRepoForce(context.Background(), record)
		}
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
		interval := repo.PollInterval
		if strings.TrimSpace(interval) == "" {
			interval = "inherit"
		}
		writeLine(stdout, "%-24s %-8s %-8s %s", repo.FullName(), enabled, interval, repo.CheckoutPath)
	}
	return 0
}

func runRepoSetInterval(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo set-interval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	all := fs.Bool("all", false, "set the interval for all registered repositories")
	parsedArgs, err := reorderFlagArgs(args, map[string]struct{}{"home": {}}, map[string]struct{}{"all": {}})
	if err != nil {
		fmt.Fprintf(stderr, "repo set-interval: %v\n", err)
		return 2
	}
	if err := fs.Parse(parsedArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var repoArg, value string
	if *all {
		switch fs.NArg() {
		case 1:
			value = fs.Arg(0)
		case 2:
			// Accept the fully-spelled form from the command synopsis; --all makes
			// the validated owner/repo selector informational.
			repoArg, value = fs.Arg(0), fs.Arg(1)
			if _, err := daemon.ParseRepository(repoArg); err != nil {
				fmt.Fprintf(stderr, "invalid repo: %v\n", err)
				return 2
			}
		default:
			fmt.Fprintln(stderr, "repo set-interval --all requires (<duration>|default)")
			return 2
		}
	} else {
		if fs.NArg() != 2 {
			fmt.Fprintln(stderr, "repo set-interval requires owner/repo and (<duration>|default)")
			return 2
		}
		repoArg, value = fs.Arg(0), fs.Arg(1)
		repo, err := daemon.ParseRepository(repoArg)
		if err != nil {
			fmt.Fprintf(stderr, "invalid repo: %v\n", err)
			return 2
		}
		repoArg = repo.FullName()
	}
	interval := ""
	if value != "default" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			fmt.Fprintf(stderr, "invalid poll interval %q; use a positive Go duration or default\n", value)
			return 2
		}
		interval = parsed.String()
	}
	updated := 0
	if err := withStore(*home, func(store *db.Store) error {
		if !*all {
			updated = 1
			return store.SetRepoPollInterval(context.Background(), repoArg, interval)
		}
		repos, err := store.ListRepos(context.Background())
		if err != nil {
			return err
		}
		for _, repo := range repos {
			if err := store.SetRepoPollInterval(context.Background(), repo.FullName(), interval); err != nil {
				return err
			}
			updated++
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "repo set-interval: %v\n", err)
		return 1
	}
	display := interval
	if display == "" {
		display = "default"
	}
	if *all {
		writeLine(stdout, "set poll interval for %d repos to %s", updated, display)
	} else {
		writeLine(stdout, "set poll interval for %s to %s", repoArg, display)
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
	repoArg, code := parseRepoPositional(fs, "repo remove", args, map[string]struct{}{"home": {}}, stderr)
	if code >= 0 {
		return code
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
	repoArg, code := parseRepoPositional(fs, "repo doctor", args, map[string]struct{}{"home": {}}, stderr)
	if code >= 0 {
		return code
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
	primary, linked, checkoutErr := inspectRegisteredRepoCheckout(context.Background(), nil, record)
	if checkoutErr != nil {
		writeLine(stdout, "repo: %s warn", repo.FullName())
		writeLine(stdout, "path: %s", checkoutErr)
		if strings.TrimSpace(record.PrimaryCheckoutPath) != "" {
			writeLine(stdout, "primary: %s", record.PrimaryCheckoutPath)
		}
		return 1
	}
	if strings.TrimSpace(record.PrimaryCheckoutPath) == "" {
		if err := withStore(*home, func(store *db.Store) error {
			_, err := store.HealRepoCheckout(context.Background(), record.FullName(), record.CheckoutPath, record.CheckoutPath, primary)
			return err
		}); err != nil {
			fmt.Fprintf(stderr, "repo doctor: backfill primary checkout: %v\n", err)
			return 1
		}
	}
	validated, err := repoRecordFromPath(context.Background(), repo, record.CheckoutPath)
	if err != nil {
		fmt.Fprintf(stderr, "repo doctor: %v\n", err)
		return 1
	}
	if linked {
		writeLine(stdout, "repo: %s warn", repo.FullName())
		writeLine(stdout, "path: %s is a linked worktree", validated.CheckoutPath)
		writeLine(stdout, "primary: %s", primary)
		return 1
	}
	writeLine(stdout, "repo: %s ok", repo.FullName())
	writeLine(stdout, "path: %s", validated.CheckoutPath)
	writeLine(stdout, "primary: %s", primary)
	writeLine(stdout, "remote: %s", validated.RemoteURL)
	if validated.DefaultBranch != "" {
		writeLine(stdout, "branch: %s", validated.DefaultBranch)
	}
	return 0
}

func inspectRegisteredRepoCheckout(ctx context.Context, store *db.Store, record db.Repo) (string, bool, error) {
	checkout := strings.TrimSpace(record.CheckoutPath)
	if checkout == "" {
		return "", false, fmt.Errorf("registered checkout is empty")
	}
	if _, err := os.Stat(checkout); err != nil {
		if os.IsNotExist(err) {
			return "", false, fmt.Errorf("registered checkout %s is missing", checkout)
		}
		return "", false, fmt.Errorf("inspect registered checkout %s: %w", checkout, err)
	}
	client := gitutil.Client{Dir: checkout}
	linked, err := client.IsLinkedWorktree(ctx)
	if err != nil {
		return "", false, err
	}
	primary := strings.TrimSpace(record.PrimaryCheckoutPath)
	if primary == "" {
		primary, err = client.PrimaryWorktree(ctx)
		if err != nil {
			return "", false, err
		}
		if store != nil {
			if _, err := store.HealRepoCheckout(ctx, record.FullName(), checkout, checkout, primary); err != nil {
				return "", false, err
			}
		}
	}
	return primary, linked, nil
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
