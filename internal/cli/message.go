package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func printMessageUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  gitmoot message send ROLE "TEXT" [--workflow LABEL] [--role ROLE] [--json] [--home DIR]
  gitmoot message inbox [--before ID] [--limit 20] [--role ROLE] [--json] [--home DIR]
  gitmoot message show ID [--role ROLE] [--json] [--home DIR]
  gitmoot message reply ID "TEXT" [--role ROLE] [--json] [--home DIR]

Messages are durable ordinary conversation between registered fleet roles.
No workflow, job, subscription or acknowledgment is required. Flags follow positional arguments.
Sender defaults to GITMOOT_ORG_ROLE, then the current registered Herdr pane.
--role is an operator attribution override, not an authentication credential.
Saved mail, notification submission, reading and task completion are separate facts.
Escalations and directives retain their existing authorized commands until their cutover.`)
}

func messageActingRole(ctx context.Context, cfg config.OrgConfig, explicit string) (string, error) {
	role := strings.ToLower(strings.TrimSpace(explicit))
	if role == "" {
		role = strings.ToLower(strings.TrimSpace(os.Getenv("GITMOOT_ORG_ROLE")))
	}
	if role == "" {
		pane := strings.TrimSpace(os.Getenv("HERDR_PANE_ID"))
		if pane != "" {
			snapshot, err := orgProviderSnapshot(ctx, cfg)
			if err != nil {
				return "", fmt.Errorf("cannot resolve current sender: %w", err)
			}
			for name, binding := range snapshot.PaneBindings {
				if binding.PaneID == pane {
					if role != "" {
						return "", errors.New("current pane maps to multiple roles; use --role")
					}
					role = name
				}
			}
		}
	}
	if role == "" {
		return "", errors.New("sender role unavailable; set GITMOOT_ORG_ROLE or use --role")
	}
	if _, ok := cfg.Role(role); !ok {
		return "", fmt.Errorf("unknown role %q", role)
	}
	return role, nil
}

func runMessage(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printMessageUsage(stdout)
		return 0
	}
	action := args[0]
	positional := 0
	switch action {
	case "send", "reply":
		positional = 2
	case "show":
		positional = 1
	case "inbox":
	default:
		fmt.Fprintln(stderr, "unknown message command")
		return 2
	}
	if len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
		printMessageUsage(stdout)
		return 0
	}
	if len(args) < positional+1 {
		printMessageUsage(stderr)
		return 2
	}
	fs := flag.NewFlagSet("message "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory")
	roleFlag := fs.String("role", "", "operator attribution override")
	jsonOutput := fs.Bool("json", false, "JSON output")
	workflowID := ""
	before := int64(0)
	limit := 20
	if action == "send" {
		fs.StringVar(&workflowID, "workflow", "", "optional registered workflow")
	}
	if action == "inbox" {
		fs.Int64Var(&before, "before", 0, "older than this message ID")
		fs.IntVar(&limit, "limit", 20, "page size 1–100")
	}
	if err := fs.Parse(args[positional+1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected message argument")
		return 2
	}
	var id int64
	if action == "show" || action == "reply" {
		var err error
		id, err = strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 0 {
			fmt.Fprintln(stderr, "invalid message ID")
			return 2
		}
	}
	if workflowID != "" {
		if err := workflow.ValidateWorkflowID(workflowID); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	if before < 0 || limit < 1 || limit > 100 {
		fmt.Fprintln(stderr, "inbox requires --before >= 0 and --limit 1–100")
		return 2
	}
	err := withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
		cfg, err := config.LoadOrg(paths)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), orgSeatExternalTimeout)
		defer cancel()
		role, err := messageActingRole(ctx, cfg, *roleFlag)
		if err != nil {
			return err
		}
		if action == "inbox" {
			messages, err := store.ListMessages(ctx, role, before, limit)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, messages)
			}
			if len(messages) == 0 {
				fmt.Fprintln(stdout, "No messages.")
			}
			for _, m := range messages {
				fmt.Fprintf(stdout, "%d  from %s  thread %d  notification=%s\n  %s\n", m.ID, m.Sender, m.ThreadID, m.Status, terminalSafeWorkflowText(m.Body))
			}
			if len(messages) == limit {
				fmt.Fprintf(stdout, "Older messages: gitmoot message inbox --before %d\n", messages[len(messages)-1].ID)
			}
			return nil
		}
		var message db.Message
		if action == "show" {
			message, err = store.GetMessage(ctx, id, role)
		} else {
			input := db.Message{Sender: role, Body: args[2], WorkflowID: workflowID}
			if action == "reply" {
				parent, lookupErr := store.GetMessage(ctx, id, role)
				if lookupErr != nil {
					return errors.New("message not found or not visible to this role")
				}
				recipient := parent.Sender
				if recipient == role {
					recipient = parent.Recipient
				}
				if _, ok := cfg.Role(recipient); !ok {
					return fmt.Errorf("recipient role %q is retired", recipient)
				}
				input.ReplyTo = id
			} else {
				input.Recipient = strings.ToLower(strings.TrimSpace(args[1]))
				if _, ok := cfg.Role(input.Recipient); !ok {
					return fmt.Errorf("unknown recipient role %q", input.Recipient)
				}
			}
			message, err = store.CreateMessage(ctx, input)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("message not found or not visible to this role")
		}
		if err != nil {
			return err
		}
		if *jsonOutput {
			return writeJSON(stdout, message)
		}
		if action != "show" {
			fmt.Fprintf(stdout, "saved message %d from %s to %s; notification=%s; no acknowledgment required\n", message.ID, message.Sender, message.Recipient, message.Status)
			return nil
		}
		fmt.Fprintf(stdout, "Message %d · thread %d · from %s to %s · notification=%s\n", message.ID, message.ThreadID, message.Sender, message.Recipient, message.Status)
		if message.ReplyTo != 0 {
			fmt.Fprintf(stdout, "Reply to message %d\n", message.ReplyTo)
		}
		if message.WorkflowID != "" {
			fmt.Fprintf(stdout, "Workflow: %s\n", message.WorkflowID)
		}
		fmt.Fprintln(stdout, scrubWorkflowText(message.Body, 0))
		return nil
	})
	if err != nil {
		fmt.Fprintln(stderr, "message:", err)
		return 1
	}
	return 0
}
