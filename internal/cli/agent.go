package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	gitutil "github.com/jerryfane/gitmoot/internal/git"
	"github.com/jerryfane/gitmoot/internal/github"
	"github.com/jerryfane/gitmoot/internal/preset"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

var newRuntimeFactory = func() runtime.Factory {
	return runtime.Factory{}
}

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
	case "subscribe":
		return runAgentSubscribe(args[1:], stdout, stderr)
	case "list":
		return runAgentList(args[1:], stdout, stderr)
	case "remove":
		return runAgentRemove(args[1:], stdout, stderr)
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
	fmt.Fprintln(w, "  gitmoot agent start <name> --runtime codex|claude --repo owner/repo [--path .] [--preset <preset-id>] [--start-daemon]")
	fmt.Fprintln(w, "  gitmoot agent ask <name> \"message\" [--repo owner/repo] [--home path] [--json]")
	fmt.Fprintln(w, "  gitmoot agent subscribe <name> --runtime codex|claude|shell --session <id|name|last|command> --role <role> [--repo owner/repo...] --capability <capability>")
	fmt.Fprintln(w, "    Codex sessions may use a UUID, thread name, or last. Claude sessions may use a UUID or last. Shell sessions are commands.")
	fmt.Fprintln(w, "  gitmoot agent allow <name> --repo owner/repo")
	fmt.Fprintln(w, "  gitmoot agent deny <name> --repo owner/repo")
	fmt.Fprintln(w, "  gitmoot agent repos <name>")
	fmt.Fprintln(w, "  gitmoot agent list")
	fmt.Fprintln(w, "  gitmoot agent remove <name>")
	fmt.Fprintln(w, "  gitmoot agent doctor <name>")
}

type agentAskOptions struct {
	home       string
	repo       string
	jsonOutput bool
	agent      string
	message    string
}

type agentAskOutput struct {
	JobID          string                `json:"job_id"`
	State          string                `json:"state"`
	Repo           string                `json:"repo"`
	Agent          string                `json:"agent"`
	Action         string                `json:"action"`
	Result         *workflow.AgentResult `json:"result,omitempty"`
	RawOutputCount int                   `json:"raw_output_count"`
}

func runAgentAsk(args []string, stdout, stderr io.Writer) int {
	options, ok := parseAgentAskOptions(args, stderr)
	if !ok {
		if containsHelpFlag(args) {
			return 0
		}
		return 2
	}
	var output agentAskOutput
	if err := withStore(options.home, func(store *db.Store) error {
		ctx := context.Background()
		repo, record, err := resolveAgentAskRepo(ctx, store, options.repo)
		if err != nil {
			return err
		}
		if err := store.UpsertRepo(ctx, record); err != nil {
			return err
		}
		agent, err := store.GetAgent(ctx, options.agent)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("agent %q not found", options.agent)
			}
			return err
		}
		if err := ensureAgentAskAccess(ctx, store, agent, repo.FullName()); err != nil {
			return err
		}
		adapter, err := runtimeStartAdapter(newRuntimeFactory(), agent.Runtime, record.CheckoutPath)
		if err != nil {
			return err
		}
		job, err := (workflow.Mailbox{Store: store}).Enqueue(ctx, workflow.JobRequest{
			ID:           localAskJobID(agent.Name),
			Agent:        agent.Name,
			Action:       "ask",
			Repo:         repo.FullName(),
			Branch:       record.DefaultBranch,
			Sender:       "local",
			Instructions: options.message,
		})
		if err != nil {
			return err
		}
		if _, err := (workflow.Mailbox{Store: store}).Run(ctx, job.ID, runtimeAgent(agent), adapter); err != nil {
			return err
		}
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: job.ID, Kind: "advance_completed", Message: "workflow advancement completed"}); err != nil {
			return err
		}
		latest, err := store.GetJob(ctx, job.ID)
		if err != nil {
			return err
		}
		payload, err := daemonJobPayload(latest)
		if err != nil {
			return err
		}
		output = agentAskOutput{
			JobID:          latest.ID,
			State:          latest.State,
			Repo:           payload.Repo,
			Agent:          latest.Agent,
			Action:         latest.Type,
			Result:         payload.Result,
			RawOutputCount: len(payload.RawOutputs),
		}
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "agent ask: %v\n", err)
		return 1
	}
	if options.jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "agent ask: %v\n", err)
			return 1
		}
		return 0
	}
	printAgentAskOutput(stdout, output)
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
	fmt.Fprintln(w, "  gitmoot agent ask <name> \"message\" [--repo owner/repo] [--home path] [--json]")
}

func resolveAgentAskRepo(ctx context.Context, store *db.Store, repoFlag string) (github.Repository, db.Repo, error) {
	repo, err := agentAskTargetRepo(ctx, repoFlag)
	if err != nil {
		return github.Repository{}, db.Repo{}, err
	}
	if strings.TrimSpace(repoFlag) == "" {
		record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: "."})
		if err != nil {
			return github.Repository{}, db.Repo{}, err
		}
		return repo, record, nil
	}
	if existing, err := store.GetRepo(ctx, repo.FullName()); err == nil && strings.TrimSpace(existing.CheckoutPath) != "" {
		record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: existing.CheckoutPath})
		if err != nil {
			return github.Repository{}, db.Repo{}, err
		}
		record.PollInterval = existing.PollInterval
		return repo, record, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return github.Repository{}, db.Repo{}, err
	}
	record, err := repoRecordForCheckout(ctx, repo, gitutil.Client{Dir: "."})
	if err != nil {
		return github.Repository{}, db.Repo{}, err
	}
	return repo, record, nil
}

func agentAskTargetRepo(ctx context.Context, repoFlag string) (github.Repository, error) {
	if strings.TrimSpace(repoFlag) != "" {
		return daemon.ParseRepository(repoFlag)
	}
	remote, err := (gitutil.Client{Dir: "."}).OriginRemote(ctx)
	if err != nil {
		return github.Repository{}, fmt.Errorf("infer repo from current checkout: %w", err)
	}
	parsed, err := gitutil.ParseGitHubRemote(remote)
	if err != nil {
		return github.Repository{}, err
	}
	return github.Repository{Owner: parsed.Owner, Name: parsed.Name}, nil
}

func ensureAgentAskAccess(ctx context.Context, store *db.Store, agent db.Agent, repo string) error {
	allowed, err := store.AgentCanAccessRepo(ctx, agent.Name, repo)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("agent %q is not allowed on %q", agent.Name, repo)
	}
	if !agentHasCapability(agent.Capabilities, "ask") {
		return fmt.Errorf("agent %q lacks ask capability", agent.Name)
	}
	return nil
}

func localAskJobID(agent string) string {
	return fmt.Sprintf("local-ask-%s-%x", agent, time.Now().UTC().UnixNano())
}

func printAgentAskOutput(stdout io.Writer, output agentAskOutput) {
	writeLine(stdout, "job: %s", output.JobID)
	writeLine(stdout, "state: %s", output.State)
	writeLine(stdout, "repo: %s", output.Repo)
	writeLine(stdout, "agent: %s", output.Agent)
	writeLine(stdout, "action: %s", output.Action)
	if output.Result == nil {
		return
	}
	writeLine(stdout, "decision: %s", output.Result.Decision)
	writeLine(stdout, "summary: %s", output.Result.Summary)
	printRawMessages(stdout, "findings", output.Result.Findings)
	printStringList(stdout, "needs", output.Result.Needs)
	printStringList(stdout, "tests_run", output.Result.TestsRun)
	printStringList(stdout, "next_agents", output.Result.NextAgents)
}

func printRawMessages(stdout io.Writer, label string, values []json.RawMessage) {
	if len(values) == 0 {
		return
	}
	writeLine(stdout, "%s:", label)
	for _, value := range values {
		writeLine(stdout, "- %s", strings.TrimSpace(string(value)))
	}
}

func printStringList(stdout io.Writer, label string, values []string) {
	if len(values) == 0 {
		return
	}
	writeLine(stdout, "%s:", label)
	for _, value := range values {
		writeLine(stdout, "- %s", value)
	}
}

func runAgentStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	runtimeName := fs.String("runtime", "", "agent runtime: codex or claude")
	repoFlag := fs.String("repo", "", "allowed repo as owner/repo")
	path := fs.String("path", ".", "local checkout path")
	role := fs.String("role", "", "agent role")
	presetID := fs.String("preset", "", "agent prompt preset")
	policy := fs.String("policy", "auto", "autonomy policy")
	updatePreset := fs.Bool("update-preset", false, "install or refresh the preset before starting")
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
	if *updatePreset && strings.TrimSpace(*presetID) == "" {
		fmt.Fprintln(stderr, "agent start --update-preset requires --preset")
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
	resolvedRole, resolvedCapabilities, err := resolveAgentDefaults(*presetID, *role, capabilities, "agent", []string{"ask", "review", "implement"})
	if err != nil {
		fmt.Fprintf(stderr, "invalid preset: %v\n", err)
		return 2
	}
	agent := runtime.Agent{
		Name:           name,
		Role:           resolvedRole,
		Runtime:        strings.TrimSpace(*runtimeName),
		RepoScope:      repo.FullName(),
		PresetID:       strings.TrimSpace(*presetID),
		Capabilities:   resolvedCapabilities,
		AutonomyPolicy: *policy,
		HealthStatus:   "unknown",
	}
	if err := runtime.ValidateStartRequest(runtime.StartRequest{Agent: agent, Prompt: "preflight"}); err != nil {
		fmt.Fprintf(stderr, "invalid agent: %v\n", err)
		return 2
	}
	if agent.Runtime == runtime.ShellRuntime {
		fmt.Fprintln(stderr, "start runtime: shell runtime does not support agent start; use gitmoot agent subscribe --runtime shell --session <command>")
		return 1
	}
	var cachedPreset db.Preset
	if err := withStore(*home, func(store *db.Store) error {
		if _, err := store.GetAgent(context.Background(), agent.Name); err == nil {
			return fmt.Errorf("agent %s already exists", agent.Name)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if agent.PresetID == "" {
			return nil
		}
		if *updatePreset {
			updated, err := updatePresetByID(context.Background(), store, agent.PresetID)
			if err != nil {
				return err
			}
			cachedPreset = updated
			return nil
		}
		installed, err := loadInstalledPreset(context.Background(), store, agent.PresetID)
		if err != nil {
			return err
		}
		cachedPreset = installed
		return nil
	}); err != nil {
		if strings.HasPrefix(err.Error(), "preset ") {
			fmt.Fprintf(stderr, "%v\n", err)
		} else {
			fmt.Fprintf(stderr, "agent start: %v\n", err)
		}
		return 1
	}
	prompt := agentStartupPrompt(agent, cachedPreset)
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
	runtimeName := fs.String("runtime", "", "agent runtime: codex, claude, or shell")
	session := fs.String("session", "", "runtime session reference, last, or shell command")
	role := fs.String("role", "", "agent role")
	presetID := fs.String("preset", "", "agent prompt preset")
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
	trimmedPresetID := strings.TrimSpace(*presetID)
	if trimmedPresetID != "" {
		if _, ok := preset.Lookup(trimmedPresetID); !ok {
			if err := withStore(*home, func(store *db.Store) error {
				_, err := loadInstalledPreset(context.Background(), store, trimmedPresetID)
				return err
			}); err != nil {
				if strings.HasPrefix(err.Error(), "preset ") {
					fmt.Fprintf(stderr, "%v\n", err)
				} else {
					fmt.Fprintf(stderr, "subscribe agent: %v\n", err)
				}
				return 1
			}
		}
	}
	resolvedRole, resolvedCapabilities, err := resolvePresetDefaults(*presetID, *role, capabilities)
	if err != nil {
		fmt.Fprintf(stderr, "invalid preset: %v\n", err)
		return 2
	}
	agent := runtime.Agent{
		Name:           name,
		Role:           resolvedRole,
		Runtime:        *runtimeName,
		RuntimeRef:     *session,
		RepoScope:      repoScope,
		PresetID:       strings.TrimSpace(*presetID),
		Capabilities:   resolvedCapabilities,
		AutonomyPolicy: *policy,
		HealthStatus:   "unknown",
	}
	if err := runtime.ValidateAgent(agent); err != nil {
		fmt.Fprintf(stderr, "invalid agent: %v\n", err)
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		if agent.PresetID != "" {
			if _, err := loadInstalledPreset(context.Background(), store, agent.PresetID); err != nil {
				return err
			}
		}
		return persistAgentSubscription(context.Background(), store, agent, normalizedRepos)
	}); err != nil {
		if strings.HasPrefix(err.Error(), "preset ") {
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

func resolvePresetDefaults(presetID string, role string, capabilities []string) (string, []string, error) {
	return resolveAgentDefaults(presetID, role, capabilities, "", nil)
}

func resolveAgentDefaults(presetID string, role string, capabilities []string, fallbackRole string, fallbackCapabilities []string) (string, []string, error) {
	presetID = strings.TrimSpace(presetID)
	role = strings.TrimSpace(role)
	resolvedCapabilities := compactValues(capabilities)
	if presetID == "" {
		if role == "" {
			role = fallbackRole
		}
		if len(resolvedCapabilities) == 0 {
			resolvedCapabilities = append([]string{}, fallbackCapabilities...)
		}
		return role, resolvedCapabilities, nil
	}
	definition, ok := preset.Lookup(presetID)
	if !ok {
		if err := preset.ValidateID(presetID); err != nil {
			return "", nil, err
		}
		if role == "" {
			if fallbackRole == "" {
				return "", nil, fmt.Errorf("preset %s does not define a default role; pass --role", presetID)
			}
			role = fallbackRole
		}
		if len(resolvedCapabilities) == 0 {
			if len(fallbackCapabilities) == 0 {
				return "", nil, fmt.Errorf("preset %s does not define default capabilities; pass --capability", presetID)
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
		return "", nil, fmt.Errorf("preset %s does not allow implement capability", definition.ID)
	}
	return role, resolvedCapabilities, nil
}

func loadInstalledPreset(ctx context.Context, store *db.Store, presetID string) (db.Preset, error) {
	cached, err := store.GetPreset(ctx, presetID)
	if errors.Is(err, sql.ErrNoRows) {
		if _, ok := preset.Lookup(presetID); !ok {
			return db.Preset{}, fmt.Errorf("preset %s is not installed; run gitmoot preset add %s --file <path>", presetID, presetID)
		}
		return db.Preset{}, fmt.Errorf("preset %s is not installed; run gitmoot preset update %s", presetID, presetID)
	}
	return cached, err
}

func persistAgentSubscription(ctx context.Context, store *db.Store, agent runtime.Agent, repos []string) error {
	if err := store.UpsertAgent(ctx, dbAgent(agent)); err != nil {
		return err
	}
	return store.ReplaceAgentRepos(ctx, agent.Name, repos)
}

func agentStartupPrompt(agent runtime.Agent, cachedPreset db.Preset) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "You are a Gitmoot-managed agent named %s.\n", agent.Name)
	fmt.Fprintf(&builder, "Runtime: %s\n", agent.Runtime)
	fmt.Fprintf(&builder, "Allowed repo: %s\n", agent.RepoScope)
	fmt.Fprintf(&builder, "Role: %s\n", agent.Role)
	fmt.Fprintf(&builder, "Capabilities: %s\n", strings.Join(agent.Capabilities, ","))
	if agent.PresetID != "" {
		fmt.Fprintf(&builder, "Preset: %s", agent.PresetID)
		if cachedPreset.ResolvedCommit != "" {
			fmt.Fprintf(&builder, " @ %s", cachedPreset.ResolvedCommit)
		}
		builder.WriteString("\n\nPreset instructions:\n")
		builder.WriteString(strings.TrimRight(cachedPreset.Content, "\n"))
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
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellArg(arg))
	}
	return strings.Join(quoted, " ")
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
		fmt.Fprintf(stdout, "%-16s %-8s %-12s %-20s %s\n", agent.Name, agent.Runtime, agent.Role, strings.Join(agentRepos[agent.Name], ","), strings.Join(agent.Capabilities, ","))
	}
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

func runAgentDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
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
	return fn(store)
}

func dbAgent(agent runtime.Agent) db.Agent {
	return db.Agent{
		Name:           agent.Name,
		Role:           agent.Role,
		Runtime:        agent.Runtime,
		RuntimeRef:     agent.RuntimeRef,
		RepoScope:      agent.RepoScope,
		PresetID:       agent.PresetID,
		Capabilities:   agent.Capabilities,
		AutonomyPolicy: agent.AutonomyPolicy,
		HealthStatus:   agent.HealthStatus,
	}
}

func runtimeAgent(agent db.Agent) runtime.Agent {
	return runtime.Agent{
		Name:           agent.Name,
		Role:           agent.Role,
		Runtime:        agent.Runtime,
		RuntimeRef:     agent.RuntimeRef,
		RepoScope:      agent.RepoScope,
		PresetID:       agent.PresetID,
		Capabilities:   agent.Capabilities,
		AutonomyPolicy: agent.AutonomyPolicy,
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
