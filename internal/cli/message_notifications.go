package cli

import (
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func inboxWakeDetail(batch []db.WakeOutboxObligation, event *events.Event) {
	if event.Type == events.EventOrgDirective {
		return
	}
	var detail strings.Builder
	if event.Type == events.EventOrgFact && len(batch) > 1 {
		batch = batch[len(batch)-1:]
	}
	for _, entry := range batch {
		if entry.MessageID == 0 {
			continue
		}
		if detail.Len() == 0 {
			detail.WriteString("New inbox activity; gitmoot message inbox. ")
		}
		preview := truncateForWake(terminalSafeWorkflowText(workflow.RedactCommentText(entry.MessageBody)), 180)
		fmt.Fprintf(&detail, "message %d [%s] thread %d from %s; gitmoot message show %d: %s. ", entry.MessageID, entry.MessageKind, entry.MessageThreadID, entry.MessageSender, entry.MessageID, preview)
		if detail.Len() > 1400 {
			detail.WriteString("More in gitmoot message inbox.")
			break
		}
	}
	if detail.Len() != 0 {
		event.Detail = detail.String()
	}
}
