package workflow

import (
	"context"
	"encoding/json"
	"fmt"
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
		detailsProse = "ListJobsByType selects jobColumns, so externally_driven is carried; a narrower list would take the weaker reason."
		messageProse = "The reaper ages a claim from the claim row's own created_at, so a resumed value does not reset the wait."
		findingProse = "An unresolved hold is keyed by turn_id plus content_hash, so changing content on the same turn bypasses it."
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
	if len(observations) != 2 {
		t.Fatalf("ledger holds %d row(s); want the two severity-bearing findings", len(observations))
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
// source, while codex and kimi pass the whole prompt as ONE argv element against
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
			got, dropped := truncateAtRune(tc.in, tc.max)
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
// prompt. The earlier regression only covered corruption introduced by cutting
// valid input, so a stored `a 0xff b` passed straight through.
func TestObligationBriefSanitisesInvalidUTF8AlreadyInTheStore(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "g7-review", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	head := strings.Repeat("e", 40)
	nextHead := strings.Repeat("5", 40)

	// Short enough that no truncation happens: the point is that the brief is
	// valid even when nothing is cut.
	bad := "a\xffb concern that was stored with an invalid byte"

	insertCompletedJob(t, store, db.Job{ID: "review-2077-f6", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-f6", PullRequest: 2093, HeadSHA: head,
		TaskID: "task-f6", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "invalid stored bytes",
			Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
			Findings: []json.RawMessage{
				mustFindingJSON(t, map[string]any{"severity": "P2", "location": "internal/pipeline/run.go:1", "message": bad}),
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "review-2077-f6"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
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
}
