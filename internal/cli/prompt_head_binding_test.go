package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/subprocess"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// promptHeadBindingCheckout builds the one repository shape all the arms need:
// a base commit on main, two commits on the reviewed branch, and a commit on a
// SECOND branch that is reachable from neither.
//
// The last one is the whole point. `readonlyReviewWorktreeGitCheckout` gives two
// heads on one branch, and a prior head on the reviewed branch is an ANCESTOR of
// the later one - so it can only exercise the arm that allows. Reproducing the
// defect needs a commit on another branch, which is what a different pull
// request's head actually is.
func promptHeadBindingCheckout(t *testing.T) (checkout string, base string, firstHead string, head string, foreign string) {
	t.Helper()
	checkout = readonlyWorktreeGitCheckout(t, "owner/repo")
	base = readonlyWorktreeHead(t, checkout)

	runGit(t, checkout, "switch", "-c", "feature/review")
	writeFile(t, filepath.Join(checkout, "review.txt"), "round one\n")
	runGit(t, checkout, "add", "review.txt")
	runGit(t, checkout, "commit", "-m", "review round one")
	firstHead = readonlyWorktreeHead(t, checkout)
	writeFile(t, filepath.Join(checkout, "review.txt"), "round two\n")
	runGit(t, checkout, "add", "review.txt")
	runGit(t, checkout, "commit", "-m", "review round two")
	head = readonlyWorktreeHead(t, checkout)

	// A different pull request's branch, cut from the same base so it is a
	// sibling rather than a descendant.
	runGit(t, checkout, "switch", "-c", "feature/other", base)
	writeFile(t, filepath.Join(checkout, "other.txt"), "other pull request\n")
	runGit(t, checkout, "add", "other.txt")
	runGit(t, checkout, "commit", "-m", "other pull request")
	foreign = readonlyWorktreeHead(t, checkout)

	runGit(t, checkout, "switch", "main")
	return checkout, base, firstHead, head, foreign
}

// seedRecordedReviewHead writes a review job row that RECORDED a head for a pull
// request, which is what the append-only arm reads. It is a real job row rather
// than a hand-built fact because the query reads payload JSON.
func seedRecordedReviewHead(t *testing.T, store *db.Store, id string, pullRequest int, head string) {
	t.Helper()
	insertCLIJobWithPayload(t, store, id, pullRequest, head)
}

func insertCLIJobWithPayload(t *testing.T, store *db.Store, id string, pullRequest int, head string) {
	t.Helper()
	payload := mustJobPayload(t, workflow.JobPayload{
		Repo: "owner/repo", PullRequest: pullRequest, HeadSHA: head, Branch: "feature/review",
	})
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID: id, Agent: "reviewer", Type: "review", State: string(workflow.JobSucceeded),
		Repo: "owner/repo", PullRequest: pullRequest, Payload: payload,
	}, db.JobEvent{Kind: "succeeded", Message: "recorded head fixture"}); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", id, err)
	}
}

// TestReviewDispatchBindsThePromptsTargetToTheDispatchHead drives the REAL
// dispatch entry, `dispatchLocalAgentJob`, once per relation.
//
// Before the fix every arm dispatched: Gitmoot recorded a `prompt_head_warning`,
// printed it, and ran the job against the dispatch head with the wrong
// instructions. Measured on this store, 911 such warnings on gitmoot/gitmoot
// review jobs alone and not one refusal.
//
// The ALLOW arms are not padding, they are the reason this guard is shaped the
// way it is. Re-derived over 644 distinct citations from real dispatches: 282
// are ancestors, 246 are recorded heads of the same pull request that are NOT
// ancestors, 98 cannot be classified at all, and only 18 are the defect. A guard
// that refuses on any non-head token refuses all 644; one without the
// recorded-head arm refuses 362; one that folds "cannot tell" into refuse
// refuses 116 instead of 18.
func TestReviewDispatchBindsThePromptsTargetToTheDispatchHead(t *testing.T) {
	checkout, base, firstHead, head, foreign := promptHeadBindingCheckout(t)
	// Syntactically valid, in no object database, so neither ancestry nor
	// identity can be established for it.
	const unresolvable = "0123456789abcdef0123456789abcdef01234567"

	for _, tt := range []struct {
		name string
		// cited is appended to the prompt as the prompt would write it.
		cited string
		// recordAs, when non-zero, seeds a prior job that recorded cited as a
		// head of that pull request.
		recordAs   int
		allow      bool
		wantRefuse bool
		// wantNamed are substrings the refusal must carry.
		wantNamed []string
	}{
		{
			name:  "the dispatch head itself dispatches",
			cited: head,
		},
		{
			name: "an ANCESTOR dispatches: this is the branch base, which every prompt that says what the branch sits on names",
			// The base was never a head of this pull request, so the
			// recorded-head arm alone would miss it. It needs the ancestry arm.
			cited: base,
		},
		{
			name:  "a prior head on the reviewed branch dispatches",
			cited: firstHead,
		},
		{
			name: "a RECORDED head of this pull request dispatches even though it is not an ancestor",
			// This is the force-push shape: a legitimate prior head that a
			// rewrite left unreachable from the current one. Ancestry says no;
			// the append-only store says yes.
			cited:    foreign,
			recordAs: 12,
		},
		{
			name: "an UNRESOLVABLE sha dispatches, because that is a fact about the checkout and not about the citation",
			// 72 of the 98 unclassifiable citations measured on this store were
			// unclassifiable because the DISPATCH HEAD was missing from the
			// clone, not the cited sha. Refusing on it would make dispatch
			// depend on clone freshness.
			cited: unresolvable,
		},
		{
			name:       "a commit outside this pull request's history is REFUSED",
			cited:      foreign,
			wantRefuse: true,
			wantNamed:  []string{foreign, head, "--allow-prompt-head-mismatch"},
		},
		{
			name:       "the refusal NAMES the pull request the sha belongs to when the store knows",
			cited:      foreign,
			recordAs:   99,
			wantRefuse: true,
			wantNamed:  []string{foreign, "#99", "not of this pull request"},
		},
		{
			name:  "the escape hatch dispatches a deliberate mismatch",
			cited: foreign,
			allow: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, home := blockerE2EHome(t)
			seedReviewDispatchFixture(t, store, checkout)
			if tt.recordAs != 0 {
				seedRecordedReviewHead(t, store, "recorded-head-fixture", tt.recordAs, tt.cited)
			}
			before, err := store.ListJobs(ctx)
			if err != nil {
				t.Fatal(err)
			}

			request := reviewDispatchRequest(home, head)
			request.Instructions = "Review this exact head. Round history: commit " + tt.cited + " for context."
			request.AllowPromptHeadMismatch = tt.allow

			out, dispatchErr := dispatchLocalAgentJob(ctx, store, request)

			after, listErr := store.ListJobs(ctx)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if !tt.wantRefuse {
				if dispatchErr != nil {
					t.Fatalf("dispatch was refused: %v\nA legitimate provenance citation must still dispatch; refusing it is how operators learn to stop pinning --head-sha.", dispatchErr)
				}
				if strings.TrimSpace(out.JobID) == "" {
					t.Fatalf("dispatch returned no job id: %+v", out)
				}
				return
			}
			if dispatchErr == nil {
				t.Fatalf("dispatch succeeded (job %s); a review whose prompt names a different review target must be refused", out.JobID)
			}
			// No job row: the refusal has to land before the row exists, which is
			// what makes it cost zero model tokens.
			if len(after) != len(before) {
				t.Errorf("job rows went from %d to %d; the refusal created a row", len(before), len(after))
			}
			for _, want := range tt.wantNamed {
				if !strings.Contains(dispatchErr.Error(), want) {
					t.Errorf("refusal %q does not name %q", dispatchErr.Error(), want)
				}
			}
		})
	}
}

// TestReviewDispatchWarnsOnlyOnCitationsNobodyHasJudged is #2054 finding 3.
//
// The identity-based scan warned on ANY cited commit that was not the dispatch
// head. For a review that fires on cases the refusal above has ALREADY accepted:
// a foreign citation never reaches the warning stage, so every warning was about
// the head's own ancestor, a recorded head of this pull request, or an
// unresolvable sha. The first two are what a prompt says deliberately - a scoped
// re-review MUST name the previous round's head to say what it is re-reviewing.
//
// A warning that fires on the routine case teaches its reader to ignore it, and
// that cost was paid: this warning was used to attribute job ownership on
// 2026-09-08 and then raised against a scoped re-review citing its own prior
// head correctly.
//
// The result is that a review emits NO prompt_head_warning at all, and that is
// the finding rather than a side effect: the scan cannot report a relation that
// is both resolvable and unjudged, because foreign is refused before enqueue.
// The refusal carries the wrong-head signal; the warning had nothing left to
// say. Ask and implement keep theirs - see the boundary test below.
// TestUnjudgedTokenCannotRetainAnotherCitationsWarning is review finding P3 on
// #2064 cycle five, and it is a RETENTION LEAK rather than a wording problem.
//
// Every warning names the dispatch head twice in its own text. The filter used
// to keep a warning when any unjudged token appeared ANYWHERE in it, so a
// non-resolving token that happens to be a 7-hex run from the middle of the
// dispatch sha classified as promptCommitUnresolved, went into the unjudged set,
// and then matched inside the dispatch-head text of an ANCESTOR's warning -
// retaining a warning the filter exists to drop, about a citation nobody
// complained about.
//
// It survived an earlier hand probe of mine for a reason worth recording: that
// probe's prompt contained no stray token, so the leak had nothing to fire on. A
// negative result from an input that cannot express the defect is not evidence.
func TestUnjudgedTokenCannotRetainAnotherCitationsWarning(t *testing.T) {
	ctx := context.Background()
	checkout, _, base, head, _ := promptHeadBindingCheckout(t)
	store, _ := blockerE2EHome(t)
	client := jobGitClient(checkout, subprocess.ExecRunner{})

	// A 7-hex run lifted from the MIDDLE of the dispatch head, which no object
	// resolves: this is the unjudged token that used to leak.
	stray := head[8:15]
	if _, err := client.RevParse(ctx, stray+"^{commit}"); err == nil {
		t.Skipf("fixture stray token %q unexpectedly resolves", stray)
	}
	// base is an ANCESTOR of head, so its warning must be dropped.
	prompt := "review " + base + " and also " + stray

	warnings := dispatchPromptHeadContradictionWarnings(ctx, client, prompt, head)
	if len(warnings) == 0 {
		t.Fatalf("fixture produced no warnings to filter; the ancestor citation must warn before filtering")
	}
	kept := retainUnjudgedPromptHeadWarnings(ctx, client, store, prompt, head, "owner/repo", 12, warnings)
	for _, warning := range kept {
		if strings.Contains(warning, base) {
			t.Fatalf("an unjudged stray token retained the ANCESTOR citation's warning: %q\nkept=%v", warning, kept)
		}
	}
}

func TestReviewDispatchWarnsOnlyOnCitationsNobodyHasJudged(t *testing.T) {
	checkout, base, firstHead, head, staleTarget := promptHeadBindingCheckout(t)
	const unresolvable = "0123456789abcdef0123456789abcdef01234567"

	for _, tt := range []struct {
		name     string
		cited    string
		recordAs int
		wantWarn bool
	}{
		{name: "a prior head on the reviewed branch is silent: this is the scoped re-review case", cited: firstHead},
		{name: "the branch base is silent: a prompt states what the branch sits on", cited: base},
		{name: "the dispatch head itself is silent", cited: head},
		// The scan SKIPS a token it cannot resolve, so an unresolvable citation
		// never produced a warning here either - my first version of this test
		// asserted it did, and that expectation was wrong about the existing
		// scanner rather than about the change.
		{name: "an unresolvable sha is silent too, because the scan never resolved it to warn about", cited: unresolvable},
		// F4: a RECORDED head of this pull request that is NOT an ancestor is the
		// force-push shape - the commit really was a head and no longer is - so a
		// prompt naming it as its target is reviewing something that is gone.
		// The first version of this filter could not tell that from provenance.
		{name: "a recorded head that is NOT an ancestor warns: that is a stale review target", cited: staleTarget, recordAs: 12, wantWarn: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, home := blockerE2EHome(t)
			seedReviewDispatchFixture(t, store, checkout)
			if tt.recordAs != 0 {
				seedRecordedReviewHead(t, store, "recorded-head-fixture", tt.recordAs, tt.cited)
			}

			request := reviewDispatchRequest(home, head)
			request.Instructions = "Review this exact head. Round history: commit " + tt.cited + " for context."
			out, err := dispatchLocalAgentJob(ctx, store, request)
			if err != nil {
				t.Fatalf("dispatch was refused: %v", err)
			}
			events, err := store.ListJobEvents(ctx, out.JobID)
			if err != nil {
				t.Fatal(err)
			}
			warned := 0
			for _, event := range events {
				if event.Kind == "prompt_head_warning" {
					warned++
				}
			}
			if tt.wantWarn && warned == 0 {
				t.Fatalf("no prompt_head_warning for an unresolvable citation: events=%+v", events)
			}
			if !tt.wantWarn && warned != 0 {
				t.Fatalf("prompt_head_warning fired on a citation the refusal already accepted (%s); "+
					"a warning on the routine case teaches its reader to ignore it", tt.cited)
			}
		})
	}
}

// TestAskDispatchKeepsItsBlanketPromptHeadWarning pins the BOUNDARY of #2054
// finding 3, which a mutant switching ask onto the review scope survived
// without it.
//
// Narrowing the warning is only safe where a refusal already judged the
// citation. Ask and implement have NO refusal in front of them - the #1819
// guard is review-only - so for them the identity-based warning is the entire
// head check, and silencing it there would remove the check rather than
// de-duplicate it. Same change, opposite correctness, decided by whether
// something else already looked.
func TestAskDispatchKeepsItsBlanketPromptHeadWarning(t *testing.T) {
	ctx := context.Background()
	checkout, base, _, head, _ := promptHeadBindingCheckout(t)
	store, home := blockerE2EHome(t)
	seedReviewDispatchFixture(t, store, checkout)

	// An ask needs an ask-capable agent; the shared fixture seeds review and
	// implement only.
	seedDaemonWorkerAgentWithPolicy(t, store, "asker", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	request := reviewDispatchRequest(home, head)
	request.Agent = "asker"
	request.LeadAgent = ""
	request.Action = "ask"
	request.PullRequest = 0
	// The BASE is an ancestor of the dispatch head, so the review path is now
	// deliberately silent about it. Ask must not be.
	request.Instructions = "Answer against commit " + base + " for context."

	out, err := dispatchLocalAgentJob(ctx, store, request)
	if err != nil {
		t.Fatalf("ask dispatch was refused: %v", err)
	}
	events, err := store.ListJobEvents(ctx, out.JobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "prompt_head_warning" {
			return
		}
	}
	t.Fatalf("ask lost its prompt_head_warning for a non-head citation: events=%+v; "+
		"ask has no refusal in front of it, so this warning is its only head check", events)
}

// TestReviewDispatchRefusalLeavesNoWorktreeBehind pins the ORDERING, separately
// from the refusal itself.
//
// The refusal has to precede the read-only worktree allocation, not merely the
// job row. A refusal placed after allocation would have to unwind a worktree it
// created, and #1817 measured what happens when nothing unwinds one: 29
// throwaway worktrees across 32 dead dispatches.
func TestReviewDispatchRefusalLeavesNoWorktreeBehind(t *testing.T) {
	ctx := context.Background()
	checkout, _, _, head, foreign := promptHeadBindingCheckout(t)
	store, home := blockerE2EHome(t)
	seedReviewDispatchFixture(t, store, checkout)

	worktreeRoot := filepath.Join(home, ".gitmoot", "worktrees")
	request := reviewDispatchRequest(home, head)
	request.Instructions = "Review commit " + foreign + " please."

	if _, err := dispatchLocalAgentJob(ctx, store, request); err == nil {
		t.Fatal("dispatch succeeded; expected a prompt-target refusal")
	}
	entries, err := os.ReadDir(worktreeRoot)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused dispatch left %d entries under %s; the refusal must precede allocation", len(entries), worktreeRoot)
	}
}

// TestAskAndImplementDispatchAreUnaffectedByThePromptTargetGuard is the scope
// control. The guard is review-only on purpose: a review's instructions ARE its
// specification, while an ask legitimately discusses any commit in the
// repository and an implement is bound by its own stricter checkout-head proof.
func TestAskAndImplementDispatchAreUnaffectedByThePromptTargetGuard(t *testing.T) {
	ctx := context.Background()
	checkout, _, _, head, foreign := promptHeadBindingCheckout(t)
	store, home := blockerE2EHome(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "responder", runtime.ShellRuntime, "true", []string{"ask"}, "owner/repo")

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "responder", Action: "ask", Background: true,
		Instructions: "What changed in commit " + foreign + " compared with " + head + "?",
		PullRequest:  12, HeadSHA: head, Home: home,
	})
	if err != nil {
		t.Fatalf("an ask naming another branch's commit was refused: %v", err)
	}
	if strings.TrimSpace(out.JobID) == "" {
		t.Fatalf("ask dispatch returned no job id: %+v", out)
	}
}
