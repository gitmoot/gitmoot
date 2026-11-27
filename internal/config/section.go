package config

import (
	"strconv"
	"strings"
)

// sectionHeader classifies one already-trimmed, comment-stripped config line as
// a TOML section header.
//
// It answers two things and nothing else - is this line a section boundary, and
// which section does it open - because that classification was the only genuinely
// shared part of the twenty-odd hand-rolled section scanners in this package
// (#1759). Each loader keeps its own field application: there is no callback, no
// generic parser and no policy flag here, so a reader of any single loader still
// sees its whole parse in one place.
//
// FAIL-CLOSED ON A MALFORMED HEADER. A line that opens with '[' but never closes
// is STILL a boundary (ok is true) and names NO section (name is empty), so the
// caller's current-section state is cleared. That is deliberate and it is the
// behaviour correction in #1759: the previous form tested for both brackets at
// once, so `[workflow` failed the test entirely, was not treated as a header,
// and every key after it was applied to whichever section was open before -
// silently misattributing configuration on invalid input. tool_cache.go and
// disk_guard.go already cleared state here, each citing the #1113 finder.
//
// WHAT THIS DOES NOT COVER, because a comment that overstates its own reach
// stops the next reader checking. It is used by the plain section scanners in
// this package. Deliberately EXCLUDED per the
// #1759 ruling (workflow note 107889) on their measured structural
// differences: agent_types.go (LoadAgentTypes and removeAgentTypeBlocks) and
// memory_pipelines.go keep the pre-#1759 two-bracket form, so LoadAgentTypes
// still MISATTRIBUTES keys after a malformed header and removeAgentTypeBlocks
// - a WRITE path - still drops the malformed line and the lines under it from
// the operator's file. Both are pre-existing, both sit outside what that
// ruling scoped, and neither is fixed here. disk_guard.go and tool_cache.go
// additionally keep private inline copies of this classification:
// behaviourally identical, but a second implementation.
//
// So: one classification for the plain scanners, NOT one for the package.
//
// ROUTING COVERAGE is partial, and NO CURRENT COUNT IS STATED HERE on purpose -
// the only numbers below are historical, inside the retraction. The
// call-site set is DERIVED FROM THE AST and ENFORCED, not asserted in prose:
// TestSectionHeaderCoverageIsDerivedNotAsserted parses this package, requires
// pinnedSectionHeaderSites and unpinnedSectionHeaderSites to partition every
// discovered site exactly, and fails naming any site that is neither. Adding a
// call site therefore breaks a test rather than quietly outdating a comment. A
// revert of an unpinned site would still not fail the suite today; which sites
// those are is listed there, by symbol.
//
// Two copies of a number is what produced the original error: this comment said
// 16 while 15 were pinned (#1795 review N1). Prose could drift silently, which
// is why the successor is a test and not a better sentence (#1795 review P3-2).
//
// A valid header is byte-equivalent to the old behaviour: same trimming, same
// name. Only the invalid-input path changes.
func sectionHeader(line string) (name string, ok bool) {
	if !strings.HasPrefix(line, "[") {
		return "", false
	}
	if !strings.HasSuffix(line, "]") {
		// Malformed: a boundary that opens nothing.
		return "", true
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")), true
}

// malformedHeaderTargets reports whether a MALFORMED header (one that opened
// with '[' and never closed) names section as a complete dotted segment.
//
// It exists so a GATE loader can refuse a file whose own section is unreadable
// without refusing files whose typo is somewhere else entirely. org.go set this
// precedent for [org: fail closed only for a genuinely org-shaped header, so
// that "a typo in an unrelated section must not brick dispatch". The same
// asymmetry applies here - `[disk_guard` must not stop the merge gate loading,
// while `[merge_gate` must stop it returning a permissive default.
//
// Matching is per dotted SEGMENT, so `[repos.owner/repo.merge_gate` targets
// merge_gate and `[merge_gateway` does not.
func malformedHeaderTargets(line, section string) bool {
	body := strings.TrimSpace(strings.TrimPrefix(line, "["))
	for _, segment := range strings.Split(body, ".") {
		if strings.TrimSpace(segment) == section {
			return true
		}
	}
	return false
}

// tableHeader is one config line read as a TOML table header, for the loaders
// whose section name is a dotted key path ([worktrees], [[worktrees.retired]]),
// which a raw-text comparison misreads (#2763 review).
type tableHeader struct {
	// path is the key path with every key unquoted, joined by "."; empty when
	// the header is malformed.
	path string
	// array reports the [[...]] array-of-tables form.
	array bool
	// loose is a best-effort reading of the keys of a malformed header, so a
	// loader can tell whether the broken line was meant for its own table.
	loose []string
}

// parseTableHeader reads a line that sectionHeader classified as a boundary as
// a TOML table header: [path] or [[path]], where path is keys joined by dots
// with optional whitespace around them, each key bare (A-Za-z0-9_-), "basic"
// or 'literal'. Anything else (unclosed, an extra ], text after the closing
// bracket, an empty or invalid key) is malformed and has no path.
func parseTableHeader(line string) tableHeader {
	h := tableHeader{array: strings.HasPrefix(line, "[[")}
	rest := strings.TrimPrefix(line, "[")
	if h.array {
		rest = strings.TrimPrefix(rest, "[")
	}
	var keys []string
	for {
		rest = strings.TrimLeft(rest, " \t")
		key, tail, ok := tableHeaderKey(rest)
		if !ok {
			return malformedTableHeader(line, h.array)
		}
		keys = append(keys, key)
		rest = strings.TrimLeft(tail, " \t")
		if !strings.HasPrefix(rest, ".") {
			break
		}
		rest = rest[1:]
	}
	closer := "]"
	if h.array {
		closer = "]]"
	}
	if rest != closer {
		return malformedTableHeader(line, h.array)
	}
	h.path = strings.Join(keys, ".")
	return h
}

// tableHeaderKey reads one key at the start of s and returns it unquoted with
// the text after it.
func tableHeaderKey(s string) (key, rest string, ok bool) {
	switch {
	case strings.HasPrefix(s, `"`):
		for i := 1; i < len(s); i++ {
			switch s[i] {
			case '\\':
				i++
			case '"':
				key, err := strconv.Unquote(s[:i+1])
				return key, s[i+1:], err == nil
			}
		}
		return "", "", false
	case strings.HasPrefix(s, "'"):
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", "", false
		}
		return s[1 : end+1], s[end+2:], true
	}
	end := 0
	for end < len(s) && (s[end] >= 'A' && s[end] <= 'Z' || s[end] >= 'a' && s[end] <= 'z' ||
		s[end] >= '0' && s[end] <= '9' || s[end] == '_' || s[end] == '-') {
		end++
	}
	return s[:end], s[end:], end > 0
}

func malformedTableHeader(line string, array bool) tableHeader {
	body := strings.TrimLeft(line, "[ \t")
	if end := strings.IndexByte(body, ']'); end >= 0 {
		body = body[:end]
	}
	var loose []string
	for _, segment := range strings.Split(body, ".") {
		loose = append(loose, strings.Trim(segment, " \t\"'"))
	}
	return tableHeader{array: array, loose: loose}
}

// malformedTargets reports whether a malformed header was meant for the table
// at path: its loose keys contain path's keys as a consecutive run, the
// per-segment rule of malformedHeaderTargets extended to dotted paths. A
// malformed header aimed elsewhere ([disk_guard) leaves the loader alone.
func (h tableHeader) malformedTargets(path string) bool {
	if h.path != "" {
		return false
	}
	want := strings.Split(path, ".")
	for start := 0; start+len(want) <= len(h.loose); start++ {
		match := true
		for i, key := range want {
			match = match && h.loose[start+i] == key
		}
		if match {
			return true
		}
	}
	return false
}
