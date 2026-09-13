package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
)

// Staged review dispatch (#1821, #1823).
//
// A staged review splits one review into two chained job stages: a cheap
// PREFLIGHT parent that answers "can this review be performed here", and a
// VERDICT child whose result is the review.
//
// THE PARENT AGENT EMITS THE DELEGATION, NOT THIS DISPATCHER. JobRequest carries
// no Delegations: a fan-out comes from the parent's own gitmoot_result and is
// expanded by the engine. That is why this file is prompt-and-refusal plumbing
// rather than an enqueue of two rows, and it is the better shape - the child
// necessarily arrives through ensureDelegatedReviewEvidence, which already
// refuses a parent whose children crashed, are unrecognised, are abstaining or
// are parked, and already refuses a fan-out that declared delegations and
// produced ZERO children. Staging inherits a defended chokepoint instead of
// inventing one.
//
// So the three guards of the non-fallback clause are all already in the tree:
//   - the parent's result is a fan-out, so it can NEVER be a verdict (#1685);
//   - a declared-but-absent child is an unfilled slot, not a pass;
//   - a runtime that cannot execute terminates BLOCKED, and a blocked child
//     makes the gate refuse the parent (#2022).
//
// This file adds the fourth: a verdict stage that cannot be NAMED refuses the
// dispatch, rather than letting the preflight pick a reviewer or answer alone.

// stagedReviewRefusalError is returned when a repo has declared a staged review
// but the declaration cannot be honoured. It refuses BEFORE the job row exists,
// which is the same judgement as #1819's foreign-commit arm and #1817's
// dispatch precondition: a dispatch that cannot possibly produce a valid review
// should not become a job somebody later has to interpret.
type stagedReviewRefusalError struct {
	Repo   string
	Agent  string
	Reason string
}

func (e stagedReviewRefusalError) Error() string {
	return fmt.Sprintf("staged review refused for %s: declared verdict agent %q %s; "+
		"a strong reviewer that cannot be identified must never degrade to a cheap-stage approval. "+
		"Fix [repos.%q] staged_review_verdict_agent, or remove it to run an unstaged review",
		e.Repo, e.Agent, e.Reason, e.Repo)
}

// resolveStagedReviewVerdictAgent reports the verdict-stage agent for a review
// dispatch, or refuses.
//
// Three outcomes, and the caller must be able to tell them apart:
//   - ("", false, nil): this repo has NOT declared a staged review. The caller
//     runs today's single-stage review, byte-identically.
//   - (name, true, nil): staged, and the named agent exists and can review.
//   - ("", false, err): staged was declared and CANNOT be honoured. Refuse.
//
// The undeclared case is deliberately not an error. Declaring nothing is how a
// repo opts out, and the owner's ruling that "undeclared behaves as refuse"
// governs the choice WITHIN a staged review - a strong reviewer that cannot be
// named must not be silently replaced - not whether unstaged reviews may run.
func resolveStagedReviewVerdictAgent(ctx context.Context, store *db.Store, paths config.Paths, repo string) (string, bool, error) {
	declared, ok, err := config.StagedReviewVerdictAgent(paths, repo)
	if err != nil {
		// #2029 review, P1. AN UNREADABLE CONFIG IS A REFUSAL; ONLY AN ABSENT ONE
		// IS "UNSTAGED".
		//
		// The previous code collapsed every error into unstaged, with the
		// argument that a read failure "says nothing about whether a declaration
		// exists". That is true and it is the reason to refuse, not to proceed:
		// LoadStagedReview parses the WHOLE config, so a malformed staged value
		// for ANOTHER repository - or an unreadable file - hides a valid
		// declaration for this one, and the review then runs on the cheap agent
		// with no marker. That is the silent fallback the campaign's non-fallback
		// rule exists to forbid, reached through a config typo elsewhere.
		//
		// A genuinely missing config is different in kind: it cannot be hiding a
		// declaration, and treating it as unstaged is what keeps a fresh home and
		// every repo that never opted in byte-identical.
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, stagedReviewRefusalError{Repo: repo, Agent: "",
			Reason: fmt.Sprintf("cannot be resolved because the config could not be read: %v", err)}
	}
	if !ok {
		// No declaration for this repo in a config that parsed. This is the opt-out.
		return "", false, nil
	}
	if store == nil {
		return "", false, stagedReviewRefusalError{Repo: repo, Agent: declared, Reason: "cannot be resolved without an agent store"}
	}
	agent, err := store.GetAgent(ctx, declared)
	if err != nil {
		return "", false, stagedReviewRefusalError{Repo: repo, Agent: declared, Reason: "is not a registered agent"}
	}
	if !agentHasCapability(agent.Capabilities, "review") {
		return "", false, stagedReviewRefusalError{Repo: repo, Agent: declared,
			Reason: "is registered but does not carry the review capability"}
	}
	return declared, true, nil
}

// stagedReviewPreflightInstructions is the block appended to a PREFLIGHT
// stage's prompt. It is what makes the parent emit exactly one delegation,
// because the dispatcher cannot emit it.
//
// IT CONTAINS NO COMMIT-SHAPED TOKEN, and that is a constraint rather than an
// observation: #1819's dispatch scan refuses a review prompt naming a commit
// outside the pull request's history, and promptCommitTokenRE matches 7 to 64
// hex characters. An agent name is not hex-shaped, and no head, SHA or diff
// excerpt is interpolated here.
func stagedReviewPreflightInstructions(verdictAgent string) string {
	return "\n\n## STAGED REVIEW: you are the PREFLIGHT stage (#1821)\n\n" +
		"You are NOT the reviewer. Your result is a FAN-OUT and can never be a verdict, whichever way it points, " +
		"so do not approve, do not request changes, and do not record findings as if you had reviewed the change.\n\n" +
		"Answer ONE question: can a review be performed in this worktree at all. Concretely, check that the " +
		"requested commit is present, that the worktree is clean at that commit, that the runtime auth resolves, " +
		"and that the verification commands a reviewer would run can actually START.\n\n" +
		"Then declare EXACTLY ONE delegation, to the agent `" + verdictAgent + "`, whose result is the review. " +
		"Pass it your findings about what can and cannot be executed here.\n\n" +
		"Set `evidence` honestly to what YOU could execute: `executed` if the verification commands ran, " +
		"`static_only` if they could not. That value becomes a CEILING on the verdict stage - it cannot claim to " +
		"have executed work you found unrunnable in this same worktree - so an inaccurate value here either " +
		"understates a real review or licenses a claim nothing supports.\n\n" +
		"If a review CANNOT be performed here, say so and delegate nothing. An unfilled verdict slot is a refusal " +
		"the merge gate understands; a cheap-stage approval is not.\n"
}
