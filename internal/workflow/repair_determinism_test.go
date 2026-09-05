package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// #1507 fixtures. truncatedReviewEnvelope is the MEASURED shape: a complete
// gitmoot_result whose enclosing envelope is never closed - brace deficit 1,
// bracket deficit 0, everything else well-formed. The instance that produced
// the issue was 3794 bytes of approved review discarded for one character.
const truncatedReviewEnvelope = `{"gitmoot_result":{"decision":"approved","summary":"one brace short","findings":[],"changes_made":[],"tests_run":["go test ./..."],"needs":[],"delegations":[]}`

const cleanReviewEnvelope = `{"gitmoot_result":{"decision":"approved","summary":"terminated cleanly","findings":[],"changes_made":[],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}`

func repairTestMailbox(t *testing.T) (*Mailbox, context.Context, runtime.Agent) {
	t.Helper()
	store := openTestStore(t)
	mailbox := &Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: "printf ok", RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
	return mailbox, context.Background(), agent
}

// repairTestStoredPayload returns the job's payload as STORED JSON.
//
// The marker is asserted against the serialized bytes rather than a typed field
// on purpose: it keeps every arm below compilable against a tree WITHOUT this
// fix, so the arms can be run as pre-fix discriminators and fail for
// BEHAVIOURAL reasons instead of failing to build (a build failure measures
// nothing about behaviour). The typed constants are exercised separately.
func repairTestStoredPayload(t *testing.T, mailbox *Mailbox, ctx context.Context, jobID string) string {
	t.Helper()
	stored, err := mailbox.store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	return stored.Payload
}

const repairMarkerJSON = `"gitmoot_result_repair":"delimiter-balance"`

const repairedEventKind = "result_repaired"

const repairIdenticalEventKind = "repair_retry_identical"

// ARM (i) - THE CONSTRUCTION ASSERTION. A first delivery that is delimiter-short
// is closed and accepted, so the repair loop is NEVER ENTERED: zero repair_retry
// events, one delivery, and the round-trip is not paid.
//
// The cross-attempt shape originally specified for this fix - attempt 1
// repairable, attempt 2 clean - is UNREACHABLE by construction once the close is
// accepted here, and it also occurs ZERO times in the measured record (0 of 63
// retained malformed-envelope deaths had a delimiter-repairable FIRST output).
// So this arm asserts the construction instead, and the clean-beats-repaired
// ranking is asserted where it does occur, in the candidate arm below.
func TestMailboxRunClosesDelimiterShortFirstOutputWithoutARepairAsk(t *testing.T) {
	mailbox, ctx, agent := repairTestMailbox(t)
	adapter := &fakeDelivery{outputs: []string{truncatedReviewEnvelope}}
	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	result, err := mailbox.Run(ctx, "job-1", agent, adapter)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Summary != "one brace short" {
		t.Fatalf("summary = %q, want the recovered verdict", result.Summary)
	}
	if len(adapter.prompts) != 1 {
		t.Fatalf("deliveries = %d, want exactly 1: a repaired first output must not be re-asked", len(adapter.prompts))
	}
	events, err := mailbox.store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if hasEvent(events, "repair_retry") || hasEvent(events, "malformed_output") {
		t.Fatalf("a closed envelope must not enter the repair loop: events = %+v", events)
	}
	if !hasEvent(events, repairedEventKind) {
		t.Fatalf("events = %+v, want a %s event", events, repairedEventKind)
	}
	stored := repairTestStoredPayload(t, mailbox, ctx, "job-1")
	if !strings.Contains(stored, repairMarkerJSON) {
		t.Fatalf("stored payload carries no repair marker: %s", stored)
	}
	// The marker rides the ENGINE's envelope record, never the agent's object:
	// the stored gitmoot_result must still be the agent's own bytes.
	if strings.Contains(stored, `"summary":"one brace short","gitmoot_result_repair"`) {
		t.Fatalf("marker leaked into the agent's object: %s", stored)
	}
}

// ARM (ii) - THE REAL-WORLD SHAPE, 18 of 18. Every recoverable output in the
// measured record sits at a REPAIR attempt (indices 1 and 2), never the first
// delivery. The issue's own item 1 reads "before invoking the repair LLM"; built
// that way it would have recovered NONE of the 18. So the close runs after every
// delivery, repair deliveries included.
func TestMailboxRunClosesDelimiterShortRepairOutput(t *testing.T) {
	mailbox, ctx, agent := repairTestMailbox(t)
	adapter := &fakeDelivery{outputs: []string{
		"findings posted, no json",
		truncatedReviewEnvelope,
	}}
	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	result, err := mailbox.Run(ctx, "job-1", agent, adapter)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Summary != "one brace short" {
		t.Fatalf("summary = %q, want the recovered verdict", result.Summary)
	}
	if len(adapter.prompts) != 2 {
		t.Fatalf("deliveries = %d, want 2 (first delivery plus one repair ask)", len(adapter.prompts))
	}
	events, err := mailbox.store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasEvent(events, "malformed_output") || !hasEvent(events, "repair_retry") {
		t.Fatalf("events = %+v, want the first delivery's malformed_output and one repair_retry", events)
	}
	if !hasEvent(events, repairedEventKind) {
		t.Fatalf("events = %+v, want a %s event", events, repairedEventKind)
	}
	if stored := repairTestStoredPayload(t, mailbox, ctx, "job-1"); !strings.Contains(stored, repairMarkerJSON) {
		t.Fatalf("stored payload carries no repair marker: %s", stored)
	}
}

// ARM (iii) - CLEAN BEATS REPAIRED, which is what makes the marker RANKED rather
// than labelled. One job in the measured record carried an attempt that already
// parsed clean and still died, so this ordering has an instance.
//
// Both directions are asserted: a clean parse alongside a truncated one wins and
// carries NO marker, and a clean parse at a later attempt CLEARS a marker an
// earlier attempt set.
func TestMailboxRunPrefersACleanParseOverADelimiterRepair(t *testing.T) {
	t.Run("within one delivery", func(t *testing.T) {
		mailbox, ctx, agent := repairTestMailbox(t)
		adapter := &fakeDelivery{outputs: []string{truncatedReviewEnvelope + "\n" + cleanReviewEnvelope}}
		if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
			t.Fatalf("Enqueue returned error: %v", err)
		}

		result, err := mailbox.Run(ctx, "job-1", agent, adapter)
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if result.Summary != "terminated cleanly" {
			t.Fatalf("summary = %q, want the CLEAN envelope to win", result.Summary)
		}
		if stored := repairTestStoredPayload(t, mailbox, ctx, "job-1"); strings.Contains(stored, "gitmoot_result_repair") {
			t.Fatalf("a clean parse must carry no marker: %s", stored)
		}
		events, err := mailbox.store.ListJobEvents(ctx, "job-1")
		if err != nil {
			t.Fatalf("ListJobEvents returned error: %v", err)
		}
		if hasEvent(events, repairedEventKind) {
			t.Fatalf("events = %+v, want no %s for a clean parse", events, repairedEventKind)
		}
	})

	t.Run("a later clean attempt clears an earlier marker", func(t *testing.T) {
		mailbox, ctx, agent := repairTestMailbox(t)
		// Attempt 1 is unparseable, attempt 2 is delimiter-short but its result
		// VIOLATES the review contract (no severity for changes_requested), so it
		// is refused and the loop continues; attempt 3 parses clean.
		adapter := &fakeDelivery{outputs: []string{
			"no json here",
			`{"gitmoot_result":{"decision":"changes_requested","summary":"missing severity","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}`,
			cleanReviewEnvelope,
		}}
		if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-2", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
			t.Fatalf("Enqueue returned error: %v", err)
		}

		result, err := mailbox.Run(ctx, "job-2", agent, adapter)
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if result.Summary != "terminated cleanly" {
			t.Fatalf("summary = %q, want the clean attempt's verdict", result.Summary)
		}
		if stored := repairTestStoredPayload(t, mailbox, ctx, "job-2"); strings.Contains(stored, "gitmoot_result_repair") {
			t.Fatalf("the accepted parse was clean, so no marker may survive: %s", stored)
		}
	})
}

// ARM (iv) - A RETRY THAT CANNOT DIFFER IS NOT A RETRY. Measured: 51 of 63
// retained malformed-envelope deaths contained a byte-identical consecutive
// re-ask. The loop stops with its remaining attempt UNSPENT and says why, so an
// operator can tell a deliberately-unspent budget from an exhausted one.
func TestMailboxRunStopsRepairingWhenTheReAskIsByteIdentical(t *testing.T) {
	mailbox, ctx, agent := repairTestMailbox(t)
	// The measured shape: the re-ask returns the SAME bytes the previous
	// delivery did, so the loop stops and the third output is never delivered.
	adapter := &fakeDelivery{outputs: []string{
		"no json here",
		"no json here",
		"this attempt must stay unspent",
	}}
	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-1", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}

	if _, err := mailbox.Run(ctx, "job-1", agent, adapter); err == nil {
		t.Fatal("Run returned nil error for output that never parses")
	}
	// Deliveries: the first plus repair attempt 1. Attempt 2 was NOT spent - the
	// loop's own budget is maxRepairAttempts=2, so a pre-#1507 run would deliver
	// three times.
	if len(adapter.prompts) != 2 {
		t.Fatalf("deliveries = %d, want 2: the second repair attempt must stay unspent", len(adapter.prompts))
	}
	events, err := mailbox.store.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasEvent(events, repairIdenticalEventKind) {
		t.Fatalf("events = %+v, want a %s event naming why the loop stopped", events, repairIdenticalEventKind)
	}
	for _, event := range events {
		if event.Kind == repairIdenticalEventKind && !strings.Contains(event.Message, "cannot differ") {
			t.Fatalf("%s message = %q, want it to name the reason", repairIdenticalEventKind, event.Message)
		}
	}
}

// TestExtractAgentResultNamesATruncation pins fix 3: the parse failure names the
// cause. "missing valid gitmoot_result JSON object" told an operator nothing
// about a 3.8 KB verdict that was one closing brace short.
func TestExtractAgentResultNamesATruncation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   string
	}{
		{"one unclosed object", truncatedReviewEnvelope, "unterminated object: 1 unclosed '{' at EOF"},
		{"unclosed array inside the envelope", `{"gitmoot_result":{"decision":"approved","summary":"s","findings":[`, "unterminated object: 2 unclosed '{' and 1 unclosed '[' at EOF"},
		{"ends inside a string", `{"gitmoot_result":{"summary":"cut off here`, "unterminated string at EOF"},
		{"balanced but envelope-less", `{"other":{"decision":"approved"}}`, "missing valid gitmoot_result JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExtractAgentResult(tc.output)
			if err == nil {
				t.Fatal("ExtractAgentResult returned nil error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}
