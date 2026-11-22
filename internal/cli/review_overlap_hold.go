package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// reviewHeldForRunningReviewEventKind is the job event a review held by the
// overlap rule (#2726) carries. It names the review it waits on.
const reviewHeldForRunningReviewEventKind = "review_held_for_running_review"

// reviewOverlapEntry is one review job of a pull request, as the overlap hold
// sees it.
type reviewOverlapEntry struct {
	job     db.Job
	head    string
	purpose string
	running bool
	// holder is the review this queued entry waits on; nil means it may
	// dispatch.
	holder *reviewOverlapEntry
	// removed: the entry left the queue during this pass (superseded here, or
	// claimed by a worker since the snapshot) and must not be handed on.
	removed bool
}

// reviewOverlapHeads keeps the earliest entries at two distinct heads, which is
// all a holder lookup needs: an entry at head h waits on the first of them
// whose head is not h.
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

// reviewOverlapGroup is every review of one (repo, PR) the hold weighs: the
// queued ones this pass may dispatch, in queue order, and the running ones.
type reviewOverlapGroup struct {
	repo        string
	pullRequest int
	queued      []*reviewOverlapEntry
	running     reviewOverlapHeads
}

// assignHolders sets each remaining queued entry's holder in one pass: a
// running review at another head first, else the earliest review queued ahead
// of it at another head. It reports whether held entries sit at two or more
// heads, the only case in which supersession has anything to decide.
func (g *reviewOverlapGroup) assignHolders() (heldAtSeveralHeads bool) {
	var ahead, held reviewOverlapHeads
	for _, entry := range g.queued {
		entry.holder = nil
		if entry.removed {
			continue
		}
		entry.holder = g.running.holderFor(entry.head)
		if entry.holder == nil {
			entry.holder = ahead.holderFor(entry.head)
		}
		ahead.add(entry)
		if entry.holder != nil {
			held.add(entry)
		}
	}
	return held.second != nil
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
	return reviewOverlapKey(payload.Repo, payload.PullRequest), &reviewOverlapEntry{
		job:     job,
		head:    head,
		purpose: db.ReviewRequestPurpose(payload.ReviewPurpose, payload.PostMergeReview, payload.ReviewPart),
	}, true
}

// holdOverlappingReviews removes from jobs (the queued list, in queue order)
// every queued review that must wait for another review of the same pull
// request (#2726).
//
// A review's prior-verdict list is rendered when it dispatches. A review of a
// new head that dispatched while an older head's review was still running froze
// that list without the running review's verdict, so it could not answer that
// review's findings. The rule here:
//
//   - SCOPE: only queued reviews this pass may dispatch (its repo and --session
//     filters) take part, so a review this daemon can never dispatch neither
//     holds nor replaces one it can. Running engine reviews hold across scopes:
//     they settle on their own.
//   - HOLD: a queued review of (repo, PR) at head h does not dispatch while a
//     review of the same (repo, PR) at another head is running, or is queued
//     ahead of it. Reviews of the same head never hold each other, so split
//     parts and the request-level dedupe keep their behaviour. The held review
//     dispatches on the first pass after the holder settles, and its list is
//     rendered then, with the settled verdict in it.
//   - CURRENT HEAD WINS: a held review is superseded only when its head is not
//     the pull request's stored head AND a held review asking the same question
//     (purpose, post-merge and part) waits at that stored head. Queue order
//     alone never cancels anything: with the head unknown, or matching neither,
//     the held reviews dispatch one after another. The supersession is
//     conditional on the job still being queued at the observed generation, so
//     a job a worker claimed since the snapshot is left running. A running
//     review is never superseded here.
//   - VISIBLE: each held review gets one review_held_for_running_review event
//     per holder, naming it. A held review is still queued, so the stalled-PR
//     check counts it as a review in progress at its head.
//
// A held review takes no checkout or runtime lock, no remote reservation and no
// spend. A local review keeps the exact-head worktree it was created with
// before enqueue, as any queued review does while it waits for a worker slot;
// a superseded one has that worktree reclaimed by the stale-head abort cleanup.
//
// The hold is bounded by its holder. A running holder is an engine-owned job
// with its own delivery timeout, runtime lease and stale-running recovery, and
// externally driven session reviews, which have none of those, never hold. A
// queued holder is first in line and dispatches as soon as it can.
//
// act is false for read-only callers (the serialization preflight): they get
// the same filtering without supersession, events or the hold log. Each queued
// review is decoded once; the running reviews are read once per call, and only
// when an in-scope queued review exists.
func holdOverlappingReviews(ctx context.Context, store *db.Store, holds *reviewHoldLog, jobs []db.Job, repoFilter, rootFilter string, act bool) ([]db.Job, error) {
	groups := map[string]*reviewOverlapGroup{}
	var order []string
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		payload, err := daemonJobPayload(job)
		if err != nil || !queuedPayloadMatchesRepo(payload, repoFilter) || !queuedPayloadMatchesSession(job.ID, payload, rootFilter) {
			continue
		}
		key, entry, ok := reviewOverlapEntryFor(job, payload)
		if !ok {
			continue
		}
		group := groups[key]
		if group == nil {
			group = &reviewOverlapGroup{repo: strings.TrimSpace(payload.Repo), pullRequest: payload.PullRequest}
			groups[key] = group
			order = append(order, key)
		}
		group.queued = append(group.queued, entry)
	}
	if len(groups) == 0 {
		if act {
			holds.sweep(jobs, groups)
		}
		return jobs, nil
	}
	running, err := store.ListRunningReviewJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list running reviews for the overlap hold: %w", err)
	}
	for _, job := range running {
		// The projected columns skip the decode for another PR's review; a row
		// with no projected repo is decoded, since only the payload can place it.
		if strings.TrimSpace(job.Repo) != "" && groups[reviewOverlapKey(job.Repo, job.PullRequest)] == nil {
			continue
		}
		payload, err := daemonJobPayload(job)
		if err != nil {
			continue
		}
		key, entry, ok := reviewOverlapEntryFor(job, payload)
		if !ok || groups[key] == nil {
			continue
		}
		entry.running = true
		groups[key].running.add(entry)
	}

	dropped := map[string]bool{}
	for _, key := range order {
		group := groups[key]
		if group.assignHolders() && act {
			changed, err := supersedeHeldOffHeadReviews(ctx, store, key, group)
			if err != nil {
				return nil, err
			}
			if changed {
				group.assignHolders()
			}
		}
		for _, entry := range group.queued {
			switch {
			case entry.removed:
				dropped[entry.job.ID] = true
			case entry.holder != nil:
				dropped[entry.job.ID] = true
				if act {
					if err := holds.record(ctx, store, entry, reviewOverlapHeldMessage(key, entry)); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if act {
		holds.sweep(jobs, groups)
	}
	if len(dropped) == 0 {
		return jobs, nil
	}
	kept := make([]db.Job, 0, len(jobs))
	for _, job := range jobs {
		if !dropped[job.ID] {
			kept = append(kept, job)
		}
	}
	return kept, nil
}

// supersedeHeldOffHeadReviews supersedes each held review whose head is not the
// pull request's stored head, when a held review of the same question waits at
// that head. It reports whether any entry left the queue.
func supersedeHeldOffHeadReviews(ctx context.Context, store *db.Store, key string, group *reviewOverlapGroup) (bool, error) {
	stored, err := store.GetPullRequest(ctx, group.repo, int64(group.pullRequest))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the stored head of %s for the overlap hold: %w", key, err)
	}
	current := strings.ToLower(strings.TrimSpace(stored.HeadSHA))
	if current == "" {
		return false, nil
	}
	winners := map[string]*reviewOverlapEntry{}
	for _, entry := range group.queued {
		if entry.holder != nil && entry.head == current && winners[entry.purpose] == nil {
			winners[entry.purpose] = entry
		}
	}
	changed := false
	for _, entry := range group.queued {
		winner := winners[entry.purpose]
		if entry.holder == nil || entry.removed || winner == nil || entry.head == current {
			continue
		}
		reason := fmt.Sprintf("review job superseded_stale_head: %s is at head %q and review %s waits at that head; this review at head %q was held behind review %s and will not dispatch",
			key, current, winner.job.ID, entry.head, entry.holder.job.ID)
		latest, transitioned, err := workflow.SupersedeQueuedStaleHeadJob(ctx, store, entry.job, reason)
		if err != nil {
			return false, fmt.Errorf("supersede held review %s: %w", entry.job.ID, err)
		}
		// Superseded, or claimed since the snapshot: either way it is no
		// longer this pass's to dispatch. A claimed one now holds the others.
		entry.removed = true
		changed = true
		if !transitioned && latest.State == string(workflow.JobRunning) {
			group.running.add(&reviewOverlapEntry{job: latest, head: entry.head, purpose: entry.purpose, running: true})
		}
	}
	return changed, nil
}

// reviewOverlapHeldMessage depends only on the held review and its holder
// (ids and heads), never on the holder's state, so the store's exact-message
// dedupe is a dedupe on that identity.
func reviewOverlapHeldMessage(key string, entry *reviewOverlapEntry) string {
	return fmt.Sprintf("review held: waiting on review %s of %s at head %s; this review at head %s dispatches once that one settles, so its prior verdicts include that verdict",
		entry.holder.job.ID, key, shortReviewHead(entry.holder.head), shortReviewHead(entry.head))
}

// reviewHoldLog is one scheduler's record of the holds it has written: held job
// id to holder job id. It saves an event write per pass for a hold already
// recorded; the store's dedupe (on a message that is a function of the same
// identity) stays the source of truth across restarts. Entries are evicted
// when the hold ends, so the log never outgrows the queue. A nil log records
// every pass and remembers nothing.
type reviewHoldLog struct {
	mu   sync.Mutex
	held map[string]string
}

func newReviewHoldLog() *reviewHoldLog {
	return &reviewHoldLog{held: map[string]string{}}
}

func (l *reviewHoldLog) record(ctx context.Context, store *db.Store, entry *reviewOverlapEntry, message string) error {
	heldID, holderID := entry.job.ID, entry.holder.job.ID
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
// pass weighed it and it is no longer held. A hold this pass did not weigh (a
// job outside its repo or session filter) is kept for the pass that does.
func (l *reviewHoldLog) sweep(queued []db.Job, groups map[string]*reviewOverlapGroup) {
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
	for _, group := range groups {
		for _, entry := range group.queued {
			if entry.removed || entry.holder == nil {
				delete(l.held, entry.job.ID)
			}
		}
	}
	for heldID := range l.held {
		if !stillQueued[heldID] {
			delete(l.held, heldID)
		}
	}
}
