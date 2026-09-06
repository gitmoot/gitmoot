package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1936, the FALSE-SUCCESS half. A finding that cites `evidence_locator` instead
// of `file` used to fall through to QUOTED, which forces `open` - so a declared
// `answered` became an open row while the summary event still read
// "recorded 4 of 4 ... (0 skipped)". Measured consequence: the merge gate
// refused a head whose reviewer had done the work and said so, and store-wide
// ZERO STATIC rows had ever reached answered against 13 EXECUTED.
//
// Driven through AdvanceJob and asserted on the PERSISTED rows, because a test
// that pins ledgerObservationFor is not a test of the path.
func TestAdvanceJobAcceptsAPathShapedLocatorAsStaticEvidence(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("c", 40)

	insertCompletedJob(t, store, db.Job{ID: "review-locator", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-locator", PullRequest: 1933, HeadSHA: head,
		TaskID: "task-locator", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "locator-only dispositions",
			// static_only on purpose: this is the reading reviewer STATIC exists for.
			Evidence: EvidenceStaticOnly,
			Findings: []json.RawMessage{
				// The #1936 shape, verbatim in structure: a declared answer whose
				// only locator is a prose-tailed path.
				json.RawMessage(`{"id":"L1","severity":"P2","state":"answered","evidence_locator":"internal/workflow/merge_gate.go: collectImplementerAttribution matching agent/role switch (~lines 1436-1449)","title":"attribution switch","detail":"read the switch at this head"}`),
				// A reviewer that DECLARES quoted evidence and also declares an
				// answer. QUOTED can never discharge, so the answer cannot stand -
				// and that reversal must be audible rather than silent. (A finding
				// with neither a locator nor execution takes a different path: the
				// writer skips it loudly, which the bare-string case below pins.)
				json.RawMessage(`{"id":"L2","severity":"P2","state":"answered","evidence_kind":"QUOTED","evidence_locator":"discussed in the review thread","title":"no citable path","detail":"nothing checkable"}`),
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-locator"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1933)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	byLabel := map[string]db.ReviewFindingObservation{}
	for _, obs := range observations {
		byLabel[obs.RoundLabel] = obs
	}
	if len(observations) != 2 {
		t.Fatalf("ledger holds %d row(s) for a verdict reporting 2 findings", len(observations))
	}

	// L1: a path-shaped locator makes the disposition dischargeable.
	l1 := byLabel["L1"]
	if l1.EvidenceKind != db.EvidenceStatic {
		t.Fatalf("[L1] evidence kind = %q, want STATIC: a path-shaped evidence_locator is checkable evidence", l1.EvidenceKind)
	}
	if l1.State != db.FindingAnswered {
		t.Fatalf("[L1] state = %q, want answered: the reviewer's declared disposition was reversed", l1.State)
	}
	if l1.File != "internal/workflow/merge_gate.go" {
		t.Fatalf("[L1] file = %q, want the leading path token from the locator", l1.File)
	}
	// The LOCATOR must be structurally checkable, because the store refuses a
	// STATIC discharge whose locator is not `path` or `path:line` and
	// answeredIsMandatory re-resolves it against the head. The reviewer's prose
	// citation is preserved as the RATIONALE instead of being lost or destroying
	// the row - both values came from the reviewer, and each lands where it is
	// usable.
	if !db.IsStructuralFindingLocator(l1.EvidenceLocator) {
		t.Fatalf("[L1] evidence locator = %q, want a structurally checkable path the re-arm can resolve", l1.EvidenceLocator)
	}
	if !strings.Contains(l1.Rationale, "collectImplementerAttribution") {
		t.Fatalf("[L1] rationale = %q, want the reviewer's prose citation preserved", l1.Rationale)
	}
	if l1.ExecutedCount != 0 {
		t.Fatalf("[L1] executed count = %d, want 0: nothing was executed and STATIC must never imply it", l1.ExecutedCount)
	}

	// L2: unciteable prose still cannot discharge - the guard must not widen.
	l2 := byLabel["L2"]
	if l2.EvidenceKind != db.EvidenceQuoted {
		t.Fatalf("[L2] evidence kind = %q, want QUOTED as declared", l2.EvidenceKind)
	}
	if l2.State != db.FindingOpen {
		t.Fatalf("[L2] state = %q, want open", l2.State)
	}

	// AND THE REVERSAL IS AUDIBLE. Without this the fix would only move the
	// silence, not remove it.
	events, err := store.ListJobEvents(ctx, "review-locator")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	var downgrade, summary string
	for _, event := range events {
		switch event.Kind {
		case "findings_ledger_downgraded":
			downgrade = event.Message
		case "findings_ledger_recorded":
			summary = event.Message
		}
	}
	if downgrade == "" {
		t.Fatal("no findings_ledger_downgraded event: a declared answer was reversed with no record of it")
	}
	if !strings.Contains(downgrade, "declared answered") || !strings.Contains(downgrade, "QUOTED") {
		t.Fatalf("downgrade event = %q, want it to name the declared state and the cause", downgrade)
	}
	if !strings.Contains(summary, "1 downgraded") {
		t.Fatalf("summary event = %q, want it to count the downgrade rather than report unqualified success", summary)
	}
}

// The live joltra shape. Review job local-review-joltra-sol-review-18d2b773bd0c534b
// returned six real findings and recorded ZERO: every finding arrived as one
// prose string with the severity inline, so the store refused each for an empty
// severity. The skip was loud, which is right; discarding values the reviewer
// actually sent is not.
func TestAdvanceJobReadsSeverityAndPathFromABareStringFinding(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "joltra-sol-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("d", 40)

	insertCompletedJob(t, store, db.Job{ID: "review-bare", Agent: "joltra-sol-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-bare", PullRequest: 1934, HeadSHA: head,
		TaskID: "task-bare", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P2", Summary: "prose findings",
			Evidence: EvidenceStaticOnly,
			Findings: []json.RawMessage{
				// Structurally the joltra finding: severity token, then a path with a
				// line, then prose.
				json.RawMessage(`"P2 apps/web/src/views/LandingView.vue:365 - an explicit element root is not clipped to the viewport, so offscreen videos keep decoding."`),
				// No severity token: the loud skip must survive, because a finding
				// the writer cannot faithfully record must never be counted.
				json.RawMessage(`"the hero column keeps four videos alive with no severity stated"`),
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-bare"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1934)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	if len(observations) != 1 {
		t.Fatalf("ledger holds %d row(s); want exactly the one finding whose severity was stated", len(observations))
	}
	obs := observations[0]
	if obs.Severity != "P2" {
		t.Fatalf("severity = %q, want P2 read from the leading token", obs.Severity)
	}
	if obs.File != "apps/web/src/views/LandingView.vue" {
		t.Fatalf("file = %q, want the path parsed from the prose", obs.File)
	}
	if obs.EvidenceKind != db.EvidenceStatic {
		t.Fatalf("evidence kind = %q, want STATIC now that a locator exists", obs.EvidenceKind)
	}
	if strings.HasPrefix(obs.Title, "P2 ") {
		t.Fatalf("title = %q, want the severity token consumed rather than left in the prose", obs.Title)
	}

	// The unparseable one stays a LOUD skip, and the summary must say so.
	events, err := store.ListJobEvents(ctx, "review-bare")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	var skip, summary string
	for _, event := range events {
		switch event.Kind {
		case "findings_ledger_skipped":
			skip = event.Message
		case "findings_ledger_recorded":
			summary = event.Message
		}
	}
	if skip == "" {
		t.Fatal("the severity-less finding was not recorded and left NO skip event; a silent drop is the defect this lane removes")
	}
	if !strings.Contains(summary, "recorded 1 of 2") || !strings.Contains(summary, "1 skipped") {
		t.Fatalf("summary event = %q, want an honest 1 of 2 with 1 skipped", summary)
	}
}
