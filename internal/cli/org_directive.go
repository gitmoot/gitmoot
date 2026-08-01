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

var orgDirectiveStdin io.Reader = os.Stdin

func runOrgDirective(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printOrgDirectiveUsage(stdout)
		return 0
	}
	switch args[0] {
	case "send":
		return runOrgDirectiveSend(args[1:], stdout, stderr)
	case "ack":
		return runOrgDirectiveReceipt("ack", args[1:], stdout, stderr)
	case "cancel":
		return runOrgDirectiveReceipt("cancel", args[1:], stdout, stderr)
	case "done":
		return runOrgDirectiveReceipt("done", args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown org directive command %q\n", args[0])
		printOrgDirectiveUsage(stderr)
		return 2
	}
}

func printOrgDirectiveUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gitmoot org directive send --to ROLE --workflow LABEL (--stdin | -F FILE | TEXT) [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org directive ack ID [--by ROLE] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org directive cancel ID [--by ROLE] [--home DIR]")
	fmt.Fprintln(w, "  gitmoot org directive done ID [--by ROLE] [--home DIR]")
	fmt.Fprintln(w, "Acknowledgment records receipt; `done` records COMPLETION and ends the")
	fmt.Fprintln(w, "obligation, including its TTL nudges. Both are restricted to the target role")
	fmt.Fprintln(w, "or an ancestor -- which includes the sender, since a sender is always an")
	fmt.Fprintln(w, "ancestor. Only the sender may cancel.")
}

func runOrgDirectiveSend(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org directive send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	toFlag := fs.String("to", "", "descendant organization role receiving the directive")
	workflowID := fs.String("workflow", "", "workflow label for the directive note")
	stdin := fs.Bool("stdin", false, "read directive body from stdin")
	file := fs.String("F", "", "read directive body from file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	body, err := readOrgDirectiveBody(fs.Args(), *stdin, *file)
	if err != nil {
		fmt.Fprintf(stderr, "org directive send: %v\n", err)
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org directive send: resolve paths: %v\n", err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org directive send: %v\n", err)
		return 1
	}
	from := strings.ToLower(strings.TrimSpace(os.Getenv("GITMOOT_ORG_ROLE")))
	if _, ok := cfg.Role(from); !ok {
		fmt.Fprintf(stderr, "org directive send: unknown acting org role %q; set GITMOOT_ORG_ROLE\n", from)
		return 2
	}
	to := strings.ToLower(strings.TrimSpace(*toFlag))
	if _, ok := cfg.Role(to); !ok {
		fmt.Fprintf(stderr, "org directive send: unknown target org role %q\n", to)
		return 2
	}
	if from == to || !slicesContains(cfg.Ancestors(to), from) {
		fmt.Fprintf(stderr, "org directive send: role %q is not an ancestor of target %q; peer and upward directives are refused\n", from, to)
		return 2
	}
	label := strings.TrimSpace(*workflowID)
	if err := workflow.ValidateWorkflowID(label); err != nil {
		fmt.Fprintf(stderr, "org directive send: %v\n", err)
		return 2
	}
	noteBody := workflow.FormatOrgDirectiveNote(from, to, label, body)
	if noteBody == "" || len(noteBody) > workflowNoteBodyMax {
		fmt.Fprintf(stderr, "org directive send: body must produce a note of at most %d bytes\n", workflowNoteBodyMax)
		return 2
	}
	var note db.WorkflowNote
	if err := withStore(*home, func(store *db.Store) error {
		var err error
		note, err = store.InsertWorkflowNote(context.Background(), db.WorkflowNote{
			WorkflowID: label, Author: from, Body: noteBody,
			AddressedTarget: to, AddressedWakeKind: db.WakeOutboxKindDirective,
		})
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "org directive send: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "sent directive %d from %s to %s in workflow %s\n", note.ID, from, to, label)
	return 0
}

func readOrgDirectiveBody(args []string, stdin bool, file string) (string, error) {
	sources := 0
	if stdin {
		sources++
	}
	if strings.TrimSpace(file) != "" {
		sources++
	}
	if len(args) != 0 {
		sources++
	}
	if sources != 1 || len(args) > 1 {
		return "", errors.New("choose exactly one body source: --stdin, -F FILE, or one TEXT argument")
	}
	var raw []byte
	var err error
	switch {
	case stdin:
		raw, err = io.ReadAll(io.LimitReader(orgDirectiveStdin, workflowNoteBodyMax+1))
	case file != "":
		raw, err = os.ReadFile(file)
	default:
		raw = []byte(args[0])
	}
	if err != nil {
		return "", err
	}
	if len(raw) > workflowNoteBodyMax || strings.TrimSpace(string(raw)) == "" {
		return "", fmt.Errorf("body must be non-empty and at most %d bytes", workflowNoteBodyMax)
	}
	return string(raw), nil
}

func runOrgDirectiveReceipt(kind string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org directive "+kind, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	byFlag := fs.String("by", "", "organization role recording the "+kind)
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			_ = fs.Parse([]string{"--help"})
			return 0
		}
	}
	idText, flagArgs, ok := orgDirectiveReceiptIDAndFlags(args)
	if !ok {
		fmt.Fprintf(stderr, "org directive %s requires exactly one directive id\n", kind)
		return 2
	}
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	directiveID, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || directiveID <= 0 {
		fmt.Fprintf(stderr, "org directive %s: invalid directive id %q\n", kind, idText)
		return 2
	}
	paths, err := pathsFromFlag(*home)
	if err != nil {
		fmt.Fprintf(stderr, "org directive %s: resolve paths: %v\n", kind, err)
		return 1
	}
	cfg, err := config.LoadOrg(paths)
	if err != nil {
		fmt.Fprintf(stderr, "org directive %s: %v\n", kind, err)
		return 1
	}
	by := strings.ToLower(strings.TrimSpace(*byFlag))
	if by == "" {
		by = strings.ToLower(strings.TrimSpace(os.Getenv("GITMOOT_ORG_ROLE")))
	}
	if by == "" {
		fmt.Fprintf(stderr, "org directive %s: acting org role is required; pass --by or set GITMOOT_ORG_ROLE\n", kind)
		return 1
	}
	var workflowID string
	if err := withStore(*home, func(store *db.Store) error {
		ctx := context.Background()
		target, err := store.GetWorkflowNote(ctx, directiveID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("directive %d not found", directiveID)
		}
		if err != nil {
			return err
		}
		from, to, _, _, ok := workflow.ParseOrgDirectiveNote(target.Body)
		if !ok {
			return fmt.Errorf("note %d is not an org directive", directiveID)
		}
		if _, ok := cfg.Role(by); !ok {
			return fmt.Errorf("unknown org role %q", by)
		}
		var body string
		if kind == "ack" || kind == "done" {
			// #1352: `done` carries ack's authorization discipline deliberately —
			// the addressed target or one of its ancestors.
			//
			// NOTE, because the obvious reading is wrong: this DOES permit the
			// sender. `send` requires the sender to be an ancestor of the target,
			// so every valid sender is already an ancestor and is authorized here.
			// Ack's discipline and a sender exclusion are mutually exclusive by
			// construction; the discipline is what #1352 specifies, so a parent
			// completing an obligation it issued is oversight, not a loophole.
			// `cancel` remains sender-ONLY, which is a genuinely different rule.
			if by != to && !slicesContains(cfg.Ancestors(to), by) {
				// The ack wording is preserved byte-for-byte: it is an established
				// user-facing contract with a test pinning it.
				if kind == "ack" {
					return fmt.Errorf("role %q cannot acknowledge directive %d addressed to %q; only the target or an ancestor may acknowledge", by, directiveID, to)
				}
				return fmt.Errorf("role %q cannot record done for directive %d addressed to %q; only the target or an ancestor may complete it", by, directiveID, to)
			}
			if kind == "ack" {
				body = workflow.FormatOrgDirectiveAckNote(directiveID, by)
			} else {
				body = workflow.FormatOrgDirectiveDoneNote(directiveID, by)
			}
		} else {
			if by != from {
				return fmt.Errorf("role %q cannot cancel directive %d sent by %q", by, directiveID, from)
			}
			body = workflow.FormatOrgDirectiveCancelNote(directiveID, by)
		}
		if body == "" {
			return errors.New("could not format directive receipt")
		}
		_, err = store.InsertWorkflowNote(ctx, db.WorkflowNote{
			WorkflowID: target.WorkflowID, Author: by, Body: body, Repo: target.Repo,
		})
		workflowID = target.WorkflowID
		return err
	}); err != nil {
		fmt.Fprintf(stderr, "org directive %s: %v\n", kind, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s directive %d in workflow %s\n", orgDirectiveReceiptPastTense(kind), directiveID, workflowID)
	return 0
}

// orgDirectiveReceiptPastTense renders the confirmation verb. The previous
// "%sed" formatting would have produced "doneed" for the #1352 completion verb;
// the ack/cancel wording is preserved byte-for-byte.
func orgDirectiveReceiptPastTense(kind string) string {
	switch kind {
	case "done":
		return "completed"
	default:
		return kind + "ed"
	}
}

func orgDirectiveReceiptIDAndFlags(args []string) (string, []string, bool) {
	needsValue := map[string]bool{"--home": true, "--by": true}
	idIndex := -1
	for i := 0; i < len(args); i++ {
		if needsValue[args[i]] {
			i++
			if i >= len(args) {
				return "", nil, false
			}
			continue
		}
		if strings.HasPrefix(args[i], "--home=") || strings.HasPrefix(args[i], "--by=") {
			continue
		}
		if strings.HasPrefix(args[i], "-") || idIndex >= 0 {
			return "", nil, false
		}
		idIndex = i
	}
	if idIndex < 0 {
		return "", nil, false
	}
	return args[idIndex], append(append([]string{}, args[:idIndex]...), args[idIndex+1:]...), true
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
