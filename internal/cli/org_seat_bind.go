package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
)

// Binding is an explicit handoff, not a guess based on an abandoned pane label.
func runOrgSeatBind(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org seat bind", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory")
	name := fs.String("name", "", "existing organization role")
	agent := fs.String("agent", "", "exact registered local Herdr agent name")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*name) == "" || strings.TrimSpace(*agent) == "" {
		fmt.Fprintln(stderr, "usage: gitmoot org seat bind --name ROLE --agent NAME [--home DIR]")
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
		fmt.Fprintln(stderr, "org seat bind: agent is absent, remote, archived or ambiguous; binding unchanged")
		return 1
	}
	desired := role
	desired.Pane = binding
	if _, _, err := config.UpsertOrgSeatRole(paths, desired, role.Pane); err != nil {
		fmt.Fprintln(stderr, "org seat bind:", err)
		return 1
	}
	fmt.Fprintf(stdout, "bound role %s to %s (current pane %s); routes unchanged, old notices not replayed\n", role.Name, binding, pane)
	return 0
}
