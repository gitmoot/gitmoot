package cli

import "github.com/gitmoot/gitmoot/internal/db"

// CLI --json shapes for the campaign and note commands (#2602). The db structs
// keep their stored/dashboard json tags; these wrappers rename the user-facing
// keys by embedding the db struct and shadowing the old key with an
// always-empty omitempty field of the same JSON name.

type campaignSummaryJSON struct {
	Campaign string `json:"campaign"`
	RunCount int    `json:"run_count"`
	db.WorkflowSummary
	HiddenWorkflowID string `json:"workflow_id,omitempty"`
	HiddenJobCount   int    `json:"job_count,omitempty"`
}

func newCampaignSummaryJSON(s db.WorkflowSummary) campaignSummaryJSON {
	return campaignSummaryJSON{Campaign: s.WorkflowID, RunCount: s.JobCount, WorkflowSummary: s}
}

type campaignMetaJSON struct {
	Campaign string `json:"campaign"`
	db.WorkflowMeta
	HiddenWorkflowID string `json:"workflow_id,omitempty"`
}

type campaignNoteJSON struct {
	Campaign string `json:"campaign"`
	db.WorkflowNote
	HiddenWorkflowID string `json:"workflow_id,omitempty"`
}

func newCampaignNoteJSON(n db.WorkflowNote) campaignNoteJSON {
	return campaignNoteJSON{Campaign: n.WorkflowID, WorkflowNote: n}
}

func newCampaignNoteJSONPtr(n *db.WorkflowNote) *campaignNoteJSON {
	if n == nil {
		return nil
	}
	out := newCampaignNoteJSON(*n)
	return &out
}

type campaignCloseJSON struct {
	Campaign        string            `json:"campaign"`
	Status          db.WorkflowStatus `json:"status"`
	AlreadyTerminal bool              `json:"already_terminal,omitempty"`
	Note            *campaignNoteJSON `json:"note,omitempty"`
}
