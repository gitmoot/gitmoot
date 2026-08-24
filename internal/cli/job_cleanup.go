package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

func runJobCleanup(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printJobCleanupUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runJobCleanupList(args[1:], stdout, stderr)
	case "reopen":
		return runJobCleanupReopen(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown job cleanup command %q\n\n", args[0])
		printJobCleanupUsage(stderr)
		return 2
	}
}

func printJobCleanupUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot job cleanup list [--state pending|retryable|removed|quarantined] [--json]")
	fmt.Fprintln(w, "  gitmoot job cleanup reopen <resource-id>")
}

func runJobCleanupList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job cleanup list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	state := fs.String("state", "", "cleanup state filter")
	jsonOutput := fs.Bool("json", false, "print cleanup obligations as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "job cleanup list does not accept positional arguments")
		return 2
	}
	if value := strings.TrimSpace(*state); value != "" && value != db.CleanupObligationPending && value != db.CleanupObligationRetryable && value != db.CleanupObligationRemoved && value != db.CleanupObligationQuarantined {
		fmt.Fprintf(stderr, "job cleanup list: invalid state %q\n", value)
		return 2
	}
	var obligations []db.CleanupObligation
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		obligations, err = store.ListCleanupObligations(context.Background(), *state)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "job cleanup list: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if obligations == nil {
			obligations = []db.CleanupObligation{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(obligations); err != nil {
			fmt.Fprintf(stderr, "job cleanup list: %v\n", err)
			return 1
		}
		return 0
	}
	for _, obligation := range obligations {
		fmt.Fprintf(stdout, "%s  %-11s attempts=%d job=%s path=%s reason=%s next=%s\n",
			obligation.ResourceID, obligation.State, obligation.AttemptCount,
			obligation.OwnerJobID, obligation.ExpectedPath, obligation.Reason,
			obligation.NextAttemptAt)
	}
	return 0
}

func runJobCleanupReopen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job cleanup reopen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
		fmt.Fprintln(stderr, "job cleanup reopen requires exactly one resource id")
		return 2
	}
	resourceID := strings.TrimSpace(fs.Arg(0))
	var obligation db.CleanupObligation
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		obligation, err = store.ReopenCleanupObligation(context.Background(), resourceID, time.Now().UTC())
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "job cleanup reopen: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "reopened cleanup obligation %s for job %s\n", obligation.ResourceID, obligation.OwnerJobID)
	return 0
}
