package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// runAgentHeartbeat is the write-side (and read-side) CLI for heartbeat schedules
// (#533). It edits [agents.<agent>.heartbeats.<name>] sections programmatically
// through the config-edit seam (config.SaveHeartbeat / SetHeartbeatEnabled /
// RemoveHeartbeat) — never by hand — and reads them back through the same
// config.LoadHeartbeats the daemon scan uses. It mirrors `agent type`'s shape.
func runAgentHeartbeat(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printAgentHeartbeatUsage(stdout)
		return 0
	}
	switch args[0] {
	case "add":
		return runAgentHeartbeatAdd(args[1:], stdout, stderr)
	case "list":
		return runAgentHeartbeatList(args[1:], stdout, stderr)
	case "show":
		return runAgentHeartbeatShow(args[1:], stdout, stderr)
	case "enable":
		return runAgentHeartbeatSetEnabled(args[1:], true, stdout, stderr)
	case "disable":
		return runAgentHeartbeatSetEnabled(args[1:], false, stdout, stderr)
	case "remove":
		return runAgentHeartbeatRemove(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown agent heartbeat command %q\n\n", args[0])
		printAgentHeartbeatUsage(stderr)
		return 2
	}
}

func printAgentHeartbeatUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot agent heartbeat add <agent> <name> --repo owner/repo --interval 24h --prompt \"...\" [--action ask|review|implement] [--runtime codex|claude|kimi] [--jitter 15m] [--max-concurrent 1] [--enabled]")
	fmt.Fprintln(w, "  gitmoot agent heartbeat list [--agent <agent>]")
	fmt.Fprintln(w, "  gitmoot agent heartbeat show <agent> <name>")
	fmt.Fprintln(w, "  gitmoot agent heartbeat enable <agent> <name>")
	fmt.Fprintln(w, "  gitmoot agent heartbeat disable <agent> <name>")
	fmt.Fprintln(w, "  gitmoot agent heartbeat remove <agent> <name>")
}

func runAgentHeartbeatAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent heartbeat add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repo := fs.String("repo", "", "managed repo as owner/repo (required)")
	interval := fs.String("interval", "", "schedule interval, e.g. 24h (required)")
	prompt := fs.String("prompt", "", "instructions the heartbeat sends the agent (required)")
	action := fs.String("action", "ask", "heartbeat action: ask, review, or implement (implement is policy-gated)")
	runtimeOverride := fs.String("runtime", "", "run this heartbeat on a specific runtime (codex|claude|kimi) instead of the agent default")
	jitter := fs.String("jitter", "", "random delay added to each interval, e.g. 15m (default 0s)")
	maxConcurrent := fs.Int("max-concurrent", 1, "maximum concurrent jobs for this heartbeat")
	enabled := fs.Bool("enabled", false, "enable the heartbeat immediately (default disabled)")
	if len(args) < 2 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) < 2 {
			fmt.Fprintln(stderr, "agent heartbeat add requires <agent> and <name>")
			return 2
		}
		return 0
	}
	agent := strings.TrimSpace(args[0])
	name := strings.TrimSpace(args[1])
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent heartbeat add requires exactly <agent> and <name>")
		return 2
	}
	if !validAgentTypeName(agent) {
		fmt.Fprintf(stderr, "invalid agent name %q\n", agent)
		return 2
	}
	if !validAgentTypeName(name) {
		fmt.Fprintf(stderr, "invalid heartbeat name %q\n", name)
		return 2
	}
	if !config.HeartbeatActionSupported(*action) {
		fmt.Fprintf(stderr, "invalid action %q; use one of: %s\n", *action, strings.Join(config.HeartbeatActions(), ", "))
		return 2
	}
	if !config.HeartbeatRuntimeSupported(*runtimeOverride) {
		fmt.Fprintf(stderr, "invalid runtime %q; use one of: %s\n", strings.TrimSpace(*runtimeOverride), strings.Join(config.HeartbeatRuntimes(), ", "))
		return 2
	}
	entry := config.Heartbeat{
		Agent:         agent,
		Name:          name,
		Enabled:       *enabled,
		Repo:          strings.TrimSpace(*repo),
		Interval:      strings.TrimSpace(*interval),
		Jitter:        strings.TrimSpace(*jitter),
		Action:        strings.TrimSpace(*action),
		Prompt:        strings.TrimSpace(*prompt),
		MaxConcurrent: *maxConcurrent,
		Runtime:       strings.TrimSpace(*runtimeOverride),
	}
	paths, err := initializedPaths(*home)
	if err != nil {
		fmt.Fprintf(stderr, "agent heartbeat add: %v\n", err)
		return 1
	}
	// A review heartbeat enqueues a review job; refuse to write one for an agent
	// that does not hold the review capability so the misconfiguration is caught at
	// write time, not silently skipped by the daemon scan.
	if entry.Action == "review" {
		if exit := requireHeartbeatReviewCapability(*home, agent, stderr); exit != 0 {
			return exit
		}
	}
	// An implement heartbeat enqueues a WRITE job. Refuse to write one unless the
	// target agent both holds the implement capability AND carries a write-granting
	// autonomy policy (workspace-write / danger-full-access). This mirrors the
	// agent-start gate exactly: under auto/read-only an implement job runs and
	// produces no files, so it is fail-closed at config-write time rather than
	// silently no-op'd by the daemon scan (#611).
	if entry.Action == "implement" {
		if exit := requireHeartbeatImplementPermission(*home, agent, stderr); exit != 0 {
			return exit
		}
	}
	if err := config.SaveHeartbeat(paths, entry); err != nil {
		fmt.Fprintf(stderr, "agent heartbeat add: %v\n", err)
		return 1
	}
	writeLine(stdout, "configured heartbeat %s/%s", agent, name)
	return 0
}

// requireHeartbeatReviewCapability returns a non-zero exit code (and prints to
// stderr) when the named agent does not currently hold the review capability.
func requireHeartbeatReviewCapability(home, agent string, stderr io.Writer) int {
	var record db.Agent
	if err := withStore(home, func(store *db.Store) error {
		got, err := store.GetAgent(context.Background(), agent)
		if err != nil {
			return err
		}
		record = got
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "agent heartbeat add: agent %q must exist and hold the review capability for a review heartbeat: %v\n", agent, err)
		return 1
	}
	if !agentHasCapability(record.Capabilities, "review") {
		fmt.Fprintf(stderr, "agent heartbeat add: agent %q lacks the review capability required for a review heartbeat\n", agent)
		return 2
	}
	return 0
}

// requireHeartbeatImplementPermission returns a non-zero exit code (and prints to
// stderr) when the named agent cannot safely run an implement heartbeat: it must
// exist, hold the "implement" capability, AND carry a write-granting autonomy
// policy (workspace-write / danger-full-access). Under auto/read-only an
// implement job would run and produce nothing, so this is the fail-closed gate
// that refuses the misconfiguration at write time (#611). It reuses the exact
// runtime predicate (PolicyGrantsImplementWrite) that agent start / implement
// dispatch use, so the heartbeat gate can never drift from the direct-job gate.
func requireHeartbeatImplementPermission(home, agent string, stderr io.Writer) int {
	var record db.Agent
	if err := withStore(home, func(store *db.Store) error {
		got, err := store.GetAgent(context.Background(), agent)
		if err != nil {
			return err
		}
		record = got
		return nil
	}); err != nil {
		fmt.Fprintf(stderr, "agent heartbeat add: agent %q must exist, hold the implement capability, and carry a write-granting policy for an implement heartbeat: %v\n", agent, err)
		return 1
	}
	if !agentHasCapability(record.Capabilities, "implement") {
		fmt.Fprintf(stderr, "agent heartbeat add: agent %q lacks the implement capability required for an implement heartbeat\n", agent)
		return 2
	}
	if !runtime.PolicyGrantsImplementWrite(record.AutonomyPolicy) {
		fmt.Fprintf(stderr, "agent heartbeat add: agent %q autonomy policy %q grants no headless write permission; an implement heartbeat needs --policy workspace-write or danger-full-access on the agent\n", agent, runtime.NormalizeStoredAutonomyPolicy(record.AutonomyPolicy))
		return 2
	}
	return 0
}

func runAgentHeartbeatList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent heartbeat list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	agentFilter := fs.String("agent", "", "only list heartbeats for this agent")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent heartbeat list does not accept positional arguments")
		return 2
	}
	heartbeats, err := loadHeartbeatConfig(*home)
	if err != nil {
		fmt.Fprintf(stderr, "agent heartbeat list: %v\n", err)
		return 1
	}
	filter := strings.TrimSpace(*agentFilter)
	rows := make([]config.Heartbeat, 0, len(heartbeats))
	for _, heartbeat := range heartbeats {
		if filter != "" && heartbeat.Agent != filter {
			continue
		}
		rows = append(rows, heartbeat)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Agent != rows[j].Agent {
			return rows[i].Agent < rows[j].Agent
		}
		return rows[i].Name < rows[j].Name
	})
	for _, heartbeat := range rows {
		enabled := "disabled"
		if heartbeat.Enabled {
			enabled = "enabled"
		}
		fmt.Fprintf(stdout, "%s/%s\t%s\t%s\t%s\t%s\n", heartbeat.Agent, heartbeat.Name, enabled, heartbeat.Action, heartbeat.Interval, heartbeat.Repo)
	}
	return 0
}

func runAgentHeartbeatShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent heartbeat show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if len(args) < 2 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) < 2 {
			fmt.Fprintln(stderr, "agent heartbeat show requires <agent> and <name>")
			return 2
		}
		return 0
	}
	agent := strings.TrimSpace(args[0])
	name := strings.TrimSpace(args[1])
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent heartbeat show requires exactly <agent> and <name>")
		return 2
	}
	heartbeats, err := loadHeartbeatConfig(*home)
	if err != nil {
		fmt.Fprintf(stderr, "agent heartbeat show: %v\n", err)
		return 1
	}
	entry, ok := findHeartbeat(heartbeats, agent, name)
	if !ok {
		fmt.Fprintf(stderr, "heartbeat %s/%s not found\n", agent, name)
		return 1
	}
	printHeartbeat(stdout, entry)
	// Best-effort: surface the persisted run state alongside the config. A store
	// error here is non-fatal — the config view still printed.
	_ = withStore(*home, func(store *db.Store) error {
		state, ok, err := store.GetHeartbeatState(context.Background(), agent, name)
		if err != nil || !ok {
			return err
		}
		writeLine(stdout, "last_run: %s", heartbeatTimeForStatus(state.LastRunAt))
		writeLine(stdout, "next_due: %s", heartbeatTimeForStatus(state.NextDueAt))
		writeLine(stdout, "last_status: %s", firstNonEmpty(state.LastStatus, "-"))
		writeLine(stdout, "last_job: %s", firstNonEmpty(state.LastJobID, "-"))
		return nil
	})
	return 0
}

func runAgentHeartbeatSetEnabled(args []string, enabled bool, stdout, stderr io.Writer) int {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	fs := flag.NewFlagSet("agent heartbeat "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if len(args) < 2 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) < 2 {
			fmt.Fprintf(stderr, "agent heartbeat %s requires <agent> and <name>\n", verb)
			return 2
		}
		return 0
	}
	agent := strings.TrimSpace(args[0])
	name := strings.TrimSpace(args[1])
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "agent heartbeat %s requires exactly <agent> and <name>\n", verb)
		return 2
	}
	paths, err := initializedPaths(*home)
	if err != nil {
		fmt.Fprintf(stderr, "agent heartbeat %s: %v\n", verb, err)
		return 1
	}
	if err := config.SetHeartbeatEnabled(paths, agent, name, enabled); err != nil {
		fmt.Fprintf(stderr, "agent heartbeat %s: %v\n", verb, err)
		return 1
	}
	writeLine(stdout, "%sd heartbeat %s/%s", verb, agent, name)
	return 0
}

func runAgentHeartbeatRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent heartbeat remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if len(args) < 2 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) < 2 {
			fmt.Fprintln(stderr, "agent heartbeat remove requires <agent> and <name>")
			return 2
		}
		return 0
	}
	agent := strings.TrimSpace(args[0])
	name := strings.TrimSpace(args[1])
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "agent heartbeat remove requires exactly <agent> and <name>")
		return 2
	}
	paths, err := initializedPaths(*home)
	if err != nil {
		fmt.Fprintf(stderr, "agent heartbeat remove: %v\n", err)
		return 1
	}
	removed, err := config.RemoveHeartbeat(paths, agent, name)
	if err != nil {
		fmt.Fprintf(stderr, "agent heartbeat remove: %v\n", err)
		return 1
	}
	if !removed {
		fmt.Fprintf(stderr, "heartbeat %s/%s not found\n", agent, name)
		return 1
	}
	writeLine(stdout, "removed heartbeat %s/%s", agent, name)
	return 0
}

func loadHeartbeatConfig(home string) ([]config.Heartbeat, error) {
	paths, err := initializedPaths(home)
	if err != nil {
		return nil, err
	}
	return config.LoadHeartbeats(paths)
}

func findHeartbeat(heartbeats []config.Heartbeat, agent, name string) (config.Heartbeat, bool) {
	agent = strings.TrimSpace(agent)
	name = strings.TrimSpace(name)
	for _, heartbeat := range heartbeats {
		if heartbeat.Agent == agent && heartbeat.Name == name {
			return heartbeat, true
		}
	}
	return config.Heartbeat{}, false
}

func printHeartbeat(stdout io.Writer, entry config.Heartbeat) {
	writeLine(stdout, "agent: %s", entry.Agent)
	writeLine(stdout, "name: %s", entry.Name)
	writeLine(stdout, "enabled: %t", entry.Enabled)
	writeLine(stdout, "repo: %s", entry.Repo)
	writeLine(stdout, "interval: %s", entry.Interval)
	writeLine(stdout, "jitter: %s", entry.Jitter)
	writeLine(stdout, "action: %s", entry.Action)
	writeLine(stdout, "runtime: %s", firstNonEmpty(entry.Runtime, "(agent default)"))
	writeLine(stdout, "max_concurrent: %d", entry.MaxConcurrent)
	writeLine(stdout, "prompt: %s", entry.Prompt)
}
