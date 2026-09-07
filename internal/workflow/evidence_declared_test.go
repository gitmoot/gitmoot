package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// #1817's acceptance criterion 2 asked for a first-class field distinguishing a
// reviewer that could not execute from one that ran the suite. That field
// already existed as `evidence`, and it already normalises an omission to
// `static_only` so silence can never read as an execution claim.
//
// What it could not do was distinguish the two REASONS a stored result says
// `static_only`. Measured over 3,431 succeeded review jobs on this host: 3,192
// (93%, 14.328 G tokens) omit the field entirely, 198 declare `executed`, 41
// declare `static_only`. So an honest "I could run nothing" and "I have never
// heard of this field" stored the same value, which is what makes any policy
// built on the field unsafe: refusing `static_only` today would refuse 93% of
// all reviews, almost none of which made the claim.
func TestEvidenceDeclaredSeparatesAnHonestStaticOnlyFromSilence(t *testing.T) {
	for _, tt := range []struct {
		name         string
		raw          string
		wantEvidence string
		wantDeclared bool
	}{
		{
			name:         "an omitted field still defaults to static_only, and is marked undeclared",
			raw:          `{"decision":"approved","summary":"looks fine"}`,
			wantEvidence: EvidenceStaticOnly,
			wantDeclared: false,
		},
		{
			name:         "an explicit static_only is the same value and IS declared",
			raw:          `{"decision":"approved","summary":"read only","evidence":"static_only"}`,
			wantEvidence: EvidenceStaticOnly,
			wantDeclared: true,
		},
		{
			name:         "executed is declared",
			raw:          `{"decision":"approved","summary":"ran it","evidence":"executed"}`,
			wantEvidence: EvidenceExecuted,
			wantDeclared: true,
		},
		{
			name:         "an empty string is silence, not a declaration",
			raw:          `{"decision":"approved","summary":"blank","evidence":""}`,
			wantEvidence: EvidenceStaticOnly,
			wantDeclared: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var result AgentResult
			if err := json.Unmarshal([]byte(tt.raw), &result); err != nil {
				t.Fatal(err)
			}
			normalizeAgentResult(&result)
			if result.Evidence != tt.wantEvidence {
				t.Errorf("evidence = %q, want %q; the safe default must not change", result.Evidence, tt.wantEvidence)
			}
			if got := EvidenceWasDeclared(result); got != tt.wantDeclared {
				t.Errorf("declared = %v, want %v", got, tt.wantDeclared)
			}
		})
	}
}

// TestEvidenceDeclaredCannotBeAssertedByTheProducer is the spoof arm, and it is
// the reason the flag is engine-owned rather than a plain input field.
//
// Provenance about a value must not come from that value's author. The same
// idiom is already used for ReviewStatusGrade, and for #1926's real-adapter
// declaration, which binds a claim to the thing it was made about rather than
// storing a bare boolean a fixture can overwrite.
func TestEvidenceDeclaredCannotBeAssertedByTheProducer(t *testing.T) {
	// A producer claiming it declared, while declaring nothing.
	var forged AgentResult
	if err := json.Unmarshal([]byte(`{"decision":"approved","summary":"trust me","evidence_declared":true}`), &forged); err != nil {
		t.Fatal(err)
	}
	normalizeAgentResult(&forged)
	if EvidenceWasDeclared(forged) {
		t.Fatalf("a producer asserted evidence_declared with no evidence and it survived normalization")
	}
	if forged.Evidence != EvidenceStaticOnly {
		t.Errorf("evidence = %q, want %q", forged.Evidence, EvidenceStaticOnly)
	}

	// And the inverse: a producer that declares executed but sends
	// evidence_declared:false does not get to hide that it made the claim.
	var denied AgentResult
	if err := json.Unmarshal([]byte(`{"decision":"approved","summary":"ran it","evidence":"executed","evidence_declared":false}`), &denied); err != nil {
		t.Fatal(err)
	}
	normalizeAgentResult(&denied)
	if !EvidenceWasDeclared(denied) {
		t.Fatalf("a producer suppressed evidence_declared while declaring executed")
	}
}

// TestEvidenceDeclaredDoesNotChangeExecutionSemantics pins the boundary this
// change deliberately does not cross. `EvidenceWasExecuted` is what every
// existing consumer asks, and a merge policy that refused static_only would
// refuse 93% of reviews on this store, so nothing here may alter that answer.
func TestEvidenceDeclaredDoesNotChangeExecutionSemantics(t *testing.T) {
	for _, raw := range []string{
		`{"decision":"approved","summary":"s"}`,
		`{"decision":"approved","summary":"s","evidence":"static_only"}`,
		`{"decision":"approved","summary":"s","evidence":"executed"}`,
	} {
		var result AgentResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			t.Fatal(err)
		}
		normalizeAgentResult(&result)
		want := strings.Contains(raw, `"evidence":"executed"`)
		if got := EvidenceWasExecuted(result); got != want {
			t.Errorf("EvidenceWasExecuted(%s) = %v, want %v", raw, got, want)
		}
	}
}
