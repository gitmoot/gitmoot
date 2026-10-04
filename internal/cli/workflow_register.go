package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func runWorkflowRegister(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("workflow register", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory")
	jsonOutput := fs.Bool("json", false, "JSON output")
	if helpRequested(args) {
		fmt.Fprintln(stdout, "usage: workflow register LABEL DESCRIPTION [--home DIR] [--json]")
		return 0
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: workflow register LABEL DESCRIPTION [--home DIR] [--json]")
		return 2
	}
	label, description := strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	if refuseFlagLikeText("workflow register", description, stderr) {
		return 2
	}
	if err := fs.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || description == "" || len(description) > workflowSummaryMax {
		fmt.Fprintln(stderr, "workflow register requires one label and non-empty description")
		return 2
	}
	if err := workflow.ValidateWorkflowID(label); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var summary db.WorkflowSummary
	err := withStore(*home, func(store *db.Store) error {
		if err := store.RegisterWorkflow(context.Background(), label, description); err != nil {
			return err
		}
		var err error
		summary, err = store.WorkflowSummary(context.Background(), label)
		return err
	})
	if err != nil {
		fmt.Fprintln(stderr, "workflow register:", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, summary); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "registered workflow %s; no job or notification created\n", label)
	return 0
}
