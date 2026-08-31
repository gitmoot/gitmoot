package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type orgMessageOutput struct {
	ID       int64  `json:"id"`
	From     string `json:"from"`
	To       string `json:"to"`
	Workflow string `json:"workflow"`
	Message  string `json:"message"`
}

func runOrgMessage(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printOrgMessageUsage(stdout)
		return 0
	}
	if args[0] != "send" {
		fmt.Fprintf(stderr, "unknown org message command %q\n", args[0])
		printOrgMessageUsage(stderr)
		return 2
	}
	return runOrgMessageSend(args[1:], stdout, stderr)
}

func printOrgMessageUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot org message send --to ROLE --workflow LABEL [--org-role ROLE] [--repo OWNER/REPO] [--json] [--home DIR] MESSAGE")
	fmt.Fprintln(w, "Messages are durable sender-attributed heads-ups with no acknowledgment or completion obligation.")
	fmt.Fprintln(w, "The sender and recipient must be distinct roles with the same parent.")
}

func runOrgMessageSend(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org message send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	toFlag := fs.String("to", "", "same-parent sibling role receiving the message")
	workflowID := fs.String("workflow", "", "workflow label for the durable message note")
	fromFlag := fs.String("org-role", "", "acting organization role")
	repo := fs.String("repo", "", "repository binding for the message note")
	jsonOutput := fs.Bool("json", false, "print the durable message as JSON")
	message, flagArgs, ok := orgAddressedTextAndFlags(args)
	if !ok {
		fmt.Fprintln(stderr, "org message send requires exactly one non-empty message")
		return 2
	}
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "org message send requires exactly one non-empty message")
		return 2
	}

	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org message send: resolve paths: %v\n", err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org message send: %v\n", err)
		return 1
	}
	if !cfg.Enabled() {
		fmt.Fprintln(stderr, "org message send requires an [org] registry")
		return 2
	}
	from := strings.ToLower(strings.TrimSpace(*fromFlag))
	if from == "" {
		from = strings.ToLower(strings.TrimSpace(os.Getenv("GITMOOT_ORG_ROLE")))
	}
	fromRole, ok := cfg.Role(from)
	if !ok {
		fmt.Fprintf(stderr, "org message send: unknown acting org role %q; set GITMOOT_ORG_ROLE or --org-role\n", from)
		return 2
	}
	to := strings.ToLower(strings.TrimSpace(*toFlag))
	toRole, ok := cfg.Role(to)
	if !ok {
		fmt.Fprintf(stderr, "org message send: unknown target org role %q\n", to)
		return 2
	}
	if from == to {
		fmt.Fprintf(stderr, "org message send: --to %q must differ from acting role %q\n", to, from)
		return 2
	}
	if fromRole.Parent == "" || fromRole.Parent != toRole.Parent {
		fmt.Fprintf(stderr, "org message send: roles %q and %q do not share a parent\n", from, to)
		return 2
	}

	label := strings.TrimSpace(*workflowID)
	if err := workflow.ValidateWorkflowID(label); err != nil {
		fmt.Fprintf(stderr, "org message send: %v\n", err)
		return 2
	}
	noteBody := workflow.FormatOrgMessageNote(from, to, label, message)
	if noteBody == "" || len(noteBody) > workflowNoteBodyMax {
		fmt.Fprintf(stderr, "org message send: message must produce a note of at most %d bytes\n", workflowNoteBodyMax)
		return 2
	}
	var note db.WorkflowNote
	if err := withStore(*home, func(store *db.Store) error {
		count, err := store.CountJobsByWorkflow(context.Background(), label)
		if err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("workflow %q has no jobs; refusing message to guard against a typo", label)
		}
		note, err = store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
			WorkflowID:      label,
			Author:          from,
			Body:            noteBody,
			Repo:            strings.TrimSpace(*repo),
			AddressedTarget: to,
		})
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "org message send: %v\n", err)
		return 1
	}

	out := orgMessageOutput{ID: note.ID, From: from, To: to, Workflow: label, Message: message}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			fmt.Fprintf(stderr, "org message send: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "sent message %d from %s to %s in workflow %s\n", note.ID, from, to, label)
	return 0
}
