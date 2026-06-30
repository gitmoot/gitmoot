package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/agenttemplate"
	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/subprocess"
)

var newRuntimeFactory = func() runtime.Factory {
	return runtime.Factory{}
}

var agentDoctorRunner subprocess.Runner = subprocess.ExecRunner{}

// runtimeStartAdapterFor builds the runtime adapter `agent restart` re-runs
// Start on. It is a package var so tests can inject a fake adapter (the start
// path's own seams generate runtime-specific refs that are hard to assert on);
// production wires it straight to runtimeStartAdapter.
var runtimeStartAdapterFor = runtimeStartAdapter

func runAgent(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printAgentUsage(stdout)
		return 0
	}
	switch args[0] {
	case "start":
		return runAgentStart(args[1:], stdout, stderr)
	case "ask":
		return runAgentAsk(args[1:], stdout, stderr)
	case "run":
		return runAgentRun(args[1:], stdout, stderr)
	case "review":
		return runAgentReview(args[1:], stdout, stderr)
	case "implement":
		return runAgentImplement(args[1:], stdout, stderr)
	case "type":
		return runAgentType(args[1:], stdout, stderr)
	case "template":
		return runAgentTemplate(args[1:], stdout, stderr)
	case "prompt":
		return runAgentPrompt(args[1:], stdout, stderr)
	case "gc":
		return runAgentGC(args[1:], stdout, stderr)
	case "subscribe":
		return runAgentSubscribe(args[1:], stdout, stderr)
	case "show":
		return runAgentShow(args[1:], stdout, stderr)
	case "list":
		return runAgentList(args[1:], stdout, stderr)
	case "remove":
		return runAgentRemove(args[1:], stdout, stderr)
	case "restart":
		return runAgentRestart(args[1:], stdout, stderr)
	case "doctor":
		return runAgentDoctor(args[1:], stdout, stderr)
	case "allow":
		return runAgentAllow(args[1:], stdout, stderr)
	case "deny":
		return runAgentDeny(args[1:], stdout, stderr)
	case "repos":
		return runAgentRepos(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown agent command %q\n\n", args[0])
		printAgentUsage(stderr)
		return 2
	}
}

func printAgentUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot agent start <name> --runtime codex|claude|kimi --repo owner/repo [--path .] [--template <template-id>] [--model model] [--start-daemon]")
	fmt.Fprintln(w, "  gitmoot agent ask <name> \"message\" [--repo owner/repo] [--background] [--model model] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot agent run <name> \"message\" [--repo owner/repo] [--task task-id] [--pr number] [--head-sha sha] [--branch branch] [--background] [--type type] [--model model] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot agent review <name> \"message\" --repo owner/repo --pr number [--head-sha sha] [--branch branch] [--background] [--type type] [--model model] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot agent implement <name> \"message\" [--repo owner/repo] [--task task-id] [--branch branch] [--background] [--type type] [--model model] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot agent type list|show|set ...")
	fmt.Fprintln(w, "  gitmoot agent template list|show|add|draft|validate|update|diff ...")
	fmt.Fprintln(w, "  gitmoot agent prompt <agent-or-template> [--json]")
	fmt.Fprintln(w, "  gitmoot agent gc")
	fmt.Fprintln(w, "  gitmoot agent subscribe <name> --runtime codex|claude|kimi|shell --session <id|name|last|command> --role <role> [--repo owner/repo...] [--model model] --capability <capability>")
	fmt.Fprintln(w, "    Codex sessions may use a UUID, thread name, or last. Claude sessions may use a UUID or last. Kimi sessions may use a Kimi session id. Shell sessions are commands.")
	fmt.Fprintln(w, "  gitmoot agent allow <name> --repo owner/repo")
	fmt.Fprintln(w, "  gitmoot agent deny <name> --repo owner/repo")
	fmt.Fprintln(w, "  gitmoot agent repos <name>")
	fmt.Fprintln(w, "  gitmoot agent show <name> [--json]")
	fmt.Fprintln(w, "  gitmoot agent list")
	fmt.Fprintln(w, "  gitmoot agent remove <name>")
	fmt.Fprintln(w, "  gitmoot agent restart <name>      # abandon + replace the runtime session (finish in-flight asks first)")
	fmt.Fprintln(w, "  gitmoot agent doctor <name>")
}

type agentAskOptions struct {
	home       string
	repo       string
	jsonOutput bool
	background bool
	typeName   string
	model      string
	force      bool
	agent      string
	message    string
}

func runAgentAsk(args []string, stdout, stderr io.Writer) int {
	options, ok := parseAgentAskOptions(args, stderr)
	if !ok {
		if containsHelpFlag(args) {
			return 0
		}
		return 2
	}
	if !options.force && looksLikeWorkflowOrchestration(options.message) {
		fmt.Fprintln(stderr, "note: this reads like implementation workflow orchestration, but `agent ask` is read-only and will only answer/analyze.")
		fmt.Fprintln(stderr, "If you want Gitmoot to manage worktrees, branches, commits, and PRs, use `gitmoot agent run` or `gitmoot agent implement`.")
	}
	var output localAgentJobOutput
	if err := withStore(options.home, func(store *db.Store) error {
		var err error
		output, err = dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
			RepoFlag:             options.repo,
			Agent:                options.agent,
			Action:               "ask",
			Instructions:         options.message,
			Background:           options.background,
			Type:                 options.typeName,
			Model:                options.model,
			Home:                 options.home,
			SelectedAction:       "ask",
			SelectedActionReason: "explicit agent ask",
			ExecutionPath:        "agent_ask",
			JSONOutput:           options.jsonOutput,
		})
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "agent ask: %v\n", err)
		return 1
	}
	if options.background {
		output.WatchCommand = jobWatchCommand(output.JobID, options.home)
		running, err := daemonIsRunning(options.home)
		if err != nil {
			fmt.Fprintf(stderr, "agent ask: %v\n", err)
			return 1
		}
		output.DaemonRunning = running
	}
	if options.jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "agent ask: %v\n", err)
			return 1
		}
		return 0
	}
	printLocalAgentJobOutput(stdout, output)
	if options.background && !output.DaemonRunning {
		writeLine(stdout, "queued: daemon is not running")
		writeLine(stdout, "process: %s", daemonStartHint(options.home, output.Repo))
		writeLine(stdout, "or: %s", jobRunCommand(output.JobID, options.home))
	}
	return 0
}

func parseAgentAskOptions(args []string, stderr io.Writer) (agentAskOptions, bool) {
	if len(args) == 0 || containsHelpFlag(args) {
		printAgentAskUsage(stderr)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "agent ask requires exactly one agent and one message")
		}
		return agentAskOptions{}, false
	}
	options := agentAskOptions{}
	positionals := []string{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--background":
			options.background = true
		case arg == "--force":
			options.force = true
		case arg == "--type":
			if index+1 >= len(args) {
				fmt.Fprintln(stderr, "agent ask requires a value for --type")
				return agentAskOptions{}, false
			}
			index++
			options.typeName = args[index]
		case arg == "--model":
			if index+1 >= len(args) {
				fmt.Fprintln(stderr, "agent ask requires a value for --model")
				return agentAskOptions{}, false
			}
			index++
			options.model = strings.TrimSpace(args[index])
		case strings.HasPrefix(arg, "--model="):
			options.model = strings.TrimSpace(strings.TrimPrefix(arg, "--model="))
		case arg == "--json":
			options.jsonOutput = true
		case arg == "--repo" || arg == "--home":
			if index+1 >= len(args) {
				fmt.Fprintf(stderr, "agent ask requires a value for %s\n", arg)
				return agentAskOptions{}, false
			}
			index++
			if arg == "--repo" {
				options.repo = args[index]
			} else {
				options.home = args[index]
			}
		case strings.HasPrefix(arg, "--repo="):
			options.repo = strings.TrimPrefix(arg, "--repo=")
		case strings.HasPrefix(arg, "--home="):
			options.home = strings.TrimPrefix(arg, "--home=")
		case strings.HasPrefix(arg, "--type="):
			options.typeName = strings.TrimPrefix(arg, "--type=")
		case strings.HasPrefix(arg, "-") && len(positionals) >= 2:
			fmt.Fprintf(stderr, "unknown agent ask flag %q\n", arg)
			return agentAskOptions{}, false
		default:
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) != 2 {
		fmt.Fprintln(stderr, "agent ask requires exactly one agent and one message")
		return agentAskOptions{}, false
	}
	options.agent = strings.TrimSpace(positionals[0])
	options.message = strings.TrimSpace(positionals[1])
	if options.agent == "" || options.message == "" {
		fmt.Fprintln(stderr, "agent ask requires exactly one agent and one message")
		return agentAskOptions{}, false
	}
	return options, true
}

func containsHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func printAgentAskUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot agent ask <name> \"message\" [--repo owner/repo] [--background] [--type type] [--model model] [--home path] [--json] [--force]")
}

type agentRunOptions struct {
	home                   string
	repo                   string
	jsonOutput             bool
	background             bool
	typeName               string
	model                  string
	taskID                 string
	prNumber               int
	headSHA                string
	branch                 string
	agent                  string
	message                string
	cockpit                bool
	cockpitSession         string
	skipNativeReviewFanout bool
	recipe                 string
}

func runAgentRun(args []string, stdout, stderr io.Writer) int {
	options, ok := parseAgentRunOptions("run", args, stderr)
	if !ok {
		if containsHelpFlag(args) {
			return 0
		}
		return 2
	}
	selected, reason := selectAgentRunAction(options)
	output, exit := dispatchAgentCommand(options, selected, reason, "agent_run", stdout, stderr)
	if exit != 0 {
		return exit
	}
	if options.jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "agent run: %v\n", err)
			return 1
		}
		return 0
	}
	printLocalAgentJobOutput(stdout, output)
	printQueuedDaemonHint(stdout, output, options.background, options.home)
	return 0
}

// runOrchestrate is the top-level `gitmoot orchestrate` command: sugar for
// `gitmoot agent run <agent> --background "<message>"`. The named agent is the
// conductor (coordinator) that returns a delegations[] score; the players
// (child agents) run in parallel or in dependency order, and a finale
// (continuation) reconvenes and synthesizes the results. It forwards to the
// SAME dispatch as `agent run` with the run action and Background forced on, so
// the engine and the delegations schema are untouched.
func runOrchestrate(args []string, stdout, stderr io.Writer) int {
	if containsHelpFlag(args) || len(args) == 0 {
		printOrchestrateUsage(stdout)
		if len(args) == 0 {
			fmt.Fprintln(stderr, "orchestrate requires exactly one agent and one message")
			return 2
		}
		return 0
	}
	options, ok := parseAgentRunOptions("orchestrate", args, stderr)
	if !ok {
		return 2
	}
	// Force the background run path: orchestrate always fans out delegations.
	options.background = true
	// Auto-detect: when orchestrate is launched from inside a Herdr session, default
	// the cockpit on so panes "just appear" without an explicit --cockpit. An
	// explicit --cockpit/--herdr/--cockpit-session already set it; outside Herdr it
	// stays off; and the daemon's [orchestrate] cockpit_mode = off still vetoes.
	options.cockpit = cockpitAutoEnabled(options.cockpit, os.Getenv("HERDR_ENV"))
	selected, reason := selectOrchestrateAction(options)
	output, exit := dispatchAgentCommand(options, selected, reason, "orchestrate", stdout, stderr)
	if exit != 0 {
		return exit
	}
	if options.jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "orchestrate: %v\n", err)
			return 1
		}
		return 0
	}
	printLocalAgentJobOutput(stdout, output)
	printQueuedDaemonHint(stdout, output, options.background, options.home)
	return 0
}

func printOrchestrateUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot orchestrate <agent> \"message\" [--repo owner/repo] [--task task-id] [--pr number] [--head-sha sha] [--branch branch] [--type type] [--model model] [--recipe id] [--cockpit] [--cockpit-session name] [--home path] [--json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Orchestrate work across agents (a coordinator that fans out delegations).")
	fmt.Fprintln(w, "Sugar for `gitmoot agent run <agent> --background`: the named agent is the")
	fmt.Fprintln(w, "conductor (coordinator) that returns a delegations[] score; the players (child")
	fmt.Fprintln(w, "agents) run in parallel or in dependency order, and a finale (continuation)")
	fmt.Fprintln(w, "reconvenes and synthesizes the results.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Example:")
	fmt.Fprintln(w, "  gitmoot orchestrate planner \"audit the auth flow and fan out fixes\" --repo owner/repo")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "--recipe <review-panel|decompose-and-verify|verifier> routes the coordinator")
	fmt.Fprintln(w, "to a named built-in recipe prompt (an opt-in deterministic decomposition")
	fmt.Fprintln(w, "shape) instead of the agent's own prompt; the agent's identity is unchanged.")
}

func runAgentReview(args []string, stdout, stderr io.Writer) int {
	options, ok := parseAgentRunOptions("review", args, stderr)
	if !ok {
		if containsHelpFlag(args) {
			return 0
		}
		return 2
	}
	if strings.TrimSpace(options.repo) == "" {
		fmt.Fprintln(stderr, "agent review requires --repo owner/repo")
		return 2
	}
	if options.prNumber <= 0 {
		fmt.Fprintln(stderr, "agent review requires --pr number")
		return 2
	}
	output, exit := dispatchAgentCommand(options, "review", "explicit agent review", "agent_review", stdout, stderr)
	if exit != 0 {
		return exit
	}
	if options.jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "agent review: %v\n", err)
			return 1
		}
		return 0
	}
	printLocalAgentJobOutput(stdout, output)
	printQueuedDaemonHint(stdout, output, options.background, options.home)
	return 0
}

func runAgentImplement(args []string, stdout, stderr io.Writer) int {
	options, ok := parseAgentRunOptions("implement", args, stderr)
	if !ok {
		if containsHelpFlag(args) {
			return 0
		}
		return 2
	}
	output, exit := dispatchAgentCommand(options, "implement", "explicit agent implement", "agent_implement", stdout, stderr)
	if exit != 0 {
		return exit
	}
	if options.jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "agent implement: %v\n", err)
			return 1
		}
		return 0
	}
	printLocalAgentJobOutput(stdout, output)
	printQueuedDaemonHint(stdout, output, options.background, options.home)
	return 0
}

func dispatchAgentCommand(options agentRunOptions, action string, reason string, executionPath string, stdout, stderr io.Writer) (localAgentJobOutput, int) {
	// Error prefix names the invoked surface: the orchestrate alias names itself;
	// the agent subcommands keep "agent <action>".
	errLabel := "agent " + action
	if executionPath == "orchestrate" {
		errLabel = "orchestrate"
	}
	var output localAgentJobOutput
	if err := withStore(options.home, func(store *db.Store) error {
		var err error
		output, err = dispatchLocalAgentJob(context.Background(), store, localAgentDispatchRequest{
			RepoFlag:               options.repo,
			Agent:                  options.agent,
			Action:                 action,
			Instructions:           options.message,
			Background:             options.background,
			Type:                   options.typeName,
			Model:                  options.model,
			Home:                   options.home,
			TaskID:                 options.taskID,
			PullRequest:            options.prNumber,
			HeadSHA:                options.headSHA,
			Branch:                 options.branch,
			Cockpit:                options.cockpit,
			CockpitSession:         options.cockpitSession,
			SkipNativeReviewFanout: options.skipNativeReviewFanout,
			Recipe:                 options.recipe,
			SelectedAction:         action,
			SelectedActionReason:   reason,
			ExecutionPath:          executionPath,
		})
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", errLabel, err)
		return localAgentJobOutput{}, 1
	}
	if options.background {
		output.WatchCommand = jobWatchCommand(output.JobID, options.home)
		running, err := daemonIsRunning(options.home)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", errLabel, err)
			return localAgentJobOutput{}, 1
		}
		output.DaemonRunning = running
	}
	// The job + delivery succeeded terminally; only a benign post-success
	// advance step errored (e.g. a merge-gate block on the freshly-opened PR, or
	// a 422 "PR already exists" race). Per clig.dev, the result still goes to
	// stdout with exit 0; the advance warning goes to stderr.
	if output.AdvanceError != "" {
		fmt.Fprintf(stderr, "%s: advance warning: %s\n", errLabel, output.AdvanceError)
	}
	return output, 0
}

// agentRunCommandLabel is how a parse/usage error names the command. The agent
// subcommands read as "agent run/review/implement"; the top-level orchestrate
// alias names itself, not "agent orchestrate".
func agentRunCommandLabel(command string) string {
	if command == "orchestrate" {
		return "orchestrate"
	}
	return "agent " + command
}

func parseAgentRunOptions(command string, args []string, stderr io.Writer) (agentRunOptions, bool) {
	label := agentRunCommandLabel(command)
	if len(args) == 0 || containsHelpFlag(args) {
		printAgentRunUsage(stderr, command)
		if len(args) == 0 {
			fmt.Fprintf(stderr, "%s requires exactly one agent and one message\n", label)
		}
		return agentRunOptions{}, false
	}
	options := agentRunOptions{}
	positionals := []string{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--background":
			options.background = true
		case arg == "--json":
			options.jsonOutput = true
		case arg == "--cockpit" || arg == "--herdr":
			options.cockpit = true
		case arg == "--skip-native-review-fanout":
			options.skipNativeReviewFanout = true
		case arg == "--type" || arg == "--model" || arg == "--repo" || arg == "--home" || arg == "--task" || arg == "--pr" || arg == "--head-sha" || arg == "--branch" || arg == "--cockpit-session" || arg == "--recipe":
			if index+1 >= len(args) {
				fmt.Fprintf(stderr, "%s requires a value for %s\n", label, arg)
				return agentRunOptions{}, false
			}
			index++
			if !setAgentRunOption(&options, arg, args[index], stderr) {
				return agentRunOptions{}, false
			}
		case strings.HasPrefix(arg, "--cockpit-session="):
			options.cockpitSession = strings.TrimSpace(strings.TrimPrefix(arg, "--cockpit-session="))
		case strings.HasPrefix(arg, "--type="):
			options.typeName = strings.TrimPrefix(arg, "--type=")
		case strings.HasPrefix(arg, "--model="):
			options.model = strings.TrimSpace(strings.TrimPrefix(arg, "--model="))
		case strings.HasPrefix(arg, "--recipe="):
			options.recipe = strings.TrimSpace(strings.TrimPrefix(arg, "--recipe="))
		case strings.HasPrefix(arg, "--repo="):
			options.repo = strings.TrimPrefix(arg, "--repo=")
		case strings.HasPrefix(arg, "--home="):
			options.home = strings.TrimPrefix(arg, "--home=")
		case strings.HasPrefix(arg, "--task="):
			options.taskID = strings.TrimPrefix(arg, "--task=")
		case strings.HasPrefix(arg, "--pr="):
			if !setAgentRunOption(&options, "--pr", strings.TrimPrefix(arg, "--pr="), stderr) {
				return agentRunOptions{}, false
			}
		case strings.HasPrefix(arg, "--head-sha="):
			options.headSHA = strings.TrimPrefix(arg, "--head-sha=")
		case strings.HasPrefix(arg, "--branch="):
			options.branch = strings.TrimPrefix(arg, "--branch=")
		case strings.HasPrefix(arg, "-") && len(positionals) >= 2:
			fmt.Fprintf(stderr, "unknown %s flag %q\n", label, arg)
			return agentRunOptions{}, false
		default:
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) != 2 {
		fmt.Fprintf(stderr, "%s requires exactly one agent and one message\n", label)
		return agentRunOptions{}, false
	}
	options.agent = strings.TrimSpace(positionals[0])
	options.message = strings.TrimSpace(positionals[1])
	if options.agent == "" || options.message == "" {
		fmt.Fprintf(stderr, "%s requires exactly one agent and one message\n", label)
		return agentRunOptions{}, false
	}
	// --cockpit-session implies --cockpit so naming a session does not silently
	// no-op when the bare --cockpit flag was omitted.
	if strings.TrimSpace(options.cockpitSession) != "" {
		options.cockpit = true
	}
	normalizedRecipe, err := validateRecipeID(options.recipe)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", label, err)
		return agentRunOptions{}, false
	}
	// --recipe is scoped to the coordinator fan-out surfaces (orchestrate and
	// `agent run`). The shared parser/dispatch means review/implement would
	// otherwise silently honor it and override their job template, so reject it
	// there rather than apply it undocumented.
	if normalizedRecipe != "" && command != "orchestrate" && command != "run" {
		fmt.Fprintf(stderr, "%s: recipe is only supported for orchestrate and agent run\n", label)
		return agentRunOptions{}, false
	}
	options.recipe = normalizedRecipe
	return options, true
}

// cockpitAutoEnabled implements the cockpit "auto" default: it stays whatever the
// user explicitly chose, otherwise turns on when launched from inside a Herdr
// session (a non-empty HERDR_ENV). The daemon's [orchestrate] cockpit_mode = off
// is the host-level veto applied later.
func cockpitAutoEnabled(explicit bool, herdrEnv string) bool {
	return explicit || strings.TrimSpace(herdrEnv) != ""
}

func setAgentRunOption(options *agentRunOptions, flagName string, value string, stderr io.Writer) bool {
	value = strings.TrimSpace(value)
	switch flagName {
	case "--type":
		options.typeName = value
	case "--model":
		options.model = value
	case "--repo":
		options.repo = value
	case "--home":
		options.home = value
	case "--task":
		options.taskID = value
	case "--head-sha":
		options.headSHA = value
	case "--branch":
		options.branch = value
	case "--cockpit-session":
		options.cockpitSession = value
	case "--recipe":
		options.recipe = value
	case "--pr":
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			fmt.Fprintln(stderr, "agent command requires a positive integer for --pr")
			return false
		}
		options.prNumber = parsed
	}
	return true
}

func printAgentRunUsage(w io.Writer, command string) {
	fmt.Fprintln(w, "Usage:")
	switch command {
	case "orchestrate":
		fmt.Fprintln(w, "  gitmoot orchestrate <agent> \"message\" [--repo owner/repo] [--task task-id] [--pr number] [--head-sha sha] [--branch branch] [--type type] [--model model] [--recipe id] [--cockpit] [--cockpit-session name] [--skip-native-review-fanout] [--home path] [--json]")
	case "review":
		fmt.Fprintln(w, "  gitmoot agent review <name> \"message\" --repo owner/repo --pr number [--head-sha sha] [--branch branch] [--background] [--type type] [--model model] [--home path] [--json]")
	case "implement":
		fmt.Fprintln(w, "  gitmoot agent implement <name> \"message\" [--repo owner/repo] [--task task-id] [--branch branch] [--background] [--type type] [--model model] [--skip-native-review-fanout] [--home path] [--json]")
	default:
		fmt.Fprintln(w, "  gitmoot agent run <name> \"message\" [--repo owner/repo] [--task task-id] [--pr number] [--head-sha sha] [--branch branch] [--background] [--type type] [--model model] [--recipe id] [--skip-native-review-fanout] [--home path] [--json]")
	}
}

// recipeTemplateIDs is the allowlist of built-in coordinator recipes the
// --recipe flag may select. It is keyed off the agenttemplate registry consts so
// the set stays coupled to the registry of installable templates.
var recipeTemplateIDs = []string{
	agenttemplate.ReviewPanelTemplateID,
	agenttemplate.DecomposeAndVerifyTemplateID,
	agenttemplate.VerifierTemplateID,
}

// validateRecipeID normalizes and validates the --recipe value against the
// built-in coordinator recipe registry. Empty (flag absent) is valid and
// means "use the agent's own prompt".
func validateRecipeID(recipe string) (string, error) {
	recipe = strings.TrimSpace(recipe)
	if recipe == "" {
		return "", nil
	}
	for _, id := range recipeTemplateIDs {
		if recipe == id {
			return recipe, nil
		}
	}
	return "", fmt.Errorf("unknown recipe %q; choose one of %s", recipe, strings.Join(recipeTemplateIDs, ", "))
}

// selectOrchestrateAction routes an orchestrate job by EXPLICIT FLAGS only
// (--task → implement, --pr/--head-sha → review), defaulting to the background
// coordinator ("ask") path. Unlike selectAgentRunAction it ignores message
// keywords, so a natural orchestration prompt that happens to mention
// "review"/"refactor" still queues a coordinator that fans out delegations
// instead of erroring on a missing --pr/--task.
func selectOrchestrateAction(options agentRunOptions) (string, string) {
	if strings.TrimSpace(options.taskID) != "" {
		return "implement", "--task selects implementation workflow"
	}
	if options.prNumber > 0 {
		return "review", "--pr selects review workflow"
	}
	if strings.TrimSpace(options.headSHA) != "" {
		return "review", "--head-sha selects review workflow"
	}
	return "ask", "orchestrate runs a background coordinator that fans out delegations"
}

func selectAgentRunAction(options agentRunOptions) (string, string) {
	if strings.TrimSpace(options.taskID) != "" {
		return "implement", "--task selects implementation workflow"
	}
	if options.prNumber > 0 {
		return "review", "--pr selects review workflow"
	}
	if strings.TrimSpace(options.headSHA) != "" {
		return "review", "--head-sha selects review workflow"
	}
	if looksLikeReviewRequest(options.message) {
		return "review", "message asks for PR review or approval"
	}
	if looksLikeImplementationRequest(options.message) {
		return "implement", "message asks for code, docs, tests, or file changes"
	}
	return "ask", "message is analysis, planning, or a question"
}

func looksLikeWorkflowOrchestration(message string) bool {
	lower := strings.ToLower(message)
	phrases := []string{
		"create branch",
		"commit and push",
		"commit, push",
		"commit changes",
		"make a commit",
		"git commit",
		"push branch",
		"push changes",
		"git push",
		"open pr",
		"open a pr",
		"create pr",
		"create a pr",
		"create pull request",
		"open pull request",
		"merge pr",
		"merge pull request",
		"full implementation workflow",
	}
	for _, phrase := range phrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func looksLikeReviewRequest(message string) bool {
	lower := strings.ToLower(message)
	phrases := []string{"review pr", "review this pr", "review pull request", "code review", "approve pr", "approve pull request", "request changes", "audit pr", "audit pull request"}
	for _, phrase := range phrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func looksLikeImplementationRequest(message string) bool {
	lower := strings.ToLower(message)
	phrases := []string{"implement", "edit", "change file", "update file", "write tests", "add test", "fix bug", "patch", "modify", "refactor", "update docs", "documentation", "write code", "change code", "code change"}
	for _, phrase := range phrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func printQueuedDaemonHint(stdout io.Writer, output localAgentJobOutput, background bool, home string) {
	if background && !output.DaemonRunning {
		writeLine(stdout, "queued: daemon is not running")
		writeLine(stdout, "process: %s", daemonStartHint(home, output.Repo))
		writeLine(stdout, "or: %s", jobRunCommand(output.JobID, home))
	}
}

func daemonIsRunning(home string) (bool, error) {
	paths, err := initializedPaths(home)
	if err != nil {
		return false, err
	}
	pid, _, err := currentDaemonPID(daemonProcessState(paths))
	return pid > 0, err
}

func jobWatchCommand(jobID string, home string) string {
	args := []string{"gitmoot", "job", "watch", jobID}
	if strings.TrimSpace(home) != "" {
		args = append(args, "--home", home)
	}
	return shellArgs(args)
}

func jobRunCommand(jobID string, home string) string {
	args := []string{"gitmoot", "job", "run", jobID}
	if strings.TrimSpace(home) != "" {
		args = append(args, "--home", home)
	}
	return shellArgs(args)
}

func shellArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellArg(arg))
	}
	return strings.Join(quoted, " ")
}

func runAgentType(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printAgentTypeUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runAgentTypeList(args[1:], stdout, stderr)
	case "show":
		return runAgentTypeShow(args[1:], stdout, stderr)
	case "set":
		return runAgentTypeSet(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown agent type command %q\n\n", args[0])
		printAgentTypeUsage(stderr)
		return 2
	}
}

func printAgentTypeUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot agent type list")
	fmt.Fprintln(w, "  gitmoot agent type show <type>")
	fmt.Fprintln(w, "  gitmoot agent type set <type> --runtime codex|claude|kimi --template <template-id> --model <model> --policy workspace-write --max-background 2 --idle-timeout 20m")
}

func runAgentTypeList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent type list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent type list does not accept positional arguments")
		return 2
	}
	types, err := loadAgentTypeConfig(*home)
	if err != nil {
		fmt.Fprintf(stderr, "agent type list: %v\n", err)
		return 1
	}
	names := make([]string, 0, len(types))
	for name := range types {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := types[name]
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%d\n", entry.Name, entry.Runtime, entry.Template, entry.MaxBackground)
	}
	return 0
}

func runAgentTypeShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent type show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "agent type show requires exactly one type")
			return 2
		}
		return 0
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent type show requires exactly one type")
		return 2
	}
	types, err := loadAgentTypeConfig(*home)
	if err != nil {
		fmt.Fprintf(stderr, "agent type show: %v\n", err)
		return 1
	}
	entry, ok := types[strings.TrimSpace(name)]
	if !ok {
		fmt.Fprintf(stderr, "agent type %q not found\n", name)
		return 1
	}
	printAgentType(stdout, entry)
	return 0
}

func runAgentTypeSet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent type set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runtimeName := fs.String("runtime", "", "agent runtime: codex, claude, kimi, or kimi-cli")
	templateID := fs.String("template", "", "agent template")
	model := fs.String("model", "", "default runtime model for this agent type")
	role := fs.String("role", "", "agent role")
	policy := fs.String("policy", "", "agent autonomy policy: auto, read-only, workspace-write, or danger-full-access")
	maxBackground := fs.Int("max-background", -1, "maximum managed background instances")
	idleTimeout := fs.String("idle-timeout", "", "managed instance idle timeout")
	jobTimeout := fs.String("job-timeout", "", "managed job timeout")
	var capabilities repeatedFlag
	fs.Var(&capabilities, "capability", "agent capability, repeatable")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "agent type set requires exactly one type")
			return 2
		}
		return 0
	}
	name := strings.TrimSpace(args[0])
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent type set requires exactly one type")
		return 2
	}
	if !validAgentTypeName(name) {
		fmt.Fprintf(stderr, "invalid agent type %q\n", name)
		return 2
	}
	paths, types, err := loadAgentTypeConfigWithPaths(*home)
	if err != nil {
		fmt.Fprintf(stderr, "agent type set: %v\n", err)
		return 1
	}
	entry := types[name]
	entry.Name = name
	if strings.TrimSpace(*runtimeName) != "" {
		entry.Runtime = strings.TrimSpace(*runtimeName)
	}
	if strings.TrimSpace(entry.Runtime) == "" {
		fmt.Fprintln(stderr, "agent type set requires --runtime for new types")
		return 2
	}
	if _, err := (runtime.Factory{}).Adapter(entry.Runtime); err != nil {
		fmt.Fprintf(stderr, "invalid runtime: %v\n", err)
		return 2
	}
	if entry.Runtime == runtime.ShellRuntime {
		fmt.Fprintln(stderr, "invalid runtime: managed agent types support codex, claude, kimi, or kimi-cli")
		return 2
	}
	if strings.TrimSpace(*templateID) != "" {
		entry.Template = strings.TrimSpace(*templateID)
	}
	if strings.TrimSpace(*model) != "" {
		entry.Model = strings.TrimSpace(*model)
	}
	if strings.TrimSpace(*role) != "" {
		entry.Role = strings.TrimSpace(*role)
	}
	if strings.TrimSpace(*policy) != "" {
		normalized, err := runtime.NormalizeAutonomyPolicy(*policy)
		if err != nil {
			fmt.Fprintf(stderr, "invalid policy: %v\n", err)
			return 2
		}
		entry.AutonomyPolicy = normalized
	}
	if *maxBackground == 0 || *maxBackground < -1 {
		fmt.Fprintln(stderr, "max background must be positive")
		return 2
	}
	if *maxBackground > 0 {
		entry.MaxBackground = *maxBackground
	}
	if strings.TrimSpace(*idleTimeout) != "" {
		parsed, err := time.ParseDuration(*idleTimeout)
		if err != nil {
			fmt.Fprintf(stderr, "invalid idle timeout: %v\n", err)
			return 2
		}
		if parsed <= 0 {
			fmt.Fprintln(stderr, "idle timeout must be positive")
			return 2
		}
		entry.IdleTimeout = strings.TrimSpace(*idleTimeout)
	}
	if strings.TrimSpace(*jobTimeout) != "" {
		parsed, err := time.ParseDuration(*jobTimeout)
		if err != nil {
			fmt.Fprintf(stderr, "invalid job timeout: %v\n", err)
			return 2
		}
		if parsed <= 0 {
			fmt.Fprintln(stderr, "job timeout must be positive")
			return 2
		}
		entry.JobTimeout = strings.TrimSpace(*jobTimeout)
	}
	if len(capabilities) > 0 {
		entry.Capabilities = compactValues(capabilities)
	}
	resolvedRole, resolvedCapabilities, err := resolveAgentDefaults(entry.Template, entry.Role, entry.Capabilities, name, []string{"ask"})
	if err != nil {
		fmt.Fprintf(stderr, "invalid template: %v\n", err)
		return 2
	}
	entry.Role = resolvedRole
	entry.Capabilities = resolvedCapabilities
	if entry.Template != "" {
		if err := withStore(*home, func(store *db.Store) error {
			_, err := loadInstalledTemplate(context.Background(), store, entry.Template)
			return err
		}); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
	}
	if err := config.SaveAgentType(paths, entry); err != nil {
		fmt.Fprintf(stderr, "agent type set: %v\n", err)
		return 1
	}
	writeLine(stdout, "configured agent type %s", name)
	return 0
}

func validAgentTypeName(name string) bool {
	if name == "" {
		return false
	}
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '-' || char == '_':
		default:
			return false
		}
	}
	return true
}

func runAgentGC(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent gc does not accept positional arguments")
		return 2
	}
	var deleted int64
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		deleted, err = store.DeleteExpiredAgentInstances(context.Background(), time.Now().UTC())
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "agent gc: %v\n", err)
		return 1
	}
	writeLine(stdout, "removed %d expired agent instances", deleted)
	return 0
}

func loadAgentTypeConfig(home string) (map[string]config.AgentType, error) {
	_, types, err := loadAgentTypeConfigWithPaths(home)
	return types, err
}

func loadAgentTypeConfigWithPaths(home string) (config.Paths, map[string]config.AgentType, error) {
	paths, err := initializedPaths(home)
	if err != nil {
		return config.Paths{}, nil, err
	}
	types, err := config.LoadAgentTypes(paths)
	return paths, types, err
}

func printAgentType(stdout io.Writer, entry config.AgentType) {
	writeLine(stdout, "name: %s", entry.Name)
	writeLine(stdout, "runtime: %s", entry.Runtime)
	writeLine(stdout, "template: %s", entry.Template)
	writeLine(stdout, "role: %s", entry.Role)
	writeLine(stdout, "capabilities: %s", strings.Join(entry.Capabilities, ","))
	writeLine(stdout, "policy: %s", runtime.NormalizeStoredAutonomyPolicy(entry.AutonomyPolicy))
	writeLine(stdout, "max_background: %d", entry.MaxBackground)
	writeLine(stdout, "idle_timeout: %s", entry.IdleTimeout)
	writeLine(stdout, "job_timeout: %s", entry.JobTimeout)
}

func runAgentStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runtimeName := fs.String("runtime", "", "agent runtime: codex, claude, kimi, or kimi-cli")
	repoFlag := fs.String("repo", "", "allowed repo as owner/repo")
	path := fs.String("path", ".", "local checkout path")
	role := fs.String("role", "", "agent role")
	templateID := fs.String("template", "", "agent template")
	model := fs.String("model", "", "default runtime model for this agent")
	policy := fs.String("policy", "auto", "autonomy policy")
	updateTemplate := fs.Bool("update-template", false, "install or refresh the agent template before starting")
	startDaemon := fs.Bool("start-daemon", false, "start the background daemon after setup")
	var capabilities repeatedFlag
	fs.Var(&capabilities, "capability", "agent capability, repeatable")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "agent start requires exactly one name")
			return 2
		}
		return 0
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent start requires exactly one name")
		return 2
	}
	if strings.TrimSpace(*runtimeName) == "" {
		fmt.Fprintln(stderr, "agent start requires --runtime")
		return 2
	}
	if strings.TrimSpace(*repoFlag) == "" {
		fmt.Fprintln(stderr, "agent start requires --repo")
		return 2
	}
	if *updateTemplate && strings.TrimSpace(*templateID) == "" {
		fmt.Fprintln(stderr, "agent start --update-template requires --template")
		return 2
	}
	repo, err := daemon.ParseRepository(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "invalid repo: %v\n", err)
		return 2
	}
	record, err := repoRecordFromPath(context.Background(), repo, *path)
	if err != nil {
		fmt.Fprintf(stderr, "agent start: %v\n", err)
		return 1
	}
	resolvedRole, resolvedCapabilities, err := resolveAgentDefaults(*templateID, *role, capabilities, "agent", []string{"ask", "review", "implement"})
	if err != nil {
		fmt.Fprintf(stderr, "invalid template: %v\n", err)
		return 2
	}
	agent := runtime.Agent{
		Name:           name,
		Role:           resolvedRole,
		Runtime:        strings.TrimSpace(*runtimeName),
		RepoScope:      repo.FullName(),
		TemplateID:     strings.TrimSpace(*templateID),
		Model:          strings.TrimSpace(*model),
		Capabilities:   resolvedCapabilities,
		AutonomyPolicy: strings.TrimSpace(*policy),
		HealthStatus:   "unknown",
	}
	if err := runtime.ValidateStartRequest(runtime.StartRequest{Agent: agent, Prompt: "preflight"}); err != nil {
		fmt.Fprintf(stderr, "invalid agent: %v\n", err)
		return 2
	}
	if err := runtime.ImplementWritePolicyError(agent.Capabilities, agent.AutonomyPolicy); err != nil {
		fmt.Fprintf(stderr, "invalid agent: %v\n", err)
		return 2
	}
	if agent.Runtime == runtime.ShellRuntime {
		fmt.Fprintln(stderr, "start runtime: shell runtime does not support agent start; use gitmoot agent subscribe --runtime shell --session <command>")
		return 1
	}
	var cachedTemplate db.AgentTemplate
	if err := withStore(*home, func(store *db.Store) error {
		if _, err := store.GetAgent(context.Background(), agent.Name); err == nil {
			return fmt.Errorf("agent %s already exists", agent.Name)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if agent.TemplateID == "" {
			return nil
		}
		if *updateTemplate {
			updated, err := updateTemplateByID(context.Background(), store, agent.TemplateID)
			if err != nil {
				return err
			}
			cachedTemplate = updated
			return nil
		}
		installed, err := loadInstalledTemplate(context.Background(), store, agent.TemplateID)
		if err != nil {
			return err
		}
		cachedTemplate = installed
		return nil
	}); err != nil {
		if strings.HasPrefix(err.Error(), "agent template ") {
			fmt.Fprintf(stderr, "%v\n", err)
		} else {
			fmt.Fprintf(stderr, "agent start: %v\n", err)
		}
		return 1
	}
	prompt := agentStartupPrompt(agent, cachedTemplate)
	adapter, err := runtimeStartAdapter(newRuntimeFactory(), agent.Runtime, record.CheckoutPath)
	if err != nil {
		fmt.Fprintf(stderr, "load adapter: %v\n", err)
		return 1
	}
	started, err := adapter.Start(context.Background(), runtime.StartRequest{Agent: agent, Prompt: prompt})
	if err != nil {
		fmt.Fprintf(stderr, "start runtime: %v\n", err)
		return 1
	}
	agent.RuntimeRef = strings.TrimSpace(started.RuntimeRef)
	if err := runtime.ValidateAgent(agent); err != nil {
		fmt.Fprintf(stderr, "invalid started agent: %v\n", err)
		return 1
	}
	if err := withStore(*home, func(store *db.Store) error {
		if err := store.UpsertRepo(context.Background(), record); err != nil {
			return err
		}
		return persistAgentSubscription(context.Background(), store, agent, []string{repo.FullName()})
	}); err != nil {
		fmt.Fprintf(stderr, "agent start: %v\n", err)
		return 1
	}
	writeLine(stdout, "started %s (%s) for %s", agent.Name, agent.Runtime, repo.FullName())
	writeLine(stdout, "session: %s", agent.RuntimeRef)
	writeLine(stdout, "invoke: /gitmoot %s review", agent.Name)
	if *startDaemon {
		writeLine(stdout, "step: start background daemon")
		return runDaemonStartWithWorkDir([]string{"--home", *home, "--repo", repo.FullName()}, record.CheckoutPath, stdout, stderr)
	}
	writeLine(stdout, "next: cd %s", shellArg(record.CheckoutPath))
	writeLine(stdout, "next: %s", daemonStartHint(*home, repo.FullName()))
	return 0
}

func runAgentSubscribe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent subscribe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runtimeName := fs.String("runtime", "", "agent runtime: codex, claude, kimi, kimi-cli, or shell")
	session := fs.String("session", "", "runtime session reference, last, or shell command")
	role := fs.String("role", "", "agent role")
	templateID := fs.String("template", "", "agent template")
	model := fs.String("model", "", "default runtime model for this agent")
	policy := fs.String("policy", "auto", "autonomy policy")
	var repos repeatedFlag
	var capabilities repeatedFlag
	fs.Var(&repos, "repo", "allowed repo as owner/repo, repeatable")
	fs.Var(&capabilities, "capability", "agent capability, repeatable")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "agent subscribe requires exactly one name")
			return 2
		}
		return 0
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent subscribe requires exactly one name")
		return 2
	}

	normalizedRepos, err := normalizeRepoFlags(repos)
	if err != nil {
		fmt.Fprintf(stderr, "invalid repo: %v\n", err)
		return 2
	}
	repoScope := ""
	if len(normalizedRepos) > 0 {
		repoScope = normalizedRepos[0]
	}
	trimmedTemplateID := strings.TrimSpace(*templateID)
	if trimmedTemplateID != "" {
		if _, ok := agenttemplate.Lookup(trimmedTemplateID); !ok {
			if err := withStore(*home, func(store *db.Store) error {
				_, err := loadInstalledTemplate(context.Background(), store, trimmedTemplateID)
				return err
			}); err != nil {
				if strings.HasPrefix(err.Error(), "agent template ") {
					fmt.Fprintf(stderr, "%v\n", err)
				} else {
					fmt.Fprintf(stderr, "subscribe agent: %v\n", err)
				}
				return 1
			}
		}
	}
	resolvedRole, resolvedCapabilities, err := resolveTemplateDefaults(*templateID, *role, capabilities)
	if err != nil {
		fmt.Fprintf(stderr, "invalid template: %v\n", err)
		return 2
	}
	agent := runtime.Agent{
		Name:           name,
		Role:           resolvedRole,
		Runtime:        *runtimeName,
		RuntimeRef:     *session,
		RepoScope:      repoScope,
		TemplateID:     strings.TrimSpace(*templateID),
		Model:          strings.TrimSpace(*model),
		Capabilities:   resolvedCapabilities,
		AutonomyPolicy: strings.TrimSpace(*policy),
		HealthStatus:   "unknown",
	}
	if err := runtime.ValidateAgent(agent); err != nil {
		fmt.Fprintf(stderr, "invalid agent: %v\n", err)
		return 2
	}
	if err := runtime.ImplementWritePolicyError(agent.Capabilities, agent.AutonomyPolicy); err != nil {
		fmt.Fprintf(stderr, "invalid agent: %v\n", err)
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		if agent.TemplateID != "" {
			if _, err := loadInstalledTemplate(context.Background(), store, agent.TemplateID); err != nil {
				return err
			}
		}
		return persistAgentSubscription(context.Background(), store, agent, normalizedRepos)
	}); err != nil {
		if strings.HasPrefix(err.Error(), "agent template ") {
			fmt.Fprintf(stderr, "%v\n", err)
		} else {
			fmt.Fprintf(stderr, "subscribe agent: %v\n", err)
		}
		return 1
	}
	if len(normalizedRepos) == 0 {
		fmt.Fprintf(stdout, "subscribed %s (%s) with no repo access\n", agent.Name, agent.Runtime)
	} else {
		fmt.Fprintf(stdout, "subscribed %s (%s) for %s\n", agent.Name, agent.Runtime, strings.Join(normalizedRepos, ","))
	}
	return 0
}

func resolveTemplateDefaults(templateID string, role string, capabilities []string) (string, []string, error) {
	return resolveAgentDefaults(templateID, role, capabilities, "", nil)
}

func resolveAgentDefaults(templateID string, role string, capabilities []string, fallbackRole string, fallbackCapabilities []string) (string, []string, error) {
	templateID = strings.TrimSpace(templateID)
	role = strings.TrimSpace(role)
	resolvedCapabilities := compactValues(capabilities)
	if templateID == "" {
		if role == "" {
			role = fallbackRole
		}
		if len(resolvedCapabilities) == 0 {
			resolvedCapabilities = append([]string{}, fallbackCapabilities...)
		}
		return role, resolvedCapabilities, nil
	}
	logicalTemplateID, _ := db.SplitAgentTemplateReference(templateID)
	definition, ok := agenttemplate.Lookup(logicalTemplateID)
	if !ok {
		if err := agenttemplate.ValidateID(logicalTemplateID); err != nil {
			return "", nil, err
		}
		if role == "" {
			if fallbackRole == "" {
				return "", nil, fmt.Errorf("agent template %s does not define a default role; pass --role", templateID)
			}
			role = fallbackRole
		}
		if len(resolvedCapabilities) == 0 {
			if len(fallbackCapabilities) == 0 {
				return "", nil, fmt.Errorf("agent template %s does not define default capabilities; pass --capability", templateID)
			}
			resolvedCapabilities = append([]string{}, fallbackCapabilities...)
		}
		return role, resolvedCapabilities, nil
	}
	if role == "" {
		role = definition.DefaultRole
	}
	if len(resolvedCapabilities) == 0 {
		resolvedCapabilities = append([]string{}, definition.DefaultCapabilities...)
	}
	if !definition.Mutation && containsValue(resolvedCapabilities, "implement") {
		return "", nil, fmt.Errorf("agent template %s does not allow implement capability", definition.ID)
	}
	return role, resolvedCapabilities, nil
}

func loadInstalledTemplate(ctx context.Context, store *db.Store, templateID string) (db.AgentTemplate, error) {
	logicalTemplateID, _ := db.SplitAgentTemplateReference(templateID)
	if agenttemplate.IsRetired(logicalTemplateID) {
		return db.AgentTemplate{}, retiredAgentTemplateError(logicalTemplateID)
	}
	cached, err := store.GetAgentTemplateReference(ctx, templateID)
	if errors.Is(err, sql.ErrNoRows) {
		if _, ok := agenttemplate.Lookup(logicalTemplateID); !ok {
			return db.AgentTemplate{}, fmt.Errorf("agent template %s is not installed; run gitmoot agent template add %s --file <path>", logicalTemplateID, logicalTemplateID)
		}
		return db.AgentTemplate{}, fmt.Errorf("agent template %s is not installed; run gitmoot agent template update %s", logicalTemplateID, logicalTemplateID)
	}
	return cached, err
}

func persistAgentSubscription(ctx context.Context, store *db.Store, agent runtime.Agent, repos []string) error {
	if err := store.UpsertAgent(ctx, dbAgent(agent)); err != nil {
		return err
	}
	return store.ReplaceAgentRepos(ctx, agent.Name, repos)
}

// registerAgentOnly persists an agent record without launching a runtime
// session — the shared body behind the dashboard's create-agent form. The agent
// becomes addressable for jobs; a runtime session starts lazily when work is
// delivered.
func registerAgentOnly(ctx context.Context, store *db.Store, name, runtimeName, templateID, repoFullName string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("agent name is required")
	}
	runtimeName = strings.TrimSpace(runtimeName)
	if runtimeName == "" {
		return errors.New("agent runtime is required")
	}
	agent := runtime.Agent{
		Name:       name,
		Runtime:    runtimeName,
		TemplateID: strings.TrimSpace(templateID),
		RepoScope:  strings.TrimSpace(repoFullName),
	}
	repos := []string{}
	if agent.RepoScope != "" {
		repo, err := daemon.ParseRepository(agent.RepoScope)
		if err != nil {
			return err
		}
		if err := store.UpsertRepo(ctx, db.Repo{Owner: repo.Owner, Name: repo.Name}); err != nil {
			return err
		}
		repos = append(repos, agent.RepoScope)
	}
	return persistAgentSubscription(ctx, store, agent, repos)
}

func agentStartupPrompt(agent runtime.Agent, cachedTemplate db.AgentTemplate) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "You are a Gitmoot-managed agent named %s.\n", agent.Name)
	fmt.Fprintf(&builder, "Runtime: %s\n", agent.Runtime)
	fmt.Fprintf(&builder, "Allowed repo: %s\n", agent.RepoScope)
	fmt.Fprintf(&builder, "Role: %s\n", agent.Role)
	fmt.Fprintf(&builder, "Capabilities: %s\n", strings.Join(agent.Capabilities, ","))
	if agent.TemplateID != "" {
		fmt.Fprintf(&builder, "Template: %s", agent.TemplateID)
		if cachedTemplate.ResolvedCommit != "" {
			fmt.Fprintf(&builder, " @ %s", cachedTemplate.ResolvedCommit)
		}
		builder.WriteString("\n\nTemplate instructions:\n")
		builder.WriteString(strings.TrimRight(agenttemplate.InstructionsForContent(cachedTemplate.Content), "\n"))
		builder.WriteString("\n\n")
	}
	builder.WriteString("Initialize this session for future Gitmoot jobs. Do not edit files, run long tasks, create commits, or open pull requests now. Reply with a short readiness acknowledgment only.")
	return builder.String()
}

func runtimeStartAdapter(factory runtime.Factory, runtimeName string, checkout string) (runtime.Adapter, error) {
	switch runtimeName {
	case runtime.CodexRuntime:
		return runtime.CodexAdapter{Runner: factory.Runner, Dir: checkout}, nil
	case runtime.ClaudeRuntime:
		return runtime.ClaudeAdapter{Runner: factory.Runner, Dir: checkout}, nil
	case runtime.KimiRuntime:
		return runtime.KimiAdapter{Runner: factory.Runner, Dir: checkout}, nil
	case runtime.KimiCLIRuntime:
		return runtime.KimiCLIAdapter{Runner: factory.Runner, Dir: checkout}, nil
	case runtime.ShellRuntime:
		return runtime.ShellAdapter{Runner: factory.Runner, Dir: checkout}, nil
	default:
		return nil, fmt.Errorf("unsupported runtime: %s", runtimeName)
	}
}

func daemonStartHint(home string, repo string) string {
	args := []string{"gitmoot", "daemon", "start"}
	if strings.TrimSpace(home) != "" {
		args = append(args, "--home", home)
	}
	args = append(args, "--repo", repo)
	return shellArgs(args)
}

func shellArg(value string) string {
	if value == "" {
		return "''"
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '_', '-', '.', '/', ':', '@', '%', '+', '=', ',':
			continue
		}
		return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
	}
	return value
}

func compactValues(values []string) []string {
	compacted := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			compacted = append(compacted, value)
		}
	}
	return compacted
}

func containsValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func runAgentList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var agents []db.Agent
	agentRepos := map[string][]string{}
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		agents, err = store.ListAgents(context.Background())
		if err != nil {
			return err
		}
		for _, agent := range agents {
			repos, err := store.ListAgentRepos(context.Background(), agent.Name)
			if err != nil {
				return err
			}
			agentRepos[agent.Name] = repos
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "list agents: %v\n", err)
		return 1
	}
	for _, agent := range agents {
		if isEphemeralAgentName(agent.Name) {
			continue // transient spawn-from-spec workers are not part of the registry
		}
		fmt.Fprintf(stdout, "%-16s %-8s %-12s %-20s %s\n", agent.Name, agent.Runtime, agent.Role, strings.Join(agentRepos[agent.Name], ","), strings.Join(agent.Capabilities, ","))
	}
	return 0
}

// isEphemeralAgentName reports whether an agent name is a transient ephemeral
// worker materialized from a delegation spec (#325). Such workers are persisted
// only for the duration of their job and auto-disposed; they are excluded from
// the registry listings (mirroring the dashboard TUI's isEphemeralAgent filter).
func isEphemeralAgentName(name string) bool {
	return strings.Contains(name, "-ephemeral-")
}

type agentShowOutput struct {
	Name         string   `json:"name"`
	Runtime      string   `json:"runtime"`
	RuntimeRef   string   `json:"runtime_ref"`
	Role         string   `json:"role"`
	Capabilities []string `json:"capabilities"`
	Policy       string   `json:"policy"`
	TemplateID   string   `json:"template_id,omitempty"`
	HealthStatus string   `json:"health_status"`
	RepoScope    string   `json:"repo_scope,omitempty"`
	AllowedRepos []string `json:"allowed_repos"`
}

func runAgentShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print agent as JSON")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "agent show requires exactly one name")
			return 2
		}
		return 0
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent show requires exactly one name")
		return 2
	}

	var output agentShowOutput
	if err := withStore(*home, func(store *db.Store) error {
		agent, err := store.GetAgent(context.Background(), name)
		if err != nil {
			return err
		}
		repos, err := store.ListAgentRepos(context.Background(), agent.Name)
		if err != nil {
			return err
		}
		output = agentShowOutput{
			Name:         agent.Name,
			Runtime:      agent.Runtime,
			RuntimeRef:   agent.RuntimeRef,
			Role:         agent.Role,
			Capabilities: agent.Capabilities,
			Policy:       runtime.NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy),
			TemplateID:   agent.TemplateID,
			HealthStatus: agent.HealthStatus,
			RepoScope:    agent.RepoScope,
			AllowedRepos: repos,
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "agent show: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "agent show: %v\n", err)
			return 1
		}
		return 0
	}
	writeLine(stdout, "name: %s", output.Name)
	writeLine(stdout, "runtime: %s", output.Runtime)
	writeLine(stdout, "runtime_ref: %s", output.RuntimeRef)
	writeLine(stdout, "role: %s", output.Role)
	writeLine(stdout, "capabilities: %s", strings.Join(output.Capabilities, ","))
	writeLine(stdout, "policy: %s", output.Policy)
	writeLine(stdout, "template: %s", emptyText(output.TemplateID))
	writeLine(stdout, "health: %s", output.HealthStatus)
	writeLine(stdout, "repo_scope: %s", emptyText(output.RepoScope))
	writeLine(stdout, "allowed_repos: %s", strings.Join(output.AllowedRepos, ","))
	return 0
}

func runAgentAllow(args []string, stdout, stderr io.Writer) int {
	return runAgentAccessChange("allow", args, stdout, stderr)
}

func runAgentDeny(args []string, stdout, stderr io.Writer) int {
	return runAgentAccessChange("deny", args, stdout, stderr)
}

func runAgentAccessChange(action string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "repo scope as owner/repo")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintf(stderr, "agent %s requires exactly one name\n", action)
			return 2
		}
		return 0
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "agent %s requires exactly one name\n", action)
		return 2
	}
	repo, err := normalizeRepoFlag(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "invalid repo: %v\n", err)
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		if _, err := store.GetAgent(context.Background(), name); err != nil {
			return err
		}
		if action == "allow" {
			return store.AllowAgentRepo(context.Background(), name, repo)
		}
		_, err := store.DenyAgentRepo(context.Background(), name, repo)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "agent %s: %v\n", action, err)
		return 1
	}
	if action == "allow" {
		fmt.Fprintf(stdout, "allowed %s on %s\n", name, repo)
	} else {
		fmt.Fprintf(stdout, "denied %s on %s\n", name, repo)
	}
	return 0
}

func runAgentRepos(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent repos", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "agent repos requires exactly one name")
			return 2
		}
		return 0
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent repos requires exactly one name")
		return 2
	}
	var repos []string
	if err := withStore(*home, func(store *db.Store) error {
		if _, err := store.GetAgent(context.Background(), name); err != nil {
			return err
		}
		var err error
		repos, err = store.ListAgentRepos(context.Background(), name)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "agent repos: %v\n", err)
		return 1
	}
	for _, repo := range repos {
		writeLine(stdout, "%s", repo)
	}
	return 0
}

func runAgentRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "agent remove requires exactly one name")
			return 2
		}
		return 0
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent remove requires exactly one name")
		return 2
	}
	var removed bool
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		removed, err = store.RemoveAgent(context.Background(), name)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "remove agent: %v\n", err)
		return 1
	}
	if !removed {
		fmt.Fprintf(stderr, "agent %q not found\n", name)
		return 1
	}
	fmt.Fprintf(stdout, "removed %s\n", name)
	return 0
}

// runAgentRestart rebinds a registered agent to a fresh runtime session in
// place: it re-runs adapter.Start for a new runtime_ref and persists ONLY that
// ref plus health="unknown", preserving every other field by reading the
// existing record, mutating two fields, and writing it back (UpsertAgent's
// ON CONFLICT is a full-row PUT). Pure preservation — no override flags, no
// rebuild-from-flags. The old session is abandoned, not torn down (the
// runtime_ref is a local resume handle).
func runAgentRestart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent restart", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "agent restart requires exactly one name")
			return 2
		}
		return 0
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent restart requires exactly one name")
		return 2
	}

	var (
		existing db.Agent
		record   db.Repo
		cached   db.AgentTemplate
	)
	// Load + busy pre-flight: resolve the existing agent and its checkout, and
	// refuse an agent with queued/running jobs BEFORE calling adapter.Start so a
	// refusal leaves runtime_ref untouched and never starts a session. This is a
	// best-effort pre-flight guard, NOT atomic with the rebind: the count and the
	// later runtime_ref write happen in separate store sessions with adapter.Start
	// between them, so a job enqueued in that window isn't re-checked. That is
	// benign for this manual recovery verb — restart abandons and replaces the
	// session rather than orphaning work.
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		existing, err = store.GetAgent(context.Background(), name)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("agent %q not found; use agent start to create it", name)
		}
		if err != nil {
			return err
		}
		if existing.Runtime == runtime.ShellRuntime {
			return fmt.Errorf("agent %q uses the shell runtime, which has no startable session", name)
		}
		active, err := store.AgentActiveJobCount(context.Background(), name)
		if err != nil {
			return err
		}
		if active > 0 {
			return fmt.Errorf("agent %s has %d queued or running job(s); cancel them first: %w", name, active, db.ErrAgentHasActiveJobs)
		}
		// Functional session-lock guard (#425): a live foreground `agent ask` holds
		// the runtime:<rt>:<ref> session lock WITHOUT a DB job, so the jobs-busy
		// check above can't see it. Refuse only a provably-LIVE same-host owner —
		// restarting under a live session would clobber it. A stranded (dead-owner)
		// or expired or absent lock does NOT block: restart is exactly the recovery
		// for an abandoned session (and it mints a new runtime_ref → new lock key,
		// so it never touches the old lock). This is the inverse of the #303
		// generation-lock reclaim, which steals only a provably-dead/expired lock.
		held, detail, err := runtimeSessionHeldByLiveOwner(context.Background(), store, runtimeAgent(existing))
		if err != nil {
			return err
		}
		if held {
			return fmt.Errorf("a foreground runtime session for %s is in use (%s); finish or cancel it first", name, detail)
		}
		record, err = store.GetRepo(context.Background(), existing.RepoScope)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if existing.TemplateID != "" {
			cached, err = loadInstalledTemplate(context.Background(), store, existing.TemplateID)
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "agent restart: %v\n", err)
		return 1
	}

	adapter, err := runtimeStartAdapterFor(newRuntimeFactory(), existing.Runtime, record.CheckoutPath)
	if err != nil {
		fmt.Fprintf(stderr, "load adapter: %v\n", err)
		return 1
	}
	started, err := adapter.Start(context.Background(), runtime.StartRequest{
		Agent:  runtimeAgent(existing),
		Prompt: agentStartupPrompt(runtimeAgent(existing), cached),
	})
	if err != nil {
		// adapter.Start failed: write NOTHING — runtime_ref + metadata stay as loaded.
		fmt.Fprintf(stderr, "start runtime: %v\n", err)
		return 1
	}

	// Rebind by read-modify-write: mutate ONLY runtime_ref + health on the
	// already-loaded record so the full-row upsert preserves everything else.
	existing.RuntimeRef = strings.TrimSpace(started.RuntimeRef)
	existing.HealthStatus = "unknown"
	if err := withStore(*home, func(store *db.Store) error {
		return store.UpsertAgent(context.Background(), existing)
	}); err != nil {
		fmt.Fprintf(stderr, "agent restart: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "restarted %s (%s); session: %s\n", existing.Name, existing.Runtime, existing.RuntimeRef)
	fmt.Fprintf(stdout, "note: this abandons and replaces %s's current runtime session; restart refuses while a live foreground `agent ask` holds it, but recovers a stranded one\n", existing.Name)
	return 0
}

func runAgentDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	live := fs.Bool("live", false, "run an explicit live Claude print-mode smoke check")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "agent doctor requires exactly one name")
			return 2
		}
		return 0
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent doctor requires exactly one name")
		return 2
	}
	var agent db.Agent
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		agent, err = store.GetAgent(context.Background(), name)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "load agent: %v\n", err)
		return 1
	}
	rtAgent := runtimeAgent(agent)
	adapter, err := (runtime.Factory{}).Adapter(rtAgent.Runtime)
	if err != nil {
		fmt.Fprintf(stderr, "load adapter: %v\n", err)
		return 1
	}
	if err := adapter.Validate(context.Background(), rtAgent); err != nil {
		_ = persistAgentHealth(*home, name, "failed")
		fmt.Fprintf(stderr, "invalid agent: %v\n", err)
		return 1
	}
	if rtAgent.Runtime == runtime.ClaudeRuntime {
		auth := runtime.InspectClaudeAuthEnv(os.LookupEnv)
		status := "ok"
		if !auth.Ready() {
			status = "warn"
		} else if auth.Warning() != "" {
			status = "warn"
		}
		detail := auth.MaskedDetail()
		if warning := auth.Warning(); warning != "" {
			detail += "; " + warning
		}
		fmt.Fprintf(stdout, "claude-auth-env %s %s\n", status, detail)
		if !auth.Ready() && !*live {
			// Env not Ready does not mean auth is broken: foreground Claude may
			// authenticate fine via cached ~/.claude credentials. Probe the real
			// dependency (claude -p) instead of false-failing on the env check.
			// A probe failure escalates to failed/1; an OK or unavailable probe
			// reports warn/0 (degraded-but-serving) with the background-token
			// caveat — never a new false-fail.
			if err := runtime.ClaudeLiveCheck(context.Background(), agentDoctorRunner, ""); err != nil {
				if runtime.ClaudeProbeUnavailable(err) {
					fmt.Fprintf(stdout, "claude-live warn probe unavailable: %s\n", err)
					if perr := persistAgentHealth(*home, name, "warn"); perr != nil {
						fmt.Fprintf(stderr, "update agent health: %v\n", perr)
						return 1
					}
					fmt.Fprintf(stdout, "agent %s warn %s\n", rtAgent.Name, runtime.ClaudeBackgroundTokenMessage)
					return 0
				}
				_ = persistAgentHealth(*home, name, "failed")
				fmt.Fprintf(stdout, "claude-live fail %s\n", err)
				fmt.Fprintf(stderr, "agent %s health failed: %s: %v\n", rtAgent.Name, runtime.ClaudeSessionAuthFailedMessage, err)
				return 1
			}
			fmt.Fprintln(stdout, "claude-live ok live Claude print-mode check succeeded")
			if perr := persistAgentHealth(*home, name, "warn"); perr != nil {
				fmt.Fprintf(stderr, "update agent health: %v\n", perr)
				return 1
			}
			fmt.Fprintf(stdout, "agent %s warn %s\n", rtAgent.Name, runtime.ClaudeBackgroundTokenMessage)
			return 0
		}
		if *live {
			if err := runtime.ClaudeLiveCheck(context.Background(), agentDoctorRunner, ""); err != nil {
				_ = persistAgentHealth(*home, name, "failed")
				fmt.Fprintf(stdout, "claude-live fail %s\n", err)
				fmt.Fprintf(stderr, "agent %s health failed: %s: %v\n", rtAgent.Name, runtime.ClaudeSessionAuthFailedMessage, err)
				return 1
			}
			fmt.Fprintln(stdout, "claude-live ok live Claude print-mode check succeeded")
		}
		if err := persistAgentHealth(*home, name, "ok"); err != nil {
			fmt.Fprintf(stderr, "update agent health: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "agent %s ok\n", rtAgent.Name)
		return 0
	}
	if err := adapter.Health(context.Background(), rtAgent); err != nil {
		_ = persistAgentHealth(*home, name, "failed")
		fmt.Fprintf(stderr, "agent %s health failed: %v\n", rtAgent.Name, err)
		return 1
	}
	if err := persistAgentHealth(*home, name, "ok"); err != nil {
		fmt.Fprintf(stderr, "update agent health: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "agent %s ok\n", rtAgent.Name)
	return 0
}

func persistAgentHealth(home, name, status string) error {
	return withStore(home, func(store *db.Store) error {
		agent, err := store.GetAgent(context.Background(), name)
		if err != nil {
			return err
		}
		agent.HealthStatus = status
		return store.UpsertAgent(context.Background(), agent)
	})
}

func withStore(home string, fn func(*db.Store) error) error {
	return withStoreAndPaths(home, func(_ config.Paths, store *db.Store) error {
		return fn(store)
	})
}

func withStoreAndPaths(home string, fn func(config.Paths, *db.Store) error) error {
	paths, err := pathsFromFlag(home)
	if err != nil {
		return err
	}
	if err := config.Initialize(paths); err != nil {
		return err
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	return fn(paths, store)
}

func dbAgent(agent runtime.Agent) db.Agent {
	policy := runtime.NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy)
	return db.Agent{
		Name:           agent.Name,
		Role:           agent.Role,
		Runtime:        agent.Runtime,
		RuntimeRef:     agent.RuntimeRef,
		RepoScope:      agent.RepoScope,
		TemplateID:     agent.TemplateID,
		Model:          agent.Model,
		Capabilities:   agent.Capabilities,
		AutonomyPolicy: policy,
		HealthStatus:   agent.HealthStatus,
	}
}

func runtimeAgent(agent db.Agent) runtime.Agent {
	policy := runtime.NormalizeStoredAutonomyPolicy(agent.AutonomyPolicy)
	return runtime.Agent{
		Name:           agent.Name,
		Role:           agent.Role,
		Runtime:        agent.Runtime,
		RuntimeRef:     agent.RuntimeRef,
		RepoScope:      agent.RepoScope,
		TemplateID:     agent.TemplateID,
		Model:          agent.Model,
		Capabilities:   agent.Capabilities,
		AutonomyPolicy: policy,
		HealthStatus:   agent.HealthStatus,
	}
}

func normalizeRepoFlags(values []string) ([]string, error) {
	repos := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		repo, err := normalizeRepoFlag(value)
		if err != nil {
			return nil, err
		}
		if !seen[repo] {
			repos = append(repos, repo)
			seen[repo] = true
		}
	}
	return repos, nil
}

func normalizeRepoFlag(value string) (string, error) {
	repo, err := daemon.ParseRepository(value)
	if err != nil {
		return "", err
	}
	return repo.FullName(), nil
}

type repeatedFlag []string

func (f *repeatedFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}
