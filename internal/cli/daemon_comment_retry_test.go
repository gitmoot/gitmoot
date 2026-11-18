package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"syscall"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// pinCommentRetryClock fixes the result-comment retry clock for one test.
func pinCommentRetryClock(t *testing.T, now time.Time) {
	t.Helper()
	previous := commentRetryNow
	commentRetryNow = func() time.Time { return now }
	t.Cleanup(func() { commentRetryNow = previous })
}

// countingCommenter counts every PostIssueComment attempt, failed or not.
type countingCommenter struct {
	cliPollFakeGitHub
	attempts int
}

func (c *countingCommenter) PostIssueComment(ctx context.Context, repo github.Repository, issueNumber int64, body string) (github.IssueComment, error) {
	c.attempts++
	return c.cliPollFakeGitHub.PostIssueComment(ctx, repo, issueNumber, body)
}

type commentRetryHarness struct {
	ctx       context.Context
	store     *db.Store
	worker    jobWorker
	commenter *countingCommenter
	jobID     string
}

// newCommentRetryHarness seeds one failed job on owner/repo#7 whose result
// comment failed failures times.
func newCommentRetryHarness(t *testing.T, failures int) *commentRetryHarness {
	t.Helper()
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	encoded, err := json.Marshal(workflow.JobPayload{Repo: "owner/repo", Branch: "main", PullRequest: 7})
	if err != nil {
		t.Fatal(err)
	}
	const jobID = "job-comment-backoff"
	if err := store.CreateJobWithEvent(ctx, db.Job{ID: jobID, Agent: "audit", Type: "ask", State: string(workflow.JobFailed), Payload: string(encoded)},
		db.JobEvent{JobID: jobID, Kind: string(workflow.JobFailed), Message: "boom"}); err != nil {
		t.Fatal(err)
	}
	for range failures {
		if err := store.AddJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: "comment_post_failed", Message: "temporary github error"}); err != nil {
			t.Fatal(err)
		}
	}
	commenter := &countingCommenter{}
	worker := defaultJobWorker(store, io.Discard)
	worker.CommenterFactory = func(string) github.Client { return commenter }
	return &commentRetryHarness{ctx: ctx, store: store, worker: worker, commenter: commenter, jobID: jobID}
}

func (h *commentRetryHarness) retryAt(t *testing.T, now time.Time) {
	t.Helper()
	pinCommentRetryClock(t, now)
	if err := retryPendingJobComments(h.ctx, h.worker, "owner/repo", "", newTickCandidates(h.store)); err != nil {
		t.Fatalf("retryPendingJobComments: %v", err)
	}
}

// latestFailedAt is the newest comment_post_failed event's timestamp.
func (h *commentRetryHarness) latestFailedAt(t *testing.T) time.Time {
	t.Helper()
	state, err := h.store.CommentRetryState(h.ctx, h.jobID)
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.DateTime, state.LatestFailedAt)
	if err != nil {
		t.Fatalf("latest comment_post_failed created_at %q: %v", state.LatestFailedAt, err)
	}
	return at
}

func (h *commentRetryHarness) eventCount(t *testing.T, kind string) int {
	t.Helper()
	events, err := h.store.ListJobEvents(h.ctx, h.jobID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

func (h *commentRetryHarness) assertNoLongerCandidate(t *testing.T) {
	t.Helper()
	pending, err := h.store.JobIDsWithPendingCommentRetry(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("JobIDsWithPendingCommentRetry = %v after abandonment, want none", pending)
	}
}

// #2688: a failed result comment was re-posted on every 1s worker tick. It now
// waits 1m after the first failure, doubling per failure.
func TestRetryPendingJobCommentsWaitsOutBackoff(t *testing.T) {
	h := newCommentRetryHarness(t, 1)
	h.commenter.postErr = fmt.Errorf("HTTP 502")
	failedAt := h.latestFailedAt(t)

	h.retryAt(t, failedAt.Add(commentRetryBaseDelay-time.Second))
	if h.commenter.attempts != 0 {
		t.Fatalf("post attempts inside the 1m backoff = %d, want 0", h.commenter.attempts)
	}

	h.retryAt(t, failedAt.Add(commentRetryBaseDelay))
	if h.commenter.attempts != 1 {
		t.Fatalf("post attempts after the 1m backoff = %d, want 1", h.commenter.attempts)
	}

	// The second failure doubles the wait to 2m, from the newest failure.
	failedAt = h.latestFailedAt(t)
	h.retryAt(t, failedAt.Add(2*commentRetryBaseDelay-time.Second))
	if h.commenter.attempts != 1 {
		t.Fatalf("post attempts inside the 2m backoff = %d, want 1", h.commenter.attempts)
	}
	h.commenter.postErr = nil
	h.retryAt(t, failedAt.Add(2*commentRetryBaseDelay))
	if h.commenter.attempts != 2 || len(h.commenter.posted) != 1 {
		t.Fatalf("attempts = %d, posted = %d after the 2m backoff; want 2 attempts, 1 post", h.commenter.attempts, len(h.commenter.posted))
	}
}

func TestCommentRetryDelayDoublesToOneHourCap(t *testing.T) {
	for failures, want := range map[int]time.Duration{
		1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 6: 32 * time.Minute, 7: time.Hour, 19: time.Hour,
	} {
		if got := commentRetryDelay(failures); got != want {
			t.Errorf("commentRetryDelay(%d) = %s, want %s", failures, got, want)
		}
	}
}

// The 20th failed post abandons the comment: one comment_post_abandoned event,
// and the retry pass never offers or posts it again.
func TestRetryPendingJobCommentsAbandonsAtAttemptCap(t *testing.T) {
	h := newCommentRetryHarness(t, commentRetryMaxAttempts-1)
	h.commenter.postErr = fmt.Errorf("HTTP 502")
	far := time.Now().Add(24 * time.Hour)

	h.retryAt(t, far)
	if h.commenter.attempts != 1 {
		t.Fatalf("post attempts = %d, want the 20th attempt", h.commenter.attempts)
	}
	if got := h.eventCount(t, commentPostAbandonedKind); got != 1 {
		t.Fatalf("comment_post_abandoned events = %d, want 1", got)
	}
	h.assertNoLongerCandidate(t)

	h.retryAt(t, far.Add(24*time.Hour))
	if h.commenter.attempts != 1 || h.eventCount(t, commentPostAbandonedKind) != 1 {
		t.Fatalf("attempts = %d, abandoned = %d after abandonment; want no more", h.commenter.attempts, h.eventCount(t, commentPostAbandonedKind))
	}
}

// A delivery that failed past the cap before the cap existed (the live jobs
// held ~48k failures) is abandoned on its next check without another post.
func TestRetryPendingJobCommentsAbandonsPreexistingBacklogWithoutPosting(t *testing.T) {
	h := newCommentRetryHarness(t, commentRetryMaxAttempts+5)
	h.retryAt(t, time.Now().Add(24*time.Hour))
	if h.commenter.attempts != 0 {
		t.Fatalf("post attempts = %d, want 0", h.commenter.attempts)
	}
	if got := h.eventCount(t, commentPostAbandonedKind); got != 1 {
		t.Fatalf("comment_post_abandoned events = %d, want 1", got)
	}
	h.assertNoLongerCandidate(t)
}

// An exec-level EINVAL (a NUL byte in argv) or E2BIG fails identically on every
// attempt: it records one comment_post_abandoned, no comment_post_failed, and
// is never retried.
func TestPermanentExecErrorAbandonsCommentOnce(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EINVAL, syscall.E2BIG} {
		t.Run(errno.Error(), func(t *testing.T) {
			h := newCommentRetryHarness(t, 1)
			h.commenter.postErr = fmt.Errorf("gh api: %w", &fs.PathError{Op: "fork/exec", Path: "/usr/bin/gh", Err: errno})
			h.retryAt(t, time.Now().Add(24*time.Hour))
			if h.commenter.attempts != 1 {
				t.Fatalf("post attempts = %d, want 1", h.commenter.attempts)
			}
			if got := h.eventCount(t, commentPostAbandonedKind); got != 1 {
				t.Fatalf("comment_post_abandoned events = %d, want 1", got)
			}
			if got := h.eventCount(t, "comment_post_failed"); got != 1 {
				t.Fatalf("comment_post_failed events = %d, want only the seeded one", got)
			}
			h.assertNoLongerCandidate(t)
			for i := range 3 {
				h.retryAt(t, time.Now().Add(time.Duration(48+i)*time.Hour))
			}
			if h.commenter.attempts != 1 {
				t.Fatalf("post attempts after abandonment = %d, want 1", h.commenter.attempts)
			}
		})
	}
}
