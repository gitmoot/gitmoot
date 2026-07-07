package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/skillopt"
)

// binaryLessonsSetYAML gives the seeded question ids their human wording so the
// lessons command can recover it via --set (the verdicts table stores only ids).
const binaryLessonsSetYAML = `
version: 1
template_or_task_kind: planner
dimensions:
  - name: correctness
    questions:
      - id: q_tests
        text: Does the change add unit tests covering new parser branches
  - name: docs
    questions:
      - id: q_docs
        text: Does the change update the CLI documentation reference
  - name: safety
    questions:
      - id: q_errors
        text: Does the code check returned filesystem errors before proceeding
  - name: observability
    questions:
      - id: q_logs
        text: Does the code emit structured logging on failure paths
  - name: style
    questions:
      - id: q_pkg
        text: Does the output declare a package clause
`

// seedBinaryLessonsHome writes two eval runs (a candidate v2 and a champion v1)
// of template "planner" and their per-question binary verdicts so the
// disagreement view has a flip, three stable failures, and one stable pass.
func seedBinaryLessonsHome(t *testing.T, home string) {
	t.Helper()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	for _, run := range []db.EvalRun{
		{ID: "cand", TemplateID: "planner", TemplateVersionID: "planner@2", TargetRepo: "owner/repo", State: "ready"},
		{ID: "champ", TemplateID: "planner", TemplateVersionID: "planner@1", TargetRepo: "owner/repo", State: "ready"},
	} {
		if err := store.UpsertEvalRun(ctx, run); err != nil {
			t.Fatalf("UpsertEvalRun %s: %v", run.ID, err)
		}
	}

	verdicts := []db.BinaryVerdict{
		// FLIP: candidate passes, champion fails.
		{RunID: "cand", QuestionID: "q_tests", Dimension: "correctness", Verdict: "yes"},
		{RunID: "champ", QuestionID: "q_tests", Dimension: "correctness", Verdict: "no", Explanation: "new parser branch untested"},
		// STABLE FAILS (both runs no).
		{RunID: "cand", QuestionID: "q_docs", Dimension: "docs", Verdict: "no", Explanation: "cli reference page stale"},
		{RunID: "champ", QuestionID: "q_docs", Dimension: "docs", Verdict: "no", Explanation: "cli reference page stale"},
		{RunID: "cand", QuestionID: "q_errors", Dimension: "safety", Verdict: "no", Explanation: "ignored filesystem write error"},
		{RunID: "champ", QuestionID: "q_errors", Dimension: "safety", Verdict: "no", Explanation: "ignored filesystem write error"},
		{RunID: "cand", QuestionID: "q_logs", Dimension: "observability", Verdict: "no", Explanation: "silent failure emits no structured log"},
		{RunID: "champ", QuestionID: "q_logs", Dimension: "observability", Verdict: "no", Explanation: "silent failure emits no structured log"},
		// STABLE PASS (both runs yes).
		{RunID: "cand", QuestionID: "q_pkg", Dimension: "style", Verdict: "yes"},
		{RunID: "champ", QuestionID: "q_pkg", Dimension: "style", Verdict: "yes"},
	}
	for _, v := range verdicts {
		if err := store.UpsertBinaryVerdict(ctx, v); err != nil {
			t.Fatalf("UpsertBinaryVerdict %s/%s: %v", v.RunID, v.QuestionID, err)
		}
	}
}

type binaryLessonsResult struct {
	Template      string                  `json:"template"`
	Applied       bool                    `json:"applied"`
	RunID         string                  `json:"run_id"`
	EventsWritten int                     `json:"events_written"`
	Lessons       []skillopt.BinaryLesson `json:"lessons"`
}

// TestSkillOptBinaryLessonsPreviewWritesNothing proves the default (no --apply)
// derives lessons but persists zero ranked feedback events.
func TestSkillOptBinaryLessonsPreviewWritesNothing(t *testing.T) {
	home := t.TempDir()
	seedBinaryLessonsHome(t, home)
	setPath := writeBinaryFixture(t, home, "set.yaml", binaryLessonsSetYAML)

	var out, errBuf bytes.Buffer
	code := Run([]string{"skillopt", "binary", "lessons", "--home", home, "--template", "planner", "--set", setPath, "--json"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("lessons preview exit=%d stderr=%s", code, errBuf.String())
	}
	var res binaryLessonsResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v (%s)", err, out.String())
	}
	if res.Applied {
		t.Fatal("preview must not report applied")
	}
	// 1 flip + 3 stable fails + 1 stable pass = 5 lessons.
	if len(res.Lessons) != 5 {
		t.Fatalf("lessons = %d, want 5: %+v", len(res.Lessons), res.Lessons)
	}
	kinds := map[string]int{}
	for _, l := range res.Lessons {
		kinds[l.Kind]++
	}
	if kinds[skillopt.BinaryLessonFlip] != 1 || kinds[skillopt.BinaryLessonStableNo] != 3 || kinds[skillopt.BinaryLessonStableYes] != 1 {
		t.Fatalf("kind counts = %v, want 1 flip / 3 stable_no / 1 stable_yes", kinds)
	}
	// The flip lesson recovered the question wording via --set.
	for _, l := range res.Lessons {
		if l.QuestionID == "q_tests" && !strings.Contains(l.Trait, "parser") {
			t.Fatalf("flip trait missing question wording/explanation: %q", l.Trait)
		}
	}

	// Nothing was written: the synthetic lessons run has no ranked feedback yet.
	assertNoBinaryLessonEvents(t, home, "planner")
}

// TestSkillOptBinaryLessonsApplyWritesConsumableEvents is the deterministic,
// no-LLM E2E: seed verdicts -> run the CLI with --apply -> assert the produced
// ranked feedback events are readable by ListRankedFeedbackEventsAcrossRuns AND
// that the existing rubric inducer consumes them into a frozen rubric.
func TestSkillOptBinaryLessonsApplyWritesConsumableEvents(t *testing.T) {
	home := t.TempDir()
	seedBinaryLessonsHome(t, home)
	setPath := writeBinaryFixture(t, home, "set.yaml", binaryLessonsSetYAML)

	var out, errBuf bytes.Buffer
	code := Run([]string{"skillopt", "binary", "lessons", "--home", home, "--template", "planner", "--set", setPath, "--apply", "--json"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("lessons apply exit=%d stderr=%s", code, errBuf.String())
	}
	var res binaryLessonsResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v (%s)", err, out.String())
	}
	if !res.Applied || res.EventsWritten != 5 {
		t.Fatalf("apply result = applied %v events %d, want true/5", res.Applied, res.EventsWritten)
	}

	// The events are readable via the cross-run join the optimizer/rubric use.
	events := readRankedFeedbackAcrossRuns(t, home, "planner")
	if len(events) != 5 {
		t.Fatalf("ranked feedback events = %d, want 5", len(events))
	}
	for _, e := range events {
		if e.Source != "binary-disagreement" {
			t.Fatalf("event source = %q, want binary-disagreement", e.Source)
		}
	}

	// The existing rubric inducer consumes the produced events with no contract
	// change and freezes a multi-metric rubric grounded in them.
	rubric, report, err := skillopt.InduceRubric("planner", events, skillopt.RubricInduceOptions{HoldoutFraction: 0})
	if err != nil {
		t.Fatalf("InduceRubric over binary-disagreement events: %v", err)
	}
	if report.UsableEvents != 5 {
		t.Fatalf("rubric usable events = %d, want 5", report.UsableEvents)
	}
	if len(rubric.Metrics) < 2 {
		t.Fatalf("rubric metrics = %d, want >= 2 (themes must separate)", len(rubric.Metrics))
	}
	// Provenance: at least one metric cites a binary-lessons event id.
	cited := false
	for _, m := range rubric.Metrics {
		for _, id := range m.SourceEventIDs {
			if strings.HasPrefix(id, "binary-lessons:planner:") {
				cited = true
			}
		}
	}
	if !cited {
		t.Fatalf("no metric cited a binary-lessons source event id: %+v", rubric.Metrics)
	}

	// Idempotent re-apply: event count stays at 5 (upsert in place).
	out.Reset()
	errBuf.Reset()
	code = Run([]string{"skillopt", "binary", "lessons", "--home", home, "--template", "planner", "--set", setPath, "--apply", "--json"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("re-apply exit=%d stderr=%s", code, errBuf.String())
	}
	if again := readRankedFeedbackAcrossRuns(t, home, "planner"); len(again) != 5 {
		t.Fatalf("re-apply events = %d, want stable 5 (idempotent)", len(again))
	}
}

func assertNoBinaryLessonEvents(t *testing.T, home, template string) {
	t.Helper()
	if events := readRankedFeedbackAcrossRuns(t, home, template); len(events) != 0 {
		t.Fatalf("expected zero ranked feedback events, found %d", len(events))
	}
}

func readRankedFeedbackAcrossRuns(t *testing.T, home, template string) []db.RankedFeedbackEventWithTemplate {
	t.Helper()
	paths := config.PathsForHome(home)
	store, err := db.Open(paths.Database)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	events, err := store.ListRankedFeedbackEventsAcrossRuns(context.Background(), template)
	if err != nil {
		t.Fatalf("ListRankedFeedbackEventsAcrossRuns: %v", err)
	}
	return events
}
