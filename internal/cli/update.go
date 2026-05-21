package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/jerryfane/gitmoot/internal/buildinfo"
	"github.com/jerryfane/gitmoot/internal/update"
)

func runUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	checkOnly := fs.Bool("check", false, "check for the latest GitHub release without updating")
	restartDaemon := fs.Bool("restart-daemon", false, "restart the daemon after a successful update")
	repo := fs.String("repo", update.DefaultRepo, "GitHub repository to check for releases")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "update does not accept positional arguments")
		return 2
	}

	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "resolve executable: %v\n", err)
		return 1
	}
	result, err := update.Check(context.Background(), update.GhReleaseClient{}, *repo, buildinfo.Current(), "", "", executable)
	if err != nil {
		fmt.Fprintf(stderr, "update check: %v\n", err)
		return 1
	}
	printUpdateCheck(stdout, result)
	if *checkOnly || result.UpToDate {
		return 0
	}

	applied, err := update.Apply(context.Background(), nil, *repo, result, executable)
	if err != nil {
		fmt.Fprintf(stderr, "update: %v\n", err)
		return 1
	}
	if !applied.Applied {
		writeLine(stdout, "auto-update unavailable: %s", applied.Reason)
		printManualCommands(stdout, result.ManualCommands)
		if *restartDaemon {
			writeLine(stdout, "daemon restart skipped: update was not applied")
		}
		return 0
	}
	writeLine(stdout, "updated gitmoot to %s", result.LatestVersion)
	if !*restartDaemon {
		return 0
	}
	return runDaemonRestartFromExecutable(executable, *home, stdout, stderr)
}

func printUpdateCheck(w io.Writer, result update.CheckResult) {
	writeLine(w, "current: %s", result.CurrentVersion)
	writeLine(w, "latest: %s", result.LatestVersion)
	if result.ReleaseURL != "" {
		writeLine(w, "release: %s", result.ReleaseURL)
	}
	if result.UpToDate {
		writeLine(w, "status: up to date")
		return
	}
	if result.NoRelease {
		writeLine(w, "status: no release found")
		return
	}
	writeLine(w, "status: update available")
	if result.Asset != nil {
		writeLine(w, "asset: %s", result.Asset.Name)
	} else {
		writeLine(w, "asset: no exact asset for this platform")
	}
}

func printManualCommands(w io.Writer, commands []string) {
	if len(commands) == 0 {
		return
	}
	writeLine(w, "manual update commands:")
	for _, command := range commands {
		writeLine(w, "  %s", command)
	}
}

func runDaemonRestartFromExecutable(executable string, home string, stdout, stderr io.Writer) int {
	args := []string{"daemon", "restart"}
	if home != "" {
		args = append(args, "--home", home)
	}
	cmd := exec.CommandContext(context.Background(), executable, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "daemon restart: %v\n", err)
		return 1
	}
	return 0
}
