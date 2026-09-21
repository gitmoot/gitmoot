package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func findingJSON(t *testing.T, fields map[string]string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal finding: %v", err)
	}
	return raw
}

// The property: a verdict may only block a merge on a defect it can point at.
//
// Measured premise (2026-09-21 review-model comparison, evidence in
// /root/fleet-tools/state/review-model-eval-2026-09-21): one reviewer declared
// P0/P1 on 79 of 157 September reviews while attaching a locator to 1% of its
// findings, and a blind judge rating those findings from their text alone
// downgraded 17 of 18 sampled P0/P1s, 15 of them to "not a defect". Because
// blocking_severity is P2, each of those unlocatable claims stopped a merge.
//
// The table is written against effectiveReviewDecisionForPayload rather than the
// bare helper because that function is the single authority every consumer reads
// — merge gate, proof, required-reviewer credit, delegation quorum. A rule that
// held only in the helper would not bind any of them.
func TestBlockingVerdictFoldsOnlyWhenNoBlockingFindingSaysWhere(t *testing.T) {
	cases := []struct {
		name     string
		severity string
		findings []json.RawMessage
		want     string
		reason   string
	}{
		{
			name:     "blocking finding with a locator still blocks",
			severity: "P1",
			findings: []json.RawMessage{findingJSON(t, map[string]string{
				"severity": "P1", "title": "nil deref on empty worktree path",
				"locator": "internal/cli/agent_dispatch.go:1442",
			})},
			want:   "changes_requested",
			reason: "",
		},
		{
			name:     "blocking finding with cited evidence but no locator still blocks",
			severity: "P1",
			findings: []json.RawMessage{findingJSON(t, map[string]string{
				"severity": "P1", "title": "race on the shared checkout",
				"evidence": "go test -race ./internal/cli/ fails at TestDispatchRace, 3 runs of 3",
			})},
			want:   "changes_requested",
			reason: "",
		},
		{
			name:     "one located finding among unlocated ones still blocks",
			severity: "P1",
			findings: []json.RawMessage{
				findingJSON(t, map[string]string{"severity": "P1", "title": "unanchored cleanup concern"}),
				findingJSON(t, map[string]string{"severity": "P1", "title": "real one", "locator": "internal/db/store.go:88"}),
			},
			want:   "changes_requested",
			reason: "",
		},
		{
			name:     "every blocking finding unlocated folds to advisory",
			severity: "P1",
			findings: []json.RawMessage{
				findingJSON(t, map[string]string{"severity": "P1", "title": "sibling abort cleanups remain unanchored"}),
				findingJSON(t, map[string]string{"severity": "P2", "title": "concurrent child advances can open two rounds"}),
			},
			want:   "approved",
			reason: reviewFoldUnlocated,
		},
		{
			name:     "finding without its own severity inherits the verdict severity",
			severity: "P1",
			findings: []json.RawMessage{findingJSON(t, map[string]string{
				"title": "the headless safety rationale ignores the fallback",
			})},
			want:   "approved",
			reason: reviewFoldUnlocated,
		},
		{
			name:     "no findings at all keeps blocking",
			severity: "P0",
			findings: nil,
			want:     "changes_requested",
			reason:   "",
		},
		{
			name:     "unlocated findings BELOW the bar cannot fold a blocking verdict on their own",
			severity: "P1",
			findings: []json.RawMessage{
				findingJSON(t, map[string]string{"severity": "P3", "title": "naming nit"}),
			},
			want:   "changes_requested",
			reason: "",
		},
		{
			name:     "below-threshold verdict still folds for the original reason",
			severity: "P3",
			findings: []json.RawMessage{findingJSON(t, map[string]string{
				"severity": "P3", "title": "docs typo", "locator": "README.md:4",
			})},
			want:   "approved",
			reason: reviewFoldBelowThreshold,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := JobPayload{
				Repo: "gitmoot/gitmoot",
				Result: &AgentResult{
					Decision: "changes_requested",
					Severity: tc.severity,
					Findings: tc.findings,
				},
			}
			if got := effectiveReviewDecisionForPayload(payload, "P2"); got != tc.want {
				t.Fatalf("effective decision = %q, want %q", got, tc.want)
			}
			if got := reviewFoldReason(payload.Result, "P2"); got != tc.reason {
				t.Fatalf("fold reason = %q, want %q", got, tc.reason)
			}
		})
	}
}

// An unreadable finding is not evidence that the reviewer pointed nowhere, so
// the gate must keep blocking rather than fold on a decode failure. Without this
// the rule would turn a malformed reviewer into a way to unblock a head.
func TestUndecodableFindingKeepsTheBlock(t *testing.T) {
	payload := JobPayload{
		Repo: "gitmoot/gitmoot",
		Result: &AgentResult{
			Decision: "changes_requested",
			Severity: "P1",
			Findings: []json.RawMessage{json.RawMessage(`"a bare string, not an object"`)},
		},
	}
	if got := effectiveReviewDecisionForPayload(payload, "P2"); got != "changes_requested" {
		t.Fatalf("effective decision = %q, want changes_requested", got)
	}
}

// A pipeline-sender review is report-only: gitmoot may not reinterpret its
// verdict in either direction, so the locator rule must not reach it either.
func TestPipelineReviewIsUntouchedByTheLocatorGate(t *testing.T) {
	payload := JobPayload{
		Repo:   "gitmoot/gitmoot",
		Sender: PipelineJobSender,
		Result: &AgentResult{
			Decision: "changes_requested",
			Severity: "P1",
			Findings: []json.RawMessage{findingJSON(t, map[string]string{"severity": "P1", "title": "no locator here"})},
		},
	}
	if got := effectiveReviewDecisionForPayload(payload, "P2"); got != "changes_requested" {
		t.Fatalf("pipeline verdict = %q, want it returned raw", got)
	}
}

// The fold event's message must name the reason it folded. Both recording sites
// build it from reviewFoldMessage, so this pins the text they will carry.
func TestFoldMessageNamesTheReason(t *testing.T) {
	unlocated := reviewFoldMessage(reviewFoldUnlocated, "P1", "P2")
	if want := "no finding at that severity names a locator or cites evidence"; !strings.Contains(unlocated, want) {
		t.Fatalf("unlocated message = %q, want it to contain %q", unlocated, want)
	}
	below := reviewFoldMessage(reviewFoldBelowThreshold, "P3", "P2")
	if want := "is below repository blocking severity"; !strings.Contains(below, want) {
		t.Fatalf("below-threshold message = %q, want it to contain %q", below, want)
	}
}
