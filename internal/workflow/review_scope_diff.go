package workflow

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// ReviewScopeDiff is what a follow-up scope resolver proves from git about one
// round of a pull request (#2682). A round's raw range, prior head to current
// head, also carries every commit a merge of the base branch brought in, so it
// is never the scope on its own: the scope is the pull request's OWN change,
// measured against its merge base at each end.
type ReviewScopeDiff struct {
	// ChangedFiles are the files whose own patch (merge base to head) differs
	// between the prior head and the current head. With no base merge between
	// the two rounds this is exactly the prior-head-to-head file list.
	ChangedFiles []string
	// MergeBase is merge-base(origin/<base>, current head): the base the
	// reviewer diffs the listed files from.
	MergeBase string
	// BaseMoved reports that the merge base moved between the two rounds: the
	// pull request merged its base branch in.
	BaseMoved bool
	// OwnFiles are the files in the pull request's own diff at the current head.
	OwnFiles []string
	// PriorOwnLines maps each file in the pull request's own diff at the PRIOR
	// head to the line ranges its hunks cover in the prior head's numbering,
	// context included, because a prior finding's file and line are named at
	// the head it was reported on. A file mapped to nil has a diff git does not
	// express in lines (binary, or a patch the forge omitted): any line counts
	// as inside. A nil map means the prior own diff is unknown.
	PriorOwnLines map[string][]ReviewLineRange
}

// EmptyAfterBaseMerge reports a scope that came out empty only because the
// base was merged in: no own change is proven to have moved, yet the pull
// request has one. "The scope is empty: approve without rereading" is right
// for a round with no base merge and no change; here it would approve on a
// narrowing nothing verified, so the round is a full review against the merge
// base instead (#2682 round 2).
func (d ReviewScopeDiff) EmptyAfterBaseMerge() bool {
	return d.BaseMoved && len(d.ChangedFiles) == 0 && len(d.OwnFiles) > 0
}

// ReviewLineRange is an inclusive range of one-based line numbers.
type ReviewLineRange struct {
	Start int
	End   int
}

// SplitOutsidePRFindings separates prior findings that name a location
// outside the pull request's own diff from the ones still carried. A finding
// is outside only when every file:line it names was outside the own diff AT
// THE HEAD IT WAS REPORTED ON: its file is not in that own diff, or its line is
// in no own hunk of the file. A finding inside the prior own diff always stays,
// whatever the file is called now (a file the pull request added and then
// renamed is still its own). A finding that names no file or an absolute path
// is kept, and so is every finding when the prior own diff is unknown, so an
// unparseable locator or an uncertain lineage can only keep a finding.
func SplitOutsidePRFindings(findings []string, diff ReviewScopeDiff) (kept []string, outside []string) {
	if diff.PriorOwnLines == nil {
		return findings, nil
	}
	for _, finding := range findings {
		locators := findingLocators(finding)
		dropped := len(locators) > 0
		for _, locator := range locators {
			if !locator.outside(diff.PriorOwnLines) {
				dropped = false
				break
			}
		}
		if dropped {
			outside = append(outside, finding)
		} else {
			kept = append(kept, finding)
		}
	}
	return kept, outside
}

type findingLocator struct {
	path       string
	start, end int
}

func (l findingLocator) outside(priorLines map[string][]ReviewLineRange) bool {
	if l.path == "" || strings.HasPrefix(l.path, "/") {
		return false
	}
	ranges, inPrior := priorLines[l.path]
	if !inPrior {
		return true
	}
	if l.start == 0 || ranges == nil {
		return false
	}
	for _, r := range ranges {
		if l.start <= r.End && l.end >= r.Start {
			return false
		}
	}
	return true
}

// findingLocators reads the `file` (with `line`) and `location` fields of a
// structured finding. A plain-text finding names no file.
func findingLocators(finding string) []findingLocator {
	var fields map[string]any
	if err := json.Unmarshal([]byte(finding), &fields); err != nil {
		return nil
	}
	location, _ := fields["location"].(string)
	fromLocation := locationLocators(location)
	file, _ := fields["file"].(string)
	file = cleanFindingPath(file)
	if file == "" {
		return fromLocation
	}
	locator := findingLocator{path: file}
	locator.start, locator.end = findingLineRange(fields["line"])
	if locator.start == 0 {
		for _, candidate := range fromLocation {
			if candidate.path == file {
				locator.start, locator.end = candidate.start, candidate.end
				break
			}
		}
	}
	return []findingLocator{locator}
}

var findingLineNumber = regexp.MustCompile(`\d+`)

// locationLocators parses locators such as `a/b.go:10`, `a/b.go:10-20`,
// `a/b.go#L10-L20` or `a/b.go:74-87,115-200`, separated by `;` or spaces. A
// token that does not look like a repository path is not a locator.
func locationLocators(location string) []findingLocator {
	var locators []findingLocator
	for _, token := range strings.FieldsFunc(location, func(r rune) bool { return r == ';' || r == ' ' || r == '\t' || r == '\n' }) {
		token = strings.TrimRight(token, ",.)")
		path, rest := token, ""
		if cut := strings.IndexAny(token, ":#"); cut >= 0 {
			path, rest = token[:cut], token[cut+1:]
		}
		path = cleanFindingPath(path)
		if path == "" || !strings.ContainsAny(path, "/.") || strings.Contains(path, "://") {
			continue
		}
		locator := findingLocator{path: path}
		for _, number := range findingLineNumber.FindAllString(rest, -1) {
			line, err := strconv.Atoi(number)
			if err != nil || line <= 0 {
				continue
			}
			if locator.start == 0 || line < locator.start {
				locator.start = line
			}
			if line > locator.end {
				locator.end = line
			}
		}
		locators = append(locators, locator)
	}
	return locators
}

// findingLineRange reads a `line` field: a number, or text such as "120" or
// "120-130". Zero means no line.
func findingLineRange(value any) (int, int) {
	switch typed := value.(type) {
	case float64:
		if typed >= 1 {
			return int(typed), int(typed)
		}
	case string:
		start, end := 0, 0
		for _, number := range findingLineNumber.FindAllString(typed, -1) {
			line, err := strconv.Atoi(number)
			if err != nil || line <= 0 {
				continue
			}
			if start == 0 || line < start {
				start = line
			}
			if line > end {
				end = line
			}
		}
		return start, end
	}
	return 0, 0
}

func cleanFindingPath(path string) string {
	path = strings.TrimSpace(path)
	for strings.HasPrefix(path, "./") {
		path = path[2:]
	}
	return path
}
