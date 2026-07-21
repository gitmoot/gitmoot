package workflow

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
)

// delegationBranchName derives the git branch for a delegated implement job. The
// branch is always namespaced with the parent-short and delegation id
// (gitmoot-delegation-<parent-short>-<id>) so sibling delegations from the same
// parent never collide, even when they share an identical or empty worktree
// hint. A slugged form of the delegation's requested worktree label, when
// present, is appended only as a human-readable suffix and never replaces the
// namespacing. retryAttempt > 0 adds a -retry-<n> suffix so a retry of the same
// delegation gets a fresh, isolated branch instead of reusing the failed
// attempt's branch.
func delegationBranchName(d Delegation, parentJobID string, delegationID string, retryAttempt int) string {
	branch := fmt.Sprintf("gitmoot-delegation-%s-%s", parentShort(parentJobID), delegationID)
	if hint := slug(d.Worktree); hint != "" {
		branch += "-" + hint
	}
	if retryAttempt > 0 {
		branch += fmt.Sprintf("-retry-%d", retryAttempt)
	}
	return branch
}

// slug normalizes an arbitrary label into a lowercase, dash-separated token
// safe for use in branch names. Mirrors internal/cli/workflow.go::slug.
func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastDash := false
	for _, char := range value {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			builder.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash && builder.Len() > 0 {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

// parentShort returns the first eight hex characters of the SHA-1 of the parent
// job id, giving a stable short identifier for delegation branch names.
func parentShort(parentJobID string) string {
	sum := sha1.Sum([]byte(parentJobID))
	return hex.EncodeToString(sum[:])[:8]
}
