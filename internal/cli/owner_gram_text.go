package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// OWNER GRAM TEXT (owner feedback 2026-10-08: the first live Gram was
// "unreadable" and cut off mid-sentence).
//
// The owner reads Grams on a phone and is not the engineer of the fleet. Every
// item therefore opens with a plain line naming who needs him, the pull request
// and why, then "What to do:" with the concrete choice, then a link he can tap.
// Known gate reasons get a plain template instead of their procedure text. The
// unedited original of every item follows below a separator and is NEVER
// shortened: when the whole Gram would exceed Herdr's text limit, the
// originals move into an attached file and the text says so.
const (
	// ownerGramMaxTextBytes is Herdr's MAX_TEXT_BYTES (persist/gram.rs). Herdr
	// refuses a longer text, so the drain must never send one.
	ownerGramMaxTextBytes = 8 * 1024
	ownerGramSeparator    = "————————"
)

// ownerAlert is one item of an owner Gram.
type ownerAlert struct {
	who      string // who needs the owner: the seat, "Gitmoot", or "A review"
	subject  string // "owner/repo#N" when known
	headline string // short plain reason, whole sentences
	why      string // further plain sentences, optional
	todo     string // the concrete choice or action
	link     string // pull request URL when known; no CLI fallback (phone)
	original string // the unedited text this item came from
}

// ownerAlertLookup is the store surface the owner Gram uses to name the pull
// request: the head named in a gate escalation, and the repository a note was
// written about.
type ownerAlertLookup interface {
	PullRequestForHead(ctx context.Context, head string) (repo string, number int, found bool, err error)
	GetWorkflowNote(ctx context.Context, id int64) (db.WorkflowNote, error)
}

var (
	ownerGramHeadPattern  = regexp.MustCompile(`for head ([0-9a-f]{40})`)
	ownerGramTaskPRNumber = regexp.MustCompile(`^review-pr-(\d+)-`)
)

func ownerAlertFor(ctx context.Context, lookup ownerAlertLookup, row db.WakeOutboxObligation, event events.Event) ownerAlert {
	alert := ownerAlert{who: "Gitmoot"}
	repo, number := strings.TrimSpace(event.Repo), 0
	switch row.SourceKind {
	case db.WakeOutboxSourceEscalation, db.WakeOutboxSourceBlocked:
		alert.original = event.Detail
		if job := strings.TrimSpace(event.JobID); job != "" {
			if match := ownerGramTaskPRNumber.FindStringSubmatch(job); match != nil {
				number, _ = strconv.Atoi(match[1])
			}
		}
	case db.WakeOutboxSourceAwaitedFact:
		var payload db.AwaitedFactWakePayload
		if json.Unmarshal([]byte(row.SourceID), &payload) == nil {
			alert.who = "A review"
			alert.original = event.Detail
			subject, _, _ := strings.Cut(payload.SubjectKey, "@")
			if r, n, ok := strings.Cut(subject, "#"); ok {
				repo = r
				number, _ = strconv.Atoi(n)
			}
		}
	case db.WakeOutboxSourceWorkflowNote:
		alert.original = event.Detail
		if id, err := strconv.ParseInt(row.SourceID, 10, 64); err == nil && lookup != nil {
			if note, err := lookup.GetWorkflowNote(ctx, id); err == nil && repo == "" {
				repo = strings.TrimSpace(note.Repo)
			}
		}
	default:
		alert.original = event.Detail
	}
	if row.MessageID != 0 {
		if sender := strings.TrimSpace(row.MessageSender); sender != "" && sender != db.MessageSystemSender {
			alert.who = sender
		}
		alert.original = row.MessageBody
	}
	alert.original = ownerGramPlainText(alert.original)
	if number == 0 && lookup != nil {
		if match := ownerGramHeadPattern.FindStringSubmatch(alert.original); match != nil {
			if r, n, found, err := lookup.PullRequestForHead(ctx, match[1]); err == nil && found {
				repo, number = r, n
			}
		}
	}
	if repo != "" && number > 0 {
		alert.subject = fmt.Sprintf("%s#%d", repo, number)
		alert.link = fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
	} else if repo != "" {
		alert.subject = repo
	}
	explainOwnerAlert(&alert, row, event)
	return alert
}

// explainOwnerAlert fills the plain headline, why and todo. Known reasons get
// a template; anything else is introduced by its own first sentence.
func explainOwnerAlert(alert *ownerAlert, row db.WakeOutboxObligation, event events.Event) {
	text := alert.original
	attribution := workflow.IsImplementerAttributionGap(text)
	noCI := strings.Contains(text, workflow.LowRiskAutoMergeLeaveOpenPrefix) && strings.Contains(text, "no external CI")
	leftOpen := strings.Contains(text, workflow.LowRiskAutoMergeLeaveOpenPrefix)
	mergeQueue := strings.Contains(text, "requires GitHub's merge queue")
	awaitingMerge := strings.Contains(text, "awaiting_human_merge")
	attributionWhy := fmt.Sprintf("Gitmoot can't confirm who wrote this change, so it won't merge it automatically. Fix: the seat that wrote it records its work (gitmoot job record --acting-role %s …).", alert.seatName())
	mergeTodo := "If you want this change, merge it on GitHub. If not, close it."
	switch {
	case leftOpen:
		alert.headline = "it's approved, but Gitmoot won't merge it by itself."
		if noCI {
			alert.why = "This repo has no automated checks (CI), so automatic merging is off for it."
		} else {
			alert.why = "Gitmoot's automatic merge left it for a person to merge."
		}
		if attribution {
			alert.why += " " + attributionWhy
		}
		alert.todo = mergeTodo
	case mergeQueue:
		alert.headline = "it's approved, but this repo only merges through GitHub's merge queue, which Gitmoot can't use."
		alert.todo = "Add it to the merge queue on GitHub."
	case attribution:
		alert.headline = "Gitmoot can't confirm who wrote this change, so it won't merge it automatically."
		alert.why = fmt.Sprintf("Fix: the seat that wrote it records its work (gitmoot job record --acting-role %s …).", alert.seatName())
		alert.todo = "Nothing is needed from you unless you want to merge it by hand on GitHub."
	case awaitingMerge:
		alert.headline = "it's approved and waiting for someone to merge it."
		alert.todo = "Merge it on GitHub, or close it if it's no longer wanted."
	case row.SourceKind == db.WakeOutboxSourceAwaitedFact:
		alert.headline = fmt.Sprintf("its review finished (%s).", strings.ReplaceAll(event.Status, "_", " "))
		alert.todo = "Open the link to see the result."
	default:
		alert.headline = ownerGramFirstSentence(text)
		if alert.who == "Gitmoot" || alert.who == "A review" {
			alert.todo = "Read the message below."
		} else {
			alert.todo = fmt.Sprintf("Read %s's message below and reply to %s.", alert.who, alert.who)
		}
	}
}

// seatName is the role named in a record-your-work hint: the seat when the
// alert came from one, otherwise a placeholder.
func (a ownerAlert) seatName() string {
	if a.who == "Gitmoot" || a.who == "A review" || a.who == "" {
		return "<role>"
	}
	return a.who
}

// renderOwnerGram composes the Gram. The readable summaries come first; the
// unedited originals follow a separator. When the whole text would exceed
// Herdr's limit the originals are attached as a file instead, and if even the
// summaries would, they are attached too: nothing is ever cut.
func renderOwnerGram(items []ownerAlert, more int) ownerGramMessage {
	summaries := ownerGramSummaries(items, more)
	originals := ownerGramOriginals(items)
	full := summaries
	if originals != "" {
		full += "\n\n" + ownerGramSeparator + "\n" + originals
	}
	if len(full) <= ownerGramMaxTextBytes {
		return ownerGramMessage{Text: full}
	}
	note := "\n\nThe unedited original messages are in the attached file."
	if len(summaries)+len(note) <= ownerGramMaxTextBytes {
		return ownerGramMessage{Text: summaries + note, Attachment: full}
	}
	noun := "things need"
	if len(items) == 1 {
		noun = "thing needs"
	}
	return ownerGramMessage{
		Text:       fmt.Sprintf("Gitmoot: %d %s you. The details are too long for one message, so they are in the attached file.", len(items), noun),
		Attachment: full,
	}
}

func ownerGramSummaries(items []ownerAlert, more int) string {
	var out strings.Builder
	if len(items) > 1 {
		fmt.Fprintf(&out, "Gitmoot: %d things need you.\n\n", len(items))
	}
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		var block strings.Builder
		block.WriteString(item.who + " needs you")
		if item.subject != "" {
			block.WriteString(": " + item.subject + " — ")
		} else {
			block.WriteString(": ")
		}
		block.WriteString(ownerGramCapitalize(item.headline, item.subject == ""))
		if item.why != "" {
			block.WriteString("\n" + item.why)
		}
		block.WriteString("\nWhat to do: " + item.todo)
		if item.link != "" {
			block.WriteString("\n" + item.link)
		}
		text := block.String()
		if _, dup := seen[text]; dup {
			continue
		}
		seen[text] = struct{}{}
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n\n") {
			out.WriteString("\n\n")
		}
		out.WriteString(text)
	}
	if more > 0 {
		fmt.Fprintf(&out, "\n\n%d more will follow in the next message.", more)
	}
	return strings.TrimSpace(out.String())
}

func ownerGramOriginals(items []ownerAlert) string {
	var out strings.Builder
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item.original == "" {
			continue
		}
		if _, dup := seen[item.original]; dup {
			continue
		}
		seen[item.original] = struct{}{}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		label := "Original message from " + item.who
		if item.subject != "" {
			label += " about " + item.subject
		}
		out.WriteString(label + ", unedited:\n" + item.original)
	}
	return out.String()
}

// ownerGramPlainText redacts secrets and drops control characters other than
// newlines and tabs. It never shortens the text.
func ownerGramPlainText(text string) string {
	text = workflow.RedactCommentText(strings.TrimSpace(text))
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return ' '
	}, text))
}

// ownerGramFirstSentence is the first whole sentence of text: up to the first
// ". ", "? ", "! " or newline, never cut inside a sentence.
func ownerGramFirstSentence(text string) string {
	text = strings.TrimSpace(text)
	if line, _, ok := strings.Cut(text, "\n"); ok {
		text = strings.TrimSpace(line)
	}
	for index := 0; index+1 < len(text); index++ {
		switch text[index] {
		case '.', '?', '!':
			if text[index+1] == ' ' {
				return text[:index+1]
			}
		}
	}
	return text
}

func ownerGramCapitalize(text string, capitalize bool) string {
	if !capitalize || text == "" {
		return text
	}
	runes := []rune(text)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
