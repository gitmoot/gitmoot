package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/doctor"
)

type command struct {
	name        string
	summary     string
	description string
	run         func(args []string, stdout, stderr io.Writer) int
}

var rootCommands = []command{
	{name: "init", summary: "initialize local Gitmoot state", run: runInit},
	{name: "doctor", summary: "check local Gitmoot prerequisites", run: runDoctor},
	{name: "daemon", summary: "run the local PR watcher", run: notImplemented("daemon")},
	{name: "agent", summary: "manage registered agents", run: notImplemented("agent")},
	{name: "status", summary: "show local workflow status", run: notImplemented("status")},
	{name: "goal", summary: "import or inspect goals", run: notImplemented("goal")},
	{name: "task", summary: "run or inspect tasks", run: notImplemented("task")},
}

func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printUsage(stdout)
		return 0
	}

	name := args[0]
	for _, cmd := range rootCommands {
		if cmd.name == name {
			return cmd.run(args[1:], stdout, stderr)
		}
	}

	fmt.Fprintf(stderr, "unknown command %q\n\n", name)
	printUsage(stderr)
	return 2
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "gitmoot coordinates local AI agent sessions through GitHub PRs.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, cmd := range rootCommands {
		fmt.Fprintf(w, "  %-8s %s\n", cmd.name, cmd.summary)
	}
}

func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "resolve paths: %v\n", err)
		return 1
	}
	if err := config.Initialize(paths); err != nil {
		fmt.Fprintf(stderr, "initialize config: %v\n", err)
		return 1
	}

	store, err := db.Open(paths.Database)
	if err != nil {
		fmt.Fprintf(stderr, "initialize database: %v\n", err)
		return 1
	}
	if err := store.Close(); err != nil {
		fmt.Fprintf(stderr, "close database: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "initialized Gitmoot at %s\n", paths.Home)
	return 0
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoDir := fs.String("repo", ".", "repository directory to check")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	checks := doctor.Checker{Dir: *repoDir}.Run(context.Background())
	for _, check := range checks {
		status := "ok"
		if !check.OK {
			if check.Required {
				status = "fail"
			} else {
				status = "warn"
			}
		}
		fmt.Fprintf(stdout, "%-12s %-5s %s\n", check.Name, status, check.Detail)
	}
	if err := doctor.FailedRequired(checks); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

func pathsFromFlag(home string) (config.Paths, error) {
	if home != "" {
		return config.PathsForHome(home), nil
	}
	return config.DefaultPaths()
}

func notImplemented(name string) func(args []string, stdout, stderr io.Writer) int {
	return func(args []string, stdout, stderr io.Writer) int {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
			fmt.Fprintf(stdout, "Usage:\n  gitmoot %s [flags]\n", name)
			return 0
		}
		fmt.Fprintf(stderr, "gitmoot %s is not implemented yet\n", name)
		if len(args) > 0 {
			fmt.Fprintf(stderr, "received args: %s\n", strings.Join(args, " "))
		}
		return 1
	}
}
