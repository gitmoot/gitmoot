package workflow

import (
	"encoding/json"
	"testing"
)

func TestClassifyRiskDefaultRoutine(t *testing.T) {
	got := ClassifyRisk(nil, "", "", nil, []string{"README.md", "internal/report/report.go"})
	if got.Tier != RiskTierRoutine {
		t.Fatalf("tier = %q, want routine", got.Tier)
	}
	if got.Source != "default" {
		t.Fatalf("source = %q, want default", got.Source)
	}
}

func TestClassifyRiskHighByPath(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"auth", "internal/auth/session.go"},
		{"security", "pkg/security/tls.go"},
		{"payment", "billing/payment/charge.go"},
		{"migration", "db/migration/0007_add_col.sql"},
		{"gomod", "go.mod"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyRisk(nil, "", "", nil, []string{"docs/x.md", tc.path})
			if got.Tier != RiskTierHigh {
				t.Fatalf("path %q tier = %q, want high", tc.path, got.Tier)
			}
			if got.Source != "path" {
				t.Fatalf("path %q source = %q, want path", tc.path, got.Source)
			}
		})
	}
}

func TestClassifyRiskLabelWinsOverPath(t *testing.T) {
	// A routine label de-escalates even when a high-risk path is present.
	got := ClassifyRisk(nil, "", "", []string{"risk:routine"}, []string{"internal/auth/session.go"})
	if got.Tier != RiskTierRoutine {
		t.Fatalf("tier = %q, want routine (label wins over path)", got.Tier)
	}
	if got.Source != "label" {
		t.Fatalf("source = %q, want label", got.Source)
	}
}

func TestClassifyRiskHighLabelEscalatesCleanPaths(t *testing.T) {
	got := ClassifyRisk(nil, "", "", []string{"risk:high"}, []string{"README.md"})
	if got.Tier != RiskTierHigh || got.Source != "label" {
		t.Fatalf("classification = %+v, want high/label", got)
	}
}

func TestClassifyRiskHighLabelBeatsRoutineLabel(t *testing.T) {
	// Safety-biased tie-break: both labels present -> high wins.
	got := ClassifyRisk(nil, "", "", []string{"risk:routine", "risk:high"}, nil)
	if got.Tier != RiskTierHigh {
		t.Fatalf("tier = %q, want high on conflicting labels", got.Tier)
	}
}

func TestClassifyRiskCustomLabelsAndPaths(t *testing.T) {
	got := ClassifyRisk([]string{"cmd/**"}, "sev:1", "sev:routine", []string{"sev:1"}, nil)
	if got.Tier != RiskTierHigh || got.Source != "label" {
		t.Fatalf("custom high label: %+v", got)
	}
	got = ClassifyRisk([]string{"cmd/**"}, "sev:1", "sev:routine", nil, []string{"cmd/root/main.go"})
	if got.Tier != RiskTierHigh || got.Source != "path" {
		t.Fatalf("custom path glob: %+v", got)
	}
	// The built-in defaults must NOT apply once a custom list is provided.
	got = ClassifyRisk([]string{"cmd/**"}, "sev:1", "sev:routine", nil, []string{"internal/auth/x.go"})
	if got.Tier != RiskTierRoutine {
		t.Fatalf("custom list must not match built-in auth glob: %+v", got)
	}
}

func TestClassifyRiskLabelCaseInsensitive(t *testing.T) {
	got := ClassifyRisk(nil, "", "", []string{"RISK:HIGH"}, nil)
	if got.Tier != RiskTierHigh {
		t.Fatalf("label match must be case-insensitive: %+v", got)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/auth/**", "internal/auth/session.go", true},
		{"**/auth/**", "a/b/c/auth/d/e.go", true},
		{"**/auth/**", "internal/authz/x.go", false},
		{"go.mod", "go.mod", true},
		{"go.mod", "internal/go.mod", false},
		{"cmd/**", "cmd/root/main.go", true},
		{"cmd/**", "internal/cmd/x.go", false},
		{"*.sql", "0001.sql", true},
		{"*.sql", "db/0001.sql", false},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.path); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestSynthesizeLensDecisionCriticalBlocks(t *testing.T) {
	findings := []LensFinding{
		{Lens: "security", Refuted: true, Severity: "critical", Confidence: 0.9, Evidence: "auth bypass"},
	}
	if got := SynthesizeLensDecision(findings); got != "blocked" {
		t.Fatalf("decision = %q, want blocked", got)
	}
}

func TestSynthesizeLensDecisionNonCriticalApproves(t *testing.T) {
	findings := []LensFinding{
		{Lens: "correctness", Refuted: true, Severity: "medium", Confidence: 0.5},
		{Lens: "security", Refuted: false, Severity: "critical"}, // not refuted -> ignored
	}
	if got := SynthesizeLensDecision(findings); got != "approved" {
		t.Fatalf("decision = %q, want approved", got)
	}
}

func TestSynthesizeLensDecisionEmptyApproves(t *testing.T) {
	if got := SynthesizeLensDecision(nil); got != "approved" {
		t.Fatalf("decision = %q, want approved", got)
	}
}

func TestParseLensFindingsSkipsMalformed(t *testing.T) {
	raw := []json.RawMessage{
		json.RawMessage(`{"lens":"security","refuted":true,"severity":"critical","confidence":0.8,"evidence":"x"}`),
		json.RawMessage(`"not an object"`),
		json.RawMessage(`{"lens":"correctness","refuted":false,"severity":"low"}`),
	}
	got := ParseLensFindings(raw)
	if len(got) != 2 {
		t.Fatalf("parsed %d findings, want 2 (malformed skipped): %+v", len(got), got)
	}
	if got[0].Lens != "security" || !got[0].Refuted || got[0].Severity != "critical" {
		t.Fatalf("first finding = %+v", got[0])
	}
}

func TestHighRiskLensDelegations(t *testing.T) {
	event := PullRequestEvent{PullRequest: 42, TaskID: "task-x", TaskTitle: "T"}

	two := highRiskLensDelegations([]string{"audit"}, event)
	if len(two) != 2 {
		t.Fatalf("single reviewer -> %d lenses, want 2", len(two))
	}
	for _, d := range two {
		if d.Agent != "audit" {
			t.Fatalf("lens %q agent = %q, want audit", d.ID, d.Agent)
		}
		if d.Action != "review" {
			t.Fatalf("lens %q action = %q, want review", d.ID, d.Action)
		}
		if d.SynthesisRule != "quorum" || d.Quorum != 2 {
			t.Fatalf("lens %q synthesis = %q quorum = %d, want quorum/2", d.ID, d.SynthesisRule, d.Quorum)
		}
	}

	three := highRiskLensDelegations([]string{"a", "b", "c"}, event)
	if len(three) != 3 {
		t.Fatalf("3 reviewers -> %d lenses, want 3 (regression added)", len(three))
	}
	if three[0].Agent != "a" || three[1].Agent != "b" || three[2].Agent != "c" {
		t.Fatalf("round-robin agents = %q/%q/%q", three[0].Agent, three[1].Agent, three[2].Agent)
	}
	for _, d := range three {
		if d.Quorum != 3 {
			t.Fatalf("3-lens quorum = %d, want 3", d.Quorum)
		}
	}

	if got := highRiskLensDelegations(nil, event); got != nil {
		t.Fatalf("no reviewers -> %+v, want nil", got)
	}
}
