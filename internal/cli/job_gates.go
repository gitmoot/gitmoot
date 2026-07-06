package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// runJobGates dispatches the resumable-gate surface (#682): make a `blocked` +
// `needs` stage actionable.
//
//	gitmoot job gates <id> [--json]                     — list the gates
//	gitmoot job gates clear <id> --need "<text>"|--all  — mark gate(s) satisfied
//
// Clearing a gate is resume-on-clear: when the LAST gate is satisfied the blocked
// stage auto-re-runs through the existing RetryJob machinery (guarded so session
// jobs and awaiting-human pauses are never bypassed).
func runJobGates(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printJobGatesUsage(stdout)
		return 0
	}
	if args[0] == "clear" {
		return runJobGatesClear(args[1:], stdout, stderr)
	}
	return runJobGatesList(args, stdout, stderr)
}

func printJobGatesUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot job gates <id> [--json]")
	fmt.Fprintln(w, "  gitmoot job gates clear <id> --need \"<text>\"|--all")
}

// jobGateEntry is the JSON shape for `job gates <id> --json`.
type jobGateEntry struct {
	Need        string `json:"need"`
	Satisfied   bool   `json:"satisfied"`
	CreatedAt   string `json:"created_at,omitempty"`
	SatisfiedAt string `json:"satisfied_at,omitempty"`
}

func runJobGatesList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job gates", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print gates as JSON")
	jobID, ok := parseSingleJobID(fs, args, stderr, "job gates")
	if !ok {
		return parseSingleJobIDExitCode(args)
	}
	var gates []db.JobGate
	if err := withStore(*home, func(store *db.Store) error {
		if _, err := store.GetJob(context.Background(), jobID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("job %q not found", jobID)
			}
			return err
		}
		var err error
		gates, err = store.ListJobGates(context.Background(), jobID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "job gates: %v\n", err)
		return 1
	}
	if *jsonOutput {
		entries := make([]jobGateEntry, 0, len(gates))
		for _, g := range gates {
			entries = append(entries, jobGateEntry{Need: g.Need, Satisfied: g.Satisfied, CreatedAt: g.CreatedAt, SatisfiedAt: g.SatisfiedAt})
		}
		if err := writeJSON(stdout, entries); err != nil {
			fmt.Fprintf(stderr, "job gates: %v\n", err)
			return 1
		}
		return 0
	}
	if len(gates) == 0 {
		fmt.Fprintln(stdout, "no gates recorded for this job")
		return 0
	}
	open := 0
	for _, g := range gates {
		status := "open"
		if g.Satisfied {
			status = "satisfied"
		} else {
			open++
		}
		fmt.Fprintf(stdout, "%s\t%s\n", status, g.Need)
	}
	fmt.Fprintf(stdout, "%d gate(s), %d open\n", len(gates), open)
	return 0
}

func runJobGatesClear(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job gates clear", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printJobGatesUsage(stderr) }
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	need := fs.String("need", "", "exact need text of the single gate to mark satisfied")
	all := fs.Bool("all", false, "mark every open gate satisfied")

	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "job gates clear requires exactly one id")
			return 2
		}
		return 0
	}
	jobID := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "job gates clear requires exactly one id")
		return 2
	}
	needText := strings.TrimSpace(*need)
	if (*all && needText != "") || (!*all && needText == "") {
		fmt.Fprintln(stderr, "job gates clear requires exactly one of --need \"<text>\" or --all")
		return 2
	}

	var (
		outcome workflow.GateResumeOutcome
		matched int
		unknown bool
	)
	if err := withStore(*home, func(store *db.Store) error {
		if _, err := store.GetJob(context.Background(), jobID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("job %q not found", jobID)
			}
			return err
		}
		if *all {
			cleared, err := store.SatisfyAllJobGates(context.Background(), jobID)
			if err != nil {
				return err
			}
			matched = cleared
		} else {
			ok, err := store.SatisfyJobGate(context.Background(), jobID, needText)
			if err != nil {
				return err
			}
			if !ok {
				unknown = true
				return nil
			}
			matched = 1
		}
		var err error
		outcome, err = workflow.MaybeResumeOnGatesCleared(context.Background(), store, jobID)
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "job gates clear: %v\n", err)
		return 1
	}
	if unknown {
		fmt.Fprintf(stderr, "job gates clear: no open gate with need %q on job %s\n", needText, jobID)
		return 1
	}
	if *all {
		fmt.Fprintf(stdout, "cleared %d gate(s) on job %s\n", matched, jobID)
	} else {
		fmt.Fprintf(stdout, "cleared gate %q on job %s\n", needText, jobID)
	}
	if outcome.Resumed {
		fmt.Fprintf(stdout, "resumed: %s\n", outcome.Reason)
	} else {
		fmt.Fprintf(stdout, "not resumed: %s\n", outcome.Reason)
	}
	return 0
}
