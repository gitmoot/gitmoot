package workflow

import (
	"strings"
	"testing"
)

// TestWithGateMissRefusesAnAllBlankMiss pins P1b's REASON, not merely its outcome: the
// constructor must REFUSE a caller defect rather than silently returning a zero value that
// renders to "" and reaches a durable escalation as a header-only note.
//
// Asserting only "the result is zero" would be an outcome assertion satisfied by the old
// silent-drop behaviour too. Requiring a NON-NIL ERROR that names the caller defect is what
// pins that the constructor DECIDED.
//
// This test also carries the DAEMON-SAFETY axis that a separate TestWithGateMissDoesNotPanic
// used to claim. That test was deleted as one guard wearing two names: restoring the
// production panic killed BOTH of them, so no mutant ever told them apart. The axis survives
// here by construction -- this test CALLS the constructor and asserts on its returned values,
// which a panicking implementation cannot satisfy.
//
// What the deleted test could NOT cover, and what actually protects the daemon, is a property
// of the CALLER rather than of this constructor: the refusal must be reported and polling must
// continue. That is asserted where it lives, in the supervisor guards, and it is a genuinely
// independent observable because a caller that silently discards the error passes every
// assertion in this file.
func TestWithGateMissRefusesAnAllBlankMiss(t *testing.T) {
	reason, err := GateMissReason("   ", "\t", "head123")
	if err == nil {
		t.Fatal("an all-blank gate/cause was accepted; the constructor must refuse a caller defect")
	}
	if !strings.Contains(err.Error(), "caller defect") {
		t.Fatalf("error = %v, want a message naming the caller defect", err)
	}
	// And it must not hand back a half-built value the caller could still deliver.
	if !reason.IsZero() {
		t.Fatalf("refused construction still returned a usable reason: %+v", reason)
	}
}

// TestGateMissKindIsTheEscalationSignal pins P1a: the KIND must be what distinguishes an
// operator instruction from a status note, because the escalation path now reads it. Before
// this round the same fact was carried twice -- a bool and the kind -- and the two had already
// drifted apart at the head-SHA-missing site.
func TestGateMissKindIsTheEscalationSignal(t *testing.T) {
	gateMiss, err := GateMissReason("merge gate", "pull request head SHA is missing", "")
	if err != nil {
		t.Fatalf("well-formed gate miss refused: %v", err)
	}
	if !gateMiss.IsGateMiss() {
		t.Fatal("a gate miss does not report IsGateMiss; the escalation path would skip it")
	}
	if PlainReason("pull request is draft").IsGateMiss() {
		t.Fatal("a status note reports IsGateMiss; a draft PR would escalate as an operator instruction")
	}
	if !PlainReason("pull request is draft").IsZero() == false {
		t.Fatal("unexpected zero-ness for a non-empty plain reason")
	}
}

// TestRenderPinsEveryGateAndTheCombinedPath closes P2: the previous rendered pin covered ONLY
// the coordinator-bridge review reason, so a conditional append on the CI-gate branch changed
// the durable operator instruction while the byte pin, the cause-naming test and the CI test
// all stayed green.
//
// Pin the rendering of EACH gate and of the COMBINED review+CI path, because the combined
// sentence is assembled by the same Render that produces each single one -- a pin on one gate
// says nothing about the join.
func TestRenderPinsEveryGateAndTheCombinedPath(t *testing.T) {
	review, err := GateMissReason("review gate", "reviewcause", "head123")
	if err != nil {
		t.Fatalf("review gate miss refused: %v", err)
	}
	if got, want := review.Render(), "review gate: reviewcause for head head123"; got != want {
		t.Fatalf("review render = %q, want %q", got, want)
	}

	ci, err := GateMissReason("CI gate", "cicause", "head123")
	if err != nil {
		t.Fatalf("CI gate miss refused: %v", err)
	}
	if got, want := ci.Render(), "CI gate: cicause for head head123"; got != want {
		t.Fatalf("CI render = %q, want %q", got, want)
	}

	combined, err := review.WithGateMiss("CI gate", "cicause", "head123")
	if err != nil {
		t.Fatalf("combined miss refused: %v", err)
	}
	want := "review gate: reviewcause for head head123; CI gate: cicause for head head123"
	if got := combined.Render(); got != want {
		t.Fatalf("combined render = %q, want %q", got, want)
	}
}
