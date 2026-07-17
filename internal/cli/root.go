package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/doctor"
	"github.com/gitmoot/gitmoot/internal/runtime"
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
	{name: "version", summary: "show Gitmoot version and build metadata", run: runVersion},
	{name: "config", summary: "show local Gitmoot config paths", run: runConfig},
	{name: "auth", summary: "manage runtime authentication", run: runAuth},
	{name: "key", summary: "manage keycard registry metadata and grants", run: runKey},
	{name: "update", summary: "check for and apply Gitmoot releases", run: runUpdate},
	{name: "setup", summary: "register a repo and initial agent", run: runSetup},
	{name: "repo", summary: "manage watched repositories", run: runRepo},
	{name: "daemon", summary: "run the local PR watcher", run: runDaemon},
	{name: "agent", summary: "manage registered agents", run: runAgent},
	{name: "runtime", summary: "inspect runtime metadata (models, capabilities, usage)", run: runRuntime},
	{name: "sandbox", summary: "probe kernel-enforced runtime sandbox support", run: runSandbox},
	{name: "orchestrate", summary: "Orchestrate work across agents (a coordinator that fans out delegations)", run: runOrchestrate},
	{name: "plugin", summary: "build and inspect Gitmoot agent plugins", run: runPlugin},
	{name: "events", summary: "show local repo events", run: runEvents},
	{name: "status", summary: "show local workflow status", run: runStatus},
	{name: "goal", summary: "import or inspect goals", run: runGoal},
	{name: "task", summary: "run or inspect tasks", run: runTask},
	{name: "job", summary: "inspect and recover jobs", run: runJob},
	{name: "proof", summary: "evidence-graded proof manifest for a work root", run: runProof},
	{name: "workflow", summary: "inspect external-coordinator workflow groups and notes", run: runWorkflowJournal},
	{name: "report", summary: "build and file bug reports", run: runReport},
	{name: "lock", summary: "inspect and release branch locks", run: runLock},
	{name: "interactive", summary: "inspect and answer interactive prompts", run: runInteractive},
	{name: "dashboard", summary: "show a snapshot of local Gitmoot state", run: runDashboard},
	{name: "skillopt", summary: "export and import SkillOpt packages", run: runSkillOpt},
	{name: "memory", summary: "inspect and measure agent persistent memory", run: runMemory},
	{name: "pipeline", summary: "define, run, and manage declarative pipelines", run: runPipeline},
	{name: "bridge", summary: "serve the authenticated localhost HTTP bridge", run: runBridge},
	{name: "activepieces", summary: "bootstrap a local Activepieces wired to the gitmoot bridge", run: runActivepieces},
	{name: "chat", summary: "durable agent chat threads (create, send, inbox)", run: runChat},
	{name: "moot", summary: "convene registered agents into a bounded multi-agent brainstorm", run: runMoot},
	{name: "router", summary: "inspect execution-grounded routing telemetry (advisory)", run: runRouter},
}

func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printUsage(stdout)
		return 0
	}

	name := args[0]
	if name == "sandbox-exec" {
		return runSandboxExec(args[1:], stdout, stderr)
	}
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
	width := 0
	for _, cmd := range rootCommands {
		if len(cmd.name) > width {
			width = len(cmd.name)
		}
	}
	for _, cmd := range rootCommands {
		fmt.Fprintf(w, "  %-*s  %s\n", width, cmd.name, cmd.summary)
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
	jsonOutput := fs.Bool("json", false, "print the checks as a JSON array instead of the text table")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// The one-shot `gitmoot doctor` opts into the live claude probe (LiveProbe)
	// so a cached-creds box is reported accurately rather than false-warned. The
	// dashboard (dashboard_tui.go) leaves LiveProbe false so its refresh loop
	// never spawns claude.
	// Resolve paths best-effort so the daemon-aware claude auth check (#427) can
	// locate the running daemon. A failure here (no initialized home) just leaves
	// Paths zero, which skips the daemon check and keeps the shell-local one.
	paths, _ := config.DefaultPaths()
	buildStatus := daemonBuildStatus(paths)
	probeRunner, authState, authSource, authErr := runtimeJobRunnerWithAuth("", runtime.ClaudeRuntime, nil)
	checker := doctor.Checker{
		Dir:               *repoDir,
		LiveProbe:         true,
		Paths:             paths,
		ClaudeProbeRunner: probeRunner,
		ClaudeAuthLookup:  runtimeAuthEffectiveLookup(authState, runtimeAuthEnvLookup),
		ClaudeAuthSource:  authSource,
		ClaudeAuthError:   authErr,
		SkipDaemonAuth:    true,
		Build:             &buildStatus,
	}
	checks := checker.Run(context.Background())
	// #631: surface a stale backlog of blocked jobs (each paused awaiting a human)
	// so an operator knows they can be bulk-dismissed. Best-effort and appended to
	// both the text table and the --json array; the dashboard's cheaper
	// GlobalChecks path deliberately omits it so a refresh never opens the store.
	if check, ok := blockedBacklogDoctorCheck(paths); ok {
		checks = append(checks, check)
	}
	checks = append(checks, repoCheckoutDoctorChecks(paths)...)
	if *jsonOutput {
		type checkJSON struct {
			Name     string `json:"name"`
			Status   string `json:"status"`
			OK       bool   `json:"ok"`
			Required bool   `json:"required"`
			Detail   string `json:"detail"`
		}
		out := make([]checkJSON, 0, len(checks))
		for _, check := range checks {
			status := "ok"
			if !check.OK {
				if check.Required {
					status = "fail"
				} else {
					status = "warn"
				}
			}
			out = append(out, checkJSON{Name: check.Name, Status: status, OK: check.OK, Required: check.Required, Detail: check.Detail})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "doctor: %v\n", err)
			return 1
		}
		if err := doctor.FailedRequired(checks); err != nil {
			return 1
		}
		return 0
	}
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

// repoCheckoutDoctorChecks is the store-aware aggregate sweep for `gitmoot
// doctor`. It stays in the CLI layer so the doctor package remains store-less.
func repoCheckoutDoctorChecks(paths config.Paths) []doctor.Check {
	if strings.TrimSpace(paths.Database) == "" {
		return nil
	}
	if _, err := os.Stat(paths.Database); err != nil {
		return nil
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		return nil
	}
	defer store.Close()
	repos, err := store.ListRepos(context.Background())
	if err != nil {
		return nil
	}
	checks := make([]doctor.Check, 0, len(repos))
	for _, repo := range repos {
		originalCheckout := strings.TrimSpace(repo.CheckoutPath)
		resolved, linked, healed, err := inspectRegisteredRepoCheckout(context.Background(), store, repo)
		check := doctor.Check{Name: "repo checkout", Required: false}
		switch {
		case err != nil:
			check.Detail = fmt.Sprintf("%s: %v", repo.FullName(), err)
			if recorded := strings.TrimSpace(repo.PrimaryCheckoutPath); recorded != "" {
				if _, statErr := os.Stat(recorded); statErr == nil {
					check.Detail += fmt.Sprintf("; primary checkout %s is available", recorded)
				} else {
					check.Detail += fmt.Sprintf("; primary checkout %s is unavailable", recorded)
				}
			}
		case healed:
			check.OK = true
			check.Detail = repoCheckoutHealMessage(repo.FullName(), originalCheckout, resolved.CheckoutPath)
		case linked:
			check.Detail = fmt.Sprintf("%s: registered checkout %s is a linked worktree; use primary checkout %s", repo.FullName(), resolved.CheckoutPath, resolved.PrimaryCheckoutPath)
		default:
			check.OK = true
			check.Detail = fmt.Sprintf("%s: registered checkout %s is primary", repo.FullName(), resolved.CheckoutPath)
		}
		checks = append(checks, check)
	}
	return checks
}

func pathsFromFlag(home string) (config.Paths, error) {
	if home != "" {
		return config.PathsForHome(home), nil
	}
	return config.DefaultPaths()
}
