package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// TaskEventDismissedManual records `gitmoot pr dismiss` (#2696): the acting
// role and its reason, in the event's reason.
const TaskEventDismissedManual = "task_dismissed_manual"

type prDismissOutput struct {
	ID        string `json:"id"`
	Repo      string `json:"repo"`
	FromState string `json:"from_state"`
	State     string `json:"state"`
	By        string `json:"by"`
	Reason    string `json:"reason"`
}

// runPRDismiss retires a task that nothing can move forward (a pull request
// closed on purpose, abandoned work) by moving it to dismissed (#2696). Only
// the task's attributed role, that role's coordinator chain, the repo owner or
// the seat its alerts go to may do it; a live job or a final task refuses it.
func runPRDismiss(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pr dismiss", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "home directory to use instead of the current user's home")
	repoFlag := fs.String("repo", "", "repository for a bare pull request number")
	reason := fs.String("reason", "", "why the pr is retired (required; recorded on the task_dismissed_manual event)")
	roleFlag := fs.String("role", "", "acting organization role (defaults to GITMOOT_ORG_ROLE, then the calling seat's pane)")
	jsonOutput := fs.Bool("json", false, "print the dismissal as JSON")
	// Accept the pr before or after the flags.
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(positional) != 1 || strings.TrimSpace(positional[0]) == "" {
		fmt.Fprintln(stderr, "pr dismiss requires exactly one <pr>")
		return 2
	}
	text := strings.TrimSpace(*reason)
	if text == "" {
		fmt.Fprintln(stderr, "pr dismiss requires --reason TEXT")
		return 2
	}
	if refuseFlagLikeText("pr dismiss", text, stderr) {
		return 2
	}
	ctx := context.Background()
	role, err := resolveDispatchOrgRole(ctx, *home, *roleFlag)
	if err != nil {
		fmt.Fprintf(stderr, "pr dismiss: %v\n", err)
		return 1
	}
	if role == "" {
		fmt.Fprintln(stderr, "pr dismiss: set --role or GITMOOT_ORG_ROLE, or run from a registered seat's pane; the dismissal records who made it")
		return 2
	}
	cfg, orgOK := loadMergeGateOrgConfig(*home)
	orgOK = orgOK && cfg.Enabled()
	if orgOK {
		if _, known := cfg.Role(role); !known {
			fmt.Fprintf(stderr, "pr dismiss: unknown role %q\n", role)
			return 2
		}
	}
	var output prDismissOutput
	err = withStore(*home, func(store *db.Store) error {
		record, err := resolvePRRecord(ctx, store, positional[0], *repoFlag)
		if err != nil {
			return err
		}
		if record.ID == "" {
			return fmt.Errorf("%s has no pr record to dismiss", record.reference())
		}
		task, err := store.GetTask(ctx, record.ID)
		if err != nil {
			return err
		}
		if db.IsTerminalTaskWorktreeState(task.State) {
			return fmt.Errorf("pr %s is already %s", task.ID, task.State)
		}
		attributed, err := blockedTaskAttributedRole(ctx, store, task)
		if err != nil {
			return fmt.Errorf("resolve the owner of %s: %w", task.ID, err)
		}
		if !prDismissAllowed(ctx, store, cfg, orgOK, role, attributed, task.RepoFullName) {
			owner := firstNonEmpty(attributed, "no recorded role")
			return fmt.Errorf("role %q may not dismiss %s: only its owner (%s), that owner's coordinator chain or the repo owner may", role, task.ID, owner)
		}
		changed, current, err := store.TransitionTaskStateWithEventIfNoActiveJob(ctx, task.ID, []string{task.State},
			string(workflow.TaskDismissed), TaskEventDismissedManual, fmt.Sprintf("dismissed by %s: %s", role, text))
		if errors.Is(err, db.ErrTaskHasActiveJob) {
			return fmt.Errorf("pr %s has a live job (%w); cancel it or wait for it to finish", task.ID, err)
		}
		if err != nil {
			return err
		}
		if !changed {
			return fmt.Errorf("pr %s moved to %s meanwhile; not dismissed", task.ID, current)
		}
		output = prDismissOutput{ID: task.ID, Repo: task.RepoFullName, FromState: task.State,
			State: string(workflow.TaskDismissed), By: role, Reason: text}
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "pr dismiss: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := writeJSON(stdout, output); err != nil {
			fmt.Fprintf(stderr, "pr dismiss: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "pr %s dismissed (was %s) by %s: %s\n", output.ID, output.FromState, role, text)
	return 0
}

// prDismissAllowed reports whether role may dismiss a task attributed to
// attributed in repo: the attributed role itself, its coordinator chain, the
// repo owner, or the seat its alerts route to (the repo's coordinator when the
// attributed role is gone, #2685). Without an enabled org only the attributed
// role may.
func prDismissAllowed(ctx context.Context, store *db.Store, cfg config.OrgConfig, orgOK bool, role, attributed, repo string) bool {
	role = workflow.NormalizeActingOrgRole(role)
	if attributed != "" && strings.EqualFold(role, attributed) {
		return true
	}
	if !orgOK {
		return false
	}
	allowed := cfg.Ancestors(attributed)
	roster := loadOrgRoster(ctx, store, cfg)
	allowed = append(allowed, resolveRepoOrgOwner(roster, cfg, repo).Owner,
		resolveAttributedOrgOwner(roster, cfg, attributed, repo).Owner)
	for _, candidate := range allowed {
		if candidate != "" && strings.EqualFold(role, candidate) {
			return true
		}
	}
	return false
}
