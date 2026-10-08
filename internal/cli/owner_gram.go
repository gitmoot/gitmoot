package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// OWNER ALERTS GO TO HERDR GRAM (owner decision 2026-10-08).
//
// The owner role has no pane and no seat, so a wake addressed to it used to
// stay pending forever: escalations matched no event rule, and notes and facts
// were deferred as "recipient has no current seat". The daemon now delivers
// every due wake addressed to the root owner role as ONE plain-text Gram per
// drain. Seat readiness and event rules do not apply; an explicitly disabled
// rule (muted) still does.
//
// Durability: the rows are claimed (pending -> attempted, CAS) BEFORE the send,
// so a second drainer cannot send them again. The outcome is recorded per row
// with a receipt in owner_gram_receipts: accepted -> delivered with the gram
// id, refused -> failed (provably unsent; `gitmoot org wake retry` may resend),
// unknown -> delivery_unknown (never resent automatically). A crash between
// claim and record leaves the row attempted, and the aged-attempt sweep makes
// it delivery_unknown. A Gram Herdr accepted is not proof the owner read it.
const (
	// ownerGramMinInterval spaces owner Grams so a burst becomes one message.
	ownerGramMinInterval = 2 * time.Minute
	// ownerGramMaxItems bounds one Gram; the rest wait for the next one.
	ownerGramMaxItems = 10
	// ownerGramSendTimeout stays well inside replyWakeAttemptedUnknownAfter, so
	// the aged-attempt sweep never races a send that is still running.
	ownerGramSendTimeout   = 15 * time.Second
	ownerGramItemDetailMax = 240
	ownerGramSenderLabel   = "gitmoot"
)

// ownerGramResult is what one send attempt proved.
type ownerGramResult struct {
	Outcome string // db.OwnerGram{Accepted,Refused,Unknown}
	GramID  string
	Detail  string
}

// ownerGramSender sends one Gram to the owner. Production uses
// sendOwnerGramViaHerdr; tests inject a fake so no real Gram is ever sent.
type ownerGramSender func(ctx context.Context, text string) ownerGramResult

// ownerGramStore is the store surface the owner path needs beyond
// wakeOutboxStore. A store without it keeps the previous behavior.
type ownerGramStore interface {
	ClaimOwnerGramWakeOutbox(ctx context.Context, ids []int64, at time.Time, minInterval time.Duration) (bool, error)
	FinishOwnerGramWakeOutbox(ctx context.Context, ids []int64, outcome, gramID, detail string, at time.Time) error
	OwnerGramLastSendAt(ctx context.Context) (time.Time, error)
}

func isOwnerWake(row db.WakeOutboxObligation) bool {
	return strings.EqualFold(strings.TrimSpace(row.TargetRole), orgChartRootRole)
}

// deliverOwnerGram sends every due owner row (up to ownerGramMaxItems) as one
// Gram and reports whether it claimed rows. Rows inside their hold, explicitly
// muted rows and undecodable rows stay pending for the health pass to grade.
func deliverOwnerGram(ctx context.Context, store wakeOutboxStore, delivery replyWakeDelivery, rows []db.WakeOutboxObligation, now time.Time, hold time.Duration) (bool, error) {
	recorder, ok := store.(ownerGramStore)
	if !ok || delivery.ownerGram == nil || len(rows) == 0 {
		return false, nil
	}
	type dueRow struct {
		row   db.WakeOutboxObligation
		event events.Event
	}
	var due []dueRow
	for _, row := range rows {
		createdAt, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
		if err != nil || now.UTC().Before(createdAt.Add(wakeOutboxRowHold(row, hold))) {
			continue
		}
		event, err := wakeOutboxEvent([]db.WakeOutboxObligation{row}, now)
		if err != nil || wakeExplicitlyMuted(delivery.rules, event) {
			continue
		}
		due = append(due, dueRow{row: row, event: event})
	}
	if len(due) == 0 {
		return false, nil
	}
	// Cheap pre-check; the claim below re-checks the slot atomically.
	last, err := recorder.OwnerGramLastSendAt(ctx)
	if err != nil {
		return false, fmt.Errorf("read latest owner gram: %w", err)
	}
	if !last.IsZero() && now.UTC().Sub(last) < ownerGramMinInterval {
		return false, nil
	}
	batch := due[:min(len(due), ownerGramMaxItems)]
	ids := make([]int64, 0, len(batch))
	items := make([]ownerGramItem, 0, len(batch))
	for _, entry := range batch {
		ids = append(ids, entry.row.ID)
		items = append(items, ownerGramItemFor(entry.row, entry.event))
	}
	text := ownerGramText(items, len(due)-len(batch))
	// The claim reserves the owner Gram slot in the same transaction, so a
	// concurrent drainer with a disjoint batch cannot send inside the interval.
	claimed, err := recorder.ClaimOwnerGramWakeOutbox(ctx, ids, now, ownerGramMinInterval)
	if err != nil {
		return false, err
	}
	if !claimed {
		// Another drainer owns these rows or the slot; it records its outcome.
		return false, nil
	}
	sendStarted := time.Now()
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ownerGramSendTimeout)
	result := delivery.ownerGram(sendCtx, text)
	cancel()
	// The outcome is stamped when the send finished, so the spacing interval
	// runs from the real send (#2345 review). The wall clock covers every bit of
	// work since the drain read `now` (slot read, batch preparation, the claim
	// transaction); the drain-clock form keeps the send's own duration when a
	// caller's `now` runs ahead of the wall clock, as tests' does.
	finishedAt := now.Add(time.Since(sendStarted))
	if wall := time.Now().UTC(); wall.After(finishedAt) {
		finishedAt = wall
	}
	if result.Outcome == db.OwnerGramAccepted && strings.TrimSpace(result.GramID) == "" {
		result = ownerGramResult{Outcome: db.OwnerGramUnknown, Detail: "send reported success without a gram id"}
	}
	switch result.Outcome {
	case db.OwnerGramAccepted, db.OwnerGramRefused, db.OwnerGramUnknown:
	default:
		result = ownerGramResult{Outcome: db.OwnerGramUnknown, Detail: fmt.Sprintf("unrecognized send outcome %q: %s", result.Outcome, result.Detail)}
	}
	detail := result.Detail
	if result.Outcome == db.OwnerGramAccepted {
		detail = "owner gram " + result.GramID + " accepted by herdr (not proof of reading)"
	}
	if err := recorder.FinishOwnerGramWakeOutbox(context.WithoutCancel(ctx), ids, result.Outcome, result.GramID, detail, finishedAt); err != nil {
		return true, fmt.Errorf("record owner gram outcome for wakes %v: %w", ids, err)
	}
	return true, nil
}

type ownerGramItem struct {
	kind    string
	subject string
	why     string
	command string
}

func ownerGramItemFor(row db.WakeOutboxObligation, event events.Event) ownerGramItem {
	item := ownerGramItem{
		kind:    ownerGramKindLabel(row, event),
		subject: strings.TrimSpace(event.Repo),
		why:     strings.TrimSpace(event.Detail),
		command: fmt.Sprintf("gitmoot org wake show %d", row.ID),
	}
	switch row.SourceKind {
	case db.WakeOutboxSourceEscalation, db.WakeOutboxSourceBlocked:
		if job := strings.TrimSpace(event.JobID); job != "" {
			item.command = "gitmoot job show " + job
		}
	case db.WakeOutboxSourceWorkflowNote:
		item.command = "gitmoot workflow show-note " + row.SourceID
	case db.WakeOutboxSourceAwaitedFact:
		var payload db.AwaitedFactWakePayload
		if json.Unmarshal([]byte(row.SourceID), &payload) == nil && payload.SubjectKey != "" {
			item.subject = payload.SubjectKey
		}
	}
	// A job command names the work itself; otherwise the shared-inbox message is
	// the most complete view of a note or notice.
	if row.MessageID != 0 && !strings.HasPrefix(item.command, "gitmoot job show ") {
		item.command = fmt.Sprintf("gitmoot message show %d", row.MessageID)
		if item.why == "" || row.SourceKind == db.WakeOutboxSourceWorkflowNote {
			item.why = fmt.Sprintf("%s from %s: %s", row.MessageKind, row.MessageSender, row.MessageBody)
		}
	}
	item.why = truncateForWake(terminalSafeWorkflowText(workflow.RedactCommentText(item.why)), ownerGramItemDetailMax)
	return item
}

func ownerGramKindLabel(row db.WakeOutboxObligation, event events.Event) string {
	switch row.SourceKind {
	case db.WakeOutboxSourceEscalation:
		return "needs you"
	case db.WakeOutboxSourceBlocked:
		return "blocked"
	case db.WakeOutboxSourceAwaitedFact:
		return "review result"
	case db.WakeOutboxSourceWorkflowNote:
		if event.Type == events.EventOrgDirective {
			return "directive"
		}
		return "message"
	default:
		return "notice"
	}
}

// ownerGramText is the plain message the owner reads on the phone: what, where,
// why, and the command that shows the rest. Identical lines are listed once.
func ownerGramText(items []ownerGramItem, more int) string {
	var body strings.Builder
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		line := "- " + item.kind
		if item.subject != "" {
			line += " " + item.subject
		}
		if item.why != "" {
			line += ": " + item.why
		}
		line += "\n  " + item.command
		if _, dup := seen[line]; dup {
			continue
		}
		seen[line] = struct{}{}
		body.WriteString(line)
		body.WriteString("\n")
	}
	noun := "items need"
	if len(items) == 1 {
		noun = "item needs"
	}
	header := fmt.Sprintf("Gitmoot: %d %s you\n", len(items), noun)
	text := header + body.String()
	if more > 0 {
		text += fmt.Sprintf("%d more will follow in the next message.\n", more)
	}
	return strings.TrimSpace(text)
}

// herdrGramResponse is the JSON `herdr gram send` prints: a success on stdout
// (exit 0) or an error on stderr (exit 1).
type herdrGramResponse struct {
	Result *struct {
		Message *struct {
			ID string `json:"id"`
		} `json:"message"`
	} `json:"result"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// sendOwnerGramViaHerdr runs `herdr gram send --from gitmoot TEXT`.
//
// Exit 0 with a gram id: accepted. An error response from Herdr (exit 1, JSON
// error: the request was refused before anything was stored), an argument
// refusal (exit 2) or a binary that cannot start: refused. Anything else,
// including a timeout or exit 0 without a readable id: unknown.
func sendOwnerGramViaHerdr(ctx context.Context, text string) ownerGramResult {
	return classifyHerdrGramSend(runHerdrGramSend(ctx, text))
}

type herdrGramRun struct {
	started  bool
	startErr error
	timedOut bool
	exitCode int
	stdout   []byte
	stderr   []byte
}

func runHerdrGramSend(ctx context.Context, text string) herdrGramRun {
	cmd := exec.CommandContext(ctx, "herdr", "gram", "send", "--from", ownerGramSenderLabel, text)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return herdrGramRun{startErr: err}
	}
	err := cmd.Wait()
	run := herdrGramRun{started: true, stdout: stdout.Bytes(), stderr: stderr.Bytes(), timedOut: ctx.Err() != nil}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		run.exitCode = 0
	case errors.As(err, &exitErr):
		run.exitCode = exitErr.ExitCode()
	default:
		run.exitCode = -1
	}
	return run
}

func classifyHerdrGramSend(run herdrGramRun) ownerGramResult {
	if !run.started {
		return ownerGramResult{Outcome: db.OwnerGramRefused, Detail: fmt.Sprintf("herdr could not start: %v", run.startErr)}
	}
	if run.timedOut {
		return ownerGramResult{Outcome: db.OwnerGramUnknown, Detail: "herdr gram send timed out; it may have been sent"}
	}
	switch run.exitCode {
	case 0:
		var response herdrGramResponse
		if json.Unmarshal(bytes.TrimSpace(run.stdout), &response) == nil && response.Result != nil && response.Result.Message != nil &&
			strings.HasPrefix(response.Result.Message.ID, "gram-") {
			return ownerGramResult{Outcome: db.OwnerGramAccepted, GramID: response.Result.Message.ID}
		}
		return ownerGramResult{Outcome: db.OwnerGramUnknown, Detail: "herdr gram send exited 0 without a gram id: " + truncateForWake(string(run.stdout), 200)}
	case 1:
		var response herdrGramResponse
		if json.Unmarshal(bytes.TrimSpace(run.stderr), &response) == nil && response.Error != nil && response.Error.Code != "" {
			return ownerGramResult{Outcome: db.OwnerGramRefused, Detail: fmt.Sprintf("herdr %s: %s", response.Error.Code, response.Error.Message)}
		}
	case 2:
		return ownerGramResult{Outcome: db.OwnerGramRefused, Detail: "herdr refused the arguments: " + truncateForWake(string(run.stderr), 200)}
	}
	return ownerGramResult{Outcome: db.OwnerGramUnknown, Detail: fmt.Sprintf("herdr gram send exit %d: %s", run.exitCode, truncateForWake(string(run.stderr), 200))}
}
