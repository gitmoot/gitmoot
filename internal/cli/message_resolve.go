package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

func runOrgEscalateResolve(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("message resolve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory")
	roleFlag := fs.String("role", "", "acting requester or addressed coordinator")
	answer := fs.String("answer", "", "decision or answer to record in the thread")
	answerID := fs.Int64("note", 0, "existing answer note in the same workflow")
	jsonOutput := fs.Bool("json", false, "JSON output")
	if messageHelpRequested(args) {
		printMessageActionUsage(stdout, "resolve")
		return 0
	}
	idText, flagArgs, ok := orgEscalateResolveIDAndFlags(args)
	if !ok {
		fmt.Fprintln(stderr, "message resolve requires exactly one escalation message ID")
		return 2
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintln(stderr, "invalid escalation message ID")
		return 2
	}
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *answerID < 0 || (*answerID != 0 && *answer != "") {
		fmt.Fprintln(stderr, "use either --answer TEXT or --note ID")
		return 2
	}
	if refuseFlagLikeText("message resolve", *answer, stderr) {
		return 2
	}
	err = withStoreAndPaths(*home, func(paths config.Paths, store *db.Store) error {
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
		note, err := store.ResolveMessageEscalation(ctx, id, role, strings.TrimSpace(*answer), *answerID)
		if err != nil {
			return err
		}
		if *jsonOutput {
			message, err := store.GetMessage(ctx, note.ID, role)
			if err != nil {
				return err
			}
			return writeJSON(stdout, message)
		}
		fmt.Fprintf(stdout, "resolved escalation message %d; receipt message %d; requester notification queued\n", id, note.ID)
		return nil
	})
	if err != nil {
		fmt.Fprintln(stderr, "message resolve:", err)
		return 1
	}
	return 0
}
