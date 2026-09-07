package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestReviewBriefDoesNotTripTheHeadContradictionScan pins an ORDERING that
// nothing else in the tree records, and that is currently correct by accident.
//
// Two changes landed within an hour of each other, by different authors, in the
// same function, with no textual conflict:
//
//	#1819 (PR #1991) classifyPromptCommitCitations + the head-contradiction scan
//	#1969 (PR #1992) the findings-ledger obligation brief appended to the prompt
//
// #1819 scans the prompt for commit-shaped tokens contradicting the dispatch
// head; promptCommitTokenRE matches 7 to 64 hex characters. The brief can inject
// exactly that: LedgerObligationsAtHead renders shortHead(obs.HeadSHA), a
// 12-character prefix of a PRIOR head, into its reason strings, and a finding's
// own title routinely cites a commit. git merged the two happily and CI never
// validated the integrated tree, so the safe order was never a decision.
//
// REVERSED, the cost splits in two, which is why both arms are here:
//
//   - a head recorded for THIS pull request produces a FALSE stale-head warning;
//   - a head belonging to ANOTHER pull request hits #1819's foreign arm and
//     REFUSES THE DISPATCH non-zero, before the job row exists.
//
// The assertion is deliberately a PROPERTY, not a line number: dispatch succeeds
// and records zero prompt_head_warning events even though the prompt ends up
// carrying a real commit that is not the dispatch head. That fails if either
// author reorders, and it needs no maintenance when the file shifts. The form
// was suggested by #1819's author.
func TestReviewBriefDoesNotTripTheHeadContradictionScan(t *testing.T) {
	for _, tc := range []struct {
		name string
		// citedHeadIsForeign selects which #1819 arm the injected token would hit
		// if the ordering regressed: the recorded-head arm (warning) or the
		// foreign arm (hard dispatch refusal).
		citedHeadIsForeign bool
	}{
		{name: "brief cites a head recorded for this pull request"},
		{name: "brief cites a head belonging to another pull request", citedHeadIsForeign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, home := blockerE2EHome(t)
			checkout, firstHead, secondHead := readonlyReviewWorktreeGitCheckout(t)
			seedReviewDispatchFixture(t, store, checkout)
			replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
				return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
			})

			// The cited token must be a REAL commit: the scan resolves each token
			// with RevParse and skips anything git cannot resolve, so an invented
			// hex string would prove nothing.
			//
			// WHICH #1819 ARM IT WOULD HIT depends on whether the commit is a
			// RECORDED head of this pull request, and recorded heads come from
			// jobs' payload head_sha for that PR (store_prompt_heads.go:31), not
			// from findings. So the foreign case needs a real commit that no job
			// recorded, which is why this makes a third one on a side branch
			// rather than reusing firstHead.
			citedCommit := firstHead
			if tc.citedHeadIsForeign {
				runGit(t, checkout, "switch", "-c", "unrelated/branch")
				writeFile(t, filepath.Join(checkout, "unrelated.txt"), "another pull request\n")
				runGit(t, checkout, "add", "unrelated.txt")
				runGit(t, checkout, "commit", "-m", "a commit no job recorded for this pr")
				citedCommit = readonlyWorktreeHead(t, checkout)
				runGit(t, checkout, "switch", "main")
			}

			if _, err := store.RecordReviewFindingObservation(ctx, db.ReviewFindingObservation{
				Repo:        "owner/repo",
				PullRequest: 12,
				HeadSHA:     firstHead,
				ObserverJob: "local-review-prior-round",
				State:       db.FindingOpen,
				Severity:    "P1",
				RoundLabel:  "F1",
				// A reviewer citing the commit a regression came from is ordinary
				// prose, and it is the same injection channel as the brief's own
				// "answered at <shortHead>" reason.
				Title:            "regression introduced in " + citedCommit[:12],
				Detail:           "the fix leg pushed before validating the branch",
				File:             "internal/cli/agent_dispatch.go",
				Line:             519,
				EvidenceKind:     db.EvidenceExecuted,
				ExecutedCommands: []string{"go test ./internal/cli"},
				ExecutedCount:    1,
			}); err != nil {
				t.Fatalf("RecordReviewFindingObservation: %v", err)
			}

			out, err := dispatchLocalAgentJob(ctx, store, reviewDispatchRequest(home, secondHead))
			if err != nil {
				t.Fatalf("dispatch was REFUSED: %v\n\nthat is #1819's foreign arm firing on the brief, which means the brief is now appended before the citation classifier", err)
			}
			job, err := store.GetJob(ctx, out.JobID)
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			payload, err := daemonJobPayload(job)
			if err != nil {
				t.Fatalf("daemonJobPayload: %v", err)
			}

			// PREMISE, asserted rather than assumed: the prompt really does end up
			// carrying the commit token. Without this the test could pass because
			// the brief was empty, which is the vacuous version of it.
			if !strings.Contains(payload.Instructions, citedCommit[:12]) {
				t.Fatalf("premise broken: the prompt carries no commit token, so the ordering is untested; prompt tail: %q",
					tailOf(payload.Instructions, 400))
			}

			events, err := store.ListJobEvents(ctx, job.ID)
			if err != nil {
				t.Fatalf("ListJobEvents: %v", err)
			}
			for _, event := range events {
				if event.Kind == "prompt_head_warning" {
					t.Fatalf("prompt_head_warning recorded: %q\n\nthe ledger brief is being scanned as if the operator had written it; the brief append must stay AFTER the head-contradiction scan",
						event.Message)
				}
			}
		})
	}
}
