package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// reviewHeldForRunningReviewEventKind is the job event a review held by the
// overlap rule (#2726) carries. It names the running review it waits on.
const reviewHeldForRunningReviewEventKind = "review_held_for_running_review"

// reviewOverlapEntry is one review job of a pull request, as the overlap hold
// sees it.
type reviewOverlapEntry struct {
	job  db.Job
	head string
}

// reviewOverlapHeads keeps the earliest running reviews of a pull request at
// two distinct heads, which is all a holder lookup needs: a review at head h
// waits on the first of them whose head is not h.
type reviewOverlapHeads struct {
	first, second *reviewOverlapEntry
}

func (h *reviewOverlapHeads) add(entry *reviewOverlapEntry) {
	switch {
	case h.first == nil:
		h.first = entry
	case h.second == nil && entry.head != h.first.head:
		h.second = entry
	}
}

func (h *reviewOverlapHeads) holderFor(head string) *reviewOverlapEntry {
	if h.first != nil && h.first.head != head {
		return h.first
	}
	if h.second != nil && h.second.head != head {
		return h.second
	}
	return nil
}

// reviewOverlapKey is the (repo, pull request) a review belongs to. The repo
// is part of the key because pull request numbers are not repo-qualified.
func reviewOverlapKey(repo string, pullRequest int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(pullRequest)
}

// reviewOverlapEntryFor places a decoded review. ok is false for a job the
// hold cannot place (no repo, no PR or no head): such a job is neither held
// nor holds another.
func reviewOverlapEntryFor(job db.Job, payload workflow.JobPayload) (key string, entry *reviewOverlapEntry, ok bool) {
	head := strings.ToLower(strings.TrimSpace(payload.HeadSHA))
	if strings.TrimSpace(payload.Repo) == "" || payload.PullRequest <= 0 || head == "" {
		return "", nil, false
	}
	return reviewOverlapKey(payload.Repo, payload.PullRequest), &reviewOverlapEntry{job: job, head: head}, true
}

// queuedJobReviewOverlap is the (repo, PR) key and head of a review job, for
// the dispatch selector's one-head-per-pull-request rule; ok is false for any
// other job or a review the hold cannot place.
func queuedJobReviewOverlap(job db.Job) (key, head string, ok bool) {
	if job.Type != "review" {
		return "", "", false
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		return "", "", false
	}
	key, entry, ok := reviewOverlapEntryFor(job, payload)
	if !ok {
		return "", "", false
	}
	return key, entry.head, true
}

// dispatchedReviewHeads returns, keyed by pull request, the heads of the
// reviews in pending that this process has dispatched (dispatched reports it)
// but whose worker has not claimed them yet, so they are still queued. The
// overlap hold reads running reviews from the store and cannot see them in that
// window, so the dispatch selector is seeded with these heads instead (#2726).
func dispatchedReviewHeads(pending []db.Job, dispatched func(jobID string) bool) map[string]string {
	var heads map[string]string
	for _, job := range pending {
		if job.Type != "review" || !dispatched(job.ID) {
			continue
		}
		if key, head, ok := queuedJobReviewOverlap(job); ok {
			if heads == nil {
				heads = map[string]string{}
			}
			heads[key] = head
		}
	}
	return heads
}

// holdOverlappingReviews removes from jobs (the queued list, in queue order)
// every review that must wait for a running review of the same pull request
// (#2726).
//
// A review's prior-verdict list is rendered when it dispatches. A review of a
// new head that dispatched while an older head's review was still running froze
// that list without the running review's verdict, so it could not answer that
// review's findings. The rule here:
//
//   - HOLD: a queued review of (repo, PR) at head h does not dispatch while a
//     review of the same (repo, PR) at another head is running. Only running
//     engine reviews hold; a queued review never holds another, so nothing
//     that may or may not dispatch later (lane, disk guard, routing cap)
//     decides a hold. Reviews of the same head never hold each other, so split
//     parts and the request-level dedupe keep their behaviour. The held review
//     dispatches on the first pass after the holder ends, with the verdicts
//     recorded by then.
//   - ORDER: once the holder ends, the scheduler picks among the waiting
//     reviews as usual; its selector admits reviews of one head per pull
//     request per selection (queuedJobResourceSelector), seeded with the
//     reviews this process dispatched that are not claimed yet
//     (dispatchedReviewHeads). The first one dispatched holds the rest from
//     then on, through the selector until it is claimed and through this hold
//     while it runs.
//   - NON-DESTRUCTIVE: the hold only filters and cancels neither review. The
//     PR watcher's stale-head supersession (Daemon.supersedeStaleReviewJobs)
//     can still cancel a queued or running review of a head the pull request
//     has moved past, as it does without a hold; a holder cancelled that way
//     records no verdict.
//   - VISIBLE: each held review gets one review_held_for_running_review event
//     per holder, naming it. A held review is still queued, so the stalled-PR
//     check counts it as a review in progress at its head.
//
// The hold runs first in the dispatch listing, so a held review is never
// routed to remote, counted against a routing cap or given a lane, and takes
// no checkout or runtime lock, no reservation and no spend. A local review
// keeps the exact-head worktree it was created with before enqueue, as any
// queued review does while it waits for a worker slot.
//
// The hold is bounded by its holder, an engine-owned job with its own delivery
// timeout, runtime lease and stale-running recovery. Externally driven session
// reviews, which have none of those, never hold. Queued reviews outside the
// pass's repo or --session filter are left alone.
//
// Cost: with no queued review it reads nothing. Otherwise it reads the running
// reviews once and decodes a queued review's payload only when a review of its
// pull request is running (matched on the projected repo and pull_request
// columns); a row without them is decoded to be placed.
//
// act is false for read-only callers (the serialization preflight): they get
// the same filtering without events or the hold log. With act, every call
// also prunes the hold log, whatever the queue holds.
func holdOverlappingReviews(ctx context.Context, store *db.Store, holds *reviewHoldLog, jobs []db.Job, repoFilter, rootFilter string, act bool) ([]db.Job, error) {
	queuedKeys := map[string]bool{}
	unprojected := false
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		if strings.TrimSpace(job.Repo) == "" {
			unprojected = true
			continue
		}
		queuedKeys[reviewOverlapKey(job.Repo, job.PullRequest)] = true
	}
	if len(queuedKeys) == 0 && !unprojected {
		if act {
			holds.sweep(jobs, nil)
		}
		return jobs, nil
	}
	running, err := store.ListRunningReviewJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list running reviews for the overlap hold: %w", err)
	}
	heads := map[string]*reviewOverlapHeads{}
	for _, job := range running {
		if strings.TrimSpace(job.Repo) != "" && !unprojected && !queuedKeys[reviewOverlapKey(job.Repo, job.PullRequest)] {
			continue
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			continue
		}
		key, entry, ok := reviewOverlapEntryFor(job, payload)
		if !ok {
			continue
		}
		if heads[key] == nil {
			heads[key] = &reviewOverlapHeads{}
		}
		heads[key].add(entry)
	}

	// held maps each queued review this pass weighed to whether it is held.
	held := map[string]bool{}
	anyHeld := false
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		if len(heads) == 0 || (strings.TrimSpace(job.Repo) != "" && heads[reviewOverlapKey(job.Repo, job.PullRequest)] == nil) {
			held[job.ID] = false
			continue
		}
		payload, err := daemonJobPayload(job)
		if err != nil || !queuedPayloadMatchesRepo(payload, repoFilter) || !queuedDecodedJobMatchesSession(job.ID, payload, nil, rootFilter) {
			continue
		}
		key, entry, ok := reviewOverlapEntryFor(job, payload)
		var holder *reviewOverlapEntry
		if ok && heads[key] != nil {
			holder = heads[key].holderFor(entry.head)
		}
		held[job.ID] = holder != nil
		if holder == nil {
			continue
		}
		anyHeld = true
		if act {
			if err := holds.record(ctx, store, job.ID, holder.job.ID, reviewOverlapHeldMessage(key, entry.head, holder)); err != nil {
				return nil, err
			}
		}
	}
	if act {
		holds.sweep(jobs, held)
	}
	if !anyHeld {
		return jobs, nil
	}
	kept := make([]db.Job, 0, len(jobs))
	for _, job := range jobs {
		if !held[job.ID] {
			kept = append(kept, job)
		}
	}
	return kept, nil
}

// reviewOverlapHeldMessage depends only on the held review and its holder
// (ids and heads), never on the holder's state, so the store's exact-message
// dedupe is a dedupe on that identity.
func reviewOverlapHeldMessage(key, head string, holder *reviewOverlapEntry) string {
	return fmt.Sprintf("review held: waiting on running review %s of %s at head %s; this review at head %s dispatches once that one ends, with the verdicts recorded by then",
		holder.job.ID, key, shortReviewHead(holder.head), shortReviewHead(head))
}

// reviewHoldLog is one scheduler's record of the holds it has written: held job
// id to holder job id. It saves an event write per pass for a hold already
// recorded; the store's dedupe (on a message that is a function of the same
// identity) stays the source of truth across restarts. Every dispatch listing
// prunes it, so it never outgrows the queue. A nil log records every pass and
// remembers nothing.
type reviewHoldLog struct {
	mu   sync.Mutex
	held map[string]string
}

func newReviewHoldLog() *reviewHoldLog {
	return &reviewHoldLog{held: map[string]string{}}
}

func (l *reviewHoldLog) record(ctx context.Context, store *db.Store, heldID, holderID, message string) error {
	if l != nil {
		l.mu.Lock()
		recorded := l.held[heldID] == holderID
		l.mu.Unlock()
		if recorded {
			return nil
		}
	}
	if _, err := store.ClaimJobEvent(ctx, db.JobEvent{JobID: heldID, Kind: reviewHeldForRunningReviewEventKind, Message: message}); err != nil {
		return fmt.Errorf("record review overlap hold on %s: %w", heldID, err)
	}
	if l != nil {
		l.mu.Lock()
		l.held[heldID] = holderID
		l.mu.Unlock()
	}
	return nil
}

// sweep evicts every hold that has ended: the held job left the queue, or this
// pass weighed it (weighed[id] present) and it is no longer held. A hold this
// pass did not weigh (a job outside its repo or session filter) is kept for the
// pass that does.
func (l *reviewHoldLog) sweep(queued []db.Job, weighed map[string]bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.held) == 0 {
		return
	}
	stillQueued := make(map[string]bool, len(queued))
	for _, job := range queued {
		stillQueued[job.ID] = true
	}
	for heldID := range l.held {
		if stillHeld, wasWeighed := weighed[heldID]; !stillQueued[heldID] || (wasWeighed && !stillHeld) {
			delete(l.held, heldID)
		}
	}
}
