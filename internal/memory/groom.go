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
// P4.2 is PROPOSE + retire-only: the detectors emit a reviewable plan the owner
// applies explicitly. Over-long "brick" memories are only FLAGGED for a later,
// human/LLM rewrite pass (P4.3) — this track never rewrites content, only
// proposes retirements the owner confirms.

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// Groom detector reason tokens. They name WHY a memory was proposed for
// retirement and become the store retire reason as "groom:<token>" at apply time.
const (
	GroomReasonDuplicate       = "duplicate"
	GroomReasonStatusChangelog = "status-changelog"
	GroomReasonTaskList        = "task-list"
)

// GroomRewriteThreshold is the content length (in bytes) above which a memory is
// flagged as a REWRITE candidate rather than retired. Multi-fact "bricks" carry
// real signal that a blind retirement would lose, so P4.2 only lists them for the
// owner; the actual rewrite is deferred to the opt-in LLM pass (P4.3).
const GroomRewriteThreshold = 1200

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

// GroomStats is the roll-up carried in the plan artifact.
type GroomStats struct {
	TotalMemories       int            `json:"total_memories"`
	ProposedRetirements int            `json:"proposed_retirements"`
	RewriteFlags        int            `json:"rewrite_flags"`
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
		if len(c.Content) > GroomRewriteThreshold {
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
