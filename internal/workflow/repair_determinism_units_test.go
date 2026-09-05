package workflow

import "testing"

// #1507 unit arms that reference the fix's own identifiers. They are kept apart
// from the behavioural arms so those stay compilable against a tree without this
// fix and can serve as pre-fix discriminators.

func TestRetryCannotDiffer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previous string
		next     string
		want     bool
	}{
		{"identical non-empty", "same", "same", true},
		{"different", "a", "b", false},
		{"empty previous is not a repeat", "", "", false},
		{"empty next differs", "a", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RetryCannotDiffer(tc.previous, tc.next); got != tc.want {
				t.Fatalf("RetryCannotDiffer(%q, %q) = %v, want %v", tc.previous, tc.next, got, tc.want)
			}
		})
	}
}

// TestDelimiterCloseRefusesWhatItCannotJustify keeps the close narrow: it must
// not invent content, and it must not turn a contract violation into a verdict.
func TestDelimiterCloseRefusesWhatItCannotJustify(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
	}{
		{"ends inside a string literal", `{"gitmoot_result":{"decision":"approved","summary":"cut`},
		{"closing a truncation still violates the review contract", `{"gitmoot_result":{"decision":"changes_requested","summary":"no severity","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}`},
		{"more closers than openers", `{"gitmoot_result":{"decision":"approved"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, repair, err := extractAgentResultForActionAllowingRepair(tc.output, "review")
			if err == nil {
				t.Fatalf("extractAgentResultForActionAllowingRepair accepted %q with repair %q", tc.output, repair)
			}
			if repair != "" {
				t.Fatalf("repair = %q, want empty on a refusal", repair)
			}
		})
	}
}
