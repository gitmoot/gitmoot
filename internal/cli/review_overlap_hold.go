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
// overlap rule (#2726) carries. It names the review it waits on.
const reviewHeldForRunningReviewEventKind = "review_held_for_running_review"

// reviewOverlapEntry is one review job of a pull request, as the overlap hold
// sees it.
type reviewOverlapEntry struct {
	job     db.Job
	head    string
	purpose string
	running bool
	// candidate: this pass may dispatch the job (it matches the pass's repo and
	// session filters). Only candidates are held, superseded or annotated.
	candidate  bool
	superseded bool
}

// reviewOverlapKey is the (repo, pull request) a review belongs to. The repo
// is part of the key because pull request numbers are not repo-qualified.
func reviewOverlapKey(repo string, pullRequest int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(pullRequest)
}

// reviewOverlapPayload decodes what the hold keys on. ok is false for a job
// the hold cannot place (no repo, no PR or no head): such a job is neither held
// nor holds another.
func reviewOverlapPayload(job db.Job) (key, head, purpose string, payload workflow.JobPayload, ok bool) {
	payload, err := daemonJobPayload(job)
	if err != nil || strings.TrimSpace(payload.Repo) == "" || payload.PullRequest <= 0 {
		return "", "", "", payload, false
	}
	head = strings.ToLower(strings.TrimSpace(payload.HeadSHA))
	if head == "" {
		return "", "", "", payload, false
	}
	purpose = db.ReviewRequestPurpose(payload.ReviewPurpose, payload.PostMergeReview, payload.ReviewPart)
	return reviewOverlapKey(payload.Repo, payload.PullRequest), head, purpose, payload, true
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
//   - HOLD: a queued review of (repo, PR) at head h does not dispatch while a
//     review of the same (repo, PR) at another head is running, or is queued
//     ahead of it. Reviews of the same head never hold each other, so split
//     parts and the request-level dedupe keep their behaviour. The held review
//     dispatches on the first pass after the holder settles, and its list is
//     rendered then, with the settled verdict in it.
//   - NEWEST WINS: of the held reviews that ask the same question (purpose,
//     post-merge and part), only the newest request stays. Each older one at
//     another head is superseded through workflow.SupersedeStaleHeadJob. A
//     running review is never superseded here.
//   - VISIBLE: each held review gets one review_held_for_running_review event
//     per holder, naming it. A held review is still queued, so the stalled-PR
//     check counts it as a review in progress at its head.
//
// The hold is bounded by its holder. A running holder is an engine-owned job
// with its own delivery timeout, runtime lease and stale-running recovery, and
// externally driven session reviews, which have none of those, never hold. A
// queued holder is first in line and dispatches as soon as it can.
//
// act is false for read-only callers (the serialization preflight): they get
// the same filtering without supersession or events. The running reviews are
// read once per call, and only when a candidate queued review exists.
func holdOverlappingReviews(ctx context.Context, store *db.Store, jobs []db.Job, repoFilter, rootFilter string, act bool) ([]db.Job, error) {
	groups := map[string][]*reviewOverlapEntry{}
	var order []string
	hasCandidate := false
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		key, head, purpose, _, ok := reviewOverlapPayload(job)
		if !ok {
			continue
		}
		candidate := queuedJobMatchesRepo(job, repoFilter) && queuedJobMatchesSession(job, rootFilter)
		hasCandidate = hasCandidate || candidate
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], &reviewOverlapEntry{job: job, head: head, purpose: purpose, candidate: candidate})
	}
	if !hasCandidate {
		return jobs, nil
	}
	running, err := store.ListRunningReviewJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list running reviews for the overlap hold: %w", err)
	}
	runningByKey := map[string][]*reviewOverlapEntry{}
	for _, job := range running {
		// The projected columns skip the decode for another PR's review; a row
		// with no projected repo is decoded, since only the payload can place it.
		if strings.TrimSpace(job.Repo) != "" {
			if _, relevant := groups[reviewOverlapKey(job.Repo, job.PullRequest)]; !relevant {
				continue
			}
		}
		key, head, purpose, _, ok := reviewOverlapPayload(job)
		if !ok {
			continue
		}
		if _, relevant := groups[key]; !relevant {
			continue
		}
		runningByKey[key] = append(runningByKey[key], &reviewOverlapEntry{job: job, head: head, purpose: purpose, running: true})
	}

	held := map[string]bool{}
	for _, key := range order {
		queued := groups[key]
		holders := runningByKey[key]
		if act {
			if err := supersedeOlderHeldReviews(ctx, store, key, queued, holders); err != nil {
				return nil, err
			}
		}
		for i, entry := range queued {
			if !entry.candidate || entry.superseded {
				continue
			}
			holder := reviewOverlapHolder(entry, queued[:i], holders)
			if holder == nil {
				continue
			}
			held[entry.job.ID] = true
			if act {
				if err := recordReviewOverlapHold(ctx, store, entry.job.ID, reviewOverlapHeldMessage(key, entry, holder)); err != nil {
					return nil, err
				}
			}
		}
		for _, entry := range queued {
			if entry.superseded {
				held[entry.job.ID] = true
			}
		}
	}
	if len(held) == 0 {
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

// reviewOverlapHolder returns the review entry waits on: a running review at
// another head first, else the first review queued ahead of it at another head
// that has not been superseded. nil means entry may dispatch.
func reviewOverlapHolder(entry *reviewOverlapEntry, ahead []*reviewOverlapEntry, running []*reviewOverlapEntry) *reviewOverlapEntry {
	for _, other := range running {
		if other.head != entry.head {
			return other
		}
	}
	for _, other := range ahead {
		if !other.superseded && other.head != entry.head {
			return other
		}
	}
	return nil
}

// supersedeOlderHeldReviews keeps, per question, only the newest held request:
// every older held candidate at another head is superseded. queued is in queue
// order, so the last held entry of a purpose is its newest request.
func supersedeOlderHeldReviews(ctx context.Context, store *db.Store, key string, queued []*reviewOverlapEntry, running []*reviewOverlapEntry) error {
	newest := map[string]*reviewOverlapEntry{}
	for i, entry := range queued {
		if reviewOverlapHolder(entry, queued[:i], running) != nil {
			newest[entry.purpose] = entry
		}
	}
	for i, entry := range queued {
		winner := newest[entry.purpose]
		if !entry.candidate || winner == nil || winner == entry || winner.head == entry.head {
			continue
		}
		holder := reviewOverlapHolder(entry, queued[:i], running)
		if holder == nil {
			continue
		}
		repo, pr := splitReviewOverlapKey(key)
		reason := fmt.Sprintf("review job superseded_stale_head: review %s of %s#%s at newer head %q was requested while this review at head %q waited on review %s; only the newest held request dispatches",
			winner.job.ID, repo, pr, winner.head, entry.head, holder.job.ID)
		if _, _, err := workflow.SupersedeStaleHeadJob(ctx, store, entry.job.ID, reason); err != nil {
			return fmt.Errorf("supersede held review %s: %w", entry.job.ID, err)
		}
		entry.superseded = true
	}
	return nil
}

func reviewOverlapHeldMessage(key string, entry, holder *reviewOverlapEntry) string {
	repo, pr := splitReviewOverlapKey(key)
	state := "queued ahead of it"
	if holder.running {
		state = "running"
	}
	return fmt.Sprintf("review held: review %s of %s#%s at head %s is %s; this review at head %s dispatches once it settles, so its prior verdicts include that verdict",
		holder.job.ID, repo, pr, shortReviewHead(holder.head), state, shortReviewHead(entry.head))
}

// reviewOverlapHoldsRecorded remembers the hold events this process has
// written, so a review held for an hour costs one INSERT and not one per
// dispatch pass (a pass runs about every second). ClaimJobEvent still dedups
// across restarts. The map is cleared when it outgrows its bound; the worst
// case is one redundant no-op INSERT per held review.
var (
	reviewOverlapHoldsMu       sync.Mutex
	reviewOverlapHoldsRecorded = map[string]bool{}
)

const reviewOverlapHoldsMax = 512

func recordReviewOverlapHold(ctx context.Context, store *db.Store, jobID, message string) error {
	key := jobID + "\x00" + message
	reviewOverlapHoldsMu.Lock()
	recorded := reviewOverlapHoldsRecorded[key]
	reviewOverlapHoldsMu.Unlock()
	if recorded {
		return nil
	}
	if _, err := store.ClaimJobEvent(ctx, db.JobEvent{JobID: jobID, Kind: reviewHeldForRunningReviewEventKind, Message: message}); err != nil {
		return fmt.Errorf("record review overlap hold on %s: %w", jobID, err)
	}
	reviewOverlapHoldsMu.Lock()
	if len(reviewOverlapHoldsRecorded) >= reviewOverlapHoldsMax {
		reviewOverlapHoldsRecorded = map[string]bool{}
	}
	reviewOverlapHoldsRecorded[key] = true
	reviewOverlapHoldsMu.Unlock()
	return nil
}

func splitReviewOverlapKey(key string) (repo, pullRequest string) {
	i := strings.LastIndex(key, "#")
	return key[:i], key[i+1:]
}
