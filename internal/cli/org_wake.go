package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/cockpit"
	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

func runOrgWake(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || messageHelpRequested(args) {
		printOrgWakeUsage(stdout)
		return 0
	}
	action := args[0]
	if action != "list" && action != "show" && action != "retry" && action != "supersede" {
		fmt.Fprintln(stderr, "unknown org wake command")
		return 2
	}
	fs := flag.NewFlagSet("org wake "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory")
	state := fs.String("state", "pending", "outbox state (list only)")
	reason := fs.String("reason", "", "relevance/ownership check or reason for superseding")
	jsonOutput := fs.Bool("json", false, "JSON output")
	var id int64
	flags := args[1:]
	if action != "list" {
		if len(flags) == 0 {
			fmt.Fprintln(stderr, "wake id required")
			return 2
		}
		var err error
		id, err = strconv.ParseInt(flags[0], 10, 64)
		if err != nil || id <= 0 {
			fmt.Fprintln(stderr, "invalid wake id")
			return 2
		}
		flags = flags[1:]
	}
	if err := fs.Parse(flags); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected argument")
		return 2
	}
	if refuseFlagLikeText("org wake "+action, *reason, stderr) {
		return 2
	}
	err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		if action == "list" {
			rows, err := store.ListWakeOutbox(ctx, *state)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, rows)
			}
			for _, row := range rows {
				fmt.Fprintf(stdout, "%d\t%s\t%s\tattempts=%d\tcreated=%s\t%s\n", row.ID, row.TargetRole, row.State, row.AttemptCount, row.CreatedAt, row.LastError)
			}
			return nil
		}
		row, err := store.GetWakeOutbox(ctx, id)
		if err != nil {
			return err
		}
		if action == "show" {
			return writeJSON(stdout, row)
		}
		if strings.TrimSpace(*reason) == "" {
			return fmt.Errorf("recovery requires --reason")
		}
		if action == "retry" {
			if !db.WakeProvenUnsent(row) {
				return fmt.Errorf("wake %d is not proven unsent; no retry", id)
			}
			paths, err := pathsFromFlag(*home)
			if err != nil {
				return err
			}
			cfg, err := config.LoadOrg(paths)
			if err != nil {
				return err
			}
			role, ok := cfg.Role(row.TargetRole)
			if !ok {
				return fmt.Errorf("recipient role no longer exists")
			}
			bounded, cancel := context.WithTimeout(ctx, orgSeatExternalTimeout)
			defer cancel()
			if _, ok := cockpit.New(cockpit.Options{HerdrBin: "herdr"}).ResolvePaneByLabel(bounded, role.Pane); !ok {
				return fmt.Errorf("recipient is not currently registered; repair its binding first")
			}
		}
		if err := store.RecoverWakeOutbox(ctx, row, action == "retry", *reason); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wake %d %s recorded; no claim of recipient acknowledgment\n", id, action)
		return nil
	})
	if err != nil {
		fmt.Fprintln(stderr, "org wake:", err)
		return 1
	}
	return 0
}

func printOrgWakeUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: gitmoot org wake list [--state STATE] [--json] [--home DIR]\n       gitmoot org wake show ID [--json] [--home DIR]\n       gitmoot org wake retry ID --reason TEXT [--home DIR]\n       gitmoot org wake supersede ID --reason TEXT [--home DIR]\nRetry accepts only proven-unsent failures. Inspect current review head and ownership first; unknown delivery cannot be retried.\nA reason that is a single flag-like word, such as --help or -x, is refused.")
}
