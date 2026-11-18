package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// A FOLLOW-UP ROUND IS SCOPED TO THE PULL REQUEST'S OWN CHANGE (#2682).
//
// The raw range from the head a reviewer last saw to the current head also
// carries everything a merge of the base branch brought in. Reviewing that as
// this pull request's change put other PRs' code in front of the reviewer, its
// findings there blocked this PR, and every later merge by any seat added more:
// energy-finance-map#759 handed the reviewer 66 files for a 10-file PR.
//
// So both ends are measured against their own merge base with the base branch,
// mbPrev = merge-base(origin/<base>, prev) and mbHead = merge-base(origin/<base>,
// head), and a file is in scope when its own patch differs between the two. A
// file that differs between prev and head only because the base moved under it
// has the same own patch at both ends and drops out. With no base merge between
// the rounds (mbPrev == mbHead) the list is exactly prev...head, as before.

// refreshReviewScopeOrigin brings origin/<base> current before a merge base is
// taken from it. A stale ref resolves an OLDER merge base, which hands the
// reviewer the base commits merged in since as if this pull request wrote them,
// the very defect this scope exists to remove. Tests whose origin is a GitHub
// URL that must not be fetched replace it.
var refreshReviewScopeOrigin = func(ctx context.Context, git gitutil.Client) error {
	return refreshRemoteReviewOrigin(ctx, git, git.Dir())
}

// reviewScopeSubject resolves, lazily and at most once, the forge facts a
// follow-up scope needs beyond local objects: the base branch when the cached
// pull request row has none, and the merge commit of a merged pull request.
type reviewScopeSubject struct {
	baseBranch string
	gh         github.Client
	repo       github.Repository
	number     int
	pull       *github.PullRequest
}

// newReviewScopeSubject reads the cached base branch of a pull request. A
// missing row is not an error: the forge answers when the base is needed.
func newReviewScopeSubject(ctx context.Context, store *db.Store, gh github.Client, repo github.Repository, number int) (*reviewScopeSubject, error) {
	subject := &reviewScopeSubject{gh: gh, repo: repo, number: number}
	pull, err := store.GetPullRequest(ctx, repo.FullName(), int64(number))
	switch {
	case err == nil:
		subject.baseBranch = strings.TrimSpace(pull.BaseBranch)
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, err
	}
	return subject, nil
}

func (s *reviewScopeSubject) forgePull(ctx context.Context) (github.PullRequest, error) {
	if s.pull != nil {
		return *s.pull, nil
	}
	if s.gh == nil || s.number <= 0 {
		return github.PullRequest{}, errors.New("review scope: no GitHub client to read the pull request")
	}
	pull, err := s.gh.GetPullRequest(ctx, s.repo, int64(s.number))
	if err != nil {
		return github.PullRequest{}, fmt.Errorf("review scope: load PR #%d: %w", s.number, err)
	}
	s.pull = &pull
	return pull, nil
}

func (s *reviewScopeSubject) base(ctx context.Context) (string, error) {
	if s.baseBranch != "" {
		return s.baseBranch, nil
	}
	pull, err := s.forgePull(ctx)
	if err != nil {
		return "", err
	}
	if s.baseBranch = strings.TrimSpace(pull.BaseRef); s.baseBranch == "" {
		return "", fmt.Errorf("review scope: PR #%d has no base branch", s.number)
	}
	return s.baseBranch, nil
}

// localReviewScopeDiff scopes a follow-up round from the daemon's own checkout,
// where every enumeration is uncapped. A ReviewScopeUnavailableError return is
// a PROOF the round is not a direct follow-up (the reviewed head is not an
// ancestor of the current head); any other error means the instrument could
// not run and the caller falls back.
func localReviewScopeDiff(ctx context.Context, git gitutil.Client, subject *reviewScopeSubject, prev, head string) (workflow.ReviewScopeDiff, error) {
	prev, head = strings.TrimSpace(prev), strings.TrimSpace(head)
	if prev == "" || head == "" {
		return workflow.ReviewScopeDiff{}, errors.New("review scope: local enumeration needs both heads")
	}
	if err := ensureReviewScopeCommits(ctx, git, subject.number, prev, head); err != nil {
		return workflow.ReviewScopeDiff{}, err
	}
	// `git merge-base --is-ancestor X X` succeeds, so this also accepts the
	// identical-heads range the compare endpoint reports as "identical".
	ancestor, err := git.IsAncestor(ctx, prev, head)
	if err != nil {
		return workflow.ReviewScopeDiff{}, err
	}
	if !ancestor {
		return workflow.ReviewScopeDiff{}, workflow.ReviewScopeUnavailableError{
			Reason: fmt.Sprintf(
				"review scope %s...%s is not a direct follow-up: the reviewed head is not an ancestor of the current head",
				prev, head),
		}
	}
	baseBranch, err := subject.base(ctx)
	if err != nil {
		return workflow.ReviewScopeDiff{}, err
	}
	if err := refreshReviewScopeOrigin(ctx, git); err != nil {
		return workflow.ReviewScopeDiff{}, fmt.Errorf("review scope: refresh origin before resolving the %s merge base: %w", baseBranch, err)
	}
	baseRef := "origin/" + baseBranch
	mbPrev, err := git.MergeBase(ctx, baseRef, prev)
	if err != nil {
		return workflow.ReviewScopeDiff{}, fmt.Errorf("review scope: merge base of %s and %s: %w", baseRef, prev, err)
	}
	mbHead, err := git.MergeBase(ctx, baseRef, head)
	if err != nil {
		return workflow.ReviewScopeDiff{}, fmt.Errorf("review scope: merge base of %s and %s: %w", baseRef, head, err)
	}
	if mbHead == head {
		// The head is already inside the base branch: a pull request merged
		// with a true merge commit. Both ends are measured from the base as it
		// stood when it landed, exactly as its full review is (#2327).
		pull, err := subject.forgePull(ctx)
		if err != nil {
			return workflow.ReviewScopeDiff{}, err
		}
		if mbHead, err = mergedPullRequestDiffBase(ctx, git, pull, head); err != nil {
			return workflow.ReviewScopeDiff{}, fmt.Errorf("review scope: merged PR #%d diff base: %w", subject.number, err)
		}
		if mbPrev, err = mergedPullRequestDiffBase(ctx, git, pull, prev); err != nil {
			return workflow.ReviewScopeDiff{}, fmt.Errorf("review scope: merged PR #%d prior diff base: %w", subject.number, err)
		}
	}
	headPatches, err := git.DiffPatches(ctx, mbHead, head)
	if err != nil {
		return workflow.ReviewScopeDiff{}, err
	}
	priorPatches, err := git.DiffPatches(ctx, mbPrev, prev)
	if err != nil {
		return workflow.ReviewScopeDiff{}, err
	}
	var changed []string
	if mbPrev == mbHead {
		// No base merge between the rounds: the scope is the round's own range.
		if changed, err = git.ChangedFiles(ctx, prev, head); err != nil {
			return workflow.ReviewScopeDiff{}, err
		}
	} else {
		changed = movedOwnPatches(priorPatches, headPatches)
	}
	priorLines := make(map[string][]workflow.ReviewLineRange, len(priorPatches))
	for path, patch := range priorPatches {
		priorLines[path] = patchLineRanges(patch)
	}
	return workflow.ReviewScopeDiff{
		ChangedFiles:  sortedUniqueReviewPaths(changed),
		MergeBase:     mbHead,
		OwnFiles:      sortedPatchPaths(headPatches),
		PriorOwnLines: priorLines,
	}, nil
}

// forgeReviewScopeDiff is the API fallback for a daemon with no usable
// checkout. It fails CLOSED on any capped compare page, and on a merged head,
// whose own change the compare endpoint cannot measure: either costs one full
// review, never an incomplete or misplaced scope.
func forgeReviewScopeDiff(ctx context.Context, gh github.Client, repo github.Repository, subject *reviewScopeSubject, prev, head string) (workflow.ReviewScopeDiff, error) {
	roundFiles, err := forgeReviewChangedFiles(ctx, gh, repo, prev, head)
	if err != nil {
		return workflow.ReviewScopeDiff{}, err
	}
	baseBranch, err := subject.base(ctx)
	if err != nil {
		return workflow.ReviewScopeDiff{}, err
	}
	prior, mbPrev, err := forgeOwnChange(ctx, gh, repo, baseBranch, prev)
	if err != nil {
		return workflow.ReviewScopeDiff{}, err
	}
	current, mbHead, err := forgeOwnChange(ctx, gh, repo, baseBranch, head)
	if err != nil {
		return workflow.ReviewScopeDiff{}, err
	}
	changed := roundFiles
	if mbPrev != mbHead {
		changed = movedForgeOwnPatches(prior, current)
	}
	priorLines := make(map[string][]workflow.ReviewLineRange, len(prior))
	for path, file := range prior {
		priorLines[path] = patchLineRanges(file.Patch)
	}
	ownFiles := make([]string, 0, len(current))
	for path := range current {
		ownFiles = append(ownFiles, path)
	}
	return workflow.ReviewScopeDiff{
		ChangedFiles:  sortedUniqueReviewPaths(changed),
		MergeBase:     mbHead,
		OwnFiles:      sortedUniqueReviewPaths(ownFiles),
		PriorOwnLines: priorLines,
	}, nil
}

func forgeOwnChange(ctx context.Context, gh github.Client, repo github.Repository, baseBranch, head string) (map[string]github.PullRequestFile, string, error) {
	compare, err := gh.CompareCommits(ctx, repo, baseBranch, head)
	if err != nil {
		return nil, "", err
	}
	if compare.Truncated {
		return nil, "", workflow.ReviewScopeUnavailableError{
			Reason: fmt.Sprintf("review scope compare %s...%s is capped at %d files and no local checkout could prove it complete", baseBranch, head, len(compare.Files)),
		}
	}
	mergeBase := strings.ToLower(strings.TrimSpace(compare.MergeBaseCommit.SHA))
	if mergeBase == "" {
		return nil, "", workflow.ReviewScopeUnavailableError{Reason: fmt.Sprintf("review scope compare %s...%s named no merge base", baseBranch, head)}
	}
	if mergeBase == strings.ToLower(strings.TrimSpace(head)) {
		return nil, "", workflow.ReviewScopeUnavailableError{
			Reason: fmt.Sprintf("review scope head %s is already inside %s; its own change needs a local checkout", head, baseBranch),
		}
	}
	files := make(map[string]github.PullRequestFile, len(compare.Files))
	for _, file := range compare.Files {
		files[file.Filename] = file
	}
	return files, mergeBase, nil
}

// movedOwnPatches names every path whose own patch differs between the two
// rounds, a path present at one end only included.
func movedOwnPatches(prior, current map[string]string) []string {
	var moved []string
	for path, patch := range current {
		if before, ok := prior[path]; !ok || normalizedOwnPatch(before) != normalizedOwnPatch(patch) {
			moved = append(moved, path)
		}
	}
	for path := range prior {
		if _, ok := current[path]; !ok {
			moved = append(moved, path)
		}
	}
	return moved
}

// movedForgeOwnPatches is movedOwnPatches over compare files. The API omits the
// patch of a binary or very large file, and an absent patch proves nothing, so
// such a file is always in scope.
func movedForgeOwnPatches(prior, current map[string]github.PullRequestFile) []string {
	var moved []string
	for path, file := range current {
		before, ok := prior[path]
		if !ok || file.Patch == "" || before.Patch == "" || before.Status != file.Status ||
			before.PreviousFilename != file.PreviousFilename || normalizedOwnPatch(before.Patch) != normalizedOwnPatch(file.Patch) {
			moved = append(moved, path)
		}
	}
	for path := range prior {
		if _, ok := current[path]; !ok {
			moved = append(moved, path)
		}
	}
	return moved
}

// normalizedOwnPatch strips what a base merge changes in a patch without
// changing the pull request's own edit: hunk line numbers and section headings,
// and the index line naming pre- and post-image blobs. A binary patch keeps its
// index line, the only thing that tells two binary changes apart. Context lines
// stay, so an own edit whose neighbouring base lines moved stays in scope.
func normalizedOwnPatch(patch string) string {
	binary := strings.Contains(patch, "\nBinary files ") || strings.HasPrefix(patch, "Binary files ")
	var normalized strings.Builder
	normalized.Grow(len(patch))
	inHunk := false
	for len(patch) > 0 {
		line := patch
		if cut := strings.IndexByte(patch, '\n'); cut >= 0 {
			line, patch = patch[:cut+1], patch[cut+1:]
		} else {
			patch = ""
		}
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
			normalized.WriteString("@@\n")
		case !inHunk && !binary && strings.HasPrefix(line, "index "):
		default:
			normalized.WriteString(line)
		}
	}
	return normalized.String()
}

var patchHunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// patchLineRanges returns the post-image line ranges a patch's hunks cover,
// context included, or nil when the patch has no hunks (binary, mode-only, or
// a patch the forge omitted): such a file has no line a finding can be outside.
func patchLineRanges(patch string) []workflow.ReviewLineRange {
	var ranges []workflow.ReviewLineRange
	for _, line := range strings.Split(patch, "\n") {
		match := patchHunkHeader.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		start, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		count := 1
		if match[2] != "" {
			if count, err = strconv.Atoi(match[2]); err != nil {
				continue
			}
		}
		if count == 0 {
			// A pure deletion sits between line start and start+1.
			ranges = append(ranges, workflow.ReviewLineRange{Start: start, End: start + 1})
			continue
		}
		ranges = append(ranges, workflow.ReviewLineRange{Start: start, End: start + count - 1})
	}
	return ranges
}

func sortedPatchPaths(patches map[string]string) []string {
	paths := make([]string, 0, len(patches))
	for path := range patches {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}
