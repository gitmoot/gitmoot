package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	gitutil "github.com/gitmoot/gitmoot/internal/git"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// headResolutionRepo builds a repo with two commits and returns the checkout plus both
// full SHAs.
func headResolutionRepo(t *testing.T) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "gitmoot@example.com")
	runGit(t, dir, "config", "user.name", "Gitmoot")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-m", "one")
	first, err := (gitutil.Client{Dir: dir}).HeadSHA(ctx)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-m", "two")
	second, err := (gitutil.Client{Dir: dir}).HeadSHA(ctx)
	if err != nil || second == first {
		t.Fatalf("second head = %q (first %q), err=%v", second, first, err)
	}
	return dir, first, second
}

// TestReviewCheckoutAcceptsAnAbbreviatedHead is the PASSING half of the required pair: an
// abbreviated --head-sha names the very commit the checkout is on, and must be accepted.
//
// This could never fail before the resync stopped laundering the mismatch, which is why a
// literal comparison survived: review_head_resynced overwrote the head before this guard ran.
func TestReviewCheckoutAcceptsAnAbbreviatedHead(t *testing.T) {
	dir, _, head := headResolutionRepo(t)
	worker := defaultJobWorker(daemonWorkerStore(t), nil)
	abbreviated := head[:7]

	// ASSERT THE PREMISE. Without this the test passes with a FULL sha substituted --
	// "accepted" would then be satisfied by simple string equality and the test could
	// not witness abbreviation at all. Measured: a premise mutant replacing head[:7]
	// with head left it green.
	if len(abbreviated) >= len(head) || abbreviated == head {
		t.Fatalf("premise not established: %q is not an abbreviation of %q", abbreviated, head)
	}

	payload := workflow.JobPayload{PullRequest: 17, HeadSHA: abbreviated}
	if err := worker.validateReviewCheckout(context.Background(), payload, dir); err != nil {
		t.Fatalf("abbreviated head %s rejected against the commit it names: %v", abbreviated, err)
	}
}

// TestReviewCheckoutRejectsADifferentCommit is the FAILING half. Without it, a fix that
// accepts everything would also pass the test above.
func TestReviewCheckoutRejectsADifferentCommit(t *testing.T) {
	dir, other, _ := headResolutionRepo(t)
	worker := defaultJobWorker(daemonWorkerStore(t), nil)
	payload := workflow.JobPayload{PullRequest: 17, HeadSHA: other}
	err := worker.validateReviewCheckout(context.Background(), payload, dir)
	if err == nil {
		t.Fatal("a genuinely different commit was accepted; the guard no longer discriminates")
	}
	if !strings.Contains(err.Error(), "not review job head") {
		t.Fatalf("error = %v, want the head-mismatch message the blocker classifier keys on", err)
	}
}

// TestReviewCheckoutRejectsAnUnresolvableHead pins that an UNEVALUATABLE comparison refuses.
// A revision that cannot be resolved is not evidence of a match; treating it as one would be
// absence-reads-as-permission at exactly the guard whose job is to refuse.
func TestReviewCheckoutRejectsAnUnresolvableHead(t *testing.T) {
	dir, _, _ := headResolutionRepo(t)
	worker := defaultJobWorker(daemonWorkerStore(t), nil)
	payload := workflow.JobPayload{PullRequest: 17, HeadSHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}
	if err := worker.validateReviewCheckout(context.Background(), payload, dir); err == nil {
		t.Fatal("an unresolvable head was accepted; an unevaluatable precondition must refuse")
	}
}

// TestHeadMismatchIsTerminalNotContention pins requirement 3: the terminal kind must take its
// OWN early exit, so no retryAt is computed and no BlockerClass is stamped.
//
// This test FAILS against a naive split that only renames the classification, because adding a
// case to an enum does not add a branch -- the new member would fall through to the shared path
// and be stamped checkout_contention with a backoff.
func TestHeadMismatchIsTerminalNotContention(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	payload := workflow.JobPayload{PullRequest: 17, HeadSHA: "abc123"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	job := db.Job{ID: "terminal-head", Agent: "reviewer", Type: "review", State: string(workflow.JobQueued), Payload: string(encoded)}
	if err := store.CreateJobWithEvent(ctx, job, db.JobEvent{JobID: job.ID, Kind: "queued"}); err != nil {
		t.Fatalf("CreateJobWithEvent: %v", err)
	}
	worker := defaultJobWorker(store, nil)

	cause := errors.New("checkout head is aaa, not review job head bbb")
	deferred, err := worker.deferCheckoutContention(ctx, job, payload, cause)
	if err != nil {
		t.Fatalf("deferCheckoutContention returned error: %v", err)
	}
	if deferred {
		t.Fatal("a head mismatch was deferred into the retry ladder; it is deterministic and must be terminal")
	}
	after, err := store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	var stored workflow.JobPayload
	if err := json.Unmarshal([]byte(after.Payload), &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if strings.TrimSpace(stored.BlockerRetryAt) != "" {
		t.Fatalf("BlockerRetryAt = %q, want empty: a terminal condition must not carry a retry time", stored.BlockerRetryAt)
	}
	if strings.TrimSpace(stored.BlockerClass) != "" {
		t.Fatalf("BlockerClass = %q, want empty: a terminal condition must not be stamped as contention", stored.BlockerClass)
	}
}

// TestContentionMembersStillRetry is the scope fence: the split must leave the two genuinely
// transient members untouched.
func TestContentionMembersStillRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want checkoutContentionKind
	}{
		{"lock", "branch x is locked by other", checkoutContentionLock},
		{"dirty", "checkout /x has uncommitted changes", checkoutContentionDirty},
		{"head mismatch", "checkout head is a, not review job head b", checkoutContentionTerminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := classifyCheckoutContention(errors.New(tc.text)); got != tc.want {
				t.Fatalf("classify(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
