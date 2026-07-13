package memory

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const GroomQualityRiskThreshold = 3

// QualitySignals is the deterministic evidence used by the general quality
// audit. Positive fields are independent corroborating families; lesson shape
// and recent retrieval are protective evidence.
type QualitySignals struct {
	TransientStatus     bool `json:"transient_status"`
	Fragment            bool `json:"fragment"`
	GenericVacuous      bool `json:"generic_vacuous"`
	NearDuplicate       bool `json:"near_duplicate"`
	AutomatedProvenance bool `json:"automated_provenance"`
	Short               bool `json:"short"`
	LessonShaped        bool `json:"lesson_shaped"`
	RecentlyRetrieved   bool `json:"recently_retrieved"`
}

// Families returns the positive signal-family names in stable order.
func (s QualitySignals) Families() []string {
	var out []string
	if s.TransientStatus {
		out = append(out, "transient_status")
	}
	if s.Fragment {
		out = append(out, "fragment")
	}
	if s.GenericVacuous {
		out = append(out, "generic_vacuous")
	}
	if s.NearDuplicate {
		out = append(out, "near_duplicate")
	}
	if s.AutomatedProvenance {
		out = append(out, "automated_provenance")
	}
	if s.Short {
		out = append(out, "short")
	}
	return out
}

// ScoreQualityRisk applies the owner-approved Hybrid-D weights.
func ScoreQualityRisk(s QualitySignals) int {
	score := 0
	if s.TransientStatus {
		score += 3
	}
	if s.Fragment {
		score += 3
	}
	if s.GenericVacuous {
		score += 3
	}
	if s.NearDuplicate {
		score += 2
	}
	if s.AutomatedProvenance {
		score++
	}
	if s.Short {
		score++
	}
	if s.LessonShaped {
		score -= 3
	}
	if s.RecentlyRetrieved {
		score -= 2
	}
	return score
}

// GroomQualityCandidate is one old-enough fact whose deterministic risk reaches
// the audit threshold. ContentHash keys the immutable LLM verdict cache.
type GroomQualityCandidate struct {
	GroomCandidate
	ContentHash    string
	Signals        QualitySignals
	Score          int
	SignalFamilies []string
}

var (
	qualityShippingVerb = regexp.MustCompile(`(?i)\b(?:MERGED|SHIPPED|DEPLOYED|CLOSED)\b`)
	qualityPRCIRef      = regexp.MustCompile(`(?i)(?:\bPR\s*#?\d+\b|#\d+|\bCI\b)`)
	qualityHistoryRef   = regexp.MustCompile(`(?i)(?:\bPRs?\s*#?\d+|#[0-9]+|\bCI\b|\bworkflow(?:s)?\b|\b[0-9a-f]{7,40}\b)`)
	qualitySnapshot     = regexp.MustCompile(`(?i)^(?:loose ends|open work|remaining work)\s*:|\bPR\s*#?\d+\b.{0,120}\bstill open\b`)
	qualityListLink     = regexp.MustCompile(`^\s*[-*+]\s+\[[^]]+\]\([^)]+\.md(?:#[^)]*)?\)(?:\s+(?:-|—)\s+.*)?\s*$`)
	qualityFragmentLead = regexp.MustCompile(`(?i)^(?:and|but|because|then|therefore|which|while|changed|fixed|added|removed|updated|deleted|losing)\b|^[a-z]{1,3}\s+(?:agent|job|task|run)\b`)
	qualityFragmentTail = regexp.MustCompile(`(?i)(?:[,;:]|\b(?:and|but|because|therefore|which))\s*$`)
	qualityVacuous      = regexp.MustCompile(`(?i)^(?:some|several|various|recent)\s+(?:ask|run|review|implement|orchestrate|agent|job|task)s?\b.*\b(?:repository|repo)\b`)
	qualitySpecific     = regexp.MustCompile(`(?i)(?:\bPR\s*#?\d+\b|#\d+|\b(?:job|task|run)[-_: ]+[a-z0-9][a-z0-9._-]*\d[a-z0-9._-]*\b|\b\d+\b|\b(?:error|exception|panic|failed|failure|exit status)\b|(?:^|[\s(])(?:\.?\.?/)?[a-z0-9_.-]+(?:/[a-z0-9_.-]+)+(?::\d+)?\b|\b[a-z0-9_-]+\.(?:go|md|json|yaml|yml|toml|ts|tsx|js|jsx|py|sh|sql)(?::\d+)?\b)`)
	qualityLessonMarker = regexp.MustCompile(`(?i)\*\*(?:Why|How to apply):\*\*`)
	qualityCauseEffect  = regexp.MustCompile(`(?i)\b(?:because|therefore|so that|root cause|causes?|prevents?|which (?:means|causes|prevents)|lesson|pattern for)\b`)
)

// IsShippingStatus reports the narrow write-time gate shape: a shipping verb in
// the leading phrase plus a PR/CI-style reference. A short coordinator prefix
// such as "bridge" is allowed before the verb to cover the live note shape.
func IsShippingStatus(content string) bool {
	line := firstQualityLine(content)
	if line == "" {
		return false
	}
	loc := qualityShippingVerb.FindStringIndex(line)
	return loc != nil && loc[0] <= 40 && qualityPRCIRef.MatchString(line)
}

// IsMemoryIndex reports a MEMORY.md-style index file: at least two Markdown
// links to .md notes and no substantive non-heading body outside that link list.
func IsMemoryIndex(content string) bool {
	links, bodyLines := 0, 0
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		bodyLines++
		if qualityListLink.MatchString(line) {
			links++
		}
	}
	return links >= 2 && links == bodyLines
}

// QualityHasSpecific is the write-time substantiveness gate shared by the
// mechanical producer and quality scorer.
func QualityHasSpecific(content string) bool {
	return qualitySpecific.MatchString(strings.TrimSpace(content))
}

// DetectGroomQualityCandidates scores old-enough active facts, marks
// near-duplicates across the current pool, and returns highest-risk candidates
// first. Lesson-shaped facts are protected even if other signals would add up.
func DetectGroomQualityCandidates(cands []GroomCandidate, now time.Time, minAge time.Duration) []GroomQualityCandidate {
	if minAge <= 0 {
		return nil
	}
	nearDuplicates := qualityNearDuplicates(cands)
	var out []GroomQualityCandidate
	for _, candidate := range cands {
		born, ok := qualityFactTime(candidate)
		if !ok || now.Sub(born) < minAge {
			continue
		}
		content := strings.TrimSpace(candidate.Content)
		if content == "" {
			continue
		}
		signals := qualitySignals(candidate, nearDuplicates[candidate.ID])
		// Explicit lesson protection is stronger than arithmetic. This keeps a
		// Why/How lesson out even if it quotes a status or comes from automation.
		if signals.LessonShaped {
			continue
		}
		score := ScoreQualityRisk(signals)
		if score < GroomQualityRiskThreshold {
			continue
		}
		out = append(out, GroomQualityCandidate{
			GroomCandidate: candidate,
			ContentHash:    GroomContentHash(content),
			Signals:        signals,
			Score:          score,
			SignalFamilies: signals.Families(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func qualitySignals(candidate GroomCandidate, nearDuplicate bool) QualitySignals {
	content := strings.TrimSpace(candidate.Content)
	return QualitySignals{
		TransientStatus:     IsShippingStatus(content) || qualitySourceHistory(content) || IsMemoryIndex(content),
		Fragment:            qualityFragment(content),
		GenericVacuous:      qualityVacuous.MatchString(content) && !QualityHasSpecific(content),
		NearDuplicate:       nearDuplicate,
		AutomatedProvenance: qualityAutomatedProvenance(candidate.Provenance),
		Short:               utf8.RuneCountInString(content) < 160,
		LessonShaped:        qualityLessonMarker.MatchString(content) || qualityCauseEffect.MatchString(content),
	}
}

func qualitySourceHistory(content string) bool {
	return qualitySnapshot.MatchString(content) || qualityShippingVerb.MatchString(content) && qualityHistoryRef.MatchString(content)
}

func qualityFragment(content string) bool {
	trimmed := strings.TrimSpace(content)
	if qualityFragmentLead.MatchString(trimmed) || qualityFragmentTail.MatchString(trimmed) {
		return true
	}
	return strings.Count(trimmed, "`")%2 != 0 || strings.Count(trimmed, "[") != strings.Count(trimmed, "]")
}

func qualityAutomatedProvenance(provenance string) bool {
	p := strings.ToLower(strings.TrimSpace(provenance))
	for _, prefix := range []string{"gitmoot-mechanical", "ingest:", "distill:", "distill-success:", "groom-split:", "harvest:"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func qualityFactTime(candidate GroomCandidate) (time.Time, bool) {
	raw := strings.TrimSpace(candidate.FirstConfirmedAt)
	if raw == "" {
		raw = strings.TrimSpace(candidate.UpdatedAt)
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func firstQualityLine(content string) string {
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		line = strings.Trim(line, "#*_ ")
		if line != "" {
			return line
		}
	}
	return ""
}

func qualityNearDuplicates(cands []GroomCandidate) map[int64]bool {
	out := make(map[int64]bool)
	words := make([]map[string]struct{}, len(cands))
	for i, candidate := range cands {
		words[i] = qualityWordSet(candidate.Content)
	}
	for i := 0; i < len(cands); i++ {
		if len(words[i]) < 8 {
			continue
		}
		for j := i + 1; j < len(cands); j++ {
			if len(words[j]) < 8 || !sameQualityDomain(cands[i], cands[j]) {
				continue
			}
			if qualityJaccard(words[i], words[j]) >= 0.82 {
				out[cands[i].ID] = true
				out[cands[j].ID] = true
			}
		}
	}
	return out
}

func sameQualityDomain(a, b GroomCandidate) bool {
	return a.OwnerKind == b.OwnerKind && a.OwnerRef == b.OwnerRef && a.OwnerVersion == b.OwnerVersion && a.Repo == b.Repo && a.Scope == b.Scope
}

func qualityWordSet(content string) map[string]struct{} {
	normalized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return ' '
	}, strings.ToLower(content))
	out := make(map[string]struct{})
	for _, word := range strings.Fields(normalized) {
		out[word] = struct{}{}
	}
	return out
}

func qualityJaccard(a, b map[string]struct{}) float64 {
	intersection := 0
	for word := range a {
		if _, ok := b[word]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}
