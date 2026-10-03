package cli

import (
	"context"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func dashboardInboxThreads(ctx context.Context, store *db.Store, legacy []dashboardCommsThread, requestedNoteID int64) ([]dashboardCommsThread, error) {
	messages, err := store.ListDashboardMessages(ctx)
	if err != nil {
		return nil, err
	}
	indexed := make(map[int64]bool, len(messages))
	positions := make(map[int64]int)
	threads := make([]dashboardCommsThread, 0, len(legacy))
	for _, m := range messages {
		indexed[m.ID] = true
		position, exists := positions[m.ThreadID]
		if !exists {
			position = len(threads)
			positions[m.ThreadID] = position
			threads = append(threads, dashboardCommsThread{ThreadID: m.ThreadID, WorkflowID: m.WorkflowID, Repo: m.Repo})
		}
		thread := &threads[position]
		message := dashboardCommsMessage{ID: m.ID, Kind: m.Kind, From: m.Sender, To: m.Recipient, Body: workflow.RedactCommentText(m.Body), CreatedAt: m.CreatedAt, Lifecycle: m.Lifecycle, NotificationStatus: m.Status, NotificationReason: workflow.RedactCommentText(m.NotificationReason)}
		message.Repo, message.PullRequest, message.HeadSHA = m.Repo, m.PullRequest, m.HeadSHA
		message.ReviewPurpose, message.ReviewDecision = m.ReviewPurpose, m.ReviewDecision
		message.SourceJobID, message.SourceState = m.SourceJobID, m.SourceState
		if (m.Kind == "escalation" && m.Lifecycle == "open") || (m.Kind == "directive" && m.Lifecycle != "completed" && m.Lifecycle != "cancelled") {
			thread.Unresolved++
		}
		thread.Messages = append(thread.Messages, message)
		if m.CreatedAt > thread.UpdatedAt {
			thread.UpdatedAt = m.CreatedAt
		}
	}
	for i := range threads {
		threads[i] = dashboardCommsBoundThread(threads[i], requestedNoteID)
	}
	// Keep unaddressed historical journal markers, but never render a second
	// copy of an addressed message under its optional workflow association.
	for _, thread := range legacy {
		kept := thread.Messages[:0]
		thread.Unresolved = 0
		for _, message := range thread.Messages {
			if indexed[message.ID] {
				continue
			}
			kept = append(kept, message)
			if message.Kind == "escalation" && message.Resolution == nil {
				thread.Unresolved++
			}
		}
		if len(kept) != 0 {
			thread.Messages = kept
			threads = append(threads, thread)
		}
	}
	return threads, nil
}

func dashboardMessageIsOpen(message dashboardCommsMessage) bool {
	return (message.Kind == "escalation" && message.Resolution == nil && message.Lifecycle != "resolved") ||
		(message.Kind == "directive" && message.Lifecycle != "completed" && message.Lifecycle != "cancelled")
}
