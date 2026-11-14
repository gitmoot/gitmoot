package workflow

import (
	"strconv"
	"strings"
)

// ReviewRecordPRNumber reads the pull request number out of a review record
// (task) id, "review-pr-<number>-<suffix>"; successors keep the prefix. It
// reports false when the id carries no positive number and non-empty suffix
// (old adhoc-/task- rows).
func ReviewRecordPRNumber(id string) (int64, bool) {
	remainder, ok := strings.CutPrefix(strings.TrimSpace(id), "review-pr-")
	if !ok {
		return 0, false
	}
	numberText, suffix, ok := strings.Cut(remainder, "-")
	if !ok || strings.TrimSpace(suffix) == "" {
		return 0, false
	}
	number, err := strconv.ParseInt(numberText, 10, 64)
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}
