package tui

import (
	"fmt"
	"strings"
)

// trainPhaseSegments are the four coarse stages shown in the phase bar.
var trainPhaseSegments = []string{"generate", "review", "optimize", "promote"}

// trainPhaseSegment maps a fine-grained status phase to a coarse segment index
// (0..3). Unknown phases map to 0.
func trainPhaseSegment(phase string) int {
	switch phase {
	case "request_confirmed", "workspace_ready", "items_ready",
		"generating_options", "generating_options_heartbeat_stale", "options_generated":
		return 0
	case "review_published", "feedback_synced":
		return 1
	case "training_package_created", "optimizer_running", "optimizer_heartbeat_stale",
		"optimizer_completed", "optimizer_completed_no_candidate", "candidate_created":
		return 2
	case "candidate_review_published", "candidate_promoted", "candidate_rejected":
		return 3
	default:
		return 0
	}
}

func (m TrainRunModel) View() string {
	if m.snap.SessionID == "" && m.loadedAt.IsZero() && m.loadErr == "" {
		return "Loading…"
	}
	var b strings.Builder

	// Header.
	header := m.snap.SessionID
	if m.snap.Template != "" {
		header += " · " + m.snap.Template
	}
	if m.snap.ReviewRepo != "" {
		header += " · " + m.snap.ReviewRepo
	}
	b.WriteString(headerStyle.Render(header))
	if !m.loadedAt.IsZero() {
		b.WriteString("  " + mutedStyle.Render("updated "+m.loadedAt.Format("15:04:05")))
	}
	b.WriteString("\n\n")

	b.WriteString(m.phaseBar())
	b.WriteString("\n\n")

	if m.loadErr != "" {
		b.WriteString(errorStyle.Render("refresh error: " + m.loadErr))
		b.WriteString("\n\n")
	}

	b.WriteString(m.body())

	if m.mode == trainModeReject {
		b.WriteString("\nreject reason: " + m.rejectInput.View() + "\n")
	}
	if m.actionErr != "" {
		b.WriteString("\n" + errorStyle.Render(m.actionErr) + "\n")
	}
	for _, line := range m.resultLines {
		b.WriteString(mutedStyle.Render(line) + "\n")
	}

	b.WriteString("\n")
	b.WriteString(mutedStyle.Render(m.footer()))
	return b.String()
}

// footer is the phase-aware key hint line.
func (m TrainRunModel) footer() string {
	if m.mode == trainModeReject {
		return "type reason  enter reject  esc cancel"
	}
	if m.actionBusy {
		return "working…  q quit"
	}
	action := ""
	switch m.snap.Phase {
	case "items_ready", "feedback_synced", "training_package_created":
		action = "enter generate  "
	case "options_generated":
		action = "enter publish review  "
	case "review_published":
		action = "enter sync feedback  "
	case "candidate_created":
		action = "enter publish candidate review  "
	case "candidate_review_published":
		action = "p promote  x reject  "
	}
	if m.snap.Terminal {
		action = "n start next iteration  "
	}
	return action + "r refresh  q quit"
}

func (m TrainRunModel) phaseBar() string {
	active := trainPhaseSegment(m.snap.Phase)
	parts := make([]string, len(trainPhaseSegments))
	for i, seg := range trainPhaseSegments {
		switch {
		case i == active:
			parts[i] = selectedRowStyle.Render(seg)
		case i < active:
			parts[i] = greenStyle.Render(seg)
		default:
			parts[i] = mutedStyle.Render(seg)
		}
	}
	line := strings.Join(parts, mutedStyle.Render(" → "))
	return line + "    " + mutedStyle.Render("phase: "+dash(m.snap.Phase))
}

func (m TrainRunModel) body() string {
	var b strings.Builder
	s := m.snap

	switch s.Phase {
	case "items_ready", "request_confirmed", "workspace_ready":
		b.WriteString(fmt.Sprintf("%d review items ready to generate options\n", s.ReviewItems))
	case "generating_options", "generating_options_heartbeat_stale":
		b.WriteString(fmt.Sprintf("generating options — %d running · %d done · %d failed", s.JobsRunning, s.JobsSucceeded, s.JobsFailed))
		if s.ETA != "" && s.ETA != "unknown" {
			b.WriteString("  (eta " + s.ETA + ")")
		}
		b.WriteByte('\n')
	case "options_generated":
		b.WriteString(fmt.Sprintf("%d options generated — ready to publish the review\n", s.GeneratedOptions))
	case "review_published":
		b.WriteString(m.issueBlock())
		b.WriteString(fmt.Sprintf("feedback so far: %d\n", s.FeedbackCount))
	case "optimizer_running", "optimizer_heartbeat_stale", "training_package_created":
		b.WriteString("optimizer running")
		if s.Elapsed != "" {
			b.WriteString(" · " + s.Elapsed)
		}
		b.WriteByte('\n')
	case "candidate_created":
		b.WriteString(fmt.Sprintf("candidate %s created — ready to publish the candidate review\n", dash(s.CandidateVersion)))
	case "candidate_review_published":
		b.WriteString(m.issueBlock())
		b.WriteString(fmt.Sprintf("candidate: %s\n", dash(s.CandidateVersion)))
	case "optimizer_completed_no_candidate":
		b.WriteString("optimizer produced no candidate")
		if s.NoCandidateReason != "" {
			b.WriteString(": " + s.NoCandidateReason)
		}
		b.WriteByte('\n')
	case "candidate_promoted":
		b.WriteString(greenStyle.Render(fmt.Sprintf("candidate %s promoted", dash(s.CandidateVersion))) + "\n")
	case "candidate_rejected":
		b.WriteString(fmt.Sprintf("candidate %s rejected\n", dash(s.CandidateVersion)))
	}

	if s.NextAction != "" {
		b.WriteString(mutedStyle.Render("next: " + s.NextAction))
		b.WriteByte('\n')
	}
	return b.String()
}

// issueBlock renders the GitHub issue URL prominently with the continue-from-
// GitHub hint.
func (m TrainRunModel) issueBlock() string {
	if strings.TrimSpace(m.snap.IssueURL) == "" {
		return ""
	}
	return selectedRowStyle.Render("review issue: "+m.snap.IssueURL) + "\n" +
		mutedStyle.Render("comment on the issue — the review watcher picks it up") + "\n"
}
