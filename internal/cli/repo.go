package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/daemon"
	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/pathutil"
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
	fmt.Fprintln(w, "  gitmoot repo add owner/repo --path <path> [--poll <duration>] [--force] [--agents-md]")
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
	agentsMD := fs.Bool("agents-md", false, "append the Gitmoot workflow-label discipline to AGENTS.md")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "repo add requires owner/repo")
			return 2
		}
		return 0
	}
	repoArg, code := parseRepoPositional(fs, "repo add", args, map[string]struct{}{"home": {}, "path": {}, "poll": {}, "agents-md": {}}, stderr)
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
	healedFrom := ""
	if err != nil {
		requestedPath, pathErr := cleanCheckoutPath(*path)
		if !*force || pathErr != nil {
			fmt.Fprintf(stderr, "repo add: %v\n", err)
			return 1
		}
		originalErr := err
		if healErr := withStore(*home, func(store *db.Store) error {
			existing, getErr := store.GetRepo(context.Background(), repo.FullName())
			if getErr != nil {
				return getErr
			}
			if !sameCheckoutPath(existing.CheckoutPath, requestedPath) {
				return originalErr
			}
			healedFrom = strings.TrimSpace(existing.CheckoutPath)
			var resolveErr error
			record, _, resolveErr = resolveRegisteredRepoRecord(context.Background(), store, repo, existing)
			return resolveErr
		}); healErr != nil {
			fmt.Fprintf(stderr, "repo add: %v\n", originalErr)
			return 1
		}
		writeLine(stderr, "WARN: %s", repoCheckoutHealMessage(repo.FullName(), healedFrom, record.CheckoutPath))
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
	if *agentsMD {
		if already, err := scaffoldAgentsMD(record.CheckoutPath); err != nil {
			writeLine(stderr, "WARN: registered %s but could not write AGENTS.md: %v", record.FullName(), err)
		} else if already {
			writeLine(stdout, "AGENTS.md discipline already present")
		} else {
			writeLine(stdout, "added Gitmoot discipline to AGENTS.md")
		}
	}
	return 0
}

const gitmootDisciplineMarker = "<!-- gitmoot:discipline -->"

const gitmootDisciplineSection = `<!-- gitmoot:discipline -->
## Gitmoot work discipline

- Label every agent dispatch: pass ` + "`--workflow <namespace>/<campaign>`" + ` on every
  ` + "`gitmoot agent ask/run/review/implement`" + ` and ` + "`gitmoot orchestrate`" + ` call.
- Journal milestones: ` + "`gitmoot workflow note <label> \"...\" --author <you>`" + ` at
  kickoff, hand-offs, PR-open, and done — the journal is the only cross-session
  memory and the operator's supervision surface.
- Check ` + "`gitmoot workflow list`" + ` / the dashboard Workflows page before dispatching:
  someone may already be on it.
`

// scaffoldAgentsMD is intentionally best-effort: repo registration remains the
// durable operation when a checkout is read-only.
func scaffoldAgentsMD(checkout string) (already bool, err error) {
	path := filepath.Join(checkout, "AGENTS.md")
	content, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return false, readErr
	}
	if strings.Contains(string(content), gitmootDisciplineMarker) {
		return true, nil
	}
	appendix := gitmootDisciplineSection
	if len(content) > 0 {
		appendix = "\n" + appendix
	}
	return false, os.WriteFile(path, append(content, []byte(appendix)...), 0644)
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
	var linked, healed bool
	originalCheckout := ""
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		record, err = store.GetRepo(context.Background(), repo.FullName())
		if err != nil {
			return err
		}
		originalCheckout = strings.TrimSpace(record.CheckoutPath)
		resolved, isLinked, wasHealed, err := inspectRegisteredRepoCheckout(context.Background(), store, record)
		if err != nil {
			return err
		}
		record, linked, healed = resolved, isLinked, wasHealed
		return err
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			fmt.Fprintf(stderr, "repo %q not found\n", repo.FullName())
			return 1
		}
		fmt.Fprintf(stderr, "repo doctor: %v\n", err)
		return 1
	}
	if healed {
		writeLine(stdout, "WARN: %s", repoCheckoutHealMessage(repo.FullName(), originalCheckout, record.CheckoutPath))
	}
	if linked {
		writeLine(stdout, "repo: %s warn", repo.FullName())
		writeLine(stdout, "path: %s is a linked worktree", record.CheckoutPath)
		writeLine(stdout, "primary: %s", record.PrimaryCheckoutPath)
		return 1
	}
	writeLine(stdout, "repo: %s ok", repo.FullName())
	writeLine(stdout, "path: %s", record.CheckoutPath)
	writeLine(stdout, "primary: %s", record.PrimaryCheckoutPath)
	writeLine(stdout, "remote: %s", record.RemoteURL)
	if record.DefaultBranch != "" {
		writeLine(stdout, "branch: %s", record.DefaultBranch)
	}
	return 0
}

func inspectRegisteredRepoCheckout(ctx context.Context, store *db.Store, record db.Repo) (db.Repo, bool, bool, error) {
	repo, err := daemon.ParseRepository(record.FullName())
	if err != nil {
		return db.Repo{}, false, false, err
	}
	resolved, healed, err := resolveRegisteredRepoRecord(ctx, store, repo, record)
	if err != nil {
		return db.Repo{}, false, false, err
	}
	linked, err := (gitutil.Client{Dir: resolved.CheckoutPath}).IsLinkedWorktree(ctx)
	if err != nil {
		return db.Repo{}, false, false, err
	}
	return resolved, linked, healed, nil
}

// resolveRepoRecord prefers a registered checkout and self-heals it through the
// stored primary before considering fallbackDir. A first-time implicit
// registration from a linked worktree is pinned to its primary so an ephemeral
// task worktree cannot become the repo-wide base checkout.
func resolveRepoRecord(ctx context.Context, store *db.Store, repo github.Repository, fallbackDir string) (db.Repo, error) {
	existing, err := store.GetRepo(ctx, repo.FullName())
	switch {
	case err == nil && (strings.TrimSpace(existing.CheckoutPath) != "" || strings.TrimSpace(existing.PrimaryCheckoutPath) != ""):
		resolved, _, err := resolveRegisteredRepoRecord(ctx, store, repo, existing)
		return resolved, err
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return db.Repo{}, err
	}
	return repoRecordFromStablePath(ctx, repo, fallbackDir)
}

func resolveRegisteredRepoRecord(ctx context.Context, store *db.Store, repo github.Repository, existing db.Repo) (db.Repo, bool, error) {
	checkout := strings.TrimSpace(existing.CheckoutPath)
	var checkoutErr error
	if checkout != "" {
		resolved, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: checkout})
		if err == nil {
			primary := strings.TrimSpace(existing.PrimaryCheckoutPath)
			if primary == "" {
				primary, err = (gitutil.Client{Dir: checkout}).PrimaryWorktree(ctx)
				if err != nil {
					return db.Repo{}, false, fmt.Errorf("resolve primary worktree for %s: %w", repo.FullName(), err)
				}
				updated, err := store.HealRepoCheckout(ctx, repo.FullName(), checkout, checkout, primary)
				if err != nil {
					return db.Repo{}, false, err
				}
				if !updated {
					current, err := store.GetRepo(ctx, repo.FullName())
					if err != nil {
						return db.Repo{}, false, err
					}
					return resolveRegisteredRepoRecord(ctx, store, repo, current)
				}
			}
			resolved.PrimaryCheckoutPath = primary
			return preserveRegisteredRepoFields(resolved, existing), false, nil
		}
		checkoutErr = err
	} else {
		checkoutErr = errors.New("registered checkout is empty")
	}

	primary := strings.TrimSpace(existing.PrimaryCheckoutPath)
	if primary == "" || sameCheckoutPath(primary, checkout) {
		return db.Repo{}, false, fmt.Errorf("registered checkout %s for %s is unusable and no distinct primary checkout is available: %w", checkout, repo.FullName(), checkoutErr)
	}
	resolved, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: primary})
	if err != nil {
		return db.Repo{}, false, fmt.Errorf("registered checkout %s for %s is unusable (%v); verify primary checkout %s: %w", checkout, repo.FullName(), checkoutErr, primary, err)
	}
	primary, err = (gitutil.Client{Dir: resolved.CheckoutPath}).PrimaryWorktree(ctx)
	if err != nil {
		return db.Repo{}, false, fmt.Errorf("resolve primary worktree for %s from %s: %w", repo.FullName(), resolved.CheckoutPath, err)
	}
	if !sameCheckoutPath(primary, resolved.CheckoutPath) {
		resolved, err = repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: primary})
		if err != nil {
			return db.Repo{}, false, fmt.Errorf("verify resolved primary checkout %s for %s: %w", primary, repo.FullName(), err)
		}
	}
	resolved.PrimaryCheckoutPath = primary
	resolved = preserveRegisteredRepoFields(resolved, existing)
	healed, err := store.HealRepoCheckout(ctx, repo.FullName(), checkout, resolved.CheckoutPath, primary)
	if err != nil {
		return db.Repo{}, false, err
	}
	if !healed {
		current, err := store.GetRepo(ctx, repo.FullName())
		if err != nil {
			return db.Repo{}, false, err
		}
		return resolveRegisteredRepoRecord(ctx, store, repo, current)
	}
	log.Printf("WARNING: %s", repoCheckoutHealMessage(repo.FullName(), checkout, resolved.CheckoutPath))
	return resolved, true, nil
}

func preserveRegisteredRepoFields(resolved, existing db.Repo) db.Repo {
	if strings.TrimSpace(existing.DefaultBranch) != "" {
		resolved.DefaultBranch = existing.DefaultBranch
	}
	resolved.PollInterval = existing.PollInterval
	resolved.Enabled = existing.Enabled
	return resolved
}

func repoCheckoutHealMessage(fullName, from, to string) string {
	return fmt.Sprintf("repo %s checkout self-healed from %s to %s", strings.TrimSpace(fullName), strings.TrimSpace(from), strings.TrimSpace(to))
}

func repoRecordFromPath(ctx context.Context, repo github.Repository, path string) (db.Repo, error) {
	checkout, err := cleanCheckoutPath(path)
	if err != nil {
		return db.Repo{}, err
	}
	return repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: checkout})
}

// repoRecordFromStablePath resolves an operator/cwd-provided checkout but pins
// linked worktrees to their primary. Explicit linked registration is reserved
// for repo add --force.
func repoRecordFromStablePath(ctx context.Context, repo github.Repository, path string) (db.Repo, error) {
	checkout, err := cleanCheckoutPath(path)
	if err != nil {
		return db.Repo{}, err
	}
	client := gitutil.Client{Dir: checkout}
	record, err := repoRecordForCheckout(ctx, repo, client)
	if err != nil {
		return db.Repo{}, err
	}
	primary, err := client.PrimaryWorktree(ctx)
	if err != nil {
		return db.Repo{}, fmt.Errorf("resolve primary worktree: %w", err)
	}
	record.PrimaryCheckoutPath = primary
	linked, err := client.IsLinkedWorktree(ctx)
	if err != nil {
		return db.Repo{}, fmt.Errorf("detect linked worktree: %w", err)
	}
	if !linked || sameCheckoutPath(record.CheckoutPath, primary) {
		return record, nil
	}
	primaryRecord, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: primary})
	if err != nil {
		return db.Repo{}, fmt.Errorf("verify primary checkout: %w", err)
	}
	primaryRecord.PrimaryCheckoutPath = primary
	return primaryRecord, nil
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
