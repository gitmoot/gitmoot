package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2062: the session display event proved a head had been ASSERTED and said
// nothing about whether it was accepted as evidence or held apart from it. The
// quarantine lived in Go comments and in one line of CLI stdout at record time -
// nowhere on the stored row.
//
// It cost three seats an hour on 2026-09-08: each read the payload plane, found
// no head, and reported the value missing while it sat on this plane. Two then
// proposed remedies for a defect that did not exist, one of which would have
// installed a real one by letting a caller-asserted head reach the field the
// merge gate reads as system-observed.

// The row must carry its own plane, readable without the source tree.
func TestSessionDisplayEventLabelsItsPlaneOnTheRow(t *testing.T) {
	event, ok := sessionJobDisplayEvent("job-1", strings.Repeat("a", 40))
	if !ok {
		t.Fatal("sessionJobDisplayEvent refused a valid head")
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(event.Message), &raw); err != nil {
		t.Fatalf("stored message is not an object: %v (%q)", err, event.Message)
	}
	if raw["plane"] != SessionJobDisplayPlane {
		t.Fatalf("stored plane = %q, want %q; a reader of the row cannot see a Go comment",
			raw["plane"], SessionJobDisplayPlane)
	}
	if raw["source"] != SessionJobDisplayHeadSource {
		t.Fatalf("stored source = %q, want %q; 'a head was asserted' and 'a head was verified' must not look the same",
			raw["source"], SessionJobDisplayHeadSource)
	}
}

// Rows written before the label existed are still display-plane rows. Thirteen
// exist on this host and every one is caller-asserted by construction, because
// this event has only ever been written from --head-sha. Rejecting them would
// delete the evidence the label was added to expose; leaving the fields blank
// would let a reader infer an unlabelled row might be evidence-grade.
func TestUnlabelledLegacyDisplayEventReadsAsCallerAsserted(t *testing.T) {
	legacy := db.JobEvent{
		Kind:    SessionJobDisplayEventKind,
		Message: `{"head_sha":"185ad0ce185ad0ce185ad0ce185ad0ce185ad0ce"}`,
	}
	display, ok := ParseSessionJobDisplayEvent(legacy)
	if !ok {
		t.Fatal("a legacy display row was rejected; that deletes the head it was recorded to preserve")
	}
	if display.Plane != SessionJobDisplayPlane || display.Source != SessionJobDisplayHeadSource {
		t.Fatalf("legacy row read as plane=%q source=%q, want display/caller_asserted", display.Plane, display.Source)
	}
	if display.HeadSHA != "185ad0ce185ad0ce185ad0ce185ad0ce185ad0ce" {
		t.Fatalf("legacy head = %q, want it preserved unchanged", display.HeadSHA)
	}
}

// THE CONTROL, and it is the property #1990 exists to hold: labelling the row
// must not become a route for the value into evidence. Driven through the real
// OpenExternalJob path, because the property is about what the STORE holds, not
// about what a struct can decode.
//
// My first draft of this arm unmarshalled the display message into a JobPayload
// and asserted the head was empty. It failed - JobPayload carries a `head_sha`
// tag, so of course a `{"head_sha":...}` object decodes into one. That measured
// a shape coincidence, not a promotion, and it is the same wrong-referent error
// this whole issue is about.
func TestLabellingDoesNotPromoteTheHeadIntoThePayload(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	head := strings.Repeat("c", 40)

	job, err := mailbox.OpenExternalJob(ctx, JobRequest{
		ID: "session-label", Action: "implement", Repo: "gitmoot/gitmoot", ActingOrgRole: "gm-integrity",
		PullRequest: 2061, HeadSHA: head, Sender: "session",
	})
	if err != nil {
		t.Fatalf("OpenExternalJob: %v", err)
	}

	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	if strings.TrimSpace(payload.HeadSHA) != "" {
		t.Fatalf("stored payload head = %q, want empty; a caller-asserted head must never reach the field the merge gate reads as system-observed (#1990)", payload.HeadSHA)
	}
	if payload.PullRequest != 2061 {
		t.Fatalf("stored pull_request = %d, want 2061 preserved; only the HEAD is quarantined", payload.PullRequest)
	}

	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	labelled := 0
	for _, event := range events {
		display, ok := ParseSessionJobDisplayEvent(event)
		if !ok {
			continue
		}
		labelled++
		if display.HeadSHA != head {
			t.Fatalf("display head = %q, want %q", display.HeadSHA, head)
		}
		if display.Plane != SessionJobDisplayPlane || display.Source != SessionJobDisplayHeadSource {
			t.Fatalf("display row unlabelled: plane=%q source=%q", display.Plane, display.Source)
		}
	}
	if labelled != 1 {
		t.Fatalf("labelled display events = %d, want exactly 1; the head must be recorded once, on the display plane", labelled)
	}
}
