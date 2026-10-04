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
	"unicode"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// messageUsage lists each message subcommand's usage line in display order, so
// the full usage and a single subcommand's usage never drift apart.
var messageUsage = []struct{ action, line string }{
	{"send", `gitmoot message send ROLE "TEXT" [--workflow LABEL] [--role ROLE] [--json] [--home DIR]`},
	{"inbox", `gitmoot message inbox [--before ID] [--limit 20] [--role ROLE] [--json] [--home DIR]`},
	{"show", `gitmoot message show ID [--thread] [--before ID] [--limit 20] [--role ROLE] [--json] [--home DIR]`},
	{"reply", `gitmoot message reply ID "TEXT" [--role ROLE] [--json] [--home DIR]`},
	{"escalate", `gitmoot message escalate [--workflow LABEL] [--to ROLE] [--repo OWNER/REPO] [--role ROLE] [--json] [--home DIR] "QUESTION"`},
	{"resolve", `gitmoot message resolve ID [--answer TEXT | --note ID] [--role ROLE] [--json] [--home DIR]`},
	{"pending", `gitmoot message pending --claim --hook UserPromptSubmit|PostToolUse|Stop --runtime claude|codex`},
	{"directive", `gitmoot message directive send|ack|done|cancel --help`},
}

const messageUsageNotes = `Messages are durable ordinary conversation between registered fleet roles.
No workflow, job, subscription or acknowledgment is required. Flags follow positional arguments.
-h, -help or --help anywhere prints usage and saves nothing.
Message text that is a single flag-like word, such as --help or -x, is refused.
Sender defaults to GITMOOT_ORG_ROLE, then the current registered Herdr pane.
--role is an operator attribution override, not an authentication credential.
Saved mail, notification submission, reading and task completion are separate facts.
Escalations track decisions; directives retain issuer and recipient authority checks.`

func printMessageUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	for _, usage := range messageUsage {
		fmt.Fprintln(w, "  "+usage.line)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, messageUsageNotes)
}

// printMessageActionUsage prints one subcommand's usage line plus the shared
// message notes.
func printMessageActionUsage(w io.Writer, action string) {
	fmt.Fprintln(w, "Usage:")
	for _, usage := range messageUsage {
		if usage.action == action {
			fmt.Fprintln(w, "  "+usage.line)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, messageUsageNotes)
}

// messageHelpRequested reports whether any argument before a "--" terminator
// asks for usage. Help wins over every other argument, so a help flag is never
// stored as message text or taken as a flag value (#2306). Text after "--" is
// left to the command, which may accept it as an explicit body.
func messageHelpRequested(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--":
			return false
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}

// messageTextLooksLikeFlag reports whether text is a single flag-like word
// such as --json or -x. That is almost always a mistyped or misplaced flag, so
// commands refuse it rather than deliver it as a message. Text that starts with
// a dash but contains other words is ordinary text.
func messageTextLooksLikeFlag(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "-") && !strings.ContainsFunc(text, unicode.IsSpace)
}

// refuseFlagLikeText reports whether text was refused as a flag-like word,
// printing the refusal for command.
func refuseFlagLikeText(command, text string, stderr io.Writer) bool {
	if !messageTextLooksLikeFlag(text) {
		return false
	}
	fmt.Fprintf(stderr, "%s: refusing text %q because it looks like a flag; nothing was saved (run gitmoot %s --help for usage)\n", command, strings.TrimSpace(text), command)
	return true
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
	if len(args) == 0 || messageHelpRequested(args[:1]) {
		printMessageUsage(stdout)
		return 0
	}
	action := args[0]
	positional := 0
	switch action {
	case "escalate":
		return runOrgEscalate(args[1:], stdout, stderr)
	case "resolve":
		return runOrgEscalateResolve(args[1:], stdout, stderr)
	case "directive":
		return runOrgDirective(args[1:], stdout, stderr)
	case "pending":
		return runMessagePending(args[1:], stdout, stderr)
	}
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
	if messageHelpRequested(args[1:]) {
		printMessageActionUsage(stdout, action)
		return 0
	}
	if len(args) < positional+1 {
		printMessageActionUsage(stderr, action)
		return 2
	}
	if positional == 2 && refuseFlagLikeText("message "+action, args[2], stderr) {
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
	thread := false
	if action == "show" {
		fs.BoolVar(&thread, "thread", false, "show the conversation thread")
	}
	if action == "send" {
		fs.StringVar(&workflowID, "workflow", "", "optional registered workflow")
	}
	if action == "inbox" || action == "show" {
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
		if action == "show" && thread {
			messages, err := store.ListMessageThread(ctx, id, role, before, limit)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writeJSON(stdout, messages)
			}
			for _, message := range messages {
				printInboxMessage(stdout, message)
			}
			if len(messages) == limit {
				fmt.Fprintf(stdout, "Older thread messages: gitmoot message show %d --thread --before %d\n", id, messages[len(messages)-1].ID)
			}
			return nil
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
				fmt.Fprintf(stdout, "%d  %s  from %s  thread %d  notification=%s  lifecycle=%s\n  %s\n", m.ID, m.Kind, m.Sender, m.ThreadID, m.Status, m.Lifecycle, terminalSafeWorkflowText(m.Body))
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
				if _, ok := cfg.Role(recipient); !ok && recipient != db.MessageSystemSender {
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
		printInboxMessage(stdout, message)
		return nil
	})
	if err != nil {
		fmt.Fprintln(stderr, "message:", err)
		return 1
	}
	return 0
}

func printInboxMessage(stdout io.Writer, message db.Message) {
	fmt.Fprintf(stdout, "Message %d · %s · thread %d · from %s to %s · notification=%s\n", message.ID, message.Kind, message.ThreadID, message.Sender, message.Recipient, message.Status)
	if message.Historical {
		fmt.Fprintln(stdout, "Historical record; not replayed by migration.")
	}
	if message.Lifecycle != "" {
		fmt.Fprintf(stdout, "Lifecycle: %s\n", message.Lifecycle)
	}
	if message.NotificationReason != "" {
		fmt.Fprintf(stdout, "Notification detail: %s\n", terminalSafeWorkflowText(message.NotificationReason))
	}
	if message.ReplyTo != 0 {
		fmt.Fprintf(stdout, "Reply to message %d\n", message.ReplyTo)
	}
	if message.WorkflowID != "" {
		fmt.Fprintf(stdout, "Workflow: %s\n", message.WorkflowID)
	}
	if message.SourceJobID != "" {
		fmt.Fprintf(stdout, "Source: job %s · state=%s\n", message.SourceJobID, terminalSafeWorkflowText(message.SourceState))
	}
	if message.HeadSHA != "" {
		fmt.Fprintf(stdout, "Review: %s#%d · head=%s · purpose=%s · decision=%s\n",
			terminalSafeWorkflowText(message.Repo), message.PullRequest, terminalSafeWorkflowText(message.HeadSHA),
			terminalSafeWorkflowText(message.ReviewPurpose), terminalSafeWorkflowText(message.ReviewDecision))
	}
	fmt.Fprintln(stdout, scrubWorkflowText(message.Body, 0))
}
