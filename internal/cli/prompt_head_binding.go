package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	gitutil "github.com/gitmoot/gitmoot/internal/git"
)

// #1819. A review dispatch whose PROMPT names a commit other than --head-sha
// used to record a prompt_head_warning and run anyway, against the dispatch head
// with the wrong instructions. For a review that is the one job type where the
// instructions ARE the specification: a review that reads PR #1810's diff
// against PR #1783's brief produces a confident, wrong, gate-eligible verdict.
//
// THE GUARD IS NOT "REFUSE IF ANY OTHER SHA APPEARS", and the issue thread has
// six measured dispatches proving why. Naming another commit is how a prompt
// states provenance: the previous round's head, the branch base, a retracted
// head cited under a do-not-trust heading. One of those was nearly destroyed on
// the strength of the warnings alone. The rule below binds the prompt's review
// TARGET to the dispatch head instead.
//
// RE-DERIVED ON REAL DATA before implementing, over every prompt_head_warning
// this store holds for a gitmoot/gitmoot review job: 911 events, 644 distinct
// (cited, head, pull request) citations. 282 are ancestors of the dispatch head,
// 246 are recorded heads of the same pull request but NOT ancestors, 18 are
// neither with both objects present, and 98 cannot be classified because an
// object is absent from the dispatching clone. So:
//
//   - REFUSING ON ANY NON-EQUAL TOKEN would refuse 644 of 644.
//   - The recorded-head arm is NOT the residual force-push corner the issue
//     describes. It is 38% of all citations, and dropping it refuses 362.
//   - UNDETERMINABLE NEEDS ITS OWN ARM, which the issue's four-arm ordering does
//     not have. Folding it into refuse turns 18 refusals into 116, and 72 of
//     those 98 are undeterminable because the DISPATCH HEAD is missing from the
//     clone rather than the cited sha - a fact about clone freshness, not about
//     the citation. That is the same "wrong side of the boundary" error #1817
//     was about, so it fails OPEN.
type promptCommitRelation string

const (
	// promptCommitIsHead is the dispatch head itself. Silent, as today.
	promptCommitIsHead promptCommitRelation = "the dispatch head"
	// promptCommitIsAncestor covers prior heads on this branch AND the branch
	// base. The base is its own category: it was never a head of the pull
	// request, so a recorded-head check alone misses it.
	promptCommitIsAncestor promptCommitRelation = "an ancestor of the dispatch head"
	// promptCommitIsPriorHead is a head this pull request recorded at some
	// dispatch. Append-only, so it survives a force push that leaves the commit
	// unreachable.
	promptCommitIsPriorHead promptCommitRelation = "a recorded head of this pull request"
	// promptCommitIsForeign is the defect: not the head, not reachable from it,
	// never a head of this pull request, and both objects resolve so the answer
	// is real rather than absent.
	promptCommitIsForeign promptCommitRelation = "not the dispatch head, not an ancestor of it, and never a recorded head of this pull request"
	// promptCommitUnresolved means the dispatching checkout could not resolve one
	// of the two commits, so no relationship could be established. Never refuses.
	promptCommitUnresolved promptCommitRelation = "unresolvable in the dispatching checkout, so its relationship to the head could not be established"
)

// recordedPullRequestHeadSource is the store surface this guard needs. It is an
// interface so the classifier can be driven without a database, and so the
// prompt-scoped nature of the query is visible at the seam.
type recordedPullRequestHeadSource interface {
	RecordedPullRequestHeads(ctx context.Context, repo string, pullRequest int) ([]string, error)
	PullRequestsForRecordedHead(ctx context.Context, repo string, prefix string) ([]int, error)
}

type promptCommitCitation struct {
	// token is the text as the prompt wrote it, which is what an operator will
	// search for when they go and read their own prompt.
	token    string
	resolved string
	relation promptCommitRelation
	// ownedBy names the pull requests that recorded this sha as a head, so a
	// refusal can say "that is #1810's head" instead of "unknown commit".
	ownedBy []int
}

// classifyPromptCommitCitations resolves every commit-shaped token in a prompt
// against the dispatch head and the store's recorded heads.
//
// It returns nothing when the dispatch head cannot be resolved: with no head
// there is no relationship to establish, and this is the existing
// promptHeadContradictionWarnings behaviour rather than a new fail-open.
func classifyPromptCommitCitations(ctx context.Context, git gitutil.Client, heads recordedPullRequestHeadSource, prompt string, dispatchHead string, repo string, pullRequest int) []promptCommitCitation {
	if strings.TrimSpace(prompt) == "" {
		return nil
	}
	headRef := strings.TrimSpace(dispatchHead)
	if headRef == "" {
		headRef = "HEAD"
	}
	resolvedHead, headErr := git.RevParse(ctx, headRef+"^{commit}")
	resolvedHead = strings.ToLower(strings.TrimSpace(resolvedHead))
	if headErr != nil || resolvedHead == "" {
		return nil
	}

	var recorded []string
	if heads != nil {
		if stored, err := heads.RecordedPullRequestHeads(ctx, repo, pullRequest); err == nil {
			recorded = stored
		}
	}

	citations := make([]promptCommitCitation, 0)
	seen := make(map[string]struct{})
	for _, token := range promptCommitTokenRE.FindAllString(prompt, -1) {
		lowered := strings.ToLower(strings.TrimSpace(token))
		if lowered == "" {
			continue
		}
		// The DEDUP KEY IS THE TOKEN, not the resolved commit, because an
		// unresolvable token has nothing to resolve to and every one of them
		// would otherwise collapse into a single entry.
		if _, duplicate := seen[lowered]; duplicate {
			continue
		}
		seen[lowered] = struct{}{}

		citation := promptCommitCitation{token: token}
		resolvedToken, tokenErr := git.RevParse(ctx, token+"^{commit}")
		citation.resolved = strings.ToLower(strings.TrimSpace(resolvedToken))
		// ANCESTRY IS ANSWERED BEFORE THE SWITCH so an INSTRUMENT FAILURE cannot
		// fall through to the foreign arm. IsAncestor already reads git's exit 1 as
		// the ordinary false, so a non-nil error is a broken repository, an invalid
		// ref or a cancelled context. Refusing a dispatch because the instrument
		// failed is the one direction this guard must never take, so an
		// undeterminable answer ends at promptCommitUnresolved, which allows.
		ancestor, ancestryKnown := false, false
		if tokenErr == nil && citation.resolved != "" {
			ancestor, ancestryKnown = promptCitationAncestry(ctx, git, citation.resolved, resolvedHead)
		}
		switch {
		case tokenErr != nil || citation.resolved == "":
			// A recorded head still identifies the citation even when git cannot
			// resolve it, which is the whole point of the append-only arm: after a
			// force push the object may be gone while the store still knows it.
			if matchesRecordedHead(lowered, recorded) {
				citation.relation = promptCommitIsPriorHead
			} else {
				citation.relation = promptCommitUnresolved
			}
		case citation.resolved == resolvedHead:
			citation.relation = promptCommitIsHead
		case ancestor:
			citation.relation = promptCommitIsAncestor
		case matchesRecordedHead(citation.resolved, recorded) || matchesRecordedHead(lowered, recorded):
			citation.relation = promptCommitIsPriorHead
		case !ancestryKnown:
			citation.relation = promptCommitUnresolved
		default:
			citation.relation = promptCommitIsForeign
		}
		if citation.relation == promptCommitIsForeign && heads != nil {
			prefix := citation.resolved
			if prefix == "" {
				prefix = lowered
			}
			if owners, err := heads.PullRequestsForRecordedHead(ctx, repo, prefix); err == nil {
				for _, owner := range owners {
					if owner != pullRequest {
						citation.ownedBy = append(citation.ownedBy, owner)
					}
				}
				sort.Ints(citation.ownedBy)
			}
		}
		citations = append(citations, citation)
	}
	return citations
}

// promptCitationAncestry answers reachability AND whether the answer was
// established at all.
//
// IsAncestor already reads git's exit 1 as the ordinary "not an ancestor", so a
// non-nil error is a broken repository, an invalid ref or a cancelled context.
// The second return exists so the caller can distinguish that from a real false:
// collapsing them lets an instrument failure refuse a dispatch, which is the one
// direction this guard must never take.
func promptCitationAncestry(ctx context.Context, git gitutil.Client, ancestor string, descendant string) (reachable bool, known bool) {
	reachable, err := git.IsAncestor(ctx, ancestor, descendant)
	if err != nil {
		return false, false
	}
	return reachable, true
}

// matchesRecordedHead compares by PREFIX in both directions, because a prompt
// cites abbreviated SHAs while the store records full ones.
func matchesRecordedHead(candidate string, recorded []string) bool {
	candidate = strings.ToLower(strings.TrimSpace(candidate))
	if candidate == "" {
		return false
	}
	for _, head := range recorded {
		head = strings.ToLower(strings.TrimSpace(head))
		if head == "" {
			continue
		}
		if strings.HasPrefix(head, candidate) || strings.HasPrefix(candidate, head) {
			return true
		}
	}
	return false
}

// reviewPromptTargetMismatchError refuses a review dispatch whose prompt cites a
// commit that is not this pull request's history at all. Only
// promptCommitIsForeign refuses; every other relation, including unresolvable,
// returns nil.
//
// The message names BOTH SHAs because the operator has to find the citation in
// their own prompt, and it names the owning pull request when the store knows
// it: "that sha is #1810's head" is a diagnosis, "unknown commit referenced" is
// a shrug. It also names the escape, because a refusal an operator cannot get
// past is answered by dropping --head-sha, which removes the binding this guard
// exists to protect.
func reviewPromptTargetMismatchError(citations []promptCommitCitation, dispatchHead string) error {
	var foreign []promptCommitCitation
	for _, citation := range citations {
		if citation.relation == promptCommitIsForeign {
			foreign = append(foreign, citation)
		}
	}
	if len(foreign) == 0 {
		return nil
	}
	described := make([]string, 0, len(foreign))
	for _, citation := range foreign {
		detail := fmt.Sprintf("%s is %s", citation.token, citation.relation)
		if len(citation.ownedBy) > 0 {
			owners := make([]string, 0, len(citation.ownedBy))
			for _, owner := range citation.ownedBy {
				owners = append(owners, fmt.Sprintf("#%d", owner))
			}
			detail = fmt.Sprintf("%s is a recorded head of %s, not of this pull request", citation.token, strings.Join(owners, ", "))
		}
		described = append(described, detail)
	}
	return fmt.Errorf(
		"review prompt names a commit outside this pull request's history, so the instructions and the dispatch head describe different changes: %s; the dispatch head is %s. A review's instructions are its specification, so this is refused before any job row exists. Cite a prior head of this pull request, its base, or the head itself; or pass --allow-prompt-head-mismatch to dispatch it deliberately",
		strings.Join(described, "; "), strings.TrimSpace(dispatchHead))
}

// retainUnjudgedPromptHeadWarnings drops the warnings a review's own refusal
// has already judged (#2054).
//
// It filters rather than replaces the scan, so the scan keeps happening on the
// allocated exact-head worktree where it is documented to happen, and only the
// EMISSION narrows. A warning survives when its cited commit could not be
// classified at all: nobody has judged that one, so the operator is the last
// check. Every other relation - the head, an ancestor of it, a recorded head of
// this pull request - is what a prompt states deliberately, and a warning on the
// routine case teaches its reader to ignore it.
func retainUnjudgedPromptHeadWarnings(
	ctx context.Context,
	git gitutil.Client,
	heads recordedPullRequestHeadSource,
	prompt string,
	dispatchHead string,
	repo string,
	pullRequest int,
	warnings []string,
) []string {
	if len(warnings) == 0 {
		return warnings
	}
	unjudged := make([]string, 0, len(warnings))
	for _, citation := range classifyPromptCommitCitations(ctx, git, heads, prompt, dispatchHead, repo, pullRequest) {
		// F4: a RECORDED HEAD THAT IS NOT AN ANCESTOR is the stale-target case,
		// and it is the one relation worth warning about. It means the prompt
		// names a head this pull request really had, which a force push or a
		// reset left off the current history - so a prompt citing it as its
		// review TARGET is reviewing something that is no longer there. An
		// ancestor is different: a scoped re-review cites its own prior head,
		// and that head's content is still under the dispatch head.
		if citation.relation == promptCommitIsPriorHead || citation.relation == promptCommitUnresolved {
			unjudged = append(unjudged, citation.token, citation.resolved)
		}
	}
	if len(unjudged) == 0 {
		return nil
	}
	// ANCHOR ON THE CITATION CLAUSE, never a bare substring. Every warning names
	// the DISPATCH HEAD twice in its own text, so a substring test let an
	// unjudged token retain a DIFFERENT citation's warning whenever that token
	// occurred inside the dispatch sha - and a prompt only has to contain a
	// non-resolving 7-hex run taken from the middle of that sha for the leak to
	// fire, which is why it survived a probe whose prompt happened not to have
	// one. Matching the leading "prompt references commit <token>," ties a
	// warning to the one citation it is about.
	kept := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		for _, token := range unjudged {
			if strings.TrimSpace(token) == "" {
				continue
			}
			if strings.HasPrefix(warning, promptHeadWarningCitationPrefix+token+",") {
				kept = append(kept, warning)
				break
			}
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}
