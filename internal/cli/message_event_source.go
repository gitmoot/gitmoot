package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/events"
)

// The public event contract omits routing metadata. Persist it explicitly so
// replay preserves observer policy, PR filters and exact-head review evidence.
type messageEventSource struct {
	events.Event
	TargetRole     string   `json:"addressed_role,omitempty"`
	TargetRoles    []string `json:"addressed_roles,omitempty"`
	PullRequest    int      `json:"pull_request,omitempty"`
	ReviewDecision string   `json:"review_decision,omitempty"`
	HeadSHA        string   `json:"head_sha,omitempty"`
	ReviewPurpose  string   `json:"review_purpose,omitempty"`
}

func (s *eventRuleSink) persistMessageEvent(ctx context.Context, event events.Event, roles []string) error {
	source := messageEventSource{Event: event, TargetRole: event.WakeTargetRole, TargetRoles: event.WakeTargetRoles, PullRequest: event.PullRequest, ReviewDecision: event.ReviewDecision}
	identity := struct {
		JobID      string `json:"job_id"`
		Generation int64  `json:"generation"`
		Type       string `json:"event_type"`
		Cause      string `json:"cause,omitempty"`
		Status     string `json:"status,omitempty"`
		Timestamp  string `json:"ts,omitempty"`
	}{JobID: event.JobID, Type: string(event.Type), Cause: event.Cause, Status: event.Status, Timestamp: event.Timestamp}
	message := db.Message{Kind: "notification", Body: event.Detail}
	if event.Cause == events.EventCauseReviewVerdict {
		message.Kind = "review"
	}
	job, err := s.store.GetJob(ctx, event.JobID)
	if err == nil {
		identity.Generation = job.LifecycleGeneration
		identity.Timestamp = ""
		var payload struct {
			WorkflowID      string `json:"workflow_id"`
			Repo            string `json:"repo"`
			PullRequest     int    `json:"pull_request"`
			HeadSHA         string `json:"head_sha"`
			ReviewPurpose   string `json:"review_purpose"`
			PostMergeReview bool   `json:"post_merge_review"`
		}
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			return fmt.Errorf("decode notification source job: %w", err)
		}
		message.WorkflowID = payload.WorkflowID
		source.HeadSHA = payload.HeadSHA
		if message.Kind == "review" {
			source.ReviewPurpose = db.ReviewRequestPurpose(payload.ReviewPurpose, payload.PostMergeReview)
		}
		if source.Repo == "" {
			source.Repo = payload.Repo
		}
		if source.PullRequest == 0 {
			source.PullRequest = payload.PullRequest
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if message.Kind == "review" && source.HeadSHA != "" {
		recipients := roles[:0]
		for _, role := range roles {
			subscribed, err := s.store.HasReviewSubscription(ctx, role, source.Repo, source.PullRequest, source.HeadSHA, source.ReviewPurpose)
			if err != nil {
				return err
			}
			if !subscribed {
				recipients = append(recipients, role)
			}
		}
		roles = recipients
		if len(roles) == 0 {
			return nil
		}
	}
	key, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	source.Detail = "" // The immutable message body stores this once.
	payload, err := json.Marshal(source)
	if err != nil {
		return err
	}
	message.SourceID = string(key)
	message.SourcePayload = string(payload)
	return s.store.EnqueueMessageNotification(ctx, message, roles)
}

func decodeMessageEvent(entry db.WakeOutboxObligation) (events.Event, error) {
	var source messageEventSource
	if err := json.Unmarshal([]byte(entry.MessageSourcePayload), &source); err != nil {
		return events.Event{}, fmt.Errorf("decode inbox event for wake %d: %w", entry.ID, err)
	}
	source.Event.Detail = entry.MessageBody
	source.Event.WakeTargetRole = source.TargetRole
	source.Event.WakeTargetRoles = source.TargetRoles
	source.Event.PullRequest = source.PullRequest
	source.Event.ReviewDecision = source.ReviewDecision
	return source.Event, nil
}

func wakeRecipient(event events.Event) string {
	if event.WakeRecipientRole != "" {
		return event.WakeRecipientRole
	}
	return event.WakeTargetRole
}

func wakeRuleKinds(event events.Event) []string {
	if event.WakeKind == db.WakeOutboxKindEvent {
		return classifyEventRuleKinds(event)
	}
	return []string{event.WakeKind}
}

// Check every source against the current policy before claiming a coalesced
// batch. A muted first item must not strand a later authorized notice, and one
// permissive route must not carry another route's filtered-out messages.
func authorizedMessageEventBatch(rules []db.EventRule, batch []db.WakeOutboxObligation, first events.Event, now time.Time, hold time.Duration) ([]db.WakeOutboxObligation, events.Event, []db.EventRule, error) {
	var selected []db.EventRule
	var event events.Event
	kept := batch[:0]
	for i := range batch {
		candidate := first
		if i != 0 {
			var err error
			candidate, err = wakeOutboxEvent(batch[i:i+1], now)
			if err != nil {
				return nil, events.Event{}, nil, err
			}
		}
		matching := matchingWakeRules(rules, candidate)
		if len(matching) == 0 {
			continue
		}
		if len(selected) == 0 {
			if i != 0 {
				created, err := time.Parse(time.RFC3339Nano, batch[i].CreatedAt)
				if err != nil {
					return nil, events.Event{}, nil, err
				}
				if now.UTC().Before(created.Add(hold)) {
					return nil, events.Event{}, nil, nil
				}
			}
			selected = matching
			event = candidate
		}
		if matching[0].ID != selected[0].ID {
			continue
		}
		kept = append(kept, batch[i])
	}
	return kept, event, selected, nil
}
