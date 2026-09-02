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
// disk_guard.go already cleared state here, each citing the #1113 finder; this
// makes all of them agree rather than leaving twenty-one loaders with the unsafe
// reading.
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
