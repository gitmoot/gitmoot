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
// silent-drop behaviour too. Requiring the panic pins that the constructor DECIDED.
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

// TestWithGateMissDoesNotPanic pins the DAEMON-SAFETY property, not merely the refusal:
// the merge gate runs inside PollOnce, which has no recover(), so a panic here would
// terminate an unattended daemon instead of rejecting one merge decision. A caller defect
// must surface as a refused decision, never as an outage.
func TestWithGateMissDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("an all-blank miss PANICKED (%v); the daemon's PollOnce has no recover()", r)
		}
	}()
	if _, err := GateMissReason("", "", ""); err == nil {
		t.Fatal("expected a refusal error")
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
