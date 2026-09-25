package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/reviewseverity"
)

const (
	postMergeFollowupsDoneEvent       = "post_merge_followups_done"
	postMergeFollowupErrorEvent       = "post_merge_followup_error"
	postMergeFollowupUnaddressedEvent = "post_merge_followup_unaddressed"
	postMergeIssueLabel               = "review-p2"
	postMergeUnassignedAgentLabel     = "agent:unassigned"
	postMergeIssueTitleMax            = 200
)

type postMergeFinding struct {
	uid      string
	severity string
	title    string
	detail   string
	location string
}

// handlePostMergeReview turns a review of an already-merged head (#2265 level
// 2) into follow-ups. The head has shipped, so no verdict here can block or
// merge anything: P0/P1 findings message the requesting role to fix or revert
// today, P2 findings each become a labelled issue, and P3 is left to the
// verdict itself. It never fails the review: a follow-up that cannot be filed
// is recorded as a job event, and the requester still receives the verdict.
func (e Engine) handlePostMergeReview(ctx context.Context, job db.Job, payload JobPayload) error {
	events, err := e.Store.ListJobEvents(ctx, job.ID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Kind == postMergeFollowupsDoneEvent {
			return nil
		}
	}
	var findings []json.RawMessage
	if payload.Result != nil {
		findings = payload.Result.Findings
	}
	urgent, followups := classifyPostMergeFindings(job.ID, findings)
	role := strings.TrimSpace(payload.ReviewRequester)
	if role == "" {
		role = strings.TrimSpace(payload.ActingOrgRole)
	}
	subject := fmt.Sprintf("%s#%d at %s", payload.Repo, payload.PullRequest, payload.HeadSHA)
	addressed := role != "" && strings.TrimSpace(payload.WorkflowID) != ""
	if !addressed && len(urgent)+len(followups) > 0 {
		e.addPostMergeEvent(ctx, job.ID, postMergeFollowupUnaddressedEvent,
			fmt.Sprintf("no role or workflow to message (role=%q workflow=%q); issues are still filed", role, payload.WorkflowID))
	}

	if len(urgent) > 0 && addressed {
		titles := make([]string, 0, len(urgent))
		for _, finding := range urgent {
			titles = append(titles, finding.severity+": "+finding.title)
		}
		body := fmt.Sprintf("Post-merge review of %s found %d P0/P1 finding(s): %s. Fix or revert today, and pause further deploys of this area until it is handled. Review job %s.",
			subject, len(urgent), strings.Join(titles, "; "), job.ID)
		if err := e.addressPostMergeRole(ctx, payload, role, body); err != nil {
			e.addPostMergeEvent(ctx, job.ID, postMergeFollowupErrorEvent, "P1 note: "+err.Error())
		}
	}

	var urls []string
	agentLabel := postMergeUnassignedAgentLabel
	if role != "" {
		agentLabel = "agent:" + role
	}
	for _, finding := range followups {
		if e.PostMergeFollowUpIssue == nil {
			e.addPostMergeEvent(ctx, job.ID, postMergeFollowupErrorEvent, "issue hook not wired")
			break
		}
		title := "[review-p2] " + finding.title
		if len(title) > postMergeIssueTitleMax {
			title = strings.ToValidUTF8(title[:postMergeIssueTitleMax], "")
		}
		body := fmt.Sprintf("Post-merge review found a P2 finding in %s.\n\n- Pull request: #%d\n- Merged head: `%s`\n- Review job: `%s`\n- Severity: %s\n- Location: %s\n\n%s\n\nFix within a few days.\n\nfinding-uid: %s\n",
			payload.Repo, payload.PullRequest, payload.HeadSHA, job.ID, finding.severity, finding.location, finding.detail, finding.uid)
		url, err := e.PostMergeFollowUpIssue(ctx, payload.Repo, title, body, []string{postMergeIssueLabel, agentLabel})
		if err != nil {
			e.addPostMergeEvent(ctx, job.ID, postMergeFollowupErrorEvent, "P2 issue "+finding.uid+": "+err.Error())
			continue
		}
		urls = append(urls, url)
	}
	if len(urls) > 0 && addressed {
		body := fmt.Sprintf("Post-merge review of %s filed %d P2 issue(s): %s. Fix within a few days. Review job %s.",
			subject, len(urls), strings.Join(urls, " "), job.ID)
		if err := e.addressPostMergeRole(ctx, payload, role, body); err != nil {
			e.addPostMergeEvent(ctx, job.ID, postMergeFollowupErrorEvent, "P2 note: "+err.Error())
		}
	}
	e.addPostMergeEvent(ctx, job.ID, postMergeFollowupsDoneEvent,
		fmt.Sprintf("p1=%d p2=%d issues=%s", len(urgent), len(followups), strings.Join(urls, ",")))
	return nil
}

func (e Engine) addressPostMergeRole(ctx context.Context, payload JobPayload, role, body string) error {
	_, err := e.Store.InsertWorkflowNote(ctx, db.WorkflowNote{
		WorkflowID:      payload.WorkflowID,
		Author:          "gitmoot",
		Body:            body,
		Repo:            payload.Repo,
		AddressedTarget: role,
	})
	return err
}

func (e Engine) addPostMergeEvent(ctx context.Context, jobID, kind, message string) {
	_ = e.Store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: kind, Message: message})
}

// classifyPostMergeFindings splits open findings into P0/P1 (fix or revert now)
// and P2 (file an issue). Answered and withdrawn findings, P3, and anything
// without a canonical severity are dropped: the verdict still carries them.
func classifyPostMergeFindings(jobID string, findings []json.RawMessage) (urgent, followups []postMergeFinding) {
	for index, raw := range findings {
		var wire reviewFindingWire
		if err := json.Unmarshal(raw, &wire); err != nil {
			var text string
			if json.Unmarshal(raw, &text) != nil {
				continue
			}
			wire = wireFromBareFindingText(text)
		}
		state := strings.ToLower(firstNonEmptyLedgerText(strings.TrimSpace(wire.State), strings.TrimSpace(wire.Disposition)))
		if state == string(db.FindingAnswered) || state == string(db.FindingWithdrawn) {
			continue
		}
		severity := strings.ToUpper(strings.TrimSpace(wire.Severity))
		rank, ok := reviewseverity.Rank(severity)
		if !ok || rank > 2 {
			continue
		}
		detail := firstNonEmptyLedgerText(strings.TrimSpace(wire.Detail), strings.TrimSpace(wire.Body),
			strings.TrimSpace(wire.Evidence), strings.TrimSpace(wire.Details), strings.TrimSpace(wire.Finding),
			strings.TrimSpace(wire.Message), strings.TrimSpace(wire.Description))
		title := firstNonEmptyLedgerText(strings.TrimSpace(wire.Title), strings.TrimSpace(wire.Summary))
		if title == "" {
			title = detail
			if len(title) > 120 {
				title = title[:120]
			}
		}
		location := strings.TrimSpace(wire.File)
		if location != "" && wire.Line > 0 {
			location = location + ":" + strconv.Itoa(wire.Line)
		}
		if location == "" {
			location = firstNonEmptyLedgerText(strings.TrimSpace(wire.Location), wire.locator(), "not stated")
		}
		finding := postMergeFinding{uid: fmt.Sprintf("%s/%d", jobID, index), severity: severity, title: title, detail: detail, location: location}
		if rank <= 1 {
			urgent = append(urgent, finding)
		} else {
			followups = append(followups, finding)
		}
	}
	return urgent, followups
}
