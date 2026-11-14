package cli

import (
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// CLI --json shapes for a run and its events (#2601). db.Job and db.JobEvent
// keep their stored/dashboard field names; these wrappers rename the
// user-facing keys by embedding the db struct and shadowing the old key with
// an always-empty omitempty field of the same Go (untagged) or JSON name.
// Every other key, payload and result content stays exactly as the db struct
// serializes it.

type runJSON struct {
	db.Job
	Reviewer    string
	ParentRunID string
	Campaign    string `json:"campaign,omitempty"`

	Agent       string `json:",omitempty"`
	ParentJobID string `json:",omitempty"`
	WorkflowID  string `json:"workflow_id,omitempty"`
}

func newRunJSON(job db.Job) runJSON {
	return runJSON{Job: job, Reviewer: job.Agent, ParentRunID: job.ParentJobID, Campaign: job.WorkflowID}
}

type runEventJSON struct {
	db.JobEvent
	RunID string

	JobID string `json:",omitempty"`
}

func newRunEventsJSON(events []db.JobEvent) []runEventJSON {
	out := make([]runEventJSON, len(events))
	for i, event := range events {
		out[i] = runEventJSON{JobEvent: event, RunID: event.JobID}
	}
	return out
}

// Cleanup --json shapes for `run cleanup list|inspect|apply`: owner_job_id
// becomes owner_run_id and a consumer's job_id becomes run_id; every other
// key is the stored/engine shape.

type cleanupObligationJSON struct {
	db.CleanupObligation
	OwnerRunID string `json:"owner_run_id"`

	OwnerJobID string `json:"owner_job_id,omitempty"`
}

func newCleanupObligationsJSON(obligations []db.CleanupObligation) []cleanupObligationJSON {
	out := make([]cleanupObligationJSON, len(obligations))
	for i, obligation := range obligations {
		out[i] = cleanupObligationJSON{CleanupObligation: obligation, OwnerRunID: obligation.OwnerJobID}
	}
	return out
}

type cleanupConsumerJSON struct {
	workflow.CleanupConsumer
	RunID string `json:"run_id"`

	JobID string `json:"job_id,omitempty"`
}

type cleanupInspectionJSON struct {
	workflow.CleanupInspection
	OwnerRunID string                 `json:"owner_run_id"`
	Consumers  []cleanupConsumerJSON  `json:"consumers"`
	Receipt    *cleanupReceiptJSON    `json:"receipt,omitempty"`
	Obligation *cleanupObligationJSON `json:"obligation,omitempty"`

	OwnerJobID string `json:"owner_job_id,omitempty"`
}

func newCleanupInspectionJSON(inspection workflow.CleanupInspection) cleanupInspectionJSON {
	var consumers []cleanupConsumerJSON
	if inspection.Consumers != nil {
		consumers = make([]cleanupConsumerJSON, len(inspection.Consumers))
	}
	for i, consumer := range inspection.Consumers {
		consumers[i] = cleanupConsumerJSON{CleanupConsumer: consumer, RunID: consumer.JobID}
	}
	out := cleanupInspectionJSON{
		CleanupInspection: inspection,
		OwnerRunID:        inspection.OwnerJobID,
		Consumers:         consumers,
		Receipt:           newCleanupReceiptJSON(inspection.Receipt),
	}
	if inspection.Obligation != nil {
		obligation := cleanupObligationJSON{CleanupObligation: *inspection.Obligation, OwnerRunID: inspection.Obligation.OwnerJobID}
		out.Obligation = &obligation
	}
	return out
}

type cleanupReceiptJSON struct {
	db.CleanupReceipt
	OwnerRunID string `json:"owner_run_id"`

	OwnerJobID string `json:"owner_job_id,omitempty"`
}

func newCleanupReceiptJSON(receipt *db.CleanupReceipt) *cleanupReceiptJSON {
	if receipt == nil {
		return nil
	}
	return &cleanupReceiptJSON{CleanupReceipt: *receipt, OwnerRunID: receipt.OwnerJobID}
}

type cleanupRefusalJSON struct {
	db.CleanupRefusal
	OwnerRunID string `json:"owner_run_id"`

	OwnerJobID string `json:"owner_job_id,omitempty"`
}

type cleanupApplyResultJSON struct {
	workflow.CleanupApplyResult
	Decision       cleanupInspectionJSON `json:"decision"`
	Receipt        *cleanupReceiptJSON   `json:"receipt,omitempty"`
	RefusalReceipt *cleanupRefusalJSON   `json:"refusal_receipt,omitempty"`
}

func newCleanupApplyResultJSON(result workflow.CleanupApplyResult) cleanupApplyResultJSON {
	out := cleanupApplyResultJSON{
		CleanupApplyResult: result,
		Decision:           newCleanupInspectionJSON(result.Decision),
		Receipt:            newCleanupReceiptJSON(result.Receipt),
	}
	if result.RefusalReceipt != nil {
		out.RefusalReceipt = &cleanupRefusalJSON{CleanupRefusal: *result.RefusalReceipt, OwnerRunID: result.RefusalReceipt.OwnerJobID}
	}
	return out
}
