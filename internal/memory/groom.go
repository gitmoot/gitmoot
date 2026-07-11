package memory

// Deterministic grooming detectors for `gitmoot memory groom` (RFC #737, P4.2).
//
// Grooming mechanizes the manual curation pass that periodically retires stale,
// low-signal confirmed memories (status/changelog snapshots, table-of-contents
// index notes, bare to-do lists, and exact duplicates). Everything here is a
// PURE, DB-free function of its inputs so the detectors can be unit-tested in
// isolation against real-shaped fixtures — same inputs ⇒ same proposal, every
// run. The db-coupled orchestration (reading the vault snapshot, writing the plan
// artifact, applying retirements in one transaction) lives in the cli package.
//
// P4.2 retirement/rekey/cross-pool actions remain owner-gated proposals. P4.3
// adds one automatic operation: a deterministic, lossless brick split whose
// children are exact source substrings. Lossy LLM rewriting remains deferred.

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Groom detector reason tokens. They name WHY a memory was proposed for
// retirement and become the store retire reason as "groom:<token>" at apply time.
const (
	GroomReasonDuplicate       = "duplicate"
	GroomReasonStatusChangelog = "status-changelog"
	GroomReasonTaskList        = "task-list"
)

// Retire reasons stamped VERBATIM (no "groom:" prefix) by the #804 plan actions,
// so a retired row names the exact mechanism that replaced it.
const (
	// GroomReasonRekeySuperseded marks legacy sibling editions retired by a
	// rekey action: the kept edition carries the stable key from now on.
	GroomReasonRekeySuperseded = "rekey: superseded edition"
	// GroomReasonCrossPoolStale marks a stale shared edition retired by a
	// cross-pool promote-and-retire action: the newer private edition was
	// promoted into the shared pool in its place.
	GroomReasonCrossPoolStale = "cross-pool: superseded by promoted edition"
)

// GroomRewriteThreshold is the content length (in bytes) above which a memory is
// flagged as a REWRITE candidate rather than retired. Multi-fact "bricks" carry
// real signal that a blind retirement would lose, so P4.2 only lists them for the
// owner; the actual rewrite is deferred to the opt-in LLM pass (P4.3).
const GroomRewriteThreshold = 1200

// GroomMinChildBytes is the minimum trimmed size of a split child. Smaller
// segments are merged into a neighbor before labels and keys are derived.
const GroomMinChildBytes = 200

// groomFirstLineMax caps the first_line preview carried in the plan so a brick's
// opening paragraph can't bloat the artifact.
const groomFirstLineMax = 160

// GroomCandidate is the DB-free projection of one active confirmed memory the
// detectors consider. It mirrors the fields the vault snapshot already exposes.
// Owner/Repo/Scope are carried so exact-duplicate detection can be scoped to the
// SAME retrieval scope: two byte-identical facts held by different owners, repos,
// or scopes surface in DIFFERENT prompt-assembly scopes, so they are not true
// duplicates and retiring one would silently drop the fact from that scope.
type GroomCandidate struct {
	ID           int64
	Key          string
	Content      string
	OwnerKind    string
	OwnerRef     string
	OwnerVersion string
	Repo         string // "" == general scope
	Scope        string
	UpdatedAt    string // RFC3339; lexicographic order == chronological order
}

// GroomRetirement is one proposed retirement: the memory id, its key, the detector
// that flagged it, a first-line preview, and the owner/repo/scope so the owner can
// eyeball the plan and tell a same-scope duplicate from a (kept) cross-scope one.
type GroomRetirement struct {
	ID        int64
	Key       string
	Reason    string
	FirstLine string
	Owner     string // "kind:ref@version" label
	Repo      string
	Scope     string
}

// GroomRewriteFlag flags an over-long memory for owner review. P4.2 does not
// rewrite; it only records the id/key/size so the plan can list it.
type GroomRewriteFlag struct {
	ID    int64
	Key   string
	Chars int
}

// GroomSplitChild is one lossless child of a brick memory. Content is an exact,
// contiguous substring of the parent's trimmed coverage; children remain in
// byte order and concatenate back to that coverage exactly.
type GroomSplitChild struct {
	Key     string
	Content string
}

// GroomSplit is one deterministic split action over an active parent. UpdatedAt
// is carried to the store as the optimistic-concurrency guard.
type GroomSplit struct {
	ParentID          int64
	ParentKey         string
	ExpectedUpdatedAt string
	Children          []GroomSplitChild
}

// GroomStats is the roll-up carried in the plan artifact. Rekeys and CrossPool
// count the #804 plan actions; DetectGroomActions leaves them zero and the plan
// builder fills them in after running the dedicated detectors.
type GroomStats struct {
	TotalMemories       int            `json:"total_memories"`
	ProposedRetirements int            `json:"proposed_retirements"`
	RewriteFlags        int            `json:"rewrite_flags"`
	Rekeys              int            `json:"rekeys"`
	CrossPool           int            `json:"cross_pool"`
	ByReason            map[string]int `json:"by_reason"`
}

// GroomProposal is the detectors' full output over a candidate set.
type GroomProposal struct {
	Retirements  []GroomRetirement
	RewriteFlags []GroomRewriteFlag
	Stats        GroomStats
}

// DetectGroomActions runs every deterministic detector over the candidate set and
// returns a stable proposal. Precedence per memory (a memory is proposed at most
// once): exact-duplicate content, then a bare to-do list, then a status/changelog/
// ToC snapshot. Over-long memories that survive retirement are flagged for a later
// rewrite. Output is fully deterministic and independent of input order (candidates
// are sorted by id first).
func DetectGroomActions(cands []GroomCandidate) GroomProposal {
	sorted := append([]GroomCandidate(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	retired := make(map[int64]bool, len(sorted))
	var retirements []GroomRetirement
	addRetire := func(c GroomCandidate, reason string) {
		if retired[c.ID] {
			return
		}
		retired[c.ID] = true
		retirements = append(retirements, GroomRetirement{
			ID:        c.ID,
			Key:       c.Key,
			Reason:    reason,
			FirstLine: groomFirstLine(c.Content),
			Owner:     groomOwnerLabel(c),
			Repo:      c.Repo,
			Scope:     c.Scope,
		})
	}

	// (d) Exact-duplicate content: group by (owner tuple, repo, scope, sha256(content)),
	// keep the lowest id in each group, propose retiring the rest. Scoping the group
	// key to the retrieval scope (owner/repo/scope) — not content alone — means a fact
	// duplicated across owners or repos is NOT deduped: each copy is the only one
	// visible in its own prompt-assembly scope, so retiring it would silently lose the
	// fact there. sorted is id-ascending so group[0] is always the lowest id; keyOrder
	// preserves first-seen order for deterministic output.
	byKey := make(map[string][]GroomCandidate, len(sorted))
	var keyOrder []string
	for _, c := range sorted {
		k := groomDedupKey(c)
		if _, seen := byKey[k]; !seen {
			keyOrder = append(keyOrder, k)
		}
		byKey[k] = append(byKey[k], c)
	}
	for _, k := range keyOrder {
		group := byKey[k]
		if len(group) < 2 {
			continue
		}
		for _, dup := range group[1:] {
			addRetire(dup, GroomReasonDuplicate)
		}
	}

	// (a)+(b) Pattern detectors over rows not already claimed as duplicates.
	for _, c := range sorted {
		if retired[c.ID] {
			continue
		}
		switch {
		case detectTaskListOnly(c.Content):
			addRetire(c, GroomReasonTaskList)
		case detectStatusChangelog(c.Content):
			addRetire(c, GroomReasonStatusChangelog)
		}
	}

	// (c) Rewrite flags over rows that survived retirement: extreme length.
	var flags []GroomRewriteFlag
	for _, c := range sorted {
		if retired[c.ID] {
			continue
		}
		if len(c.Content) > GroomRewriteThreshold || len(SplitBrick(c.Key, c.Content)) > 0 {
			flags = append(flags, GroomRewriteFlag{ID: c.ID, Key: c.Key, Chars: len(c.Content)})
		}
	}

	byReason := map[string]int{}
	for _, r := range retirements {
		byReason[r.Reason]++
	}
	return GroomProposal{
		Retirements:  retirements,
		RewriteFlags: flags,
		Stats: GroomStats{
			TotalMemories:       len(sorted),
			ProposedRetirements: len(retirements),
			RewriteFlags:        len(flags),
			ByReason:            byReason,
		},
	}
}

// DetectGroomSplits returns every active-candidate brick that can be split
// losslessly. The caller supplies only active rows; sorting by parent id makes
// output independent of query/input order.
func DetectGroomSplits(cands []GroomCandidate) []GroomSplit {
	sorted := append([]GroomCandidate(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	usedByDomain := make(map[string]map[string]struct{})
	for _, c := range sorted {
		domain := groomSplitDomain(c)
		if usedByDomain[domain] == nil {
			usedByDomain[domain] = make(map[string]struct{})
		}
		usedByDomain[domain][c.Key] = struct{}{}
	}
	var out []GroomSplit
	for _, c := range sorted {
		children := SplitBrick(c.Key, c.Content)
		if len(children) == 0 {
			continue
		}
		used := usedByDomain[groomSplitDomain(c)]
		for i := range children {
			base := children[i].Key
			key := base
			for n := 2; ; n++ {
				if _, exists := used[key]; !exists {
					break
				}
				key = base + "-" + strconv.Itoa(n)
			}
			children[i].Key = key
			used[key] = struct{}{}
		}
		out = append(out, GroomSplit{
			ParentID: c.ID, ParentKey: c.Key, ExpectedUpdatedAt: c.UpdatedAt, Children: children,
		})
	}
	return out
}

func groomSplitDomain(c GroomCandidate) string {
	return strings.Join([]string{c.OwnerKind, c.OwnerRef, c.OwnerVersion, c.Repo, c.Scope}, "\x00")
}

// groomBoldHeader matches the house-style story seam used by ingested session
// notes: a whole line wrapped in Markdown bold markers.
var groomBoldHeader = regexp.MustCompile(`^\s*\*\*[^*\r\n].*\*\*\s*:?[ \t]*$`)

// groomBoldLead matches the OTHER house seam shape: a line that STARTS with a
// bold header but continues with prose on the same line
// ("**Waveform refinement (2026-06-19, PR #241):** fixed ..."). To avoid
// over-fragmenting at sub-field leads like "**Why:**" / "**How to apply:**",
// a bold-lead only counts as a story seam when the bold span itself carries
// dated/PR evidence (groomSeamEvidence) — story headers do, sub-fields don't.
var groomBoldLead = regexp.MustCompile(`^\s*\*\*([^*\r\n]+)\*\*`)

// groomSeamEvidence is the date/PR signature that promotes a bold-lead line to
// a story seam.
var groomSeamEvidence = regexp.MustCompile(`(?i)\d{4}-\d{2}-\d{2}|PR\s*#?\d+|#\d+`)

// groomPRMarker recognizes stand-alone PR/story markers that commonly lead a
// shipped-work paragraph. Date-led lines reuse groomDateLed below.
var groomPRMarker = regexp.MustCompile(`(?i)^\s*(?:[-*]\s*)?(?:PR\s*#?\d+\b|#\d+\b|(?:SHIPPED|MERGED|DEPLOYED)\b[^\r\n]*#\d+\b)`)

// groomListItem rejects every Markdown list item before the broader date and PR
// seam recognizers run. List entries are details within a story, not new stories.
var groomListItem = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s`)

type groomTextUnit struct {
	start int
	end   int
}

// SplitBrick partitions one parent at strong-story-seam byte offsets only. In
// every case at least two substantive segments are required, and any coverage
// mismatch fails closed by returning nil so the existing rewrite flag remains
// the only action.
func SplitBrick(parentKey, content string) []GroomSplitChild {
	coverage := strings.TrimSpace(content)
	if coverage == "" {
		return nil
	}
	if detectStatusChangelog(coverage) {
		return nil
	}
	strong := groomStrongSeams(coverage)
	if len(strong) < 2 {
		return nil
	}

	cutStarts := []int{0}
	for _, seam := range strong {
		if seam.start > 0 {
			cutStarts = append(cutStarts, seam.start)
		}
	}
	cutStarts = uniqueSortedOffsets(cutStarts, len(coverage))
	if len(cutStarts) < 2 {
		return nil
	}
	segments := make([]groomTextUnit, 0, len(cutStarts))
	for i, start := range cutStarts {
		end := len(coverage)
		if i+1 < len(cutStarts) {
			end = cutStarts[i+1]
		}
		if start < 0 || start >= end || end > len(coverage) {
			return nil
		}
		segments = append(segments, groomTextUnit{start: start, end: end})
	}
	segments = mergeGroomRunts(segments, coverage)
	if len(segments) < 2 {
		return nil
	}

	children := make([]GroomSplitChild, 0, len(segments))
	usedKeys := make(map[string]struct{}, len(segments))
	substantive := 0
	for _, segment := range segments {
		text := coverage[segment.start:segment.end]
		if strings.TrimSpace(text) == "" {
			return nil
		}
		if groomSubstantive(text) {
			substantive++
		}
		label := groomFirstNonBlankLine(segment, coverage)
		base := parentKey + "-" + Slug(groomSeamLabel(label))
		key := base
		for n := 2; ; n++ {
			if _, exists := usedKeys[key]; !exists {
				break
			}
			key = base + "-" + strconv.Itoa(n)
		}
		usedKeys[key] = struct{}{}
		children = append(children, GroomSplitChild{Key: key, Content: text})
	}
	if substantive < 2 || concatGroomSplitChildren(children) != coverage {
		return nil
	}
	return children
}

func mergeGroomRunts(segments []groomTextUnit, content string) []groomTextUnit {
	segments = append([]groomTextUnit(nil), segments...)
	for len(segments) > 1 {
		merged := false
		for i, segment := range segments {
			if len(strings.TrimSpace(content[segment.start:segment.end])) >= GroomMinChildBytes {
				continue
			}
			if i == 0 {
				segments[1].start = segment.start
				segments = segments[1:]
			} else {
				segments[i-1].end = segment.end
				segments = append(segments[:i], segments[i+1:]...)
			}
			merged = true
			break
		}
		if !merged {
			break
		}
	}
	return segments
}

func groomStrongSeams(content string) []groomTextUnit {
	var out []groomTextUnit
	offset := 0
	for _, withNewline := range strings.SplitAfter(content, "\n") {
		line := strings.TrimSuffix(withNewline, "\n")
		trimmed := strings.TrimSpace(line)
		if isGroomStrongSeam(trimmed) {
			out = append(out, groomTextUnit{start: offset, end: offset + len(withNewline)})
		}
		offset += len(withNewline)
	}
	return out
}

// groomParagraphUnits mirrors atomicUnits' first boundary: non-empty paragraph
// groups separated by one or more blank lines, while retaining byte offsets.
func groomParagraphUnits(content string) []groomTextUnit {
	blankLine := regexp.MustCompile(`\r?\n[ \t]*\r?\n(?:[ \t]*\r?\n)*`)
	locs := blankLine.FindAllStringIndex(content, -1)
	starts := []int{0}
	for _, loc := range locs {
		if loc[1] < len(content) {
			starts = append(starts, loc[1])
		}
	}
	starts = uniqueSortedOffsets(starts, len(content))
	out := make([]groomTextUnit, 0, len(starts))
	for i, start := range starts {
		end := len(content)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		if strings.TrimSpace(content[start:end]) != "" {
			out = append(out, groomTextUnit{start: start, end: end})
		}
	}
	return out
}

func groomFirstNonBlankLine(unit groomTextUnit, content string) string {
	if unit.start < 0 || unit.end > len(content) || unit.start >= unit.end {
		return ""
	}
	part := content[unit.start:unit.end]
	for _, line := range strings.SplitAfter(part, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\n"))
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func isGroomStrongSeam(line string) bool {
	if groomListItem.MatchString(line) {
		return false
	}
	label := strings.ToLower(groomSeamLabel(line))
	if label == "why" || label == "how to apply" {
		return false
	}
	if groomBoldHeader.MatchString(line) || groomDateLed.MatchString(line) || groomPRMarker.MatchString(line) {
		return true
	}
	if m := groomBoldLead.FindStringSubmatch(line); m != nil && groomSeamEvidence.MatchString(m[1]) {
		return true
	}
	return false
}

func groomSeamLabel(line string) string {
	label := strings.TrimSpace(line)
	label = strings.TrimSuffix(label, ":")
	if strings.HasPrefix(label, "**") && strings.HasSuffix(label, "**") && len(label) >= 4 {
		label = strings.TrimSpace(label[2 : len(label)-2])
	} else if m := groomBoldLead.FindStringSubmatch(label); m != nil {
		// Bold-lead seam ("**Header:** prose..."): the header span is the label,
		// not the whole line.
		label = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(m[1]), ":"))
	}
	return strings.TrimSpace(strings.TrimSuffix(label, ":"))
}

func groomSubstantive(content string) bool {
	count := 0
	seenContent := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !seenContent && isGroomStrongSeam(trimmed) {
			seenContent = true
			trimmed = groomStrongSeamPayload(trimmed)
			if trimmed == "" {
				continue
			}
		}
		seenContent = true
		for _, r := range trimmed {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				count++
			}
		}
	}
	return count >= 8
}

func groomStrongSeamPayload(line string) string {
	if groomBoldHeader.MatchString(line) {
		return ""
	}
	if loc := groomDateLed.FindStringIndex(line); loc != nil && loc[0] == 0 {
		return strings.TrimSpace(line[loc[1]:])
	}
	if loc := groomPRMarker.FindStringIndex(line); loc != nil && loc[0] == 0 {
		return strings.TrimSpace(line[loc[1]:])
	}
	return line
}

func uniqueSortedOffsets(offsets []int, max int) []int {
	sort.Ints(offsets)
	out := offsets[:0]
	last := -1
	for _, offset := range offsets {
		if offset < 0 || offset >= max || offset == last {
			continue
		}
		out = append(out, offset)
		last = offset
	}
	return out
}

func concatGroomSplitChildren(children []GroomSplitChild) string {
	var b strings.Builder
	for _, child := range children {
		b.WriteString(child.Content)
	}
	return b.String()
}

// groomOwnerLabel renders a candidate's owner as a stable "kind:ref@version" label
// (version omitted when empty) for the plan's human-readable output.
func groomOwnerLabel(c GroomCandidate) string {
	label := c.OwnerKind + ":" + c.OwnerRef
	if c.OwnerVersion != "" {
		label += "@" + c.OwnerVersion
	}
	return label
}

// groomDedupKey is the exact-duplicate grouping key: the owner tuple, repo, and
// scope joined with a NUL delimiter (never present in any component) followed by the
// content hash. Two candidates share a key only when they are TRUE duplicates —
// identical content held by the same owner in the same repo and scope — so a fact
// duplicated across retrieval scopes is never proposed for retirement.
func groomDedupKey(c GroomCandidate) string {
	sum := sha256.Sum256([]byte(c.Content))
	return strings.Join([]string{
		c.OwnerKind, c.OwnerRef, c.OwnerVersion, c.Repo, c.Scope,
		hex.EncodeToString(sum[:]),
	}, "\x00")
}

// groomFirstLine returns the first non-blank line of content, trimmed and capped,
// for the plan's human-readable preview.
func groomFirstLine(content string) string {
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if len(t) > groomFirstLineMax {
			t = strings.TrimSpace(t[:groomFirstLineMax])
		}
		return t
	}
	return ""
}

// groomTaskLine matches a Markdown checkbox item ("- [ ]", "* [x]", "1. [X]").
var groomTaskLine = regexp.MustCompile(`^([-*]|\d+\.)\s+\[[ xX]\]`)

// groomTocLine matches a Markdown list item whose payload is a link ("- [Title]",
// "- [[wikilink]]") — the shape of a table-of-contents / changelog index note. The
// negative lookahead-free form excludes checkboxes (handled by groomTaskLine): the
// character after '[' must NOT be a checkbox marker (space, x, X immediately
// followed by ']').
var groomTocLine = regexp.MustCompile(`^[-*]\s+\[[^\]]`)

// groomDateLed matches a changelog line led by an ISO date, optionally after a
// list bullet ("2026-07-08 …", "- 2026-07-08 …", "* [2026-07-08] …").
var groomDateLed = regexp.MustCompile(`^[-*]?\s*\[?\d{4}-\d{2}-\d{2}`)

// isTaskLine reports whether a single line is a Markdown checkbox item.
func isTaskLine(line string) bool {
	return groomTaskLine.MatchString(strings.TrimSpace(line))
}

// isStrongStatusLine reports whether a single line is an UNAMBIGUOUS status-snapshot
// marker on its own: an explicit "STATUS:" header or a "…& deployed" changelog
// phrase. These name a status note even as a lone line. The WEAK markers a
// status/changelog line can also carry — a leading ISO date, a stray "SHIPPED"
// mention, or a bracketed-ref/ToC list item — are common in substantive one-line
// keepers, so they only indicate a changelog when they DOMINATE a multi-line note
// (see detectStatusChangelog's minimum-line guard).
func isStrongStatusLine(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	upper := strings.ToUpper(t)
	lower := strings.ToLower(t)
	switch {
	case strings.HasPrefix(upper, "STATUS:"):
		return true
	case strings.Contains(lower, "merged & deployed"),
		strings.Contains(lower, "merged and deployed"),
		strings.Contains(lower, "shipped & deployed"),
		strings.Contains(lower, "shipped and deployed"):
		return true
	}
	return false
}

// isStatusChangelogLine reports whether a single line looks like a status,
// changelog, or table-of-contents entry: a "STATUS:" marker, a shipped/deployed
// changelog phrase, an ISO-date-led line, or a Markdown link list item.
func isStatusChangelogLine(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	if isTaskLine(t) {
		// A checkbox is a task-list line, not a ToC/changelog line; keep the two
		// detectors' populations disjoint so a pure checkbox list is reported as
		// task-list (not status-changelog).
		return false
	}
	upper := strings.ToUpper(t)
	lower := strings.ToLower(t)
	switch {
	case strings.HasPrefix(upper, "STATUS:"):
		return true
	case strings.Contains(t, "SHIPPED"):
		return true
	case strings.Contains(lower, "merged & deployed"),
		strings.Contains(lower, "merged and deployed"),
		strings.Contains(lower, "shipped & deployed"),
		strings.Contains(lower, "shipped and deployed"):
		return true
	case groomTocLine.MatchString(t):
		return true
	case groomDateLed.MatchString(t):
		return true
	}
	return false
}

// nonBlankLines returns the trimmed-nonblank lines of content.
func nonBlankLines(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// detectTaskListOnly reports whether content is nothing but a to-do list: at least
// one non-blank line and EVERY non-blank line is a checkbox item.
func detectTaskListOnly(content string) bool {
	lines := nonBlankLines(content)
	if len(lines) == 0 {
		return false
	}
	for _, line := range lines {
		if !isTaskLine(line) {
			return false
		}
	}
	return true
}

// groomStatusDominance is the fraction of non-blank lines that must look like
// status/changelog/ToC entries for the whole note to be proposed for retirement.
// Requiring dominance (not a single marker) protects substantive memories that
// merely mention "SHIPPED #754" in one line from being retired — only notes that
// ARE a changelog/index get flagged.
const groomStatusDominance = 0.8

// groomStatusMinLines is the minimum non-blank line count at which the WEAK
// changelog markers (a leading ISO date, a stray "SHIPPED", a bracketed-ref/ToC
// list item) are trusted to prove a note IS a changelog/index. Below it, only a
// STRONG marker (an explicit "STATUS:" header or a "…& deployed" phrase) retires,
// so a lone high-value fact that merely leads with a date or mentions SHIPPED — the
// RFC's #1 use case: the lead's date-led one-line ingested notes — is kept, not
// retired. The dominance guard alone is vacuous at n=1 (a single matching line is
// trivially 100% of the note).
const groomStatusMinLines = 3

// ---- #804 legacy-key rekey detector ----------------------------------------

// legacyIngestKeySuffix recognizes the pre-#804 ingest key shape: a trailing
// "-<8 hex>" content-hash suffix appended by the old IngestKey.
var legacyIngestKeySuffix = regexp.MustCompile(`-[0-9a-f]{8}$`)

// StableKey returns the #804 stable form of an ingest key: the trailing legacy
// content-hash suffix stripped when present, the key unchanged otherwise. It is
// a deterministic HEURISTIC — a key whose final segment legitimately happens to
// be 8 hex characters is treated as legacy — which is one reason rekey proposals
// flow through the human-reviewed groom plan instead of applying silently. A key
// that is NOTHING but a hash suffix is returned unchanged (stripping would leave
// an empty key).
func StableKey(key string) string {
	stripped := legacyIngestKeySuffix.ReplaceAllString(key, "")
	if stripped == "" {
		return key
	}
	return stripped
}

// GroomRekeyRetire is one older sibling edition a rekey group retires.
type GroomRekeyRetire struct {
	ID        int64
	Key       string
	FirstLine string
}

// GroomRekeyAction is one proposed legacy-key migration (#804): keep the current
// edition, rewrite its key to the stable form, retire the older sibling
// editions. Organic sweeps can never fix legacy keys on their own — content-hash
// dedup skips unchanged notes, and the first edit would spawn a stable-keyed
// THIRD sibling — so groom is the only path that converges a legacy group.
type GroomRekeyAction struct {
	KeepID    int64
	KeepKey   string // the keeper's current key
	NewKey    string // the stable key; equals KeepKey when the keeper already carries it
	Retire    []GroomRekeyRetire
	Owner     string // "kind:ref@version" label
	Repo      string
	Scope     string
	FirstLine string // keeper content preview
}

// DetectGroomRekeys groups active candidates per (owner, repo, scope) by their
// STABLE key and proposes one rekey action for every group containing at least
// one legacy-suffixed key. The keeper is the row already carrying the stable key
// when one exists (under the post-#804 write path that row is the current
// edition by construction); otherwise the newest edition by UpdatedAt (ties
// break to the highest id). Every other group member is proposed for retirement
// with GroomReasonRekeySuperseded. Output is deterministic and independent of
// input order.
func DetectGroomRekeys(cands []GroomCandidate) []GroomRekeyAction {
	sorted := append([]GroomCandidate(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	groups := make(map[string][]GroomCandidate)
	var order []string
	for _, c := range sorted {
		k := strings.Join([]string{
			c.OwnerKind, c.OwnerRef, c.OwnerVersion, c.Repo, c.Scope, StableKey(c.Key),
		}, "\x00")
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], c)
	}

	var out []GroomRekeyAction
	for _, gk := range order {
		group := groups[gk]
		stable := StableKey(group[0].Key)
		hasLegacy := false
		for _, c := range group {
			if c.Key != stable {
				hasLegacy = true
				break
			}
		}
		if !hasLegacy {
			continue
		}
		keep := group[0]
		haveStable := false
		for _, c := range group {
			if c.Key == stable {
				keep = c
				haveStable = true
				break
			}
		}
		if !haveStable {
			for _, c := range group[1:] {
				if c.UpdatedAt > keep.UpdatedAt || (c.UpdatedAt == keep.UpdatedAt && c.ID > keep.ID) {
					keep = c
				}
			}
		}
		action := GroomRekeyAction{
			KeepID:    keep.ID,
			KeepKey:   keep.Key,
			NewKey:    stable,
			Owner:     groomOwnerLabel(keep),
			Repo:      keep.Repo,
			Scope:     keep.Scope,
			FirstLine: groomFirstLine(keep.Content),
		}
		for _, c := range group {
			if c.ID == keep.ID {
				continue
			}
			action.Retire = append(action.Retire, GroomRekeyRetire{
				ID:        c.ID,
				Key:       c.Key,
				FirstLine: groomFirstLine(c.Content),
			})
		}
		out = append(out, action)
	}
	return out
}

// ---- #804 cross-pool staleness detector -------------------------------------

// CrossPoolBM25Strong is the minimum bm25 relevance (as -bm25: higher is better)
// at which a private fact's TOP shared-pool match counts as secondary evidence
// of a cross-pool duplicate. bm25 magnitudes are corpus-dependent, so the bar is
// deliberately high AND the signal additionally requires an existing
// memory_links edge between the two rows — a strong bm25 score alone never
// proposes anything.
const CrossPoolBM25Strong = 15.0

// GroomCrossPoolSignal is one store-computed secondary-evidence tuple for
// DetectCrossPoolStaleness: a private fact's top bm25 match in the shared pool
// and whether a memory_links edge connects the two rows in either direction.
type GroomCrossPoolSignal struct {
	PrivateID int64
	SharedID  int64
	Score     float64 // -bm25 relevance, higher is better
	Linked    bool
}

// Cross-pool proposal bases: the deterministic primary signal (stable-key
// equality) and the composite secondary signal (strong bm25 top-match plus a
// memory_links edge).
const (
	CrossPoolBasisStableKey = "stable-key"
	CrossPoolBasisBM25Link  = "bm25-link"
)

// GroomCrossPoolAction proposes one promote-and-retire pair (#804): promote the
// newer private edition into the shared pool (author preserved) and retire the
// stale shared edition it replaces.
type GroomCrossPoolAction struct {
	PrivateID  int64
	PrivateKey string
	Owner      string // private owner label
	SharedID   int64
	SharedKey  string
	Basis      string // CrossPoolBasisStableKey | CrossPoolBasisBM25Link
	Repo       string
	Scope      string
	FirstLine  string // the newer (private) edition's preview
}

// DetectCrossPoolStaleness finds shared-pool facts that a NEWER private-pool
// edition has superseded. Primary, fully deterministic signal: a private agent
// fact whose STABLE key equals a shared fact's stable key in the same repo and
// scope. Secondary, composite signal: a store-computed bm25 top-match at or
// above CrossPoolBM25Strong that ALSO shares a memory_links edge — bm25 alone is
// never enough. Both require the private edition to be strictly newer
// (UpdatedAt) than the shared one. At most one action is proposed per shared
// row (the newest qualifying private edition wins; primary beats secondary).
// Output is deterministic and independent of input order.
func DetectCrossPoolStaleness(cands []GroomCandidate, signals []GroomCrossPoolSignal) []GroomCrossPoolAction {
	sorted := append([]GroomCandidate(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	byID := make(map[int64]GroomCandidate, len(sorted))
	var private, shared []GroomCandidate
	for _, c := range sorted {
		byID[c.ID] = c
		switch {
		case c.OwnerKind == OwnerKindAgent:
			private = append(private, c)
		case c.OwnerKind == OwnerKindShared && c.OwnerRef == SharedOwnerRef:
			shared = append(shared, c)
		}
	}

	claimed := make(map[int64]bool) // shared id -> already has an action
	var out []GroomCrossPoolAction
	propose := func(p, s GroomCandidate, basis string) {
		claimed[s.ID] = true
		out = append(out, GroomCrossPoolAction{
			PrivateID:  p.ID,
			PrivateKey: p.Key,
			Owner:      groomOwnerLabel(p),
			SharedID:   s.ID,
			SharedKey:  s.Key,
			Basis:      basis,
			Repo:       s.Repo,
			Scope:      s.Scope,
			FirstLine:  groomFirstLine(p.Content),
		})
	}

	// Primary: stable-key equality within the same repo/scope, private newer.
	for _, s := range shared {
		var best GroomCandidate
		found := false
		for _, p := range private {
			if p.Repo != s.Repo || p.Scope != s.Scope {
				continue
			}
			if StableKey(p.Key) != StableKey(s.Key) {
				continue
			}
			if !(p.UpdatedAt > s.UpdatedAt) {
				continue
			}
			if !found || p.UpdatedAt > best.UpdatedAt || (p.UpdatedAt == best.UpdatedAt && p.ID > best.ID) {
				best, found = p, true
			}
		}
		if found {
			propose(best, s, CrossPoolBasisStableKey)
		}
	}

	// Secondary: strong bm25 top-match plus a memory_links edge, private newer.
	ordered := append([]GroomCrossPoolSignal(nil), signals...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].PrivateID != ordered[j].PrivateID {
			return ordered[i].PrivateID < ordered[j].PrivateID
		}
		return ordered[i].SharedID < ordered[j].SharedID
	})
	for _, sig := range ordered {
		if !sig.Linked || sig.Score < CrossPoolBM25Strong {
			continue
		}
		if claimed[sig.SharedID] {
			continue
		}
		p, okP := byID[sig.PrivateID]
		s, okS := byID[sig.SharedID]
		if !okP || !okS {
			continue
		}
		if p.OwnerKind != OwnerKindAgent || s.OwnerKind != OwnerKindShared || s.OwnerRef != SharedOwnerRef {
			continue
		}
		if p.Repo != s.Repo || p.Scope != s.Scope {
			continue
		}
		if !(p.UpdatedAt > s.UpdatedAt) {
			continue
		}
		propose(p, s, CrossPoolBasisBM25Link)
	}
	return out
}

// detectStatusChangelog reports whether content is predominantly a status,
// changelog, or table-of-contents snapshot: at least groomStatusDominance of the
// non-blank lines are status/changelog/ToC lines. For short notes (fewer than
// groomStatusMinLines non-blank lines) the weak markers are not enough on their own
// — at least one line must be a STRONG status marker — so a single substantive
// keeper is never retired just for leading with a date or containing "SHIPPED".
func detectStatusChangelog(content string) bool {
	lines := nonBlankLines(content)
	if len(lines) == 0 {
		return false
	}
	matching, strong := 0, 0
	for _, line := range lines {
		if isStatusChangelogLine(line) {
			matching++
		}
		if isStrongStatusLine(line) {
			strong++
		}
	}
	if float64(matching) < groomStatusDominance*float64(len(lines)) {
		return false
	}
	if len(lines) < groomStatusMinLines {
		return strong >= 1
	}
	return true
}
