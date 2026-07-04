package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

const (
	SourceKindJob          = "job"
	LabelDashboardReport   = "gitmoot-dashboard-report"
	LabelBug               = "bug"
	fingerprintMarkerStart = "<!-- gitmoot:dashboard-report fingerprint:"
	fingerprintMarkerEnd   = " -->"
	defaultRecentEventSize = 8
)

// Report is a redacted GitHub-ready issue draft produced from a local Gitmoot
// error source.
type Report struct {
	Title             string
	Body              string
	Labels            []string
	Fingerprint       string
	Source            SourceMetadata
	RedactionSummary  RedactionSummary
	SelectedErrorText string
}

type SourceMetadata struct {
	Kind        string
	ID          string
	Repo        string
	Agent       string
	Runtime     string
	Action      string
	State       string
	TaskID      string
	TaskTitle   string
	Branch      string
	HeadSHA     string
	PullRequest int
}

type RedactionSummary struct {
	Notes              []string
	RawOutputCount     int
	RawOutputsOmitted  bool
	RecentEventCount   int
	IncludedEventCount int
}

type JobOptions struct {
	RecentEventLimit int
}

// FingerprintMarker returns the stable markdown marker used for duplicate
// detection in GitHub issue bodies.
func FingerprintMarker(fingerprint string) string {
	return fingerprintMarkerStart + strings.TrimSpace(fingerprint) + fingerprintMarkerEnd
}

type jobReportData struct {
	job           db.Job
	payload       workflow.JobPayload
	agent         db.Agent
	agentLoaded   bool
	events        []db.JobEvent
	recentEvents  []db.JobEvent
	selectedError string
}

// BuildJobReport loads a job and returns a redacted, structured issue draft.
func BuildJobReport(ctx context.Context, store *db.Store, jobID string, opts JobOptions) (Report, error) {
	if store == nil {
		return Report{}, errors.New("store is required")
	}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return Report{}, errors.New("job id is required")
	}

	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Report{}, fmt.Errorf("job %q not found", jobID)
		}
		return Report{}, err
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		return Report{}, err
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		return Report{}, err
	}
	agent, agentLoaded, err := loadAgentBestEffort(ctx, store, job.Agent)
	if err != nil {
		return Report{}, err
	}

	limit := opts.RecentEventLimit
	if limit <= 0 {
		limit = defaultRecentEventSize
	}
	data := jobReportData{
		job:           job,
		payload:       payload,
		agent:         agent,
		agentLoaded:   agentLoaded,
		events:        events,
		recentEvents:  tailEvents(events, limit),
		selectedError: selectJobError(job, payload, events),
	}
	return renderJobReport(data), nil
}

func loadAgentBestEffort(ctx context.Context, store *db.Store, name string) (db.Agent, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return db.Agent{}, false, nil
	}
	agent, err := store.GetAgent(ctx, name)
	if err == nil {
		return agent, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return db.Agent{}, false, nil
	}
	return db.Agent{}, false, err
}

func renderJobReport(data jobReportData) Report {
	source := SourceMetadata{
		Kind:        SourceKindJob,
		ID:          data.job.ID,
		Repo:        data.payload.Repo,
		Agent:       data.job.Agent,
		Action:      data.job.Type,
		State:       data.job.State,
		TaskID:      data.payload.TaskID,
		TaskTitle:   data.payload.TaskTitle,
		Branch:      data.payload.Branch,
		HeadSHA:     data.payload.HeadSHA,
		PullRequest: data.payload.PullRequest,
	}
	if data.agentLoaded {
		source.Runtime = data.agent.Runtime
	}
	source = sanitizeSourceMetadata(source)

	fingerprint := jobFingerprint(data.job, data.payload, data.selectedError)
	redaction := RedactionSummary{
		RawOutputCount:     len(data.payload.RawOutputs),
		RawOutputsOmitted:  len(data.payload.RawOutputs) > 0,
		RecentEventCount:   len(data.events),
		IncludedEventCount: len(data.recentEvents),
		Notes: []string{
			"Applied Gitmoot comment redaction to all rendered fields.",
			"Neutralized /gitmoot command-looking lines in rendered text.",
		},
	}
	if redaction.RawOutputsOmitted {
		redaction.Notes = append(redaction.Notes, "Omitted raw runtime output from the report body.")
	}

	report := Report{
		Title:             reportTitle(data.job, data.payload),
		Labels:            []string{LabelDashboardReport, LabelBug},
		Fingerprint:       fingerprint,
		Source:            source,
		RedactionSummary:  redaction,
		SelectedErrorText: sanitizeField(data.selectedError),
	}
	report.Body = renderMarkdown(report, data)
	return report
}

func sanitizeSourceMetadata(source SourceMetadata) SourceMetadata {
	source.Kind = sanitizeField(source.Kind)
	source.ID = sanitizeField(source.ID)
	source.Repo = sanitizeField(source.Repo)
	source.Agent = sanitizeField(source.Agent)
	source.Runtime = sanitizeField(source.Runtime)
	source.Action = sanitizeField(source.Action)
	source.State = sanitizeField(source.State)
	source.TaskID = sanitizeField(source.TaskID)
	source.TaskTitle = sanitizeField(source.TaskTitle)
	source.Branch = sanitizeField(source.Branch)
	source.HeadSHA = sanitizeField(source.HeadSHA)
	return source
}

func reportTitle(job db.Job, payload workflow.JobPayload) string {
	parts := []string{"Gitmoot"}
	if state := strings.TrimSpace(job.State); state != "" {
		parts = append(parts, state)
	} else {
		parts = append(parts, "error")
	}
	parts = append(parts, "job")
	if action := strings.TrimSpace(job.Type); action != "" {
		parts = append(parts, action)
	}
	if agent := strings.TrimSpace(job.Agent); agent != "" {
		parts = append(parts, "for", agent)
	}
	if repo := strings.TrimSpace(payload.Repo); repo != "" {
		parts = append(parts, "in", repo)
	}
	return workflow.LimitCommentText(strings.Join(parts, " "))
}

func renderMarkdown(report Report, data jobReportData) string {
	var builder strings.Builder
	builder.WriteString(FingerprintMarker(report.Fingerprint))
	builder.WriteString("\n\n")

	builder.WriteString("## What happened\n\n")
	selectedErrorSummary := selectedErrorHeadline(report.SelectedErrorText)
	writeBullet(&builder, "Job", jobSummary(report.Source))
	writeBullet(&builder, "Selected error", selectedErrorSummary)

	builder.WriteString("\n## Quick context\n\n")
	writeContextTable(&builder, report, data)

	if data.payload.Result != nil {
		builder.WriteString("\n## Agent result\n\n")
		writeBullet(&builder, "Decision", data.payload.Result.Decision)
		writeBullet(&builder, "Summary", data.payload.Result.Summary)
		writeJSONList(&builder, "Findings", data.payload.Result.Findings)
		writeList(&builder, "Changes made", data.payload.Result.ChangesMade)
		writeList(&builder, "Tests run", data.payload.Result.TestsRun)
		writeList(&builder, "Needs", data.payload.Result.Needs)
		writeList(&builder, "Delegations", delegationAgentNames(data.payload.Result.Delegations))
	}

	writeDetails(&builder, "Selected error details", func(details *strings.Builder) {
		if strings.TrimSpace(report.SelectedErrorText) == "" || strings.TrimSpace(report.SelectedErrorText) == selectedErrorSummary {
			return
		}
		details.WriteString(sanitizeBlock(report.SelectedErrorText))
		details.WriteString("\n")
	})
	writeDetails(&builder, "Request instructions", func(details *strings.Builder) {
		if strings.TrimSpace(data.payload.Instructions) == "" {
			return
		}
		details.WriteString(sanitizeBlock(data.payload.Instructions))
		details.WriteString("\n")
	})
	writeDetails(&builder, "Job context", func(details *strings.Builder) {
		writeBullet(details, "Job ID", report.Source.ID)
		writeBullet(details, "Task", taskLabel(report.Source.TaskID, report.Source.TaskTitle))
		writeBullet(details, "Sender", data.payload.Sender)
		writeBullet(details, "Lead agent", data.payload.LeadAgent)
		writeBullet(details, "Review round", data.payload.ReviewRound)
		writeBullet(details, "Template", data.payload.TemplateID)
		if len(data.payload.Reviewers) > 0 {
			writeBullet(details, "Reviewers", strings.Join(data.payload.Reviewers, ", "))
		}
		if len(data.payload.Constraints) > 0 {
			writeList(details, "Constraints", data.payload.Constraints)
		}
	})
	writeDetails(&builder, "Recent events", func(details *strings.Builder) {
		writeRecentEvents(details, data)
	})
	writeDetails(&builder, "Redaction notes", func(details *strings.Builder) {
		writeRedactionNotes(details, report)
	})
	writeDetails(&builder, "Fingerprint", func(details *strings.Builder) {
		details.WriteString("`")
		details.WriteString(report.Fingerprint)
		details.WriteString("`\n")
	})

	return workflow.LimitCommentBody(builder.String())
}

func delegationAgentNames(delegations []workflow.Delegation) []string {
	names := make([]string, 0, len(delegations))
	for _, d := range delegations {
		name := strings.TrimSpace(d.Agent)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func jobSummary(source SourceMetadata) string {
	parts := compactStrings([]string{source.State, source.Action, source.Kind})
	summary := strings.Join(parts, " ")
	if source.Agent != "" {
		summary = strings.TrimSpace(summary + " for " + source.Agent)
	}
	if source.Repo != "" {
		summary = strings.TrimSpace(summary + " in " + source.Repo)
	}
	return summary
}

func selectedErrorHeadline(value string) string {
	lines := compactStrings(strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n"))
	if len(lines) == 0 {
		return ""
	}
	headline := lines[0]
	for _, line := range lines {
		if strings.HasPrefix(strings.ToUpper(line), "ERROR:") {
			headline = line
			break
		}
	}
	return limitRunes(headline, 280)
}

func writeContextTable(builder *strings.Builder, report Report, data jobReportData) {
	pullRequest := ""
	if report.Source.PullRequest > 0 {
		pullRequest = "#" + strconv.Itoa(report.Source.PullRequest)
	}
	rows := []struct {
		label string
		value string
	}{
		{"Repository", report.Source.Repo},
		{"Job ID", report.Source.ID},
		{"Agent", report.Source.Agent},
		{"Runtime", report.Source.Runtime},
		{"Action", report.Source.Action},
		{"State", report.Source.State},
		{"Pull request", pullRequest},
		{"Branch", report.Source.Branch},
		{"Task", taskLabel(report.Source.TaskID, report.Source.TaskTitle)},
		{"Head SHA", report.Source.HeadSHA},
		{"Template", data.payload.TemplateID},
	}

	builder.WriteString("| Field | Value |\n")
	builder.WriteString("| --- | --- |\n")
	for _, row := range rows {
		value := sanitizeTableCell(row.value)
		if value == "" {
			continue
		}
		builder.WriteString("| ")
		builder.WriteString(sanitizeTableCell(row.label))
		builder.WriteString(" | ")
		builder.WriteString(value)
		builder.WriteString(" |\n")
	}
}

func writeRecentEvents(builder *strings.Builder, data jobReportData) {
	if len(data.recentEvents) == 0 {
		builder.WriteString("- No job events recorded.\n")
	} else {
		for _, event := range data.recentEvents {
			builder.WriteString("- `")
			builder.WriteString(sanitizeInline(event.Kind))
			builder.WriteString("`: ")
			builder.WriteString(sanitizeField(event.Message))
			builder.WriteString("\n")
		}
	}
}

func writeRedactionNotes(builder *strings.Builder, report Report) {
	for _, note := range report.RedactionSummary.Notes {
		builder.WriteString("- ")
		builder.WriteString(sanitizeField(note))
		builder.WriteString("\n")
	}
	if report.RedactionSummary.RawOutputsOmitted {
		builder.WriteString("- Raw runtime outputs omitted: ")
		builder.WriteString(strconv.Itoa(report.RedactionSummary.RawOutputCount))
		builder.WriteString(" retained locally.\n")
	}
	if report.RedactionSummary.RecentEventCount > report.RedactionSummary.IncludedEventCount {
		builder.WriteString("- Recent events truncated to the latest ")
		builder.WriteString(strconv.Itoa(report.RedactionSummary.IncludedEventCount))
		builder.WriteString(" of ")
		builder.WriteString(strconv.Itoa(report.RedactionSummary.RecentEventCount))
		builder.WriteString(".\n")
	}
}

func writeDetails(builder *strings.Builder, summary string, writeContent func(*strings.Builder)) {
	var content strings.Builder
	writeContent(&content)
	body := strings.TrimSpace(content.String())
	if body == "" {
		return
	}
	builder.WriteString("\n<details>\n<summary>")
	builder.WriteString(sanitizeField(summary))
	builder.WriteString("</summary>\n\n")
	builder.WriteString(body)
	builder.WriteString("\n\n</details>\n")
}

func writeBullet(builder *strings.Builder, label string, value string) {
	value = sanitizeField(value)
	if value == "" {
		return
	}
	builder.WriteString("- **")
	builder.WriteString(label)
	builder.WriteString(":** ")
	builder.WriteString(value)
	builder.WriteString("\n")
}

func writeList(builder *strings.Builder, title string, values []string) {
	items := compactStrings(values)
	if len(items) == 0 {
		return
	}
	builder.WriteString("\n### ")
	builder.WriteString(title)
	builder.WriteString("\n\n")
	for _, item := range items {
		builder.WriteString("- ")
		builder.WriteString(sanitizeField(item))
		builder.WriteString("\n")
	}
}

func writeJSONList(builder *strings.Builder, title string, values []json.RawMessage) {
	if len(values) == 0 {
		return
	}
	builder.WriteString("\n### ")
	builder.WriteString(title)
	builder.WriteString("\n\n")
	for _, value := range values {
		text := sanitizeField(formatRawJSON(value))
		if text == "" {
			continue
		}
		builder.WriteString("- ")
		builder.WriteString(text)
		builder.WriteString("\n")
	}
}

func formatRawJSON(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		return compact.String()
	}
	return string(raw)
}

func taskLabel(id string, title string) string {
	id = strings.TrimSpace(id)
	title = strings.TrimSpace(title)
	switch {
	case id != "" && title != "":
		return id + " - " + title
	case id != "":
		return id
	default:
		return title
	}
}

func selectJobError(job db.Job, payload workflow.JobPayload, events []db.JobEvent) string {
	terminalIndex := latestTerminalJobEventIndex(events)
	if terminalIndex >= 0 {
		if message := latestPriorityDiagnosticMessage(events[terminalIndex+1:]); message != "" {
			return message
		}
	} else if payload.Result == nil || strings.TrimSpace(payload.Result.Summary) == "" {
		if message := latestPriorityDiagnosticMessage(events); message != "" {
			return message
		}
	}
	if payload.Result != nil && strings.TrimSpace(payload.Result.Summary) != "" {
		return payload.Result.Summary
	}
	if terminalIndex >= 0 && strings.TrimSpace(events[terminalIndex].Message) != "" {
		return events[terminalIndex].Message
	}
	if message := latestPriorityDiagnosticMessage(events); message != "" {
		return message
	}
	for i := len(events) - 1; i >= 0; i-- {
		if strings.TrimSpace(events[i].Message) == "" {
			continue
		}
		if isDiagnosticEvent(events[i].Kind) {
			return events[i].Message
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		if strings.TrimSpace(events[i].Message) != "" {
			return events[i].Message
		}
	}
	if strings.TrimSpace(job.State) != "" {
		return "job " + job.State
	}
	return "No error summary recorded."
}

func latestPriorityDiagnosticMessage(events []db.JobEvent) string {
	commentPostResolved := false
	advanceRetryResolved := false
	for i := len(events) - 1; i >= 0; i-- {
		kind := strings.TrimSpace(events[i].Kind)
		switch kind {
		case "comment_posted":
			commentPostResolved = true
		case "advance_completed", "advance_retried":
			advanceRetryResolved = true
		case "retry_queued":
			commentPostResolved = true
			advanceRetryResolved = true
		}
		if strings.TrimSpace(events[i].Message) == "" {
			continue
		}
		if kind == "comment_post_failed" && commentPostResolved {
			continue
		}
		if kind == "advance_retry" && advanceRetryResolved {
			continue
		}
		if isPriorityDiagnosticEvent(kind) {
			return events[i].Message
		}
	}
	return ""
}

func latestTerminalJobEventIndex(events []db.JobEvent) int {
	for i := len(events) - 1; i >= 0; i-- {
		if isTerminalJobEvent(events[i].Kind) {
			return i
		}
	}
	return -1
}

// isTerminalJobEvent reports whether an event kind names a settled job state.
// Event kinds mirror job-state strings for terminal transitions, so it uses
// SETTLED (barrier) semantics — blocked included — to locate the latest
// terminal event. See workflow.IsSettledJobState (#632).
func isTerminalJobEvent(kind string) bool {
	return workflow.IsSettledJobState(strings.TrimSpace(kind))
}

func isPriorityDiagnosticEvent(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "malformed_output", "repair_retry", "permission_blocked", "comment_post_failed",
		"advance_blocked", "advance_retry", "runtime_lock_wait", "cancel_settled":
		return true
	default:
		return false
	}
}

func isDiagnosticEvent(kind string) bool {
	switch strings.TrimSpace(kind) {
	case string(workflow.JobFailed), string(workflow.JobBlocked), string(workflow.JobCancelled),
		"malformed_output", "repair_retry", "permission_blocked", "comment_post_failed",
		"advance_blocked", "advance_retry", "runtime_lock_wait", "cancel_settled":
		return true
	default:
		return strings.Contains(kind, "fail") || strings.Contains(kind, "error") || strings.Contains(kind, "blocked")
	}
}

func jobFingerprint(job db.Job, payload workflow.JobPayload, selectedError string) string {
	values := []string{
		SourceKindJob,
		job.ID,
		job.State,
		job.Type,
		job.Agent,
		payload.Repo,
		payload.TaskID,
		strconv.Itoa(payload.PullRequest),
		resultDecision(payload),
		normalizeFingerprintText(selectedError),
	}
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:])[:16]
}

func resultDecision(payload workflow.JobPayload) string {
	if payload.Result == nil {
		return ""
	}
	return payload.Result.Decision
}

func normalizeFingerprintText(value string) string {
	value = workflow.RedactCommentText(value)
	value = strings.TrimSpace(value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 500 {
		value = string(runes[:500])
	}
	return value
}

func tailEvents(events []db.JobEvent, limit int) []db.JobEvent {
	if limit <= 0 || len(events) <= limit {
		return append([]db.JobEvent(nil), events...)
	}
	return append([]db.JobEvent(nil), events[len(events)-limit:]...)
}

func compactStrings(values []string) []string {
	output := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			output = append(output, value)
		}
	}
	return output
}

func sanitizeField(value string) string {
	return workflow.LimitCommentText(value)
}

func sanitizeBlock(value string) string {
	return workflow.LimitCommentText(value)
}

func sanitizeInline(value string) string {
	return strings.ReplaceAll(sanitizeField(value), "`", "'")
}

func sanitizeTableCell(value string) string {
	value = sanitizeField(value)
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\n", "<br>")
	return strings.TrimSpace(value)
}

func limitRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + " [truncated]"
}
