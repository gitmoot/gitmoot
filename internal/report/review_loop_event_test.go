package report

import "testing"

func TestReviewLoopDetectedIsExplicitPriorityDiagnostic(t *testing.T) {
	const kind = "review_loop_detected"
	if !isPriorityDiagnosticEvent(kind) {
		t.Fatal("review_loop_detected is not a priority diagnostic")
	}
	if !isDiagnosticEvent(kind) {
		t.Fatal("review_loop_detected is not a diagnostic")
	}
}
