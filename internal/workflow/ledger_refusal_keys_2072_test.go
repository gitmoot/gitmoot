package workflow

import (
	"context"
	"encoding/json"
	"fmt"
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
	// #2078 f3: EVERY CLASSIFIED KEY, not only the advertised ones. This test
	// used to check the content keys alone, so a PHANTOM entry in the non-content
	// list was invisible - and there was one: "disposition", which is a real wire
	// field on the #2069 branch and does not exist here. A classification list
	// naming a field that does not exist gives false confidence in exactly the
	// direction this file is about: it looks like a decision was made about a
	// field, and no such field was ever there.
	//
	// The three directions now all hold: every tag is classified (below), every
	// classified key is a tag (here), and every advertised key rescues a finding.
	all := append(append(append([]string{}, ledgerContentKeys...),
		ledgerConditionalContentKeys...), ledgerNonContentKeys...)
	for _, key := range all {
		if !tags[key] {
			t.Fatalf("%q is classified but is not a json tag on reviewFindingWire; either the "+
				"refusal would send reviewers to a key the reader ignores, or the classification "+
				"records a decision about a field that does not exist", key)
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

// ledgerNonContentKeys are the wire's string fields that are NOT finding text:
// identity, classification, and locators. Named explicitly so the classification
// test below is a decision rather than a filter.
var ledgerNonContentKeys = []string{
	"id", "severity", "file", "continues_uid", "state", "evidence_kind", "evidence_locator", "locator", "location", "withdraw_reason", "line", "relevance_keys", "lens", "evidence",
}

// THE REVERSE DRIFT, which the advertised-keys test alone does not catch.
//
// TestAdvertisedKeysExistOnTheWire proves the message never names a key the
// reader ignores. It cannot prove the opposite: that a NEW content field gets
// advertised. A future `description` field read for prose but absent from
// ledgerContentKeys would reintroduce exactly #2072 - the reader takes a
// spelling the refusal never mentions.
//
// So every string field on the wire must be classified. Adding one fails here
// until somebody decides whether it carries finding text.
func TestEveryWireStringFieldIsClassified(t *testing.T) {
	classified := map[string]bool{}
	for _, key := range append(append(append([]string{}, ledgerContentKeys...),
		ledgerConditionalContentKeys...), ledgerNonContentKeys...) {
		classified[key] = true
	}
	wire := reflect.TypeOf(reviewFindingWire{})
	for i := range wire.NumField() {
		// #2078 review, P3: EVERY TAGGED FIELD, not only strings. The first
		// version skipped non-string kinds, so a future content-bearing field of
		// another type - a []string of notes, say - would have slipped the guard
		// silently, which is the drift this test exists to stop.
		field := wire.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		// #2078 f4: AN ABSENT TAG IS NOT AN EXCLUSION DECISION. `json:"-"` is one -
		// somebody wrote it. A MISSING tag is silence, and encoding/json does not
		// read silence as "ignore this": it matches an untagged exported field by
		// NAME, case-insensitively. So a content field added without a tag would be
		// decoded from reviewer payloads and skip this guard entirely.
		//
		// The two cases are now distinguished: `-` is honoured as the decision it
		// is, and an untagged exported field must be classified under the key
		// encoding/json would actually use.
		if name == "-" {
			continue
		}
		if name == "" {
			if !field.IsExported() {
				continue
			}
			name = strings.ToLower(field.Name)
		}
		if !classified[name] {
			t.Fatalf("reviewFindingWire field %q (json %q) is neither in ledgerContentKeys nor "+
				"ledgerNonContentKeys. If the reader takes finding text from it, the refusal must "+
				"advertise it or #2072 returns; if not, list it as non-content.", field.Name, name)
		}
	}
}

// THE ADVERTISEMENT MUST BE TRUE OF EACH KEY ALONE.
//
// This is the test that caught the bug in my own fix: the first version
// advertised `rationale`, and a rationale-only finding is REFUSED, because
// obs.Rationale is copied only on the STATIC arm (which requires a file). A
// refusal that names a key which does not work is worse than the silence it
// replaced - it sends the reviewer to a second failure.
func TestEveryAdvertisedKeyAloneRescuesAFinding(t *testing.T) {
	for _, key := range ledgerContentKeys {
		t.Run(key, func(t *testing.T) {
			raw := json.RawMessage(fmt.Sprintf(`{"severity":"P3","%s":"the boundary check is inverted"}`, key))
			for _, event := range refuseContentlessFinding(t, raw) {
				if event.Kind == "findings_ledger_refused" {
					t.Fatalf("the refusal advertises %q, but a finding carrying only %q was refused: %s", key, key, event.Message)
				}
			}
		})
	}
}

// THE CONDITIONAL KEY, PINNED IN BOTH DIRECTIONS. `rationale` is advertised only
// alongside a file, because that is exactly what the writer requires: it is
// copied on the STATIC arm, which needs a locator. Both arms matter - the first
// is why the qualification exists, the second is why the key is still offered
// instead of hidden.
func TestRationaleRescuesOnlyWithALocator(t *testing.T) {
	alone := refuseContentlessFinding(t, json.RawMessage(`{"severity":"P3","rationale":"the boundary check is inverted"}`))
	if refusalPresent(alone) != true {
		t.Fatal("a rationale-only finding was recorded; the qualification in the refusal is now wrong")
	}
	withFile := refuseContentlessFinding(t, json.RawMessage(`{"severity":"P3","file":"internal/x.go","rationale":"the boundary check is inverted"}`))
	if refusalPresent(withFile) {
		t.Fatal("a rationale WITH a file was refused; the refusal advertises a combination that does not work")
	}
}

func refusalPresent(events []db.JobEvent) bool {
	for _, event := range events {
		if event.Kind == "findings_ledger_refused" {
			return true
		}
	}
	return false
}
