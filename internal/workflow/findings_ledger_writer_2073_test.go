package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2073. A shape census over every finding element in this store found reviewer
// prose riding under three keys the writer never read: `details`, `message` and
// `finding`. A row lands articulate in the job payload and empty in the ledger,
// and the merge gate then names an obligation nobody can read.
//
// The keys are READ, never invented: each is measured from real verdicts. What
// this test pins is the consumer-observable claim - the prose reaches the
// ledger - rather than which struct field it passed through.
func TestAdvanceJobKeepsProseFromTheThreeUnreadKeys(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("c", 40)

	const (
		detailsProse     = "ListJobsByType selects jobColumns, so externally_driven is carried; a narrower list would take the weaker reason."
		messageProse     = "The reaper ages a claim from the claim row's own created_at, so a resumed value does not reset the wait."
		findingProse     = "An unresolved hold is keyed by turn_id plus content_hash, so changing content on the same turn bypasses it."
		descriptionProse = "Doc still states the reviewer is checked as the fallback lead, which is the exact behaviour this change removes."
	)

	insertCompletedJob(t, store, db.Job{ID: "review-2073", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-2073", PullRequest: 2073, HeadSHA: head,
		TaskID: "task-2073", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "three unread prose keys",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: []json.RawMessage{
				// {details, file, line, severity, summary}: title survives via
				// `summary`, detail was lost.
				json.RawMessage(`{"severity":"P2","file":"internal/db/store_jobs.go","line":449,"summary":"column list carries externally_driven","details":"` + detailsProse + `"}`),
				// {location, message, severity}: title and detail both lost.
				json.RawMessage(`{"severity":"P2","location":"internal/pipeline/run.go:266","message":"` + messageProse + `"}`),
				// {file, finding, line}: title, detail and severity all lost.
				json.RawMessage(`{"file":"herdres_connector/source_sync.py","line":7105,"finding":"` + findingProse + `"}`),
				// {severity, file, line, description}: the sixth shape. Measured in
				// the ledger's lifetime, 51 of 814 finding objects carry
				// `description`, and in SIXTEEN of those it is the only prose key,
				// so those sixteen recorded an empty detail. (The 51-of-806 figure
				// this comment first carried counted title+description findings as
				// description-only, because the query omitted `title` and `summary`
				// from its own list of prose keys.)
				json.RawMessage(`{"severity":"P2","file":"docs/local-workflow.md","line":84,"description":"` + descriptionProse + `"}`),
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-2073"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2073)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}

	// TWO of the three land, and the third is refused for a reason this change
	// must NOT fix. {file, finding, line} carries no severity, so the store's
	// ErrFindingSeverity rejects it: a row no severity policy could ever
	// disposition. Reading its prose is necessary and not sufficient, and
	// minting a severity for it would be the invention this writer exists to
	// avoid. The refusal is recorded rather than silent, which is the property
	// worth pinning.
	if len(observations) != 3 {
		t.Fatalf("ledger holds %d row(s); want the three severity-bearing findings", len(observations))
	}

	details := make([]string, 0, len(observations))
	for _, obs := range observations {
		details = append(details, strings.TrimSpace(obs.Detail))
	}
	joined := strings.Join(details, "\n")

	for _, want := range []struct {
		key   string
		prose string
	}{
		{"details", detailsProse},
		{"message", messageProse},
		{"description", descriptionProse},
	} {
		if !strings.Contains(joined, want.prose) {
			t.Fatalf("prose sent under %q never reached the ledger; details column held:\n%s", want.key, joined)
		}
	}

	// A row with prose but no title is still readable; a row with neither is the
	// #1968 population. Neither of these may land bare.
	for _, obs := range observations {
		if strings.TrimSpace(obs.Title) == "" && strings.TrimSpace(obs.Detail) == "" {
			t.Fatalf("observation %q landed with neither title nor detail: a bare obligation nobody can answer", obs.FindingUID)
		}
	}

	// The severity-less finding is REFUSED, not discarded: the writer records an
	// event so the loss is countable. Without this the fix would look complete
	// while one measured shape still vanished silently.
	events, err := store.ListJobEvents(ctx, "review-2073")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	refusal := ""
	for _, event := range events {
		if strings.Contains(event.Kind, "refus") || strings.Contains(event.Message, "refus") {
			refusal += event.Kind + " :: " + event.Message + "\n"
		}
	}
	if !strings.Contains(refusal, "severity") {
		t.Fatalf("no recorded refusal naming severity for the {file, finding, line} shape; events were:\n%s", refusal)
	}
}

// The canonical key must still win when a reviewer sends both, or this fix would
// silently reorder prose for the shapes that already worked.
func TestAdvanceJobPrefersTheCanonicalDetailOverTheAlternates(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("d", 40)

	insertCompletedJob(t, store, db.Job{ID: "review-2073-order", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-order", PullRequest: 2074, HeadSHA: head,
		TaskID: "task-order", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "precedence",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: []json.RawMessage{
				json.RawMessage(`{"severity":"P1","file":"a.go","line":1,"title":"t","detail":"canonical wins","details":"alternate loses","message":"alternate loses","finding":"alternate loses"}`),
			},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-2073-order"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2074)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	if len(observations) != 1 {
		t.Fatalf("ledger holds %d row(s), want 1", len(observations))
	}
	if got := strings.TrimSpace(observations[0].Detail); got != "canonical wins" {
		t.Fatalf("detail = %q, want the canonical `detail` key to win over the alternates", got)
	}
}

// #2077 review F2. The three new aliases must not re-rank prose for inputs that
// ALREADY resolved to something. Before this change {body, details} resolved to
// body and {evidence, message} resolved to evidence; both must still do so.
func TestAdvanceJobDoesNotReorderDetailForInputsThatAlreadyResolved(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("e", 40)

	insertCompletedJob(t, store, db.Job{ID: "review-2077-f2", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-f2", PullRequest: 2078, HeadSHA: head,
		TaskID: "task-f2", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "precedence compatibility",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: []json.RawMessage{
				json.RawMessage(`{"severity":"P1","file":"a.go","line":1,"title":"body case","body":"body wins","details":"details must not win"}`),
				json.RawMessage(`{"severity":"P2","file":"b.go","line":2,"title":"evidence case","evidence":"b.go:2 - evidence wins","message":"message must not win"}`),
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "review-2077-f2"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}
	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2078)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	if len(observations) != 2 {
		t.Fatalf("ledger holds %d row(s), want 2", len(observations))
	}
	for _, obs := range observations {
		if strings.Contains(obs.Detail, "must not win") {
			t.Fatalf("a #2073 alias outranked a pre-existing key: detail = %q", obs.Detail)
		}
	}
}

// #2077 review F1. An obligation whose prose arrived under a detail-only key
// renders as "title=" in the brief: mandatory, and unreadable. The gate refuses
// the head until it is observed and the reviewer is told nothing to observe.
func TestObligationBriefShowsTheConcernWhenTheTitleIsEmpty(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("f", 40)
	const prose = "The reaper ages a claim from the claim row's own created_at, so a resumed value does not reset the wait."

	insertCompletedJob(t, store, db.Job{ID: "review-2077-f1", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-f1", PullRequest: 2079, HeadSHA: head,
		TaskID: "task-f1", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "detail-only obligation",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: []json.RawMessage{
				json.RawMessage(`{"severity":"P1","location":"internal/pipeline/run.go:266","message":"` + prose + `"}`),
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "review-2077-f1"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	// A finding recorded AT a head is already observed there. It becomes an
	// obligation for the NEXT head, which is the round-two case the brief exists
	// to serve.
	nextHead := strings.Repeat("9", 40)
	brief := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 2079, nextHead, "task-f1")
	if strings.TrimSpace(brief) == "" {
		t.Fatalf("no obligation brief rendered; the finding did not become an obligation")
	}
	if !strings.Contains(brief, "title=\n") && !strings.Contains(brief, "title= ") && !strings.Contains(brief, "no title") {
		t.Logf("brief:\n%s", brief)
	}
	if !strings.Contains(brief, prose) {
		t.Fatalf("the brief names a mandatory obligation without its concern; reviewer cannot answer it.\nbrief:\n%s", brief)
	}
}

// #2077 review F3. The concern text is reviewer-authored and unbounded at the
// source, while codex and claude pass the whole prompt as ONE argv element against
// ~128 KiB MAX_ARG_STRLEN. An oversize brief does not degrade: the required
// review fails to exec and the gate waits forever for an observation that can
// never arrive. So the brief must bound what it quotes, and must SAY that it
// did, because a silent cut is the same defect one level up.
func TestObligationBriefBoundsTheConcernTextItQuotes(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("a", 40)
	nextHead := strings.Repeat("8", 40)

	// Forty findings, each carrying 6 KiB of prose: 240 KiB unbounded, which is
	// past MAX_ARG_STRLEN on its own before the rest of the prompt.
	findings := make([]json.RawMessage, 0, 40)
	for i := 0; i < 40; i++ {
		prose := strings.Repeat("x", 6144)
		findings = append(findings, json.RawMessage(fmt.Sprintf(
			`{"severity":"P2","location":"internal/pipeline/run.go:%d","message":"%s"}`, 100+i, prose)))
	}

	insertCompletedJob(t, store, db.Job{ID: "review-2077-f3", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-f3", PullRequest: 2080, HeadSHA: head,
		TaskID: "task-f3", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "oversize concerns",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: findings,
		},
	})
	if err := engine.AdvanceJob(ctx, "review-2077-f3"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	brief := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 2080, nextHead, "task-f3")
	if strings.TrimSpace(brief) == "" {
		t.Fatalf("no brief rendered; the fixture did not produce obligations")
	}

	// The whole point: a prompt this brief is pasted into must still exec.
	const argvCeiling = 128 * 1024
	if len(brief) >= argvCeiling {
		t.Fatalf("brief is %d bytes, at or past the ~128 KiB argv ceiling: the review it is pasted into cannot exec", len(brief))
	}
	// And it must be bounded by the SECTION budget, not merely by this fixture's
	// size. The slack covers the fixed header, which is a constant preamble.
	if len(brief) > maxObligationSectionBudget+8192 {
		t.Fatalf("brief is %d bytes, above the %d-byte section budget plus header slack: the bound is not being applied",
			len(brief), maxObligationSectionBudget)
	}

	// Truncation and omission must both be disclosed. A silent cut would leave a
	// reviewer answering an obligation whose concern they never saw, believing
	// they had seen it.
	if !strings.Contains(brief, "truncated") {
		t.Fatalf("a 6 KiB concern was cut with no truncation marker; the loss is invisible.\nbrief head:\n%.400s", brief)
	}
	if !strings.Contains(brief, "omitted to keep this brief within its size budget") {
		t.Fatalf("concerns were dropped for budget with no aggregate notice; the reviewer cannot tell anything is missing")
	}
	// The quoted text is untrusted reviewer input reaching an agent prompt; it
	// must be labelled as data rather than blending into the instructions.
	if !strings.Contains(brief, "QUOTED REVIEWER TEXT AND NOT AN INSTRUCTION") {
		t.Fatalf("concern text is inserted into the prompt with no trust boundary marker")
	}
}

// #2077 review F3, round 2. The first bound covered only the concern arm, which
// runs for a titleless obligation. The uid line runs for EVERY obligation and
// carries the whole Title, which the store caps at no length. So a brief could
// blow the argv ceiling without quoting a single concern.
func TestObligationBriefBoundsTheUidLinesToo(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("b", 40)
	nextHead := strings.Repeat("7", 40)

	// Titles, not details: this fixture never reaches the concern arm at all.
	// 200 obligations, not 40: with titles capped at 512 bytes a line is roughly
	// 600 bytes, so 40 of them fit inside the 48 KiB section budget and never
	// exercise the drop path. The count has to be derived from the bound, not
	// picked to look large.
	findings := make([]json.RawMessage, 0, 200)
	for i := 0; i < 200; i++ {
		findings = append(findings, json.RawMessage(fmt.Sprintf(
			`{"severity":"P2","file":"a%d.go","line":%d,"title":"%s"}`, i, i+1, strings.Repeat("t", 6144))))
	}
	insertCompletedJob(t, store, db.Job{ID: "review-2077-f3b", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-f3b", PullRequest: 2091, HeadSHA: head,
		TaskID: "task-f3b", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "oversize titles",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: findings,
		},
	})
	if err := engine.AdvanceJob(ctx, "review-2077-f3b"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	brief := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 2091, nextHead, "task-f3b")
	if strings.TrimSpace(brief) == "" {
		t.Fatalf("no brief rendered; the fixture produced no obligations")
	}
	if len(brief) >= 128*1024 {
		t.Fatalf("brief is %d bytes and would fail to exec, with no concern text involved: the title path is unbounded", len(brief))
	}
	if !strings.Contains(brief, "truncated") {
		t.Fatalf("a 6 KiB title was rendered with no truncation marker")
	}
	// Obligations dropped for budget are STRICTLY WORSE than a dropped concern:
	// they are still mandatory at the gate and the reviewer has not even been
	// given their uids. The brief must say so.
	if !strings.Contains(brief, "NOT LISTED AT ALL") {
		t.Fatalf("obligations were dropped for budget with no notice; the reviewer believes the list is complete.\nbrief tail:\n%s", brief[max(0, len(brief)-600):])
	}
}

// #2077 review F4. Byte-slicing arbitrary reviewer prose can keep the first byte
// of a multi-byte rune, and the invalid sequence survives strings.Builder and
// argv all the way to the runtime, where it is silently replaced rather than
// refused. One cut finding corrupts the ENTIRE brief.
func TestObligationBriefStaysValidUTF8WhenItTruncates(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("c", 40)
	nextHead := strings.Repeat("6", 40)

	// "x" plus 1,024 copies of U+00E9 is valid UTF-8 and 2,049 bytes, so a cut at
	// 2,048 lands inside the final rune. Same shape for the title arm.
	prose := "x" + strings.Repeat("\u00e9", 1024)
	title := "y" + strings.Repeat("\u00e9", 256)

	insertCompletedJob(t, store, db.Job{ID: "review-2077-f4", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-f4", PullRequest: 2092, HeadSHA: head,
		TaskID: "task-f4", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "multibyte prose",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: []json.RawMessage{
				mustFindingJSON(t, map[string]any{"severity": "P2", "location": "internal/pipeline/run.go:1", "message": prose}),
				mustFindingJSON(t, map[string]any{"severity": "P2", "file": "b.go", "line": 2, "title": title}),
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "review-2077-f4"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	brief := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 2092, nextHead, "task-f4")
	if strings.TrimSpace(brief) == "" {
		t.Fatalf("no brief rendered")
	}
	if !utf8.ValidString(brief) {
		t.Fatalf("the brief is not valid UTF-8 after truncation: one split rune corrupts the whole prompt")
	}
	if !strings.Contains(brief, "truncated") {
		t.Fatalf("nothing was truncated, so this fixture does not exercise the cut")
	}
}

func mustFindingJSON(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal finding returned error: %v", err)
	}
	return raw
}

// #2077 review F5 and F6, as unit properties of the helper. The brief-level
// tests above cover the paths production uses; these pin the contract itself,
// because both findings are about inputs no current caller produces and a
// contract that only holds for today's callers is not a contract.
func TestTruncateAtRuneHonoursItsContract(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       string
		max      int
		want     string
		wantDrop int
	}{
		// F5: nonpositive means NOTHING fits, not "no limit". The old code
		// returned the input unchanged and reported zero dropped.
		{"zero max drops everything", "abc", 0, "", 3},
		{"negative max drops everything", "abc", -1, "", 3},
		{"exact fit is untouched", "abc", 3, "abc", 0},
		{"cut at a rune start", "ab\u00e9", 2, "ab", 2},
		// A single rune longer than the limit yields the empty string rather
		// than half a rune.
		{"one rune wider than the limit", "\u00e9", 1, "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped, _ := truncateAtRune(tc.in, tc.max)
			if got != tc.want || dropped != tc.wantDrop {
				t.Fatalf("truncateAtRune(%q, %d) = (%q, %d), want (%q, %d)",
					tc.in, tc.max, got, dropped, tc.want, tc.wantDrop)
			}
			if tc.max > 0 && len(got) > tc.max {
				t.Fatalf("returned %d bytes for a %d-byte limit", len(got), tc.max)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("returned invalid UTF-8: %q", got)
			}
		})
	}
}

// F6 end to end: invalid UTF-8 that is ALREADY IN THE STORE must not reach the
// prompt, and the rewrite must be disclosed (#2077 review F4 and F6).
//
// THE FIRST VERSION OF THIS TEST COULD NOT FAIL. It built the finding with
// json.Marshal, and Go's JSON encoder replaces invalid UTF-8 with U+FFFD on the
// way in - so the stored Detail was already valid and the "invalid stored bytes"
// case was never constructed. It asserted validity of a brief that had no
// invalid bytes to survive. The only way to put a raw 0xff in the ledger is to
// write the observation directly, which is also the only way the store can
// really acquire one.
func TestObligationBriefSanitisesInvalidUTF8AlreadyInTheStore(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	head := strings.Repeat("e", 40)
	nextHead := strings.Repeat("5", 40)

	bad := "a\xffb concern that was stored with an invalid byte"
	if utf8.ValidString(bad) {
		t.Fatalf("fixture is not invalid UTF-8, so this test cannot exercise the case")
	}

	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 2093, HeadSHA: head,
		ObserverJob: "review-2077-f6", State: db.FindingOpen, Severity: "P2",
		RoundLabel: "F-1", Detail: bad, File: "internal/pipeline/run.go", Line: 1,
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./internal/workflow/"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation returned error: %v", err)
	}

	stored, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2093)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d rows, want 1", len(stored))
	}
	if utf8.ValidString(stored[0].Detail) {
		t.Fatalf("the store sanitised the byte on write, so the brief cannot be what fixes it")
	}

	brief := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 2093, nextHead, "task-f6")
	if strings.TrimSpace(brief) == "" {
		t.Fatalf("no brief rendered")
	}
	if !utf8.ValidString(brief) {
		t.Fatalf("the brief carries invalid UTF-8 inherited from the store, with no truncation involved")
	}
	if strings.Contains(brief, "\xff") {
		t.Fatalf("the raw invalid byte reached the prompt")
	}
	// Coercion must be DISCLOSED. A long invalid run collapses to one replacement
	// rune and can then fit the budget, so it reports zero bytes dropped and emits
	// no truncation marker. Without a separate signal the reviewer reads silently
	// rewritten prose as if it were what was recorded.
	if !strings.Contains(brief, "undecodable bytes") {
		t.Fatalf("prose was rewritten with U+FFFD and the brief said nothing.\nbrief:\n%s", brief)
	}
}

// The ORDER of coercion and truncation is the correctness argument for F6, and a
// test written after the fact passes either way. This one cannot: the input is
// valid-length before coercion and over-budget after it, because each invalid
// byte becomes a 3-byte replacement rune. Truncating first would satisfy the
// bound and emit invalid UTF-8; coercing first and not re-checking would emit
// valid UTF-8 over the bound. Only coerce-then-truncate satisfies both.
func TestTruncateAtRuneCoercesBeforeItMeasures(t *testing.T) {
	// EACH INVALID BYTE MUST BE ITS OWN RUN. strings.ToValidUTF8 replaces a RUN of
	// invalid bytes with ONE replacement rune, so 100 consecutive 0xff collapse to
	// 3 bytes and never cross any budget. Interleaved, each 0xff is a separate run:
	// 100 x "a\xff" is 200 bytes raw and 400 coerced.
	//
	// The growth factor therefore depends on the DISTRIBUTION of invalid bytes, not
	// their count, which is the detail that made the first version of this test
	// pass for the wrong reason.
	in := strings.Repeat("a\xff", 100)
	const max = 200

	got, dropped, _ := truncateAtRune(in, max)

	if !utf8.ValidString(got) {
		t.Fatalf("result is not valid UTF-8: coercion did not happen, or happened after the cut")
	}
	if len(got) > max {
		t.Fatalf("result is %d bytes for a %d-byte limit: the bound was applied BEFORE coercion, so coercion then grew it past the limit", len(got), max)
	}
	if dropped == 0 {
		t.Fatalf("nothing reported dropped, but this 200-byte input has 100 SEPARATED invalid runs and so coerces to 400 bytes, which cannot fit in %d. "+
			"Note the arithmetic: growth is per invalid RUN, not per invalid byte", max)
	}
}

// #2077 review F7. The truncation marker says "bytes of normalised text" rather
// than "bytes on the row" because the count is measured AFTER coercion, and the
// two differ whenever coercion changed the length. Every existing truncation
// test asserted only strings.Contains(brief, "truncated"), so the reviewer
// restored the disproven "bytes on the row" wording in an isolated archive of
// this head and the whole package stayed green.
//
// This fixture makes the two wordings SAY DIFFERENT NUMBERS: the stored title is
// invalid UTF-8, so coercion lengthens it, and the marker's count is then only
// correct for the normalised text. Asserting the arithmetic pins the wording to
// the thing that makes it true, rather than pinning the sentence.
func TestTruncationMarkerCountsNormalisedBytesNotStoredBytes(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	head := strings.Repeat("7", 40)

	// The invalid bytes must be INTERLEAVED, not contiguous: ToValidUTF8 collapses
	// each contiguous run of invalid bytes into ONE replacement rune, so 900
	// consecutive 0xff coerce to 3 bytes and SHORTEN the title. Interleaved, each
	// 1-byte 0xff becomes a 3-byte U+FFFD and the coerced title is strictly
	// longer, which is what makes a count "on the row" differ from a count of the
	// normalised text. (This fixture caught its own author making that error.)
	stored := strings.Repeat("a\xff", 450) + " title"
	if utf8.ValidString(stored) {
		t.Fatalf("fixture is valid UTF-8, so coercion cannot change its length")
	}
	coerced := strings.ToValidUTF8(stored, "\uFFFD")
	if len(coerced) <= len(stored) {
		t.Fatalf("coercion did not lengthen the title (%d -> %d), so this fixture cannot tell the two counts apart",
			len(stored), len(coerced))
	}

	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 2094, HeadSHA: head,
		ObserverJob: "review-2077-f7", State: db.FindingOpen, Severity: "P2",
		RoundLabel: "F-7", Title: stored, Detail: "a detail that is present",
		File: "internal/workflow/findings_ledger_writer.go", Line: 756,
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./internal/workflow/"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation returned error: %v", err)
	}

	// A finding recorded at `head` becomes an obligation when a LATER head is
	// reviewed; asking at the same head renders an empty brief.
	nextHead := strings.Repeat("8", 40)
	brief := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 2094, nextHead, "task-2077-f7")
	if strings.TrimSpace(brief) == "" {
		t.Fatalf("no brief rendered, so this fixture cannot exercise the marker")
	}
	if !strings.Contains(brief, "truncated") {
		t.Fatalf("nothing was truncated, so this fixture does not exercise the marker")
	}

	// The marker must report the bytes dropped from the COERCED text. The stored
	// length is a different, larger-gap number; asserting the coerced one is what
	// kills the "bytes on the row" wording.
	rendered := 0
	keptText := ""
	for _, line := range strings.Split(brief, "\n") {
		if idx := strings.Index(line, "[truncated, "); idx >= 0 {
			rest := line[idx+len("[truncated, "):]
			end := strings.Index(rest, " more bytes")
			if end < 0 {
				t.Fatalf("marker does not carry a byte count: %q", line)
			}
			n, convErr := strconv.Atoi(rest[:end])
			if convErr != nil {
				t.Fatalf("marker byte count %q is not a number: %v", rest[:end], convErr)
			}
			rendered = n
			if !strings.Contains(line, "more bytes of normalised text") {
				t.Fatalf("marker must name the text it measured; got %q", line)
			}
			// The bytes the brief ACTUALLY retained, read off the rendered line
			// rather than derived from the number under test.
			titleAt := strings.Index(line, "  title=")
			if titleAt < 0 {
				t.Fatalf("brief line carries no title field: %q", line)
			}
			// The marker is appended as " [truncated, ...", so the byte before it
			// is the separator space and is not part of the retained title.
			if idx == 0 || line[idx-1] != ' ' {
				t.Fatalf("marker is not space-separated from the title, so this slice would miscount: %q", line)
			}
			keptText = line[titleAt+len("  title=") : idx-1]
		}
	}
	if rendered == 0 {
		t.Fatalf("no truncation marker found in brief")
	}

	kept := len(keptText)
	if kept <= 0 || kept >= len(coerced) {
		t.Fatalf("brief retained %d bytes of a %d-byte coerced title, which is not a cut", kept, len(coerced))
	}

	// THE ASSERTION IS AGAINST THE BYTES ON THE PAGE (#2077 review F11). The
	// first version of this test derived `kept` as len(coerced)-rendered and then
	// checked len(stored)-kept != rendered, which reduces by substitution to
	// len(stored) != len(coerced) - a fact the fixture already asserts above. It
	// never compared the marker to anything the brief produced, so any wrong
	// count survived it.
	//
	// `kept` is now read off the rendered line, so the marker is checked against
	// the text it accompanies.
	if want := len(coerced) - kept; rendered != want {
		t.Fatalf("marker reports %d dropped, but the brief retained %d of %d coerced bytes, so %d were dropped",
			rendered, kept, len(coerced), want)
	}
	// And the same arithmetic against the STORED bytes gives a different number,
	// so a marker claiming "bytes on the row" would be stating a figure this
	// brief never computed.
	if onTheRow := len(stored) - kept; onTheRow == rendered {
		t.Fatalf("stored and coerced counts coincide (%d), so this fixture cannot discriminate the two wordings", rendered)
	}
}

// #2100 f1. A NUL IN A FINDING PREVENTS THE REVIEW FROM STARTING AT ALL.
//
// U+0000 is valid UTF-8, so every UTF-8 repair in this file passes it through.
// The brief is handed to a runtime as an argv element, and Go's
// syscall.SlicePtrFromStrings rejects an argument containing NUL - so one
// malformed finding stops the process that would have judged it. That is worse
// than a corrupted prompt: nothing runs to report the problem.
//
// THE ASSERTION IS THAT THE BRIEF CAN ACTUALLY BE EXECVE'D, not that a byte
// changed. Checking for the absence of "\x00" would pass on a fix that dropped
// the whole title, and would not notice a different control byte with the same
// effect. exec.Cmd.Start is the consumer's own rejection path.
func TestObligationBriefCanBePassedToARuntime(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	head := strings.Repeat("9", 40)
	nextHead := strings.Repeat("f", 40)

	hostile := "before\x00after and an escape \x1b[31m and a carriage \r return"
	if !strings.ContainsRune(hostile, 0) {
		t.Fatalf("fixture carries no NUL, so it cannot exercise the case")
	}
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 2100, HeadSHA: head,
		ObserverJob: "review-2100-f1", State: db.FindingOpen, Severity: "P1",
		RoundLabel: "f1", Title: hostile, Detail: "a detail carrying " + hostile,
		File: "internal/workflow/findings_ledger_writer.go", Line: 1,
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./internal/workflow/"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation returned error: %v", err)
	}
	// PRECONDITION: the store must have kept the NUL, or the brief is not what
	// fixes this and the test proves nothing.
	stored, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2100)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	if len(stored) != 1 || !strings.ContainsRune(stored[0].Title, 0) {
		t.Fatalf("the store stripped the NUL on write, so the brief cannot be what fixes it: %+v", stored)
	}

	brief := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 2100, nextHead, "task-2100")
	if strings.TrimSpace(brief) == "" {
		t.Fatalf("no brief rendered, so this fixture cannot exercise the case")
	}

	// THE REAL CONSUMER'S REJECTION PATH. exec.Cmd.Start fails with
	// "invalid argument" when any argv element contains a NUL.
	cmd := exec.CommandContext(ctx, "/bin/true", brief)
	if err := cmd.Start(); err != nil {
		t.Fatalf("the obligation brief cannot be passed to a runtime as argv, so the review it belongs to would never start: %v", err)
	}
	_ = cmd.Wait()

	// And the reviewer must be able to see that something was rewritten, rather
	// than reading silently altered prose.
	if !strings.Contains(brief, "\uFFFD") {
		t.Fatalf("the hostile bytes vanished without a replacement marker; a reviewer cannot tell their text was changed:\n%s", brief)
	}
}

// #2100 f3. TWO REPAIRS ON ONE STRING, AND THE SECOND HID THE FIRST.
//
// A title carrying BOTH a hostile control and an invalid byte was fully repaired
// by the control map - strings.Map decodes an invalid byte as utf8.RuneError and
// writes a real U+FFFD - so the validity check that follows saw a clean string,
// `coerced` stayed false, and the brief omitted the undecodable-bytes
// disclosure. The bytes were rewritten and the reviewer was not told.
//
// The single-defect fixtures could not catch this: one has a control and no
// invalid byte, the other an invalid byte and no control. It takes both.
func TestObligationBriefDisclosesCoercionWhenAControlIsAlsoPresent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	head := strings.Repeat("3", 40)
	nextHead := strings.Repeat("4", 40)

	mixed := "before\x00after and an undecodable \xff byte"
	if !strings.ContainsRune(mixed, 0) || utf8.ValidString(mixed) {
		t.Fatalf("fixture must carry BOTH a control and invalid UTF-8, or it cannot exercise the interaction")
	}
	if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
		Repo: "gitmoot/gitmoot", PullRequest: 2103, HeadSHA: head,
		ObserverJob: "review-2100-f3", State: db.FindingOpen, Severity: "P2",
		RoundLabel: "f3", Title: mixed, Detail: "a detail",
		File: "internal/workflow/findings_ledger_writer.go", Line: 1,
		EvidenceKind: db.EvidenceExecuted, ExecutedCommands: []string{"go test ./internal/workflow/"}, ExecutedCount: 1,
	}); err != nil {
		t.Fatalf("RecordReviewFindingObservation returned error: %v", err)
	}
	stored, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2103)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations returned error: %v", err)
	}
	if len(stored) != 1 || utf8.ValidString(stored[0].Title) {
		t.Fatalf("the store repaired the title on write, so the brief cannot be what fixes this: %+v", stored)
	}

	brief := engine.ledgerObligationBrief(ctx, "gitmoot/gitmoot", 2103, nextHead, "task-2103")
	if strings.TrimSpace(brief) == "" {
		t.Fatalf("no brief rendered")
	}
	if !strings.Contains(brief, "row held undecodable bytes") {
		t.Fatalf("the row's invalid bytes were rewritten with no disclosure, because the control repair got there first:\n%s", brief)
	}
}
