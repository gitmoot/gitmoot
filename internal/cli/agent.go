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
	default:
		fmt.Fprintf(stderr, "unknown agent command %q\n\n", args[0])
		printAgentUsage(stderr)
		return 2
	}
}

func printAgentUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot agent subscribe <name> --runtime codex|claude|shell --session <id|name|last|command> --role <role> --repo owner/repo --capability <capability>")
	fmt.Fprintln(w, "    Codex sessions may use a UUID, thread name, or last. Claude sessions may use a UUID or last. Shell sessions are commands.")
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
	repo := fs.String("repo", "", "repo scope as owner/repo")
	policy := fs.String("policy", "auto", "autonomy policy")
	var capabilities repeatedFlag
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

	agent := runtime.Agent{
		Name:           name,
		Role:           *role,
		Runtime:        *runtimeName,
		RuntimeRef:     *session,
		RepoScope:      *repo,
		Capabilities:   capabilities,
		AutonomyPolicy: *policy,
		HealthStatus:   "unknown",
	}
	if err := runtime.ValidateAgent(agent); err != nil {
		fmt.Fprintf(stderr, "invalid agent: %v\n", err)
		return 2
	}
	if err := withStore(*home, func(store *db.Store) error {
		return store.UpsertAgent(context.Background(), dbAgent(agent))
	}); err != nil {
		fmt.Fprintf(stderr, "subscribe agent: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "subscribed %s (%s) for %s\n", agent.Name, agent.Runtime, agent.RepoScope)
	return 0
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
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		agents, err = store.ListAgents(context.Background())
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "list agents: %v\n", err)
		return 1
	}
	for _, agent := range agents {
		fmt.Fprintf(stdout, "%-16s %-8s %-12s %-20s %s\n", agent.Name, agent.Runtime, agent.Role, agent.RepoScope, strings.Join(agent.Capabilities, ","))
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
		Capabilities:   agent.Capabilities,
		AutonomyPolicy: agent.AutonomyPolicy,
		HealthStatus:   agent.HealthStatus,
	}
}

type repeatedFlag []string

func (f *repeatedFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}
