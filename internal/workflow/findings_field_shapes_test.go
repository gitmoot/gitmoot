package workflow

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// ONE ODD FIELD MUST NOT DROP THE FINDING (tandem seat, message 412412,
// jerryfane/tandem#2). encoding/json fails a whole object when one field has
// another shape, and the writer's only fallback was "a bare string", so a
// complete P2 whose `evidence` was an array recorded 0 of 1. Every fixture here
// is a shape measured in the live store; tandemFinding is the reported finding
// itself, copied read-only from job local-review-review-router-18e5a038be33e82e-1.
const tandemFinding = `{"id":"F1","severity":"P2","title":"Propagate receipt retrieval failure instead of returning success","file":"scripts/macserve-job.sh","line_start":72,"line_end":75,"body":"When the job state is succeeded but the receipt request fails, the failed get is the left-hand command of an \u0026\u0026 list, so set -e does not terminate the script. Execution reaches the final successful state comparison and exits 0. This contradicts the script's documented requirement that success requires a signed receipt and lets callers accept an unverified build or test result. Independently reproduced exit 0 for receipt HTTP 404 in both build and unit_test modes, and for ten consecutive receipt transport failures in build mode. Make receipt retrieval failure terminate with a nonzero status.","evidence":["https://github.com/jerryfane/tandem/blob/24ce1fdbbc8e53217131f009012298bcdb852116/scripts/macserve-job.sh#L72-L75","Executed the unchanged script through an in-memory Bash harness replacing only git, curl, and sleep. Receipt HTTP 404 and transport-exhaustion scenarios printed their GET failure, omitted 'receipt: present', and returned exit 0."]}`

type shapeRun struct {
	events []db.JobEvent
	rows   []db.ReviewFindingObservation
}

func (r shapeRun) skipped() []string {
	var messages []string
	for _, event := range r.events {
		if event.Kind == "findings_ledger_skipped" {
			messages = append(messages, event.Message)
		}
	}
	return messages
}

func (r shapeRun) summary() string {
	for _, event := range r.events {
		if event.Kind == "findings_ledger_recorded" {
			return event.Message
		}
	}
	return ""
}

// recordShapedFindings runs the production writer over findings. executed
// selects an EXECUTED verdict; otherwise the verdict is static, so a finding's
// own locator decides its evidence kind. failed makes it a failed review.
func recordShapedFindings(t *testing.T, executed, failed bool, findings ...string) shapeRun {
	t.Helper()
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "rev", []string{"review"}, "jerryfane/tandem")
	engine := testEngine(store)
	job := db.Job{ID: "rev-shapes", Agent: "rev", Type: "review"}
	result := &AgentResult{Decision: "changes_requested", Severity: "P2", Summary: "s"}
	if executed {
		result.TestsRun = []string{"bash scripts/macserve-job.sh"}
		result.Evidence = "EXECUTED"
	}
	if failed {
		result.Decision = "failed"
	}
	for _, finding := range findings {
		result.Findings = append(result.Findings, json.RawMessage(finding))
	}
	payload := JobPayload{
		Repo: "jerryfane/tandem", Branch: "b", PullRequest: 2,
		HeadSHA: "24ce1fdbbc8e53217131f009012298bcdb852116", TaskID: "t", ReviewRound: "review-1",
		Result: result,
	}
	insertCompletedJob(t, store, job, payload)
	stored := mustJob(t, store, job.ID)
	if failed {
		stored.State = string(JobFailed)
	}
	if err := engine.RecordReviewFindingsToLedger(ctx, stored, payload); err != nil {
		t.Fatalf("RecordReviewFindingsToLedger: %v", err)
	}
	events, err := store.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	rows, err := store.ListReviewFindingObservations(ctx, "jerryfane/tandem", 2)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations: %v", err)
	}
	return shapeRun{events: events, rows: rows}
}

func onlyRow(t *testing.T, run shapeRun) db.ReviewFindingObservation {
	t.Helper()
	if len(run.rows) != 1 {
		t.Fatalf("recorded %d rows, want 1; skips=%q summary=%q", len(run.rows), run.skipped(), run.summary())
	}
	return run.rows[0]
}

// THE REPORTED FINDING, VERBATIM. Before: "finding is neither an object nor a
// string: json: cannot unmarshal array into ... evidence of type string" and
// "recorded 0 of 1".
func TestTandemFindingWithEvidenceArrayIsRecorded(t *testing.T) {
	run := recordShapedFindings(t, true, false, tandemFinding)
	row := onlyRow(t, run)
	if row.Severity != "P2" || row.RoundLabel != "F1" || row.File != "scripts/macserve-job.sh" ||
		row.Title != "Propagate receipt retrieval failure instead of returning success" {
		t.Fatalf("row = %+v, want the reviewer's severity, id, file and title", row)
	}
	// body still outranks evidence for detail: precedence is unchanged.
	if !strings.HasPrefix(row.Detail, "When the job state is succeeded") {
		t.Fatalf("detail = %q, want the body", row.Detail)
	}
	if !strings.Contains(run.summary(), "recorded 1 of 1") {
		t.Fatalf("summary = %q", run.summary())
	}
}

// ONE FINDING PER MEASURED SHAPE. Each fixture used to be skipped whole.
func TestEachMeasuredFieldShapeIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		static bool
		check  func(t *testing.T, row db.ReviewFindingObservation)
	}{
		{
			name: "line as numeric string",
			raw:  `{"severity":"P2","title":"t","file":"internal/a.go","line":"145"}`,
			check: func(t *testing.T, row db.ReviewFindingObservation) {
				if row.Line != 145 {
					t.Fatalf("line = %d, want 145", row.Line)
				}
			},
		},
		{
			name: "line as range string",
			raw:  `{"severity":"P2","title":"t","file":"internal/a.go","line":"145-169"}`,
			check: func(t *testing.T, row db.ReviewFindingObservation) {
				if row.Line != 145 {
					t.Fatalf("line = %d, want the range's first number 145", row.Line)
				}
			},
		},
		{
			name: "line as prose is left 0 and the rest kept",
			raw:  `{"severity":"P2","title":"t","file":"internal/a.go","line":"expect(\"feed\") at L958-963"}`,
			check: func(t *testing.T, row db.ReviewFindingObservation) {
				if row.Line != 0 || row.Title != "t" || row.File != "internal/a.go" {
					t.Fatalf("row = %+v, want line 0 with title and file kept", row)
				}
			},
		},
		{
			name: "evidence as array of strings",
			raw:  `{"severity":"P2","title":"t","evidence":["internal/a.go:12 - the guard is inverted","go test ./internal/a printed FAIL"]}`,
			check: func(t *testing.T, row db.ReviewFindingObservation) {
				if row.Detail != "internal/a.go:12 - the guard is inverted\ngo test ./internal/a printed FAIL" {
					t.Fatalf("detail = %q, want the elements joined by newline", row.Detail)
				}
				if row.File != "internal/a.go" {
					t.Fatalf("file = %q, want the lens path from the first element", row.File)
				}
			},
		},
		{
			name: "evidence as object keeps its JSON text",
			raw:  `{"severity":"P2","title":"t","evidence":{"source":"internal/a.go:12","execution":"go test printed FAIL"}}`,
			check: func(t *testing.T, row db.ReviewFindingObservation) {
				if row.Detail != `{"source":"internal/a.go:12","execution":"go test printed FAIL"}` {
					t.Fatalf("detail = %q, want the object's compact JSON", row.Detail)
				}
			},
		},
		{
			name: "evidence_locator as array",
			raw:  `{"severity":"P2","title":"t","evidence_locator":["internal/a.go:12","internal/b.go:30"]}`,
			check: func(t *testing.T, row db.ReviewFindingObservation) {
				if row.File != "internal/a.go" {
					t.Fatalf("file = %q, want the first locator's path", row.File)
				}
			},
		},
		{
			name: "evidence_locator as {source,execution} object keeps its JSON text",
			raw:  `{"severity":"P2","title":"t","detail":"d","evidence_locator":{"source":"internal/a.go:12","execution":"go test"}}`,
			check: func(t *testing.T, row db.ReviewFindingObservation) {
				if row.Detail != "d" || row.File != "" {
					t.Fatalf("row = %+v, want detail kept and no invented file", row)
				}
			},
		},
		{
			name:   "evidence_locator as {file,line} object is a structural locator",
			raw:    `{"severity":"P2","title":"t","file":"internal/a.go","rationale":"read the guard","evidence_locator":{"file":"internal/a.go","line":12}}`,
			static: true,
			check: func(t *testing.T, row db.ReviewFindingObservation) {
				if row.EvidenceKind != db.EvidenceStatic || row.EvidenceLocator != "internal/a.go:12" {
					t.Fatalf("row kind=%s locator=%q, want STATIC at internal/a.go:12", row.EvidenceKind, row.EvidenceLocator)
				}
			},
		},
		{
			name: "location as {path,start_line,end_line} object",
			raw:  `{"severity":"P2","title":"t","location":{"path":"internal/a.go","start_line":145,"end_line":169}}`,
			check: func(t *testing.T, row db.ReviewFindingObservation) {
				if row.File != "internal/a.go" {
					t.Fatalf("file = %q, want the location's path", row.File)
				}
			},
		},
		{
			name: "location as {path,line_start,line_end} object",
			raw:  `{"severity":"P2","title":"t","location":{"path":"internal/a.go","line_start":7,"line_end":9}}`,
			check: func(t *testing.T, row db.ReviewFindingObservation) {
				if row.File != "internal/a.go" {
					t.Fatalf("file = %q, want the location's path", row.File)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := recordShapedFindings(t, !tc.static, false, tc.raw)
			tc.check(t, onlyRow(t, run))
		})
	}
}

// SEVERAL ODD FIELDS AT ONCE. Field-by-field decoding has to keep every other
// field, not just survive the first odd one.
func TestFindingWithSeveralOddFieldsIsRecorded(t *testing.T) {
	run := recordShapedFindings(t, true, false, `{
		"id":"F7","severity":"P1","title":"the retry loop never backs off",
		"line":"14,27-28,52-55",
		"evidence":["internal/retry.go:14 - no sleep",{"command":"go test ./internal/retry","observed":"FAIL"}],
		"evidence_locator":["internal/retry.go:14","internal/retry.go:52"],
		"location":{"path":"internal/retry.go","start_line":14,"end_line":28},
		"relevance_keys":"internal/backoff.go",
		"state":"open"}`)
	row := onlyRow(t, run)
	if row.RoundLabel != "F7" || row.Severity != "P1" || row.Title != "the retry loop never backs off" ||
		row.Line != 14 || row.File != "internal/retry.go" {
		t.Fatalf("row = %+v", row)
	}
	if want := "internal/retry.go:14 - no sleep\n" + `{"command":"go test ./internal/retry","observed":"FAIL"}`; row.Detail != want {
		t.Fatalf("detail = %q, want %q", row.Detail, want)
	}
	// A single string is one key (the store adds the row's own file beside it).
	if !slices.Contains(row.RelevanceKeys, "internal/backoff.go") {
		t.Fatalf("relevance keys = %q, want internal/backoff.go among them", row.RelevanceKeys)
	}
}

// A FINDING THAT IS NEITHER AN OBJECT NOR A STRING IS STILL SKIPPED, and the
// skip says what it was instead of quoting a decoder error about a struct.
func TestMalformedFindingIsSkippedWithAClearReason(t *testing.T) {
	for _, tc := range []struct{ raw, kind string }{
		{raw: `42`, kind: "number"},
		{raw: `["P2","internal/a.go"]`, kind: "array"},
		{raw: `true`, kind: "boolean"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			run := recordShapedFindings(t, true, false, tc.raw)
			if len(run.rows) != 0 {
				t.Fatalf("a JSON %s was recorded as a finding: %+v", tc.kind, run.rows)
			}
			skips := run.skipped()
			want := "finding[0] not recorded to the #1822 ledger: finding is a JSON " + tc.kind + ", neither an object nor a string"
			if len(skips) != 1 || skips[0] != want {
				t.Fatalf("skips = %q, want [%q]", skips, want)
			}
			if !strings.Contains(run.summary(), "recorded 0 of 1") || !strings.Contains(run.summary(), "1 skipped") {
				t.Fatalf("summary = %q", run.summary())
			}
		})
	}
}

// A FINDING STILL NOT RECORDED NAMES THE FIELD IT COULD NOT READ. A failed
// review's quoted-only finding is a deliberate refusal; when that finding also
// carried an unreadable line, the skip event says which field and what value.
func TestStillSkippedFindingNamesTheUnreadableField(t *testing.T) {
	run := recordShapedFindings(t, false, true, `{"severity":"P2","title":"t","line":"see L958"}`)
	skips := run.skipped()
	if len(skips) != 1 || !strings.HasPrefix(skips[0], "finding[0] ") ||
		!strings.Contains(skips[0], `field "line" value "see L958" is not a line number, left 0`) {
		t.Fatalf("skips = %q, want finding[0] naming the line field", skips)
	}
}

// WELL-TYPED FINDINGS DECODE EXACTLY AS BEFORE. plainReviewFindingWire has the
// same fields and no UnmarshalJSON, so it is the stock encoding/json reading:
// every existing alias, case-insensitive key and duplicate-key rule must match.
type plainReviewFindingWire reviewFindingWire

func TestWellTypedFindingsDecodeAsEncodingJSONDoes(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`null`,
		`{"id":"F1","severity":"P1","file":"a/b.go","line":12,"title":"t","detail":"d"}`,
		`{"uid":"#1930-f9","disposition":"answered","evidence":"internal/a.go:3 - why","lens":"refute"}`,
		`{"locator":"internal/workflow/merge_gate.go: collectImplementerAttribution (~lines 1436-1449)","evidence_locator":"a/b.go:3"}`,
		`{"summary":"s","details":"x","message":"m","finding":"f","description":"desc","body":"b","location":"a/b.go:1"}`,
		`{"continues_uid":"u","state":"withdrawn","withdraw_reason":"r","evidence_kind":"quoted","rationale":"why","relevance_keys":["a/b.go","c/d.go"]}`,
		`{"Title":"case-folded key","SEVERITY":"P2","Evidence_Locator":"a/b.go"}`,
		`{"title":"first","title":"second","Title":"third"}`,
		`{"title":"t","unknown_key":{"nested":[1,2]},"line":-3}`,
		`{"title":"t","line":7,"relevance_keys":null,"detail":null}`,
		// NULL IN KEY ORDER (PR #2544 round 1). encoding/json sets a slice to nil
		// on null and leaves a string or int untouched, so a later null wins for
		// the slice and is a no-op for the others.
		`{"relevance_keys":["a"],"relevance_keys":null}`,
		`{"relevance_keys":["a"],"Relevance_Keys":null}`,
		`{"relevance_keys":null,"relevance_keys":["b"]}`,
		`{"title":"a","title":null}`,
		`{"title":null,"title":"b"}`,
		`{"evidence_locator":"a/b.go:3","Evidence_Locator":null}`,
		`{"line":12,"line":null}`,
		`{"line":null,"line":5}`,
		`{"title":"a","title":null,"line":12,"line":null,"relevance_keys":["a","b"],"relevance_keys":null}`,
	} {
		var plain plainReviewFindingWire
		if err := json.Unmarshal([]byte(raw), &plain); err != nil {
			t.Fatalf("stock decode of %s: %v", raw, err)
		}
		var wire reviewFindingWire
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			t.Fatalf("decode of %s: %v", raw, err)
		}
		if !reflect.DeepEqual(reviewFindingWire(plain), wire) {
			t.Fatalf("decode of %s\n got %+v\nwant %+v", raw, wire, reviewFindingWire(plain))
		}
	}
}

// THE POST-MERGE CLASSIFIER READS THE SAME WAY. It had the same whole-object
// decode, so the tandem P2 would have raised no follow-up issue either.
func TestPostMergeClassifierKeepsAFindingWithAnOddField(t *testing.T) {
	_, followups := classifyPostMergeFindings("job-1", []json.RawMessage{json.RawMessage(tandemFinding)})
	if len(followups) != 1 {
		t.Fatalf("followups = %+v, want the tandem P2", followups)
	}
	if got := followups[0]; got.location != "scripts/macserve-job.sh" ||
		got.title != "Propagate receipt retrieval failure instead of returning success" {
		t.Fatalf("followup = %+v", got)
	}
	urgent, _ := classifyPostMergeFindings("job-2", []json.RawMessage{json.RawMessage(
		`{"severity":"P1","title":"t","location":{"path":"internal/a.go","start_line":9}}`)})
	if len(urgent) != 1 || urgent[0].location != "internal/a.go:9" {
		t.Fatalf("urgent = %+v, want location rendered as internal/a.go:9", urgent)
	}
}
