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
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("an all-blank gate/cause was accepted; the constructor must refuse a caller defect")
		}
		if msg, ok := recovered.(string); !ok || !strings.Contains(msg, "caller defect") {
			t.Fatalf("panic = %v, want a message naming the caller defect", recovered)
		}
	}()
	_ = GateMissReason("   ", "\t", "head123")
}

// TestGateMissKindIsTheEscalationSignal pins P1a: the KIND must be what distinguishes an
// operator instruction from a status note, because the escalation path now reads it. Before
// this round the same fact was carried twice -- a bool and the kind -- and the two had already
// drifted apart at the head-SHA-missing site.
func TestGateMissKindIsTheEscalationSignal(t *testing.T) {
	if !GateMissReason("merge gate", "pull request head SHA is missing", "").IsGateMiss() {
		t.Fatal("a gate miss does not report IsGateMiss; the escalation path would skip it")
	}
	if PlainReason("pull request is draft").IsGateMiss() {
		t.Fatal("a status note reports IsGateMiss; a draft PR would escalate as an operator instruction")
	}
	if !PlainReason("pull request is draft").IsZero() == false {
		t.Fatal("unexpected zero-ness for a non-empty plain reason")
	}
}
