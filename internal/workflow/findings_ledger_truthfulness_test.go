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
				json.RawMessage(`{"id":"L1","severity":"P2","state":"answered","evidence_locator":"internal/workflow/merge_gate.go: collectImplementerAttribution matching agent/role switch (~lines 1436-1449)","title":"attribution switch","detail":"read the switch at this head","rationale":"read the agent/role switch at this head; the conditional write is present"}`),
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
	// BEHAVIOURAL, not a helper call (#1941 review f3): the reviewer noted the
	// previous version invoked db.IsStructuralFindingLocator directly, which pins
	// a helper rather than the path. The store REFUSES a STATIC discharge whose
	// locator is not path-shaped, so the row existing as STATIC/answered above
	// already proves structural validity; what remains to assert is that the
	// locator is the path and carries none of the prose.
	if l1.EvidenceLocator != "internal/workflow/merge_gate.go" {
		t.Fatalf("[L1] evidence locator = %q, want exactly the path the re-arm can resolve", l1.EvidenceLocator)
	}
	if strings.Contains(l1.EvidenceLocator, " ") {
		t.Fatalf("[L1] evidence locator %q carries prose; the store would refuse it", l1.EvidenceLocator)
	}
	// The rationale is the REVIEWER'S, verbatim - not the citation, not the
	// title, not the detail, and not a sentence this writer authored (#1941 f5).
	if l1.Rationale != "read the agent/role switch at this head; the conditional write is present" {
		t.Fatalf("[L1] rationale = %q, want the reviewer's own rationale verbatim", l1.Rationale)
	}
	if strings.Contains(l1.Rationale, "~lines 1436-1449") {
		t.Fatalf("[L1] the prose citation was promoted into the rationale: %q", l1.Rationale)
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
	// A BARE STRING CARRIES NO RATIONALE KEY, so it can never be STATIC under
	// #1941 f5: a discharge needs an explicit reviewer rationale and prose
	// cannot be promoted into one. It records fully, loudly and discharges
	// nothing, which is the honest floor for a finding whose only form is text.
	if obs.EvidenceKind != db.EvidenceQuoted {
		t.Fatalf("evidence kind = %q, want QUOTED: a bare string supplies no rationale, so it cannot discharge", obs.EvidenceKind)
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

// #1941 review f2 and f3, reproduced as the reviewer measured them on the
// production path. Each input is one it actually ran, and each arm pins a
// behaviour my first head got wrong in the PERMISSIVE direction: the parser
// rescued a severity and then recorded a row that said nothing, or invented a
// locator out of prose.
func TestAdvanceJobRefusesManufacturedBareStringFindings(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("e", 40)

	insertCompletedJob(t, store, db.Job{ID: "review-manufactured", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-man", PullRequest: 1942, HeadSHA: head,
		TaskID: "task-man", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P2", Summary: "manufactured shapes",
			Evidence: EvidenceStaticOnly,
			Findings: []json.RawMessage{
				// [M1] A severity token alone. Recorded as QUOTED with the token as
				// its whole title before this change.
				json.RawMessage(`"P2:"`),
				// [M2] A version is not a path. Invented File/EvidenceLocator "v1.2"
				// and recorded STATIC before this change.
				json.RawMessage(`"P2 v1.2 is a version, not a repository file"`),
				// [M3] An object carrying only id and severity: recorded with title
				// and detail both empty.
				json.RawMessage(`{"id":"M3","severity":"P2"}`),
				// [M4] LOWERCASE severity, which is the fifth mutant's discriminator:
				// with strings.ToUpper removed this records nothing at all.
				json.RawMessage(`"p2 internal/workflow/findings_ledger_writer.go:226 - lowercase severities are real reviewer output"`),
				// [M5] Colon-suffixed severity followed by a real path.
				json.RawMessage(`"P2: internal/db/review_findings.go:247 - the discharge bar refuses a prose locator"`),
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-manufactured"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1942)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	byLabel := map[string]db.ReviewFindingObservation{}
	for _, obs := range observations {
		byLabel[obs.RoundLabel] = obs
	}

	// M1 and M3 say nothing, so they must not be rows at all. M2 says something,
	// so it IS recorded - but with no invented locator.
	if len(observations) != 3 {
		var got []string
		for _, obs := range observations {
			got = append(got, obs.RoundLabel+"/"+string(obs.EvidenceKind)+"/"+obs.File)
		}
		t.Fatalf("ledger holds %d row(s) %v; want 3 - the two content-free findings must be skipped, not recorded", len(observations), got)
	}

	for _, obs := range observations {
		if strings.TrimSpace(obs.Title) == "" && strings.TrimSpace(obs.Detail) == "" {
			t.Fatalf("row %q was recorded with no title and no detail; that is the empty-row defect this lane closes", obs.RoundLabel)
		}
		if obs.File == "v1.2" || obs.EvidenceLocator == "v1.2" {
			t.Fatalf("row %q invented a locator from prose: file=%q locator=%q", obs.RoundLabel, obs.File, obs.EvidenceLocator)
		}
	}

	// M4 is the lowercase discriminator: it must be recorded, with its severity
	// normalised and its real path read.
	lower := byLabel[""]
	found := false
	for _, obs := range observations {
		if strings.Contains(obs.Title, "lowercase severities") {
			found = true
			lower = obs
		}
	}
	if !found {
		t.Fatal("the lowercase-severity finding was not recorded; strings.ToUpper is the only thing that reads it")
	}
	if lower.Severity != "P2" {
		t.Fatalf("lowercase severity recorded as %q, want normalised P2", lower.Severity)
	}
	if lower.File != "internal/workflow/findings_ledger_writer.go" {
		t.Fatalf("lowercase finding file = %q, want the real path", lower.File)
	}

	// The skips must be LOUD and the summary must count them.
	events, err := store.ListJobEvents(ctx, "review-manufactured")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	skips, summary := 0, ""
	for _, event := range events {
		switch event.Kind {
		case "findings_ledger_skipped":
			skips++
		case "findings_ledger_recorded":
			summary = event.Message
		}
	}
	if skips != 2 {
		t.Fatalf("skip events = %d, want 2: a finding that cannot be recorded must fail loudly", skips)
	}
	if !strings.Contains(summary, "recorded 3 of 5") || !strings.Contains(summary, "2 skipped") {
		t.Fatalf("summary = %q, want an honest 3 of 5 with 2 skipped", summary)
	}
}

// #1941 review f1, THE P1, reproduced through the production path. A
// path-shaped locator was promoted to File and STATIC, and the writer then
// MANUFACTURED the rationale the store demands for a discharge - so a
// continuation whose only content was a path I parsed myself persisted as
// answered/STATIC and cleared its obligation. That is inventing evidence, the
// permissive mirror of the defect this lane was opened to fix.
//
// A STATIC row now requires that the REVIEWER supplied something of its own.
func TestAdvanceJobRefusesToDischargeOnAManufacturedRationale(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("f", 40)

	insertCompletedJob(t, store, db.Job{ID: "review-manufactured-rationale", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-mr", PullRequest: 1944, HeadSHA: head,
		TaskID: "task-mr", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "content-free discharge attempt",
			Evidence: EvidenceStaticOnly,
			Findings: []json.RawMessage{
				// [N1] The reviewer's own construction: a declared answer whose only
				// content is a locator citing a path that does not exist. No title,
				// no detail, no rationale, nothing executed.
				json.RawMessage(`{"id":"N1","severity":"P1","state":"answered","evidence_locator":"does/not/exist.go"}`),
				// [N2] The same locator WITH reviewer prose. This one is a legitimate
				// static reading and must still be recorded as STATIC, so the guard
				// cannot be satisfied by refusing everything.
				json.RawMessage(`{"id":"N2","severity":"P1","state":"answered","evidence_locator":"does/not/exist.go","detail":"read the call site at this head and the guard is present","rationale":"read the call site at this head and the guard is present"}`),
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-manufactured-rationale"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1944)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	byLabel := map[string]db.ReviewFindingObservation{}
	for _, obs := range observations {
		byLabel[obs.RoundLabel] = obs
	}

	// N1 carries NOTHING of the reviewer's own - no title, no detail, no
	// rationale, nothing executed - so it is refused outright and never counted.
	// That is stronger than recording it as QUOTED: it cannot discharge AND it
	// cannot pad a "recorded n of n" line. What must never happen is the
	// original behaviour, a STATIC row whose rationale this writer invented.
	if n1, ok := byLabel["N1"]; ok {
		t.Fatalf("N1 was recorded (kind=%s state=%s rationale=%q); a finding whose only content is a path the WRITER parsed must not become a row",
			n1.EvidenceKind, n1.State, n1.Rationale)
	}

	// The positive control. Without it, refusing everything would pass.
	n2 := byLabel["N2"]
	if n2.EvidenceKind != db.EvidenceStatic {
		t.Fatalf("N2 evidence kind = %q, want STATIC: a reviewer that supplied prose and a locator did the work STATIC exists for", n2.EvidenceKind)
	}
	if n2.State != db.FindingAnswered {
		t.Fatalf("N2 state = %q, want answered", n2.State)
	}

	// And N1's reversal is audible rather than silent.
	events, err := store.ListJobEvents(ctx, "review-manufactured-rationale")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	skip, summary := "", ""
	for _, event := range events {
		switch event.Kind {
		case "findings_ledger_skipped":
			skip = event.Message
		case "findings_ledger_recorded":
			summary = event.Message
		}
	}
	if skip == "" {
		t.Fatal("N1 was refused with NO skip event; a silent drop is the other half of this lane's defect")
	}
	if !strings.Contains(summary, "recorded 1 of 2") || !strings.Contains(summary, "1 skipped") {
		t.Fatalf("summary = %q, want an honest 1 of 2 with 1 skipped", summary)
	}
}

// The control for the ENTRY check, and it exists because my own first
// remediation would have broken it: refusing every finding with no title and no
// detail newly rejected a legitimate one carrying a `file` and a `rationale`,
// which is a real static reading with its evidence in the rationale field. A
// guard that refuses valid input is the failure mode every bound in this
// campaign has had a version of.
func TestAdvanceJobStillRecordsAFindingWhoseContentIsItsRationale(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("a", 40)

	insertCompletedJob(t, store, db.Job{ID: "review-rationale-only", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-ro", PullRequest: 1945, HeadSHA: head,
		TaskID: "task-ro", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "rationale carries the reading",
			Evidence: EvidenceStaticOnly,
			Findings: []json.RawMessage{
				json.RawMessage(`{"id":"R1","severity":"P2","state":"answered","file":"internal/workflow/findings_ledger.go","line":198,"rationale":"read dischargedAtHead at this head: a same-head non-QUOTED row is treated as discharged"}`),
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-rationale-only"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1945)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	if len(observations) != 1 {
		t.Fatalf("ledger holds %d row(s); a finding whose content is its rationale must still be recorded", len(observations))
	}
	obs := observations[0]
	if obs.EvidenceKind != db.EvidenceStatic || obs.State != db.FindingAnswered {
		t.Fatalf("row = kind %s / state %s, want STATIC/answered", obs.EvidenceKind, obs.State)
	}
	if obs.EvidenceLocator != "internal/workflow/findings_ledger.go:198" {
		t.Fatalf("evidence locator = %q, want path:line built from the declared file", obs.EvidenceLocator)
	}
}

// THE REAL #1936 SHAPE, keyed as the actual verdict keyed it. Job
// local-review-gm-review-opus-18d2a757546655c2 emitted `"locator"`, not
// `"evidence_locator"`, so reading only the canonical key left the exact
// instance the issue was filed about unread - my first head fixed #1936
// against a schema no reviewer had sent. Copied from the stored payload.
func TestAdvanceJobReadsTheLocatorKeyTheRealVerdictUsed(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "gm-review-opus", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("9", 40)

	insertCompletedJob(t, store, db.Job{ID: "review-realkey", Agent: "gm-review-opus", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-rk", PullRequest: 1946, HeadSHA: head,
		TaskID: "task-rk", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "three continuations",
			Evidence: EvidenceStaticOnly,
			Findings: []json.RawMessage{
				json.RawMessage(`{"id":"K1","severity":"P2","state":"answered","locator":"internal/workflow/merge_gate.go: collectImplementerAttribution matching agent/role switch (~lines 1436-1449)","detail":"static read of the merge tree confirms the conditional-write fix survives","rationale":"static read of the merge tree at this head confirms the conditional-write fix survives"}`),
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-realkey"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1946)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	if len(observations) != 1 {
		t.Fatalf("ledger holds %d row(s), want 1", len(observations))
	}
	obs := observations[0]
	if obs.File != "internal/workflow/merge_gate.go" {
		t.Fatalf("file = %q; the `locator` key was not read, so the real #1936 shape is still dropped", obs.File)
	}
	if obs.EvidenceKind != db.EvidenceStatic || obs.State != db.FindingAnswered {
		t.Fatalf("row = kind %s / state %s, want STATIC/answered: the declared disposition must survive", obs.EvidenceKind, obs.State)
	}
	if obs.EvidenceLocator != "internal/workflow/merge_gate.go" {
		t.Fatalf("evidence locator = %q, want the bare path the re-arm can resolve", obs.EvidenceLocator)
	}
	if obs.Rationale != "static read of the merge tree at this head confirms the conditional-write fix survives" {
		t.Fatalf("rationale = %q, want the reviewer's own rationale verbatim", obs.Rationale)
	}
}

// #1941 f4, P1: THE SAME-HEAD DISCHARGE MUST NOT BYPASS LOCATOR RESOLUTION.
// dischargedAtHead ran BEFORE answeredIsMandatory and skipped any same-head
// non-QUOTED row, so a discharge recorded at the head being judged was accepted
// without its locator ever being resolved. The reviewer's probe continued a
// mandatory P1 citing does/not/exist.go, AdvanceJob persisted it STATIC/answered,
// EnsureLedgerObligationsObserved accepted it, and PathExistsAtHead ran ZERO
// times.
//
// The resolver's invocation count is OBSERVABLE here on purpose: "the obligation
// was refused" could be true for other reasons, and the specific claim is that
// the existence check now runs at the same head.
//
// KILLS: restoring the early same-head discharge (re-adding STATIC rows to
// dischargedAtHead, value consumed so the mutant compiles).
func TestSameHeadDischargeResolvesTheLocatorBeforeAccepting(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	head := strings.Repeat("7", 40)
	uid := "gitmoot/gitmoot#1947-f1"

	// A prior MANDATORY open finding, recorded at an earlier head.
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 1947, HeadSHA: strings.Repeat("6", 40),
		ObserverJob: "seed-review", SourceJob: "seed-review", Severity: "P1",
		State: db.FindingOpen, EvidenceKind: db.EvidenceQuoted,
		Title: "prior defect", Detail: "the guard is missing",
	}); err != nil {
		t.Fatalf("seeding the prior finding: %v", err)
	}
	seeded, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1947)
	if err != nil || len(seeded) != 1 {
		t.Fatalf("seed rows = %d err=%v", len(seeded), err)
	}
	priorUID := seeded[0].FindingUID
	_ = uid

	// A continuation AT THE HEAD BEING JUDGED, declaring an answer and citing a
	// path that does not exist, with an explicit rationale so it clears the
	// store's STATIC bar on its own terms.
	engine := testEngine(store)
	insertCompletedJob(t, store, db.Job{ID: "review-samehead", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-sh", PullRequest: 1947, HeadSHA: head,
		TaskID: "task-sh", ReviewRound: "review-2",
		Result: &AgentResult{
			Decision: "approved", Summary: "answering the prior finding",
			Evidence: EvidenceStaticOnly,
			Findings: []json.RawMessage{json.RawMessage(
				`{"id":"S1","severity":"P1","state":"answered","continues_uid":"` + priorUID +
					`","evidence_locator":"does/not/exist.go","rationale":"read does/not/exist.go at this head and the guard is present"}`)},
		},
	})
	if err := engine.AdvanceJob(ctx, "review-samehead"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	// The row must exist and be STATIC/answered: this test is about the READER,
	// so the writer's output has to be the shape the reviewer produced.
	rows, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1947)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	answered := false
	for _, obs := range rows {
		if obs.State == db.FindingAnswered && obs.EvidenceKind == db.EvidenceStatic {
			answered = true
		}
	}
	if !answered {
		t.Fatalf("no STATIC/answered row persisted at the judged head; rows=%d - the reader is not being exercised", len(rows))
	}

	// THE RESOLVER IS COUNTED. Zero invocations is the defect, regardless of the
	// outcome that follows it.
	calls := 0
	missing := LedgerScope{PathExistsAtHead: func(_ context.Context, _ string, _ string) (bool, error) {
		calls++
		return false, nil
	}}
	err = EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 1947, head, missing)
	if calls == 0 {
		t.Fatal("PathExistsAtHead was invoked ZERO times at the judged head: the same-head short-circuit is still bypassing locator resolution")
	}
	if err == nil {
		t.Fatal("an answer citing a nonexistent locator discharged its obligation at the head being judged")
	}

	// POSITIVE CONTROL, so the guard is not satisfied by refusing everything: the
	// same row with the path present legitimately answers at its own head.
	present := LedgerScope{PathExistsAtHead: func(context.Context, string, string) (bool, error) { return true, nil }}
	if err := EnsureLedgerObligationsObserved(ctx, store, "gitmoot/gitmoot", 1947, head, present); err != nil {
		t.Fatalf("a same-head answer whose cited path EXISTS was refused: %v", err)
	}
}

// #1941 f5's NEGATIVE CONTROL, and it is here because its absence let a mutant
// survive: re-synthesizing the rationale from title/detail/generated text broke
// nothing I had written, because every fixture that reached STATIC supplied a
// rationale of its own. A guard whose violation no test can observe is not
// guarded.
//
// The reviewer's own probe was exactly this shape: detail supplied, rationale
// absent, detail read back as the persisted rationale, and the store's STATIC
// bar satisfied by text the reviewer never wrote as a rationale.
//
// KILLS: restoring any promotion into Rationale - proseCitation, Title, Detail,
// or the generated "reported by a review that declared no executed checks".
func TestAdvanceJobNeverPromotesProseIntoTheRationale(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("8", 40)

	insertCompletedJob(t, store, db.Job{ID: "review-nopromote", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-np", PullRequest: 1948, HeadSHA: head,
		TaskID: "task-np", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "detail but no rationale",
			Evidence: EvidenceStaticOnly,
			Findings: []json.RawMessage{
				// [P1] A declared answer with a real file, a real detail and NO
				// rationale. The detail must not become the rationale, so the row
				// cannot be STATIC and cannot discharge.
				json.RawMessage(`{"id":"P1","severity":"P1","state":"answered","file":"internal/workflow/findings_ledger.go","line":198,"title":"same-head ordering","detail":"read dischargedAtHead at this head"}`),
				// [P2] The positive control: the identical finding WITH an explicit
				// rationale is STATIC and answered, so the guard cannot be satisfied
				// by refusing every static row.
				json.RawMessage(`{"id":"P2","severity":"P1","state":"answered","file":"internal/workflow/findings_ledger.go","line":198,"title":"same-head ordering","detail":"read dischargedAtHead at this head","rationale":"resolved the ordering at this head: STATIC rows now reach answeredIsMandatory"}`),
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-nopromote"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 1948)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	byLabel := map[string]db.ReviewFindingObservation{}
	for _, obs := range observations {
		byLabel[obs.RoundLabel] = obs
	}

	p1 := byLabel["P1"]
	if p1.EvidenceKind == db.EvidenceStatic {
		t.Fatalf("[P1] recorded STATIC with rationale %q; the reviewer supplied NO rationale, so this is prose promoted into a discharge claim", p1.Rationale)
	}
	if strings.TrimSpace(p1.Rationale) != "" {
		t.Fatalf("[P1] rationale = %q, want empty: the writer must not author the reviewer's explanation", p1.Rationale)
	}
	if p1.State != db.FindingOpen {
		t.Fatalf("[P1] state = %q, want open: a finding with no explicit rationale discharges nothing", p1.State)
	}
	// The detail itself must survive - refusing the discharge must not drop the
	// reviewer's words, which is the other half of the invariant.
	if p1.Detail != "read dischargedAtHead at this head" {
		t.Fatalf("[P1] detail = %q, want the reviewer's detail preserved verbatim", p1.Detail)
	}

	p2 := byLabel["P2"]
	if p2.EvidenceKind != db.EvidenceStatic || p2.State != db.FindingAnswered {
		t.Fatalf("[P2] kind %s / state %s, want STATIC/answered: an explicit rationale is exactly what STATIC requires", p2.EvidenceKind, p2.State)
	}
	if p2.Rationale != "resolved the ordering at this head: STATIC rows now reach answeredIsMandatory" {
		t.Fatalf("[P2] rationale = %q, want the reviewer's own sentence verbatim", p2.Rationale)
	}
}
