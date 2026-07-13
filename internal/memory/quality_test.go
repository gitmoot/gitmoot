package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func qualityFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "quality", name+".md"))
	if err != nil {
		t.Fatalf("read quality fixture %s: %v", name, err)
	}
	return string(data)
}

func TestScoreQualityRiskLiveFixtures(t *testing.T) {
	tests := []struct {
		name       string
		provenance string
		junk       bool
	}{
		{name: "392", provenance: "workflow:build-864#11", junk: true},
		{name: "396", provenance: "workflow:build-793#24", junk: true},
		{name: "171", provenance: "gitmoot-mechanical", junk: true},
		{name: "401", provenance: "ingest:keycard-transcript-wave-shipped.md", junk: true},
		{name: "280", provenance: "ingest:pipeline-primitive-shipped.md", junk: true},
		{name: "memory-index-165", provenance: "ingest:MEMORY.md", junk: true},
		{name: "85", provenance: "ingest:issue-226-planner-download-shipped.md", junk: true},
		{name: "380", provenance: "groom-split:235", junk: true},
		{name: "388", provenance: "groom-split:244", junk: true},
		{name: "390", provenance: "workflow:demo-843#2"},
		{name: "204", provenance: "ingest:headless-visual-verification.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := GroomCandidate{Content: qualityFixture(t, tc.name), Provenance: tc.provenance}
			signals := qualitySignals(candidate, false)
			score := ScoreQualityRisk(signals)
			if tc.junk && score < GroomQualityRiskThreshold {
				t.Fatalf("junk score = %d, signals=%+v; want >= %d", score, signals, GroomQualityRiskThreshold)
			}
			if !tc.junk && score >= GroomQualityRiskThreshold {
				t.Fatalf("good fact score = %d, signals=%+v; want < %d", score, signals, GroomQualityRiskThreshold)
			}
		})
	}
}

func TestDetectGroomQualityCandidatesAgeOrderAndLessonProtection(t *testing.T) {
	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-2 * time.Hour).Format(time.RFC3339)
	cands := []GroomCandidate{
		{ID: 390, Content: qualityFixture(t, "390"), Provenance: "workflow:demo-843#2", FirstConfirmedAt: old},
		{ID: 392, Content: qualityFixture(t, "392"), Provenance: "workflow:build-864#11", FirstConfirmedAt: old},
		{ID: 171, Content: qualityFixture(t, "171"), Provenance: "gitmoot-mechanical", FirstConfirmedAt: old},
		{ID: 204, Content: qualityFixture(t, "204"), Provenance: "ingest:headless-visual-verification.md", FirstConfirmedAt: old},
		{ID: 396, Content: qualityFixture(t, "396"), Provenance: "workflow:build-793#24", FirstConfirmedAt: recent},
	}
	got := DetectGroomQualityCandidates(cands, now, 24*time.Hour)
	if len(got) != 2 {
		t.Fatalf("candidates = %+v, want old junk 171 and 392 only", got)
	}
	if got[0].ID != 171 || got[1].ID != 392 {
		t.Fatalf("risk order ids = [%d %d], want [171 392]", got[0].ID, got[1].ID)
	}
	for _, candidate := range got {
		if candidate.ContentHash != GroomContentHash(candidate.Content) || len(candidate.SignalFamilies) < 2 {
			t.Fatalf("candidate metadata = %+v", candidate)
		}
	}
}

func TestQualityWriteTimeShapes(t *testing.T) {
	if !IsShippingStatus(qualityFixture(t, "392")) || !IsShippingStatus(qualityFixture(t, "396")) {
		t.Fatal("live workflow shipping statuses must trip the write-time gate")
	}
	if IsShippingStatus(qualityFixture(t, "390")) {
		t.Fatal("durable fan-out lesson must not look like a shipping status")
	}
	if QualityHasSpecific(qualityFixture(t, "171")) {
		t.Fatal("content-free mechanical observation unexpectedly has a specific")
	}
	if !QualityHasSpecific("Some ask jobs in this repository changed after PR #42.") {
		t.Fatal("a PR-bearing mechanical observation must pass the substantiveness gate")
	}
	if !IsMemoryIndex(qualityFixture(t, "memory-index-165")) {
		t.Fatal("live MEMORY.md link list must be recognized as an index")
	}
	if IsMemoryIndex("# Runbook\n\nSee [deploy](deploy.md) before changing the service.\n") {
		t.Fatal("ordinary prose with one Markdown link must not be skipped")
	}
}

func TestScoreQualityRiskWeights(t *testing.T) {
	all := QualitySignals{
		TransientStatus: true, Fragment: true, GenericVacuous: true,
		NearDuplicate: true, AutomatedProvenance: true, Short: true,
		LessonShaped: true, RecentlyRetrieved: true,
	}
	if got := ScoreQualityRisk(all); got != 8 {
		t.Fatalf("all-signal score = %d, want 8", got)
	}
}
