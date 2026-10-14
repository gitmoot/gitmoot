package workflow

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryObligationBriefSiteAlsoAppendsTheFindingsLogBrief is a SOURCE-LEVEL
// guard, which is unusual enough to justify.
//
// The two briefs are siblings: both are appended to a review's instructions, and
// both are useless to a review that never receives them. #1969 already fixed
// exactly this omission once, for the obligation brief, after it reached some
// dispatch paths and not others. Round 1 review of #2230 found the findings-log
// brief repeating that history — one call site against the obligation brief's
// three — so a review arriving through the native fan-out or the high-risk lens
// path was never asked for a log, and its partials stayed unrecoverable.
//
// No behavioural test catches this: each path works perfectly, it just silently
// omits an instruction. The only thing that distinguishes "wired everywhere"
// from "wired where I happened to look" is the set of call sites itself.
//
// If a future path appends the obligation brief without the findings-log brief,
// this fails and names the line.
func TestEveryObligationBriefSiteAlsoAppendsTheFindingsLogBrief(t *testing.T) {
	root := ".."
	obligation := regexp.MustCompile(`(ledgerObligationBrief|ReviewObligationBrief)\(`)

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			// Skip definitions, doc comments, and the thin wrapper that merely
			// re-exports the obligation brief - a `return e.ledgerObligationBrief(...)`
			// is a delegation, not a place where review instructions are assembled.
			if strings.Contains(line, "func ") || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "return ") {
				continue
			}
			if !obligation.MatchString(line) {
				continue
			}
			// A call site qualifies if the findings-log brief appears within a few
			// lines either side — the two are appended together.
			if !findingsLogBriefNear(lines, i) {
				offenders = append(offenders, path+":"+itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk returned error: %v", err)
	}
	if len(offenders) != 0 {
		t.Fatalf("these review-instruction sites append the obligation brief but not the findings-log brief, so reviews reaching them are never asked for a log:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func findingsLogBriefNear(lines []string, index int) bool {
	// Wide enough to span a call site and the comment block that explains the
	// second append. Narrower windows flagged this file's own correct call site.
	const window = 20
	start := index - window
	if start < 0 {
		start = 0
	}
	end := index + window
	if end >= len(lines) {
		end = len(lines) - 1
	}
	for i := start; i <= end; i++ {
		if strings.Contains(lines[i], "ReviewFindingsLogBrief()") {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
