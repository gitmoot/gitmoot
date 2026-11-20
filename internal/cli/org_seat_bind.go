package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
)

// Binding is an explicit handoff, not a guess based on an abandoned pane label.
func runOrgSeatBind(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org seat bind", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory")
	name := fs.String("name", "", "existing organization role")
	agent := fs.String("agent", "", "exact registered Herdr agent name; MACHINE/NAME for an agent on a federated Herdr machine (saved machine id or label)")
	retireOlderThan := fs.Duration("retire-older-than", defaultBindRetireAge, "retire the role's waiting notifications older than this (0 retires all waiting from before the bind); newer ones stay pending")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*name) == "" || strings.TrimSpace(*agent) == "" || *retireOlderThan < 0 {
		fmt.Fprintln(stderr, "usage: gitmoot org seat bind --name ROLE --agent NAME [--retire-older-than 24h] [--home DIR]")
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	role, ok := cfg.Role(strings.ToLower(strings.TrimSpace(*name)))
	if !ok {
		fmt.Fprintln(stderr, "org seat bind: unknown role")
		return 2
	}
	binding := "agent:" + strings.TrimSpace(*agent)
	ctx, cancel := context.WithTimeout(context.Background(), orgSeatExternalTimeout)
	defer cancel()
	pane, ok := cockpit.New(cockpit.Options{HerdrBin: "herdr"}).ResolvePaneByLabel(ctx, binding)
	if !ok {
		fmt.Fprintln(stderr, "org seat bind: agent is absent, archived or ambiguous (a remote agent needs MACHINE/NAME); binding unchanged")
		return 1
	}
	desired := role
	desired.Pane = binding
	// Captured before the config write: mail that arrives once the binding
	// is in place is the new agent's, and is never retired below.
	boundAt := time.Now().UTC()
	if _, _, err := config.UpsertOrgSeatRole(paths, desired, role.Pane); err != nil {
		fmt.Fprintln(stderr, "org seat bind:", err)
		return 1
	}
	where := "current pane"
	if _, _, remote := cockpit.RemoteBinding(binding); remote {
		where = "remote pane; messages wait for that machine's gitmoot client"
	}
	retired, kept, err := retireBacklogOnBind(paths, role.Name, binding, boundAt, *retireOlderThan)
	if err != nil {
		fmt.Fprintf(stderr, "org seat bind: bound role %s to %s, but retiring its older notifications failed: %v; run the bind again before delivery starts\n", role.Name, binding, err)
		return 1
	}
	fmt.Fprintf(stdout, "bound role %s to %s (%s %s); routes unchanged\n", role.Name, binding, where, pane)
	fmt.Fprintf(stdout, "notifications: %d retired (waiting longer than %s; the messages stay in gitmoot message inbox --role %s), %d kept pending for delivery\n",
		retired, *retireOlderThan, role.Name, kept)
	return 0
}

// defaultBindRetireAge keeps a routine re-bind (a seat's pane changed) from
// dropping work that arrived minutes earlier, while a binding to a new agent
// still never inherits weeks of stale mail (#2721).
const defaultBindRetireAge = 24 * time.Hour

// retireBacklogOnBind retires role's notifications that have waited longer
// than olderThan at boundAt (all from before the bind when olderThan is 0),
// so a newly bound agent is not handed stale mail as new work: for
// mac-studio that was weeks of instructions (#2721). The messages stay
// readable in the role's inbox. It returns the retired and kept counts.
func retireBacklogOnBind(paths config.Paths, role, binding string, boundAt time.Time, olderThan time.Duration) (int64, int64, error) {
	store, err := openOrgDatabase(paths)
	if err != nil {
		return 0, 0, err
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), orgSeatExternalTimeout)
	defer cancel()
	detail := fmt.Sprintf("retired when role %s was bound to %s at %s (waiting longer than %s); not delivered, still in the inbox", role, binding, boundAt.Format(time.RFC3339), olderThan)
	return store.RetireWakeBacklogForRole(ctx, role, boundAt.Add(-olderThan), boundAt, detail)
}
