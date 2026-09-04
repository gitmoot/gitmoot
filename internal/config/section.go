package config

import "strings"

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
// this package - 26 call sites across 22 files. Deliberately EXCLUDED per the
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
// ROUTING COVERAGE is also partial and counted rather than implied: 12 of the
// 26 call sites are pinned through their production loader (see
// TestMalformedHeaderRoutingPerCallSite, which names the 14 that are not). A
// revert of an unpinned site to the old two-bracket form would not fail the
// suite today.
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
