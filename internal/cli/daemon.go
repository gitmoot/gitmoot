package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

func runDaemon(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printDaemonUsage(stdout)
		return 0
	}
	if args[0] != "start" {
		fmt.Fprintf(stderr, "unknown daemon command %q\n\n", args[0])
		printDaemonUsage(stderr)
		return 2
	}
	return runDaemonStart(args[1:], stdout, stderr)
}

func printDaemonUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot daemon start --repo owner/repo --poll 30s")
}

func runDaemonStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("daemon start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "GitHub repository as owner/repo")
	poll := fs.Duration("poll", 30*time.Second, "poll interval")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "daemon start does not accept positional arguments")
		return 2
	}
	repo, err := daemon.ParseRepository(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "invalid repo: %v\n", err)
		return 2
	}
	if *poll <= 0 {
		fmt.Fprintln(stderr, "poll interval must be positive")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = withStore(*home, func(store *db.Store) error {
		checkout, err := resolveDaemonCheckout(ctx, repo, gitutil.Client{Dir: "."})
		if err != nil {
			return err
		}
		gh := github.NewClient(checkout)
		engine := workflow.Engine{
			Store: store,
			MergeGate: workflow.PolicyMergeGate{
				Store:        store,
				GitHub:       gh,
				Git:          gitutil.Client{Dir: checkout},
				DeleteBranch: true,
			},
		}
		fmt.Fprintf(stdout, "watching %s every %s\n", repo.FullName(), poll.String())
		return (daemon.Daemon{
			Repo:         repo,
			PollInterval: *poll,
			Store:        store,
			GitHub:       gh,
			Workflow:     &engine,
		}).Run(ctx)
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "daemon start: %v\n", err)
		return 1
	}
	return 0
}

func resolveDaemonCheckout(ctx context.Context, repo github.Repository, client gitutil.Client) (string, error) {
	root, err := client.Root(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve daemon checkout: %w", err)
	}
	remote, err := client.OriginRemote(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve daemon checkout remote: %w", err)
	}
	remoteRepo, err := gitutil.ParseGitHubRemote(remote)
	if err != nil {
		return "", err
	}
	if remoteRepo.String() != repo.FullName() {
		return "", fmt.Errorf("current checkout origin is %s, not %s", remoteRepo.String(), repo.FullName())
	}
	return root, nil
}
