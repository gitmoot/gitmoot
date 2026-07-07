package skillopt

import (
	"strings"
	"testing"
)

type flatRow = struct {
	QuestionID  string
	Dimension   string
	RunID       string
	VersionID   string
	Verdict     string
	Explanation string
}

func TestDeriveBinaryLessonsClassifiesFlipStableFailAndPass(t *testing.T) {
	rows := []flatRow{
		// q-flip: yes in candidate, no in champion -> flip.
		{QuestionID: "q-flip", Dimension: "correctness", RunID: "cand", VersionID: "v2", Verdict: "yes", Explanation: "candidate handles it"},
		{QuestionID: "q-flip", Dimension: "correctness", RunID: "champ", VersionID: "v1", Verdict: "no", Explanation: "champion missed the edge case"},
		// q-fail: no in every run -> stable failure.
		{QuestionID: "q-fail", Dimension: "style", RunID: "cand", VersionID: "v2", Verdict: "no", Explanation: "no docstring"},
		{QuestionID: "q-fail", Dimension: "style", RunID: "champ", VersionID: "v1", Verdict: "no", Explanation: "no docstring"},
		// q-pass: yes in every run -> stable pass (useful trait).
		{QuestionID: "q-pass", Dimension: "safety", RunID: "cand", VersionID: "v2", Verdict: "yes"},
		{QuestionID: "q-pass", Dimension: "safety", RunID: "champ", VersionID: "v1", Verdict: "yes"},
	}
	groups := GroupBinaryObservations(rows)
	lessons := DeriveBinaryLessons(groups, nil, BinaryLessonsOptions{})
	if len(lessons) != 3 {
		t.Fatalf("lessons = %d, want 3: %+v", len(lessons), lessons)
	}
	byID := map[string]BinaryLesson{}
	for _, l := range lessons {
		byID[l.QuestionID] = l
	}

	flip := byID["q-flip"]
	if flip.Kind != BinaryLessonFlip || flip.Sign != AspectSignNegative {
		t.Fatalf("q-flip kind/sign = %s/%s", flip.Kind, flip.Sign)
	}
	if flip.YesCount != 1 || flip.NoCount != 1 || flip.Runs != 2 {
		t.Fatalf("q-flip counts = yes %d no %d runs %d", flip.YesCount, flip.NoCount, flip.Runs)
	}
	if !strings.Contains(flip.Text, "flips") || !strings.Contains(flip.Text, "champion missed the edge case") {
		t.Fatalf("q-flip text missing signal: %q", flip.Text)
	}
	if len(flip.Versions) != 2 {
		t.Fatalf("q-flip versions = %v, want 2 distinct", flip.Versions)
	}

	fail := byID["q-fail"]
	if fail.Kind != BinaryLessonStableNo || fail.Sign != AspectSignNegative {
		t.Fatalf("q-fail kind/sign = %s/%s", fail.Kind, fail.Sign)
	}
	if !strings.Contains(fail.Text, "Consistently fails") {
		t.Fatalf("q-fail text = %q", fail.Text)
	}
	// Duplicate explanations dedupe to a single evidence entry.
	if strings.Count(fail.Text, "no docstring") != 1 {
		t.Fatalf("q-fail evidence not deduped: %q", fail.Text)
	}

	pass := byID["q-pass"]
	if pass.Kind != BinaryLessonStableYes || pass.Sign != AspectSignPositive {
		t.Fatalf("q-pass kind/sign = %s/%s", pass.Kind, pass.Sign)
	}
	if !strings.Contains(pass.Text, "preserve this behavior") {
		t.Fatalf("q-pass text = %q", pass.Text)
	}
}

func TestDeriveBinaryLessonsNoPassesOmitsStableYes(t *testing.T) {
	rows := []flatRow{
		{QuestionID: "q-pass", Dimension: "safety", RunID: "cand", Verdict: "yes"},
		{QuestionID: "q-pass", Dimension: "safety", RunID: "champ", Verdict: "yes"},
		{QuestionID: "q-fail", Dimension: "style", RunID: "cand", Verdict: "no"},
	}
	groups := GroupBinaryObservations(rows)
	lessons := DeriveBinaryLessons(groups, nil, BinaryLessonsOptions{}.WithIncludeStableYes(false))
	if len(lessons) != 1 || lessons[0].QuestionID != "q-fail" {
		t.Fatalf("with --no-passes want only q-fail, got %+v", lessons)
	}
}

func TestDeriveBinaryLessonsUsesQuestionText(t *testing.T) {
	rows := []flatRow{
		{QuestionID: "q1", Dimension: "d", RunID: "r1", Verdict: "no"},
	}
	groups := GroupBinaryObservations(rows)
	lessons := DeriveBinaryLessons(groups, map[string]string{"q1": "Does the PR include tests?"}, BinaryLessonsOptions{})
	if len(lessons) != 1 {
		t.Fatalf("lessons = %d", len(lessons))
	}
	if lessons[0].QuestionText != "Does the PR include tests?" {
		t.Fatalf("question text not carried: %q", lessons[0].QuestionText)
	}
	if !strings.Contains(lessons[0].Text, "Does the PR include tests?") {
		t.Fatalf("lesson text should use question wording: %q", lessons[0].Text)
	}
	// The projected Trait is content-focused (question wording), free of the
	// framing boilerplate present in Text, so token clustering separates themes.
	if lessons[0].Trait != "Does the PR include tests?" {
		t.Fatalf("trait = %q, want the bare question wording", lessons[0].Trait)
	}
}

func TestDeriveBinaryLessonsAllPassNoLessonsWhenNoPassesAndOnlyYes(t *testing.T) {
	rows := []flatRow{
		{QuestionID: "q-pass", Dimension: "safety", RunID: "cand", Verdict: "yes"},
	}
	groups := GroupBinaryObservations(rows)
	lessons := DeriveBinaryLessons(groups, nil, BinaryLessonsOptions{}.WithIncludeStableYes(false))
	if len(lessons) != 0 {
		t.Fatalf("all-pass with --no-passes should yield no lessons, got %+v", lessons)
	}
}

func TestQuestionTextIndex(t *testing.T) {
	set := BinaryQuestionSet{
		Version: 1,
		Dimensions: []BinaryDimension{
			{Name: "d", Questions: []BinaryQuestion{{ID: "a", Text: "Text A"}, {ID: "b", Text: "Text B"}}},
		},
	}
	idx := QuestionTextIndex(set)
	if idx["a"] != "Text A" || idx["b"] != "Text B" {
		t.Fatalf("index = %v", idx)
	}
}

func TestGroupBinaryObservationsDeterministicOrder(t *testing.T) {
	rows := []flatRow{
		{QuestionID: "z", Dimension: "b", RunID: "r1", Verdict: "no"},
		{QuestionID: "a", Dimension: "b", RunID: "r1", Verdict: "yes"},
		{QuestionID: "m", Dimension: "a", RunID: "r1", Verdict: "no"},
	}
	groups := GroupBinaryObservations(rows)
	// Sorted by (dimension, question_id): a/m, then b/a, then b/z.
	want := []string{"m", "a", "z"}
	for i, g := range groups {
		if g.QuestionID != want[i] {
			t.Fatalf("group %d = %s, want %s", i, g.QuestionID, want[i])
		}
	}
}
