package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/subprocess"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// daemonWorkerHeadSHA reads the current HEAD sha of a git checkout for the review
// head-resync tests.
func daemonWorkerHeadSHA(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func hasResyncEvent(events []db.JobEvent, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

// #684 failure mode A (the reliability fix): a PR review job is pinned to the head
// SHA the branch had at ENQUEUE time; in an active dev loop the branch advances
// before the queued review runs, so the registered checkout sits on a NEWER head.
// The review must re-target the checkout's current head (what a human reviewer
// does) instead of failing on the mismatch — as long as the PR is still OPEN.
func TestDefaultCheckoutResyncsReviewHeadWhenPRIsOpen(t *testing.T) {
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "feat/x")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	// The head the review was pinned to at enqueue (the branch's original tip).
	staleHead := daemonWorkerHeadSHA(t, checkout)

	// The branch advances: a newer commit is pushed before the review runs.
	if err := os.WriteFile(checkout+"/feature.txt", []byte("more work\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runDaemonWorkerGit(t, checkout, "add", "feature.txt")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "advance the branch")
	newHead := daemonWorkerHeadSHA(t, checkout)
	if newHead == staleHead {
		t.Fatal("test setup: branch head did not advance")
	}

	// The PR is still OPEN.
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo",
		Number:       23,
		HeadBranch:   "feat/x",
		HeadSHA:      staleHead,
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}

	// A queued review job pinned to the now-stale head.
	if _, err := (workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("test enqueue-only mailbox"))).Enqueue(ctx, workflow.JobRequest{
		ID:          "workflow-review-1",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "feat/x",
		PullRequest: 23,
		HeadSHA:     staleHead,
		TaskID:      "review-task-1", // no task row → resolves the shared checkout
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "workflow-review-1")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}

	got, err := worker.defaultCheckoutForRunner(ctx, job, payload, runtime.Agent{Name: "reviewer"}, subprocess.ExecRunner{})
	if err != nil {
		t.Fatalf("defaultCheckoutForRunner failed the stale-head review instead of re-syncing: %v", err)
	}
	if got != checkout {
		t.Fatalf("defaultCheckoutForRunner = %q, want shared checkout %q", got, checkout)
	}

	// The job payload is re-targeted to the checkout's CURRENT head so RunJob (which
	// re-reads the payload) delivers a review of the newest commit.
	reloaded, err := store.GetJob(ctx, "workflow-review-1")
	if err != nil {
		t.Fatalf("GetJob (reload) returned error: %v", err)
	}
	reloadedPayload, err := daemonJobPayload(reloaded)
	if err != nil {
		t.Fatalf("daemonJobPayload (reload) returned error: %v", err)
	}
	if reloadedPayload.HeadSHA != newHead {
		t.Fatalf("re-synced HeadSHA = %q, want current head %q (was %q)", reloadedPayload.HeadSHA, newHead, staleHead)
	}
	events, err := store.ListJobEvents(ctx, "workflow-review-1")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasResyncEvent(events, "review_head_resynced") {
		t.Fatalf("events = %+v, want a review_head_resynced event", events)
	}
}

// #684 mode A boundary: a head-SHA mismatch on a CLOSED (or merged) PR must NOT
// re-sync — a stale review of a dead PR is not useful, so the job keeps the
// existing terminal path and fails cleanly on the mismatch.
func TestDefaultCheckoutFailsReviewHeadMismatchWhenPRClosed(t *testing.T) {
	ctx := context.Background()
	checkout := createDaemonWorkerGitCheckout(t, "feat/x")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	staleHead := daemonWorkerHeadSHA(t, checkout)
	if err := os.WriteFile(checkout+"/feature.txt", []byte("more work\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runDaemonWorkerGit(t, checkout, "add", "feature.txt")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "advance the branch")

	// The PR is CLOSED.
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo",
		Number:       23,
		HeadBranch:   "feat/x",
		HeadSHA:      staleHead,
		State:        "closed",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}

	if _, err := (workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("test enqueue-only mailbox"))).Enqueue(ctx, workflow.JobRequest{
		ID:          "workflow-review-2",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "feat/x",
		PullRequest: 23,
		HeadSHA:     staleHead,
		TaskID:      "review-task-2",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "workflow-review-2")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}

	_, err = worker.defaultCheckoutForRunner(ctx, job, payload, runtime.Agent{Name: "reviewer"}, subprocess.ExecRunner{})
	if err == nil {
		t.Fatal("defaultCheckoutForRunner re-synced a review whose PR is closed; want a clean head-mismatch failure")
	}
	if !strings.Contains(err.Error(), "not review job head") {
		t.Fatalf("expected the review head-mismatch error, got: %v", err)
	}

	// Payload head is unchanged, and no re-sync event was recorded.
	reloaded, err := store.GetJob(ctx, "workflow-review-2")
	if err != nil {
		t.Fatalf("GetJob (reload) returned error: %v", err)
	}
	reloadedPayload, err := daemonJobPayload(reloaded)
	if err != nil {
		t.Fatalf("daemonJobPayload (reload) returned error: %v", err)
	}
	if reloadedPayload.HeadSHA != staleHead {
		t.Fatalf("closed-PR HeadSHA = %q, want it left unchanged at %q", reloadedPayload.HeadSHA, staleHead)
	}
	events, err := store.ListJobEvents(ctx, "workflow-review-2")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if hasResyncEvent(events, "review_head_resynced") {
		t.Fatalf("events = %+v, want NO review_head_resynced event for a closed PR", events)
	}
}

// #684 mode A safety guard: when a review falls back to the registered SHARED
// checkout (which sits on `main`, not the PR branch — e.g. the task worktree was
// cleaned up while a stale review was still queued), it must NOT be re-synced to
// main's head. Re-targeting there would review main's tree (none of the PR's
// changes) and could post an approval against a SHA that is not the PR head. The
// branch mismatch (checkout on `main`, review pinned to `feat/x`) makes resync
// decline, so the job keeps the existing head-mismatch failure.
func TestDefaultCheckoutDeclinesResyncWhenCheckoutOnWrongBranch(t *testing.T) {
	ctx := context.Background()
	// The shared checkout sits on `main`, NOT the PR's head branch.
	checkout := createDaemonWorkerGitCheckout(t, "main")
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	worker := defaultJobWorker(store, io.Discard)

	// The head the review was pinned to (the PR branch's tip) — differs from main's
	// head, so validateReviewCheckout raises the head-SHA mismatch.
	staleHead := daemonWorkerHeadSHA(t, checkout)
	if err := os.WriteFile(checkout+"/main.txt", []byte("main advances\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runDaemonWorkerGit(t, checkout, "add", "main.txt")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "advance main")
	mainHead := daemonWorkerHeadSHA(t, checkout)
	if mainHead == staleHead {
		t.Fatal("test setup: main head did not advance")
	}

	// The PR is OPEN on branch feat/x — the store would otherwise green-light a resync.
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo",
		Number:       23,
		HeadBranch:   "feat/x",
		HeadSHA:      staleHead,
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}

	// The queued review is pinned to the feat/x branch + its stale head, but its task
	// worktree is gone so it resolves the shared checkout (which is on main).
	if _, err := (workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("test enqueue-only mailbox"))).Enqueue(ctx, workflow.JobRequest{
		ID:          "workflow-review-3",
		Agent:       "reviewer",
		Action:      "review",
		Repo:        "owner/repo",
		Branch:      "feat/x",
		PullRequest: 23,
		HeadSHA:     staleHead,
		TaskID:      "review-task-3", // no task row → resolves the shared checkout
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	job, err := store.GetJob(ctx, "workflow-review-3")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}

	_, err = worker.defaultCheckoutForRunner(ctx, job, payload, runtime.Agent{Name: "reviewer"}, subprocess.ExecRunner{})
	if err == nil {
		t.Fatal("defaultCheckoutForRunner re-synced a review to a checkout on the wrong branch; want a clean head-mismatch failure")
	}
	if !strings.Contains(err.Error(), "not review job head") {
		t.Fatalf("expected the review head-mismatch error, got: %v", err)
	}

	// The payload head is left unchanged (NOT re-targeted to main's head) and no
	// re-sync event was recorded.
	reloaded, err := store.GetJob(ctx, "workflow-review-3")
	if err != nil {
		t.Fatalf("GetJob (reload) returned error: %v", err)
	}
	reloadedPayload, err := daemonJobPayload(reloaded)
	if err != nil {
		t.Fatalf("daemonJobPayload (reload) returned error: %v", err)
	}
	if reloadedPayload.HeadSHA != staleHead {
		t.Fatalf("wrong-branch HeadSHA = %q, want it left unchanged at %q (not main's head %q)", reloadedPayload.HeadSHA, staleHead, mainHead)
	}
	events, err := store.ListJobEvents(ctx, "workflow-review-3")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if hasResyncEvent(events, "review_head_resynced") {
		t.Fatalf("events = %+v, want NO review_head_resynced event for a wrong-branch checkout", events)
	}
}

// commitDaemonWorkerFile writes and commits one file in a test checkout, so a
// direction case can build a specific commit graph.
func commitDaemonWorkerFile(t *testing.T, dir, name, body, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile %s returned error: %v", name, err)
	}
	runDaemonWorkerGit(t, dir, "add", name)
	runDaemonWorkerGit(t, dir, "commit", "-m", message)
	return daemonWorkerHeadSHA(t, dir)
}

func jobEventMessages(events []db.JobEvent, kind string) []string {
	var messages []string
	for _, e := range events {
		if e.Kind == kind {
			messages = append(messages, e.Message)
		}
	}
	return messages
}

// daemonWorkerCurrentHead is the wantHead expectation for every case whose payload
// must end up holding the checkout's canonical 40-char head.
func daemonWorkerCurrentHead(t *testing.T, checkout, _ string) string {
	t.Helper()
	return daemonWorkerHeadSHA(t, checkout)
}

// #1561: the re-sync is DIRECTIONAL now. Its only gates were "the checkout is on
// the payload branch" and "the head differs from the payload head", so a queued
// review could be re-pointed at a commit that was not the branch tip in either
// direction — backwards onto a superseded commit, and after a force-push onto a
// commit reachable from no branch at all (issue comments 5326259747 and
// 5330559022) — while the recorded event asserted "branch advanced", a direction
// nothing measured. Re-targeting now requires the checkout head to have the
// DISPATCHED head as an ancestor; a same-commit abbreviation is recorded as an
// expansion rather than a re-sync; and every refusal keeps the caller's existing
// head-mismatch failure with the payload untouched.
func TestDefaultCheckoutReviewHeadResyncMeasuresDirection(t *testing.T) {
	// A syntactically valid SHA that is in no object database, so `merge-base
	// --is-ancestor` cannot decide the relationship at all.
	const absentHead = "0123456789abcdef0123456789abcdef01234567"

	cases := []struct {
		name string
		// branch is the branch recorded on the review payload; empty means the
		// checkout's own branch (feat/x).
		branch string
		// setup shapes the checkout's commit graph and returns the head SHA the
		// review is dispatched with.
		setup func(t *testing.T, checkout string) string
		// wantErr is the substring the resolution must fail with; empty means the
		// resolution must succeed and return the checkout.
		wantErr string
		// wantErrExact, when set, is the WHOLE error text the resolution must fail
		// with, given the checkout head and the dispatched rev. It pins the promise
		// that a refusal leaves the caller's original error byte-for-byte, which is
		// what keeps the job on the #532 deferral surface — a substring match would
		// still pass if the refusal wrapped or re-worded that error.
		wantErrExact func(head, dispatched string) string
		// wantHead is the payload head expected afterwards, given the dispatched head.
		wantHead func(t *testing.T, checkout, dispatched string) string
		// wantEvent is the single event kind the job must carry, with wantMessage a
		// substring of it. Empty wantEvent means no re-sync event of any kind.
		wantEvent   string
		wantMessage string
	}{
		{
			name: "fast_forward_descendant_re_syncs",
			setup: func(t *testing.T, checkout string) string {
				dispatched := daemonWorkerHeadSHA(t, checkout)
				commitDaemonWorkerFile(t, checkout, "feature.txt", "more work\n", "advance the branch")
				return dispatched
			},
			wantHead: func(t *testing.T, checkout, _ string) string {
				return daemonWorkerHeadSHA(t, checkout)
			},
			wantEvent:   "review_head_resynced",
			wantMessage: "has dispatched head",
		},
		{
			name: "backwards_onto_superseded_head_refused",
			setup: func(t *testing.T, checkout string) string {
				base := daemonWorkerHeadSHA(t, checkout)
				dispatched := commitDaemonWorkerFile(t, checkout, "feature.txt", "newer work\n", "advance the branch")
				// The checkout falls back to the older commit the review was NOT
				// dispatched against: re-targeting here reviews superseded code.
				runDaemonWorkerGit(t, checkout, "reset", "--hard", base)
				return dispatched
			},
			wantErr: "not review job head",
			wantHead: func(_ *testing.T, _, dispatched string) string {
				return dispatched
			},
			wantEvent:   "review_head_resync_refused",
			wantMessage: "does not have dispatched head",
		},
		{
			name: "orphaned_dispatch_head_refused",
			setup: func(t *testing.T, checkout string) string {
				base := daemonWorkerHeadSHA(t, checkout)
				// The force-push shape from #1564: the dispatched commit is still in the
				// object database but reachable from no branch, and the checkout head is
				// on a divergent line, so neither is an ancestor of the other.
				dispatched := commitDaemonWorkerFile(t, checkout, "feature.txt", "force-pushed away\n", "discarded work")
				runDaemonWorkerGit(t, checkout, "reset", "--hard", base)
				commitDaemonWorkerFile(t, checkout, "rebuilt.txt", "rebuilt branch\n", "rebuild the branch")
				return dispatched
			},
			wantErr: "not review job head",
			wantErrExact: func(head, dispatched string) string {
				return fmt.Sprintf("checkout head is %s, not review job head %s", head, dispatched)
			},
			wantHead: func(_ *testing.T, _, dispatched string) string {
				return dispatched
			},
			wantEvent:   "review_head_resync_refused",
			wantMessage: "superseded or divergent commit",
		},
		{
			name: "unresolvable_dispatch_head_preserves_the_deferrable_error",
			setup: func(t *testing.T, checkout string) string {
				commitDaemonWorkerFile(t, checkout, "feature.txt", "more work\n", "advance the branch")
				return absentHead
			},
			// An unresolvable rev (an unfetched object) is UNDECIDABLE, not a measured
			// direction — and it must stay as recoverable as a wrong-commit checkout.
			// The caller's original error is what classifyCheckoutContention matches to
			// defer and auto-retry (job_blocker_checkout.go:101); a distinctly-worded
			// error classified as nothing and terminally failed the job. The diagnosis
			// goes in the job record instead of the control flow.
			wantErr: "not review job head",
			wantErrExact: func(head, dispatched string) string {
				return fmt.Sprintf("checkout head is %s, not review job head %s", head, dispatched)
			},
			wantHead: func(_ *testing.T, _, dispatched string) string {
				return dispatched
			},
			wantEvent:   "review_head_resync_refused",
			wantMessage: "does not resolve in this checkout",
		},
		{
			// Every spelling below names the commit the checkout is ALREADY on, with
			// the branch never moving. Identity is decided by resolving both revs to
			// 40-char object ids, so none of these is a re-target, whatever its shape.
			// Measured against the previous shape heuristics by the round-1 reviewer at
			// 0c0a0b0e: the 6-hex, "^0" and branch-name spellings each produced a false
			// review_head_resynced "fast-forward" claim for a head that never moved,
			// and "HEAD" errored outright because the code lowercased it to "head".
			name: "same_commit_abbreviated_12",
			setup: func(t *testing.T, checkout string) string {
				return daemonWorkerHeadSHA(t, checkout)[:12]
			},
			wantHead:    daemonWorkerCurrentHead,
			wantEvent:   "review_head_normalized",
			wantMessage: "the SAME commit",
		},
		{
			name: "same_commit_abbreviated_6",
			setup: func(t *testing.T, checkout string) string {
				return daemonWorkerHeadSHA(t, checkout)[:6]
			},
			wantHead:    daemonWorkerCurrentHead,
			wantEvent:   "review_head_normalized",
			wantMessage: "the SAME commit",
		},
		{
			name: "same_commit_uppercase_40",
			setup: func(t *testing.T, checkout string) string {
				return strings.ToUpper(daemonWorkerHeadSHA(t, checkout))
			},
			wantHead:    daemonWorkerCurrentHead,
			wantEvent:   "review_head_normalized",
			wantMessage: "the SAME commit",
		},
		{
			name: "same_commit_rev_expression",
			setup: func(t *testing.T, checkout string) string {
				return daemonWorkerHeadSHA(t, checkout) + "^0"
			},
			wantHead:    daemonWorkerCurrentHead,
			wantEvent:   "review_head_normalized",
			wantMessage: "the SAME commit",
		},
		{
			name: "same_commit_branch_name",
			setup: func(t *testing.T, _ string) string {
				return "feat/x"
			},
			wantHead:    daemonWorkerCurrentHead,
			wantEvent:   "review_head_normalized",
			wantMessage: "the SAME commit",
		},
		{
			name: "same_commit_symbolic_head",
			setup: func(t *testing.T, _ string) string {
				// Case-sensitive ref: the dispatched rev reaches git verbatim, so
				// "HEAD" resolves instead of being lowercased into an invalid "head".
				return "HEAD"
			},
			wantHead:    daemonWorkerCurrentHead,
			wantEvent:   "review_head_normalized",
			wantMessage: "the SAME commit",
		},
		{
			name:   "wrong_branch_checkout_still_declines_before_any_measurement",
			branch: "feat/other",
			setup: func(t *testing.T, checkout string) string {
				dispatched := daemonWorkerHeadSHA(t, checkout)
				commitDaemonWorkerFile(t, checkout, "feature.txt", "more work\n", "advance the branch")
				return dispatched
			},
			wantErr: "not review job head",
			wantHead: func(_ *testing.T, _, dispatched string) string {
				return dispatched
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			checkout := createDaemonWorkerGitCheckout(t, "feat/x")
			store := daemonWorkerStore(t)
			seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
			worker := defaultJobWorker(store, io.Discard)

			dispatched := tc.setup(t, checkout)
			branch := tc.branch
			if branch == "" {
				branch = "feat/x"
			}

			// The PR is OPEN, so the store green-lights a re-sync and the ancestry gate
			// is the only thing that can refuse one.
			if err := store.UpsertPullRequest(ctx, db.PullRequest{
				RepoFullName: "owner/repo",
				Number:       23,
				HeadBranch:   branch,
				HeadSHA:      dispatched,
				State:        "open",
			}); err != nil {
				t.Fatalf("UpsertPullRequest returned error: %v", err)
			}

			const jobID = "workflow-review-direction"
			enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
				ID:          jobID,
				Agent:       "reviewer",
				Action:      "review",
				Repo:        "owner/repo",
				Branch:      branch,
				PullRequest: 23,
				HeadSHA:     dispatched,
				TaskID:      "review-task-direction", // no task row → resolves the shared checkout
			})
			job, err := store.GetJob(ctx, jobID)
			if err != nil {
				t.Fatalf("GetJob returned error: %v", err)
			}
			payload, err := daemonJobPayload(job)
			if err != nil {
				t.Fatalf("daemonJobPayload returned error: %v", err)
			}

			got, err := worker.defaultCheckoutForRunner(ctx, job, payload, runtime.Agent{Name: "reviewer"}, subprocess.ExecRunner{})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("defaultCheckoutForRunner returned error: %v", err)
				}
				if got != checkout {
					t.Fatalf("defaultCheckoutForRunner = %q, want the shared checkout %q", got, checkout)
				}
			} else {
				if err == nil {
					t.Fatalf("defaultCheckoutForRunner returned %q and no error, want a failure containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				if tc.wantErrExact != nil {
					if want := tc.wantErrExact(daemonWorkerHeadSHA(t, checkout), dispatched); err.Error() != want {
						t.Fatalf("error = %q, want the caller's original error VERBATIM: %q", err.Error(), want)
					}
				}
			}

			reloaded, err := store.GetJob(ctx, jobID)
			if err != nil {
				t.Fatalf("GetJob (reload) returned error: %v", err)
			}
			reloadedPayload, err := daemonJobPayload(reloaded)
			if err != nil {
				t.Fatalf("daemonJobPayload (reload) returned error: %v", err)
			}
			if want := tc.wantHead(t, checkout, dispatched); reloadedPayload.HeadSHA != want {
				t.Fatalf("payload HeadSHA = %q, want %q (dispatched %q)", reloadedPayload.HeadSHA, want, dispatched)
			}

			events, err := store.ListJobEvents(ctx, jobID)
			if err != nil {
				t.Fatalf("ListJobEvents returned error: %v", err)
			}
			for _, kind := range []string{"review_head_resynced", "review_head_normalized", "review_head_resync_refused"} {
				if kind == tc.wantEvent {
					continue
				}
				if hasResyncEvent(events, kind) {
					t.Fatalf("events = %+v, want NO %s event", events, kind)
				}
			}
			if tc.wantEvent == "" {
				return
			}
			messages := jobEventMessages(events, tc.wantEvent)
			if len(messages) != 1 {
				t.Fatalf("events = %+v, want exactly one %s event", events, tc.wantEvent)
			}
			if !strings.Contains(messages[0], tc.wantMessage) {
				t.Fatalf("%s message = %q, want it to contain %q", tc.wantEvent, messages[0], tc.wantMessage)
			}
		})
	}
}

// The refusal path's whole value is that the CALLER's original error survives:
// that string is what the #532 classifier matches to defer the job and auto-retry
// it, so an undecidable comparison stays at least as recoverable as a
// wrong-commit checkout. A refusal that invented its own error text classified as
// nothing (checkoutContentionNone) and terminally failed a job that used to
// self-heal, which is what a round-1 finding measured on this PR.
func TestReviewHeadMismatchErrorStaysDeferrable(t *testing.T) {
	cause := fmt.Errorf("checkout head is %s, not review job head %s", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if !isReviewHeadMismatch(cause) {
		t.Fatalf("isReviewHeadMismatch(%v) = false, want true — the re-sync path would never see this error", cause)
	}
	kind, action := classifyCheckoutContention(cause)
	if kind != checkoutContentionDirty {
		t.Fatalf("classifyCheckoutContention kind = %v, want checkoutContentionDirty (%v) so the job defers and auto-retries", kind, checkoutContentionDirty)
	}
	if strings.TrimSpace(action) == "" {
		t.Fatal("classifyCheckoutContention returned an empty suggested_action for the review head mismatch")
	}
	// The wording the refusal path must NOT introduce: a comparison-specific error
	// is outside every classifier family, so it terminally fails instead.
	invented := fmt.Errorf("compare review checkout head %s with dispatched head %s: exit status 128", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if kind, _ := classifyCheckoutContention(invented); kind != checkoutContentionNone {
		t.Fatalf("classifyCheckoutContention(%q) kind = %v, want checkoutContentionNone — this test's premise is that such wording is unclassified", invented, kind)
	}
}

// #684 failure mode B: a foreground review whose serialized runtime session is
// busy must be LEFT QUEUED for the daemon to run (a review is naturally
// asynchronous), not cancelled and dropped. Ask/implement keep their existing
// synchronous cancel behavior (covered by TestRunAgentAskCancelsQueuedJobWhenRuntimeSessionBusy).
func TestRunAgentReviewRequeuesQueuedJobWhenRuntimeSessionBusy(t *testing.T) {
	home := t.TempDir()
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "branch", "-m", "main")
	runGit(t, repoDir, "config", "user.email", "gitmoot@example.com")
	runGit(t, repoDir, "config", "user.name", "Gitmoot")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/owner/repo.git")
	if err := os.WriteFile(repoDir+"/README.md", []byte("test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	runGit(t, repoDir, "add", "README.md")
	runGit(t, repoDir, "commit", "-m", "initial")
	t.Chdir(repoDir)
	head := daemonWorkerHeadSHA(t, repoDir)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{
		"agent", "subscribe", "reviewer",
		"--home", home,
		"--runtime", "codex",
		"--session", "550e8400-e29b-41d4-a716-446655440042",
		"--role", "reviewer",
		"--repo", "owner/repo",
		"--capability", "review",
		"--capability", "implement",
		"--policy", "workspace-write",
	}, &stdout, &stderr); code != 0 {
		t.Fatalf("subscribe reviewer exit code = %d, stderr=%s", code, stderr.String())
	}

	store := openCLIJobStore(t, home)
	// A pre-seeded review task whose worktree is at the requested head lets the
	// review dispatch resolve locally without any GitHub call.
	if err := store.UpsertTask(context.Background(), db.Task{
		ID:           "review-task",
		RepoFullName: "owner/repo",
		GoalID:       "local-review",
		Title:        "Review PR #1",
		State:        string(workflow.TaskReviewing),
		Branch:       "feat/x",
		WorktreePath: repoDir,
	}); err != nil {
		t.Fatalf("UpsertTask returned error: %v", err)
	}
	// The runtime session is busy (held by another owner).
	if acquired, err := store.AcquireResourceLock(context.Background(), db.ResourceLock{
		ResourceKey: "runtime:codex:550e8400-e29b-41d4-a716-446655440042",
		OwnerJobID:  "other-job",
		OwnerToken:  "other-token",
		ExpiresAt:   time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano),
	}, time.Now().UTC()); err != nil || !acquired {
		t.Fatalf("AcquireResourceLock returned acquired=%v err=%v", acquired, err)
	}
	store.Close()

	runner := &agentStartRunner{}
	restoreFactory := replaceRuntimeFactory(runtime.Factory{Runner: runner})
	defer restoreFactory()

	stdout.Reset()
	stderr.Reset()
	code := Run([]string{
		"agent", "review", "reviewer", "Please review",
		"--home", home,
		"--repo", "owner/repo",
		"--pr", "1",
		"--head-sha", head,
		"--branch", "feat/x",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("busy review exit code = %d, want 0 (requeued, not dropped); stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runtime calls = %+v, want none (job left queued, not run)", runner.calls)
	}
	// No daemon is running in this test, so the foreground caller MUST be told the
	// review is only queued and will not run until a daemon picks it up — otherwise a
	// bare "state: queued" reads as "the review ran" and the review silently never
	// executes.
	if out := stdout.String(); !strings.Contains(out, "daemon is not running") || !strings.Contains(out, "job run") {
		t.Fatalf("foreground busy-review stdout missing daemon guidance; got:\n%s", out)
	}

	store = openCLIJobStore(t, home)
	defer store.Close()
	jobs, err := store.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want one queued review job", jobs)
	}
	if jobs[0].State != string(workflow.JobQueued) {
		t.Fatalf("job state = %q, want queued (left for the daemon)", jobs[0].State)
	}
	events, err := store.ListJobEvents(context.Background(), jobs[0].ID)
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	if !hasResyncEvent(events, "requeued_runtime_busy") || !hasResyncEvent(events, "runtime_lock_wait") {
		t.Fatalf("events = %+v, want requeued_runtime_busy and runtime_lock_wait", events)
	}
	if hasResyncEvent(events, string(workflow.JobCancelled)) {
		t.Fatalf("events = %+v, review job must not be cancelled/dropped", events)
	}
}
