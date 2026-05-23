package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/preset"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

func runAgent(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printAgentUsage(stdout)
		return 0
	}
	switch args[0] {
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
	fmt.Fprintln(w, "  gitmoot agent subscribe <name> --runtime codex|claude|shell --session <id|name|last|command> --role <role> [--repo owner/repo...] --capability <capability>")
	fmt.Fprintln(w, "    Codex sessions may use a UUID, thread name, or last. Claude sessions may use a UUID or last. Shell sessions are commands.")
	fmt.Fprintln(w, "  gitmoot agent allow <name> --repo owner/repo")
	fmt.Fprintln(w, "  gitmoot agent deny <name> --repo owner/repo")
	fmt.Fprintln(w, "  gitmoot agent repos <name>")
	fmt.Fprintln(w, "  gitmoot agent list")
	fmt.Fprintln(w, "  gitmoot agent remove <name>")
	fmt.Fprintln(w, "  gitmoot agent doctor <name>")
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
			if _, err := store.GetPreset(context.Background(), agent.PresetID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("preset %s is not installed; run gitmoot preset update %s", agent.PresetID, agent.PresetID)
				}
				return err
			}
		}
		if err := store.UpsertAgent(context.Background(), dbAgent(agent)); err != nil {
			return err
		}
		return store.ReplaceAgentRepos(context.Background(), agent.Name, normalizedRepos)
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
	presetID = strings.TrimSpace(presetID)
	role = strings.TrimSpace(role)
	resolvedCapabilities := compactValues(capabilities)
	if presetID == "" {
		return role, resolvedCapabilities, nil
	}
	definition, ok := preset.Lookup(presetID)
	if !ok {
		return "", nil, fmt.Errorf("unknown preset %q", presetID)
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
