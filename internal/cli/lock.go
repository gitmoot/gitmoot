package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
)

func runLock(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printLockUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runLockList(args[1:], stdout, stderr)
	case "show":
		return runLockShow(args[1:], stdout, stderr)
	case "release":
		return runLockRelease(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown lock command %q\n\n", args[0])
		printLockUsage(stderr)
		return 2
	}
}

func printLockUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot lock list [--repo owner/repo]")
	fmt.Fprintln(w, "  gitmoot lock show owner/repo <branch>")
	fmt.Fprintln(w, "  gitmoot lock release owner/repo <branch> --owner <agent>")
	fmt.Fprintln(w, "  gitmoot lock release owner/repo <branch> --force")
}

func runLockList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lock list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "repo scope as owner/repo")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "lock list does not accept positional arguments")
		return 2
	}
	repoFullName, ok := normalizeOptionalRepoFlag(*repoFlag, stderr)
	if !ok {
		return 2
	}
	var locks []db.BranchLock
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		locks, err = store.ListBranchLocks(context.Background(), repoFullName)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "lock list: %v\n", err)
		return 1
	}
	for _, lock := range locks {
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", lock.RepoFullName, lock.Branch, lock.Owner)
	}
	return 0
}

func runLockShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lock show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFullName, branch, ok := parseLockTarget(fs, args, stderr, "lock show")
	if !ok {
		return parseLockTargetExitCode(args)
	}
	var lock db.BranchLock
	var events []db.BranchLockEvent
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		lock, err = store.GetBranchLock(context.Background(), repoFullName, branch)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock %s %q not found", repoFullName, branch)
		}
		if err != nil {
			return err
		}
		events, err = store.ListBranchLockEvents(context.Background(), repoFullName, branch)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "lock show: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "repo: %s\n", lock.RepoFullName)
	fmt.Fprintf(stdout, "branch: %s\n", lock.Branch)
	fmt.Fprintf(stdout, "owner: %s\n", lock.Owner)
	for _, event := range events {
		fmt.Fprintf(stdout, "event: %s\t%s\t%s\n", event.Kind, event.Owner, event.Message)
	}
	return 0
}

func runLockRelease(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lock release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	owner := fs.String("owner", "", "agent that owns the lock")
	force := fs.Bool("force", false, "release the lock regardless of owner")
	repoFullName, branch, ok := parseLockTarget(fs, args, stderr, "lock release")
	if !ok {
		return parseLockTargetExitCode(args)
	}
	if *force && strings.TrimSpace(*owner) != "" {
		fmt.Fprintln(stderr, "lock release accepts either --owner or --force, not both")
		return 2
	}
	if !*force && strings.TrimSpace(*owner) == "" {
		fmt.Fprintln(stderr, "lock release requires --owner unless --force is passed")
		return 2
	}
	var releasedLock db.BranchLock
	if err := withStore(*home, func(store *db.Store) error {
		event := db.BranchLockEvent{Kind: "released", Message: "released by gitmoot lock release"}
		if *force {
			event.Kind = "force_released"
			event.Message = "force released by gitmoot lock release"
			lock, released, err := store.ForceReleaseLockWithEvent(context.Background(), repoFullName, branch, event)
			if err != nil {
				return err
			}
			if !released {
				return fmt.Errorf("lock %s %q not found", repoFullName, branch)
			}
			releasedLock = lock
			return nil
		}

		lock, err := store.GetBranchLock(context.Background(), repoFullName, branch)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock %s %q not found", repoFullName, branch)
		}
		if err != nil {
			return err
		}
		if lock.Owner != *owner {
			return fmt.Errorf("lock %s %q is owned by %s, not %s", repoFullName, branch, lock.Owner, *owner)
		}
		released, err := store.ReleaseLockWithEvent(context.Background(), lock, event)
		if err != nil {
			return err
		}
		if !released {
			return fmt.Errorf("lock %s %q was not released", repoFullName, branch)
		}
		releasedLock = lock
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "lock release: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "released lock %s %s owned by %s\n", releasedLock.RepoFullName, releasedLock.Branch, releasedLock.Owner)
	return 0
}

func parseLockTarget(fs *flag.FlagSet, args []string, stderr io.Writer, command string) (string, string, bool) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintf(stderr, "%s requires owner/repo and branch\n", command)
			return "", "", false
		}
		return "", "", false
	}
	if len(args) < 2 {
		fmt.Fprintf(stderr, "%s requires owner/repo and branch\n", command)
		return "", "", false
	}
	repoFullName, ok := normalizeRepoArg(args[0], stderr)
	if !ok {
		return "", "", false
	}
	branch := strings.TrimSpace(args[1])
	if branch == "" {
		fmt.Fprintf(stderr, "%s requires branch\n", command)
		return "", "", false
	}
	if err := fs.Parse(args[2:]); err != nil {
		return "", "", false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s requires owner/repo and branch\n", command)
		return "", "", false
	}
	return repoFullName, branch, true
}

func parseLockTargetExitCode(args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		return 0
	}
	return 2
}

func normalizeOptionalRepoFlag(value string, stderr io.Writer) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", true
	}
	return normalizeRepoArg(value, stderr)
}

func normalizeRepoArg(value string, stderr io.Writer) (string, bool) {
	repo, err := daemon.ParseRepository(value)
	if err != nil {
		fmt.Fprintf(stderr, "invalid repo: %v\n", err)
		return "", false
	}
	return repo.FullName(), true
}
