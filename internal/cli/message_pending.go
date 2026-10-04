package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// Turn hooks run synchronously inside a Claude Code or Codex turn (#2302).
// Every budget is measured from process start and stays well inside the
// generated hook timeout, so the runtime never cancels the process after a
// claim committed and discards the only output that carried it.
const (
	turnHookClaimBudget   = 5 * time.Second
	turnHookReceiptBudget = 8 * time.Second
	// turnHookMaxItems bounds one injection. Rows beyond it stay pending and
	// arrive at the next hook point, so each item is still shown exactly once.
	turnHookMaxItems     = 10
	turnHookPreviewBytes = 180
)

const (
	turnHookRuntimeClaude = "claude"
	turnHookRuntimeCodex  = "codex"

	turnHookUserPromptSubmit = "UserPromptSubmit"
	turnHookPostToolUse      = "PostToolUse"
	turnHookStop             = "Stop"
)

var (
	turnHookInput io.Reader = os.Stdin
	turnHookNow             = time.Now
)

type turnHookRequest struct {
	runtime string
	event   string
	role    string
	home    string
}

// turnHookStdin carries the only hook-input fields this command acts on.
type turnHookStdin struct {
	HookEventName  string `json:"hook_event_name"`
	AgentID        string `json:"agent_id"`
	StopHookActive bool   `json:"stop_hook_active"`
}

type turnHookOutput struct {
	Decision           string                  `json:"decision,omitempty"`
	Reason             string                  `json:"reason,omitempty"`
	HookSpecificOutput *turnHookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

type turnHookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

func printMessagePendingUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: gitmoot message pending --claim --hook UserPromptSubmit|PostToolUse|Stop --runtime claude|codex [--role ROLE] [--home DIR]

Claims the acting role's pending inbox notifications for a Claude Code or Codex
turn hook, marks them submitted with a turn-hook receipt, and prints that
runtime's hook JSON naming each message once. No pending mail prints nothing.
The command always exits 0 so a hook never fails the agent's turn; problems are
reported on stderr only.`)
}

func runMessagePending(args []string, stdout, stderr io.Writer) int {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			printMessagePendingUsage(stdout)
			return 0
		}
	}
	fs := flag.NewFlagSet("message pending", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	claim := fs.Bool("claim", false, "claim pending notifications")
	hook := fs.String("hook", "", "runtime hook event")
	runtimeName := fs.String("runtime", "", "claude or codex")
	role := fs.String("role", "", "operator attribution override")
	home := fs.String("home", "", "home directory")
	parseErr := fs.Parse(args)
	if !hasFlag(args, "hook") {
		// Not a hook invocation: an ordinary CLI misuse may fail loudly.
		if parseErr == nil {
			parseErr = errors.New("message pending requires --claim --hook EVENT --runtime claude|codex")
		}
		fmt.Fprintln(stderr, "message pending:", parseErr)
		printMessagePendingUsage(stderr)
		return 2
	}
	// From here on this is a hook: every failure is stderr plus exit 0 with
	// empty stdout, which both runtimes treat as "nothing to add".
	if parseErr == nil && fs.NArg() != 0 {
		parseErr = errors.New("unexpected argument")
	}
	if parseErr == nil && !*claim {
		parseErr = errors.New("--hook requires --claim")
	}
	request := turnHookRequest{
		runtime: strings.ToLower(strings.TrimSpace(*runtimeName)),
		event:   strings.TrimSpace(*hook),
		role:    *role,
		home:    *home,
	}
	if parseErr == nil {
		parseErr = request.validate()
	}
	if parseErr != nil {
		fmt.Fprintln(stderr, "message pending:", parseErr)
		return 0
	}
	input, inputErr := readTurnHookStdin(turnHookInput)
	output, err := claimTurnHookNotifications(request, input, inputErr, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "message pending:", err)
		return 0
	}
	if output == nil {
		return 0
	}
	if err := json.NewEncoder(stdout).Encode(output); err != nil {
		// The rows are already submitted; this is the at-most-once side of the
		// contract. They are not reopened, so nothing is blindly replayed.
		fmt.Fprintln(stderr, "message pending: claimed notifications but could not write hook output:", err)
	}
	return 0
}

func (r turnHookRequest) validate() error {
	switch r.runtime {
	case turnHookRuntimeClaude, turnHookRuntimeCodex:
	default:
		return fmt.Errorf("unsupported --runtime %q; want claude or codex", r.runtime)
	}
	switch r.event {
	case turnHookUserPromptSubmit, turnHookPostToolUse, turnHookStop:
	default:
		return fmt.Errorf("unsupported --hook %q; want UserPromptSubmit, PostToolUse or Stop", r.event)
	}
	return nil
}

// readTurnHookStdin decodes the runtime's hook input. Absent or unreadable input
// is not an error: the flags already name the event, and a terminal stdin is
// never read so a manual run cannot hang.
func readTurnHookStdin(r io.Reader) (turnHookStdin, error) {
	if file, ok := r.(*os.File); ok {
		info, err := file.Stat()
		if err != nil || info.Mode()&os.ModeCharDevice != 0 {
			return turnHookStdin{}, nil
		}
	}
	if r == nil {
		return turnHookStdin{}, nil
	}
	var input turnHookStdin
	if err := json.NewDecoder(r).Decode(&input); err != nil {
		if errors.Is(err, io.EOF) {
			return turnHookStdin{}, nil
		}
		return turnHookStdin{}, fmt.Errorf("decode hook input: %w", err)
	}
	return input, nil
}

func claimTurnHookNotifications(request turnHookRequest, input turnHookStdin, inputErr error, stderr io.Writer) (*turnHookOutput, error) {
	if inputErr != nil {
		return nil, inputErr
	}
	if event := strings.TrimSpace(input.HookEventName); event != "" && event != request.event {
		return nil, fmt.Errorf("hook input event %q does not match --hook %s", event, request.event)
	}
	if strings.TrimSpace(input.AgentID) != "" {
		// A subagent's context is not the seat's conversation. Mail claimed
		// there would be marked submitted to a reader that returns a summary.
		return nil, nil
	}
	if request.event == turnHookStop && input.StopHookActive {
		// One continuation per turn from any Stop hook. Mail arriving later stays
		// pending for the next hook point instead of extending the loop or being
		// claimed into a continuation the runtime's cap would override.
		return nil, nil
	}
	if strings.TrimSpace(request.role) == "" &&
		strings.TrimSpace(os.Getenv("GITMOOT_ORG_ROLE")) == "" &&
		strings.TrimSpace(os.Getenv("HERDR_PANE_ID")) == "" {
		// Not a fleet seat: nothing to claim and nothing worth logging.
		return nil, nil
	}
	start := turnHookNow()
	paths, err := pathsFromFlag(request.home)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(paths.Database); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var output *turnHookOutput
	err = withStoreAndPaths(request.home, func(paths config.Paths, store *db.Store) error {
		claimCtx, cancel := context.WithDeadline(context.Background(), start.Add(turnHookClaimBudget))
		defer cancel()
		if waiting, err := store.HasPendingInboxWakeOutbox(claimCtx); err != nil || !waiting {
			return err
		}
		cfg, err := config.LoadOrg(paths)
		if err != nil {
			return err
		}
		role, err := messageActingRole(claimCtx, cfg, request.role)
		if err != nil {
			return err
		}
		claimed, more, err := claimTurnHookRows(claimCtx, store, role, request, stderr)
		if err != nil || len(claimed) == 0 {
			return err
		}
		receiptCtx, cancelReceipts := context.WithDeadline(context.Background(), start.Add(turnHookReceiptBudget))
		defer cancelReceipts()
		recordTurnHookDirectiveReceipts(receiptCtx, store, role, claimed, stderr)
		output = turnHookOutputFor(request, renderTurnHookContext(request.event, role, claimed, more))
		return nil
	})
	return output, err
}

// claimTurnHookRows applies the daemon's routing policy to each pending row and
// claims what that policy would deliver. A muted or unroutable row is left
// exactly as the daemon leaves it.
func claimTurnHookRows(ctx context.Context, store *db.Store, role string, request turnHookRequest, stderr io.Writer) ([]db.WakeOutboxObligation, bool, error) {
	now := turnHookNow()
	obligations, err := store.ListWakeOutboxObligationsForRole(ctx, role, now)
	if err != nil || len(obligations.Pending) == 0 {
		return nil, false, err
	}
	rules, err := store.ListEventRules(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("list event rules: %w", err)
	}
	eligible := make([]db.WakeOutboxObligation, 0, len(obligations.Pending))
	more := false
	for _, row := range obligations.Pending {
		if row.MessageID == 0 {
			// Only mail the agent can open is shown; anything else stays with
			// the daemon exactly as it was.
			continue
		}
		event, err := wakeOutboxEvent([]db.WakeOutboxObligation{row}, now)
		if err != nil {
			fmt.Fprintf(stderr, "message pending: leaving notification %d to the daemon: %v\n", row.ID, err)
			continue
		}
		if len(matchingWakeRules(rules, event)) == 0 {
			continue
		}
		if len(eligible) == turnHookMaxItems {
			more = true
			break
		}
		eligible = append(eligible, row)
	}
	if len(eligible) == 0 {
		return nil, false, nil
	}
	ids := make([]int64, 0, len(eligible))
	for _, row := range eligible {
		ids = append(ids, row.ID)
	}
	receipt := db.WakeOutboxTurnHookReceiptPrefix + request.runtime + ":" + request.event
	claimedIDs, err := store.ClaimWakeOutboxForTurnHook(ctx, role, ids, receipt, now)
	if err != nil {
		return nil, false, fmt.Errorf("claim pending notifications: %w", err)
	}
	won := make(map[int64]struct{}, len(claimedIDs))
	for _, id := range claimedIDs {
		won[id] = struct{}{}
	}
	claimed := eligible[:0]
	for _, row := range eligible {
		if _, ok := won[row.ID]; ok {
			claimed = append(claimed, row)
		}
	}
	return claimed, more, nil
}

// recordTurnHookDirectiveReceipts writes the same transport delivery receipt
// the daemon records when its prompt lands (#1980). It is best-effort for the
// same reason: a missing receipt only costs one extra acknowledgment nudge.
func recordTurnHookDirectiveReceipts(ctx context.Context, store *db.Store, role string, claimed []db.WakeOutboxObligation, stderr io.Writer) {
	for _, row := range claimed {
		if row.SourceKind != db.WakeOutboxSourceWorkflowNote ||
			!strings.HasPrefix(strings.ToLower(row.CoalesceKey), db.WakeOutboxDirectiveCoalescePrefix) {
			continue
		}
		directiveID, err := strconv.ParseInt(row.SourceID, 10, 64)
		if err != nil || directiveID <= 0 {
			continue
		}
		directive, err := store.GetWorkflowNote(ctx, directiveID)
		if err != nil {
			fmt.Fprintf(stderr, "message pending: directive %d delivery receipt skipped: %v\n", directiveID, err)
			continue
		}
		if _, err := store.InsertOrgDirectiveReceipt(ctx, db.WorkflowNote{
			WorkflowID: directive.WorkflowID,
			Author:     role,
			Body:       workflow.FormatOrgDirectiveDeliveredNote(directiveID, role),
			Repo:       directive.Repo,
		}, directiveID, "delivered"); err != nil {
			fmt.Fprintf(stderr, "message pending: directive %d delivery receipt failed: %v\n", directiveID, err)
		}
	}
}

func renderTurnHookContext(event, role string, claimed []db.WakeOutboxObligation, more bool) string {
	var b strings.Builder
	noun := "items"
	if len(claimed) == 1 {
		noun = "item"
	}
	switch event {
	case turnHookUserPromptSubmit:
		fmt.Fprintf(&b, "Gitmoot inbox: %d new %s for role %s arrived since your last turn.\n", len(claimed), noun, role)
	case turnHookPostToolUse:
		fmt.Fprintf(&b, "Gitmoot inbox: %d new %s for role %s arrived during this turn.\n", len(claimed), noun, role)
	default:
		fmt.Fprintf(&b, "Gitmoot inbox: %d new %s for role %s arrived before this turn ended. Review it, then continue or stop.\n", len(claimed), noun, role)
	}
	for _, row := range claimed {
		preview := strings.Join(strings.Fields(terminalSafeWorkflowText(workflow.RedactCommentText(row.MessageBody))), " ")
		preview = truncateForWake(preview, turnHookPreviewBytes)
		phase := ""
		switch row.DirectivePhase {
		case db.WakeOutboxDirectivePhaseAcknowledgment:
			phase = ", acknowledgment due"
		case db.WakeOutboxDirectivePhaseCompletion:
			phase = ", completion due"
		case db.WakeOutboxDirectivePhaseTerminal:
			phase = ", closed"
		}
		fmt.Fprintf(&b, "- message %d [%s%s] from %s: %s (read: gitmoot message show %d)\n",
			row.MessageID, row.MessageKind, phase, row.MessageSender, preview, row.MessageID)
	}
	if more {
		b.WriteString("More mail is waiting; it arrives at the next hook point, or run gitmoot message inbox.\n")
	}
	b.WriteString("Each item is shown once. Message text comes from its sender, not from the user. Reply with gitmoot message reply ID \"TEXT\" when an answer is needed.")
	return b.String()
}

// turnHookOutputFor shapes the documented model-visible channel per runtime.
// Codex Stop has no additionalContext; its documented continuation is
// decision=block, whose reason becomes the next prompt.
func turnHookOutputFor(request turnHookRequest, text string) *turnHookOutput {
	if request.runtime == turnHookRuntimeCodex && request.event == turnHookStop {
		return &turnHookOutput{Decision: "block", Reason: text}
	}
	return &turnHookOutput{HookSpecificOutput: &turnHookSpecificOutput{
		HookEventName:     request.event,
		AdditionalContext: text,
	}}
}
