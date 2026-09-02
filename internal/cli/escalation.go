package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// runEscalation is the OPERATOR SURFACE for parked human-escalation rounds (#1673).
//
// A round whose claimed human decision could not be applied parks in needs_repair,
// which deliberately blocks both a new escalation and ordinary advance for that
// coordinator. Without a command to move it, that block would be resolvable only by
// database surgery — so the surface is part of the fix, not a follow-up.
func runEscalation(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printEscalationUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runEscalationList(args[1:], stdout, stderr)
	case "repair":
		return runEscalationRepair(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown escalation command %q\n\n", args[0])
		printEscalationUsage(stderr)
		return 2
	}
}

func printEscalationUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot escalation list [--home DIR] [--json]")
	fmt.Fprintln(w, "  gitmoot escalation repair <coordinator-job-id> --round ID --retry [--home DIR] [--json]")
	fmt.Fprintln(w, "  gitmoot escalation repair <coordinator-job-id> --round ID --supersede --reason TEXT [--by NAME] [--home DIR] [--json]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "A parked round blocks new escalations and ordinary advance for its coordinator.")
	fmt.Fprintln(w, "--retry     re-arms the PRESERVED human decision; the next daemon sweep replays it.")
	fmt.Fprintln(w, "--supersede DISCARDS that decision without applying it. It is the only such path,")
	fmt.Fprintln(w, "            it requires --reason, and it records who did it.")
}

type escalationRepairRow struct {
	JobID   string `json:"job_id"`
	RoundID string `json:"round_id"`
	Verb    string `json:"verb"`
	Cause   string `json:"cause"`
}

type escalationRepairOutput struct {
	Action  string `json:"action"`
	JobID   string `json:"job_id"`
	RoundID string `json:"round_id"`
	Reason  string `json:"reason,omitempty"`
	By      string `json:"by,omitempty"`
}

func runEscalationList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("escalation list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	jsonOutput := fs.Bool("json", false, "print the blocked rounds as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var rows []escalationRepairRow
	err := withStore(*home, func(store *db.Store) error {
		reports, err := (workflow.Engine{Store: store}).EscalationRoundsNeedingRepair(context.Background())
		if err != nil {
			return err
		}
		for _, report := range reports {
			rows = append(rows, escalationRepairRow{
				JobID:   report.JobID,
				RoundID: report.RoundID,
				Verb:    report.Verb,
				Cause:   report.Cause,
			})
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "gitmoot: escalation list: %v\n", err)
		return 1
	}
	if *jsonOutput {
		encoded, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "gitmoot: escalation list: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
		return 0
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no escalation rounds need repair")
		return 0
	}
	for _, row := range rows {
		fmt.Fprintf(stdout, "%s round=%s verb=%s cause=%s\n", row.JobID, row.RoundID, row.Verb, row.Cause)
	}
	return 0
}

func runEscalationRepair(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("escalation repair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	roundFlag := fs.String("round", "", "round id to repair (see `gitmoot escalation list`)")
	retry := fs.Bool("retry", false, "re-arm the preserved human decision for the next sweep")
	supersede := fs.Bool("supersede", false, "DISCARD the claimed human decision without applying it")
	reasonFlag := fs.String("reason", "", "required with --supersede: why the decision is being discarded")
	byFlag := fs.String("by", "operator", "who is performing the repair, recorded in the trail")
	jsonOutput := fs.Bool("json", false, "print the repair result as JSON")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fs.Usage()
		if len(args) == 0 {
			fmt.Fprintln(stderr, "escalation repair requires a coordinator job id")
			return 2
		}
		return 0
	}
	jobID := strings.TrimSpace(args[0])
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	roundID := strings.TrimSpace(*roundFlag)
	if jobID == "" || roundID == "" {
		fmt.Fprintln(stderr, "escalation repair requires a coordinator job id and --round")
		return 2
	}
	if *retry == *supersede {
		fmt.Fprintln(stderr, "escalation repair requires exactly one of --retry or --supersede")
		return 2
	}
	if *supersede && strings.TrimSpace(*reasonFlag) == "" {
		fmt.Fprintln(stderr, "--supersede discards a human decision, so it requires --reason")
		return 2
	}
	output := escalationRepairOutput{JobID: jobID, RoundID: roundID, By: strings.TrimSpace(*byFlag)}
	if *supersede {
		output.Action = "supersede"
		output.Reason = strings.TrimSpace(*reasonFlag)
	} else {
		output.Action = "retry"
	}
	err := withStore(*home, func(store *db.Store) error {
		return (workflow.Engine{Store: store}).RepairEscalationRound(context.Background(),
			jobID, roundID, *supersede, output.By, output.Reason)
	})
	if err != nil {
		fmt.Fprintf(stderr, "gitmoot: escalation repair: %v\n", err)
		return 1
	}
	if *jsonOutput {
		encoded, mErr := json.MarshalIndent(output, "", "  ")
		if mErr != nil {
			fmt.Fprintf(stderr, "gitmoot: escalation repair: %v\n", mErr)
			return 1
		}
		fmt.Fprintln(stdout, string(encoded))
		return 0
	}
	if *supersede {
		fmt.Fprintf(stdout, "superseded round %s on %s without applying its %s decision: %s\n",
			roundID, jobID, "claimed", output.Reason)
		return 0
	}
	fmt.Fprintf(stdout, "re-armed round %s on %s; its preserved decision replays on the next sweep\n", roundID, jobID)
	return 0
}
