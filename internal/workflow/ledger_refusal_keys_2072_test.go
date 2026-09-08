package workflow

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2072: A REFUSAL THAT DOES NOT NAME THE ACCEPTED KEYS CANNOT TEACH ANYONE.
//
// Measured on gitmoot/gitmoot#2069-f1: a reviewer emitted a legible P3 under
// `description` and `location`, keys this reader does not take text from. The
// finding was refused, correctly, and lost - and the refusal said only "restate
// the concern with a title and a detail", so the producer learned nothing about
// which spellings would have worked.
//
// Four producers, four spellings, each previously repaired by teaching the
// reader one more. This asserts the other direction: the producer is told.

func refuseContentlessFinding(t *testing.T, raw json.RawMessage) []db.JobEvent {
	t.Helper()
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "rev", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	job := db.Job{ID: "rev-2072", Agent: "rev", Type: "review"}
	payload := JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "b", PullRequest: 2072,
		HeadSHA: strings.Repeat("e", 40), TaskID: "t", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Severity: "P3", Summary: "s",
			TestsRun: []string{"go test ./internal/workflow/"}, Evidence: "EXECUTED",
			Findings: []json.RawMessage{raw},
		},
	}
	insertCompletedJob(t, store, job, payload)
	if err := engine.RecordReviewFindingsToLedger(ctx, mustJob(t, store, job.ID), payload); err != nil {
		t.Fatalf("RecordReviewFindingsToLedger: %v", err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	return events
}

func refusalMessage(t *testing.T, events []db.JobEvent) string {
	t.Helper()
	for _, event := range events {
		if event.Kind == "findings_ledger_refused" {
			return event.Message
		}
	}
	t.Fatal("no findings_ledger_refused event: the contentless finding was not refused")
	return ""
}

// f1's exact shape, verbatim keys.
func TestContentRefusalNamesTheAcceptedKeys(t *testing.T) {
	events := refuseContentlessFinding(t, json.RawMessage(
		`{"severity":"P3","description":"state silently beats disposition","location":"internal/workflow/findings_ledger_writer.go"}`))
	message := refusalMessage(t, events)
	for _, key := range ledgerContentKeys {
		if !strings.Contains(message, key) {
			t.Fatalf("refusal does not name accepted key %q, so the producer cannot learn it: %s", key, message)
		}
	}
	// The rejected spelling must still be echoed, or the reviewer cannot tell
	// which of their findings was dropped.
	if !strings.Contains(message, "description") {
		t.Fatalf("refusal dropped the reviewer's own text: %s", message)
	}
}

// THE LIST MUST DESCRIBE THE PARSER, NOT A COMMENT ABOUT IT. Every name in
// ledgerContentKeys has to be a real json tag on reviewFindingWire, so the
// refusal cannot advertise a key the reader does not read.
func TestAdvertisedKeysExistOnTheWire(t *testing.T) {
	tags := map[string]bool{}
	wire := reflect.TypeOf(reviewFindingWire{})
	for i := range wire.NumField() {
		tag := wire.Field(i).Tag.Get("json")
		if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
			tags[name] = true
		}
	}
	for _, key := range ledgerContentKeys {
		if !tags[key] {
			t.Fatalf("refusal advertises %q, which is not a json tag on reviewFindingWire; "+
				"the message would send reviewers to a key the reader ignores", key)
		}
	}
}

// NEGATIVE CONTROL: a finding WITH content must not be refused at all, so the
// advertisement cannot be satisfied by refusing everything.
func TestFindingWithContentIsNotRefused(t *testing.T) {
	events := refuseContentlessFinding(t, json.RawMessage(
		`{"severity":"P3","title":"state beats disposition","detail":"no audit event is emitted"}`))
	for _, event := range events {
		if event.Kind == "findings_ledger_refused" {
			t.Fatalf("a finding carrying title and detail was refused: %s", event.Message)
		}
	}
}
