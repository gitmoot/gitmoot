package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

func runSetup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "repo scope as owner/repo")
	path := fs.String("path", ".", "local checkout path")
	agentName := fs.String("agent", "", "agent name to subscribe")
	runtimeName := fs.String("runtime", "", "agent runtime: codex, claude, or shell")
	session := fs.String("session", "", "runtime session reference, last, or shell command")
	role := fs.String("role", "agent", "agent role")
	startDaemon := fs.Bool("start-daemon", false, "start the background daemon after setup")
	watchIssues := fs.Bool("watch-issues", true, "watch open issues and route @<agent> ask comments to jobs (#389); on by default so the daemon is tagging-ready")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "setup does not accept positional arguments")
		return 2
	}
	if strings.TrimSpace(*repoFlag) == "" || strings.TrimSpace(*agentName) == "" || strings.TrimSpace(*runtimeName) == "" || strings.TrimSpace(*session) == "" {
		fmt.Fprintln(stderr, "setup requires --repo, --agent, --runtime, and --session")
		return 2
	}
	repo, err := daemon.ParseRepository(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "invalid repo: %v\n", err)
		return 2
	}
	record, err := repoRecordFromPath(context.Background(), repo, *path)
	if err != nil {
		fmt.Fprintf(stderr, "setup: %v\n", err)
		return 1
	}
	agent := runtime.Agent{
		Name:           strings.TrimSpace(*agentName),
		Role:           strings.TrimSpace(*role),
		Runtime:        strings.TrimSpace(*runtimeName),
		RuntimeRef:     strings.TrimSpace(*session),
		RepoScope:      repo.FullName(),
		Capabilities:   []string{"review", "implement", "ask"},
		AutonomyPolicy: "auto",
		HealthStatus:   "unknown",
	}
	if err := runtime.ValidateAgent(agent); err != nil {
		fmt.Fprintf(stderr, "invalid agent: %v\n", err)
		return 2
	}

	writeLine(stdout, "step: initialize local state")
	writeLine(stdout, "step: register repo %s at %s", record.FullName(), record.CheckoutPath)
	writeLine(stdout, "step: subscribe agent %s (%s)", agent.Name, agent.Runtime)
	if err := withStore(*home, func(store *db.Store) error {
		exists, err := existingCompatibleAgent(context.Background(), store, agent)
		if err != nil {
			return err
		}
		if err := store.UpsertRepo(context.Background(), record); err != nil {
			return err
		}
		if !exists {
			if err := store.UpsertAgent(context.Background(), dbAgent(agent)); err != nil {
				return err
			}
		}
		return store.AllowAgentRepo(context.Background(), agent.Name, repo.FullName())
	}); err != nil {
		fmt.Fprintf(stderr, "setup: %v\n", err)
		return 1
	}
	writeLine(stdout, "configured %s with agent %s", repo.FullName(), agent.Name)
	if *startDaemon {
		writeLine(stdout, "step: start background daemon")
		daemonArgs := []string{"--home", *home, "--repo", repo.FullName()}
		if *watchIssues {
			daemonArgs = append(daemonArgs, "--watch-issues")
		}
		code := runDaemonStartWithWorkDir(daemonArgs, record.CheckoutPath, stdout, stderr)
		if code != 0 {
			return code
		}
		writeSetupReadiness(stdout, agent, repo.FullName(), *watchIssues, true)
		return 0
	}
	writeSetupReadiness(stdout, agent, repo.FullName(), *watchIssues, false)
	return 0
}

// writeSetupReadiness prints a post-setup readiness summary so a freshly
// configured tagging agent is usable without tribal knowledge: which
// prerequisites are wired, whether the daemon watches issues, a daemon
// runtime-auth note, and the exact comment to post (#428).
func writeSetupReadiness(stdout io.Writer, agent runtime.Agent, repoFullName string, watchIssues bool, daemonStarted bool) {
	writeLine(stdout, "")
	writeLine(stdout, "readiness:")
	writeLine(stdout, "  [ok] repo registered: %s", repoFullName)
	writeLine(stdout, "  [ok] agent access: %s -> %s", agent.Name, repoFullName)
	if daemonStarted {
		if watchIssues {
			writeLine(stdout, "  [ok] daemon: started with --watch-issues (answers issue tags)")
		} else {
			writeLine(stdout, "  [warn] daemon: started WITHOUT --watch-issues; it will NOT answer issue tags")
			writeLine(stdout, "         re-run with --watch-issues, or: gitmoot daemon restart --repo %s --watch-issues", repoFullName)
		}
		writeSetupDaemonAuthNote(stdout, agent)
	} else {
		if watchIssues {
			writeLine(stdout, "  [next] daemon not started; run from a shell that holds the runtime token:")
		} else {
			writeLine(stdout, "  [next] daemon not started; issue-watching is OFF for this run.")
		}
		writeLine(stdout, "next: gitmoot daemon start --repo %s --watch-issues", repoFullName)
	}
	if watchIssues {
		writeLine(stdout, "")
		writeLine(stdout, "to tag the agent, post this as the FIRST token of a line in a %s issue/PR comment:", repoFullName)
		writeLine(stdout, "  @%s ask <your question>", agent.Name)
		writeLine(stdout, "note: on issues only the `ask` action is acted on (review/implement apply to PRs).")
	}
}

// writeSetupDaemonAuthNote surfaces a fail-open daemon runtime-auth note. The
// authoritative daemon-aware auth check is tracked in #427 (a sibling PR); to
// keep this flow independently mergeable we only inspect the current shell's
// env for the claude runtime and never fail setup on it — the daemon inherits
// the env of the shell that (re)started it, which is the common footgun.
func writeSetupDaemonAuthNote(stdout io.Writer, agent runtime.Agent) {
	if !strings.EqualFold(strings.TrimSpace(agent.Runtime), "claude") {
		return
	}
	env := runtime.InspectClaudeAuthEnv(os.LookupEnv)
	if env.Ready() {
		writeLine(stdout, "  [ok] daemon runtime auth: claude credentials present in this shell (%s)", env.MaskedDetail())
		return
	}
	writeLine(stdout, "  [warn] daemon runtime auth: no claude credentials in this shell; the daemon inherits this env.")
	writeLine(stdout, "         %s", env.Warning())
	writeLine(stdout, "         (daemon-aware auth validation is tracked in #427)")
}

func existingCompatibleAgent(ctx context.Context, store *db.Store, agent runtime.Agent) (bool, error) {
	existing, err := store.GetAgent(ctx, agent.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if existing.Runtime != agent.Runtime || existing.RuntimeRef != agent.RuntimeRef {
		return false, fmt.Errorf("agent %s already exists with runtime %s session %s", existing.Name, existing.Runtime, existing.RuntimeRef)
	}
	return true, nil
}
