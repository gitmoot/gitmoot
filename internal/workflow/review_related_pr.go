package workflow

import (
	"fmt"
	"strconv"
)

// ReviewRelatedPRsDir is the directory, relative to a review's workspace root,
// under which the exact head trees of related pull requests are materialized
// (#2509). A local review gets it inside its exact-head worktree and a remote
// E2B review inside its sandbox workspace, so a reviewer finds the same path
// either way.
const ReviewRelatedPRsDir = ".gitmoot-related"

// ReviewRelatedPR is one same-repository pull request whose exact head tree a
// review receives read-only (#2509). HeadSHA is resolved on the host when the
// review is requested; the tree delivered at run time is that commit's, even if
// the pull request has moved since. TreeSHA256 is the digest of that tree's
// paths, kinds and contents, pinned at dispatch and re-measured where the tree
// is installed and again after the review ran, so a tree changed under the
// reviewer fails the review instead of backing its verdict.
type ReviewRelatedPR struct {
	PR         int    `json:"pr"`
	HeadSHA    string `json:"head_sha"`
	TreeSHA256 string `json:"tree_sha256,omitempty"`
}

// Path is where the related tree sits, relative to the review workspace root.
func (r ReviewRelatedPR) Path() string {
	return ReviewRelatedPRsDir + "/pr-" + strconv.Itoa(r.PR)
}

// ValidateReviewRelatedPRNumbers checks requested related pull request numbers
// against the reviewed one: every number must be positive, appear once, and
// name a pull request other than the one under review.
func ValidateReviewRelatedPRNumbers(reviewed int, related []int) error {
	seen := make(map[int]bool, len(related))
	for _, number := range related {
		switch {
		case number <= 0:
			return fmt.Errorf("--related-pr %d: a pull request number must be positive", number)
		case number == reviewed:
			return fmt.Errorf("--related-pr %d is the pull request under review; name a different pull request", number)
		case seen[number]:
			return fmt.Errorf("--related-pr %d is given more than once", number)
		}
		seen[number] = true
	}
	return nil
}
