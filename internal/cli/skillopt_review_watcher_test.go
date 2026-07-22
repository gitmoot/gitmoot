package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/skillopt"
)

func TestSkillOptReviewWatcherImportsValidYAML(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			67: {
				{ID: 100, Body: "```yaml\nrun_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n  - item_id: item-002\n    ranking:\n      - B > C > A > D\n```\n", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"},
			},
		},
	}
	var stdout bytes.Buffer
	paths := config.PathsForHome(home)
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}
	events, err := store.ListRankedFeedbackEvents(context.Background(), "watcher-review-001")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents returned error: %v", err)
	}
	if len(events) != 2 || events[0].Reviewer != "alice" {
		t.Fatalf("ranked events = %+v", events)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusClosed || watch.LastSeenCommentID != 100 {
		t.Fatalf("watch = %+v", watch)
	}
	if len(fake.postedComments) != 1 || !strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchSuccessMarker) {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	if len(fake.closedIssues) != 1 || fake.closedIssues[0].IssueNumber != 67 {
		t.Fatalf("closed issues = %+v", fake.closedIssues)
	}
	iteration, err := store.GetSkillOptTrainIteration(context.Background(), "watcher-train-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateFeedbackSynced {
		t.Fatalf("iteration state = %s, want %s", iteration.State, skillopt.TrainStateFeedbackSynced)
	}
	if !strings.Contains(stdout.String(), "imported 2 skillopt review feedback events") {
		t.Fatalf("stdout = %q; home=%s", stdout.String(), home)
	}
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("second pollSkillOptReviewWatches returned error: %v", err)
	}
	events, err = store.ListRankedFeedbackEvents(context.Background(), "watcher-review-001")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents after second poll returned error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ranked events after second poll = %+v", events)
	}
}

func TestSkillOptReviewWatcherCommentsInvalidYAMLDeduped(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	setSkillOptReviewWatchStaleAfter(t, store, time.Now().UTC().Add(-time.Minute))
	paths := config.PathsForHome(home)
	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			67: {
				{ID: 100, Body: "run_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"},
			},
		},
	}
	var stdout bytes.Buffer
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}
	if len(fake.postedComments) != 1 {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	if !strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchErrorMarker) ||
		!strings.Contains(fake.postedComments[0].Body, "missing feedback for expected item_id(s): item-002") {
		t.Fatalf("error comment = %q", fake.postedComments[0].Body)
	}
	if strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchStaleMarker) {
		t.Fatalf("invalid feedback produced stale notice = %q", fake.postedComments[0].Body)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusWatching || watch.LastSeenCommentID != 0 || watch.LastImportErrorHash == "" {
		t.Fatalf("watch after invalid import = %+v", watch)
	}
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("second pollSkillOptReviewWatches returned error: %v", err)
	}
	if len(fake.postedComments) != 1 {
		t.Fatalf("posted comments after second poll = %+v", fake.postedComments)
	}
	events, err := store.ListRankedFeedbackEvents(context.Background(), "watcher-review-001")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents returned error: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("ranked events = %+v", events)
	}
	fake.comments[67][0].Body = "run_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n  - item_id: item-002\n    ranking:\n      - B > C > A > D\n"
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("third pollSkillOptReviewWatches returned error: %v", err)
	}
	events, err = store.ListRankedFeedbackEvents(context.Background(), "watcher-review-001")
	if err != nil {
		t.Fatalf("ListRankedFeedbackEvents after edit returned error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ranked events after edit = %+v", events)
	}
}

func TestSkillOptReviewWatcherPostsStaleNoticeOnce(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	setSkillOptReviewWatchStaleAfter(t, store, time.Now().UTC().Add(-time.Minute))
	fake := &skillOptFakeGitHub{comments: map[int64][]github.IssueComment{67: {}}}
	var stdout bytes.Buffer
	paths := config.PathsForHome(home)

	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}

	if len(fake.postedComments) != 1 {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	body := fake.postedComments[0].Body
	for _, want := range []string{
		skillOptReviewWatchStaleMarker,
		"review_issue: `owner/previews#67`",
		"run_id: `watcher-review-001`",
		"waiting_for: complete YAML feedback for item_ids `item-001, item-002`",
		"I posted the SkillOpt review feedback for run watcher-review-001 on owner/previews#67.",
		"gitmoot skillopt train continue --session watcher-train",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stale notice missing %q:\n%s", want, body)
		}
	}
	if len(fake.closedIssues) != 0 {
		t.Fatalf("closed issues = %+v", fake.closedIssues)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusStaleNotified || !watch.StaleNotified {
		t.Fatalf("watch after stale notice = %+v", watch)
	}
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("second pollSkillOptReviewWatches returned error: %v", err)
	}
	if len(fake.postedComments) != 1 {
		t.Fatalf("posted comments after second poll = %+v", fake.postedComments)
	}

	watch.Status = db.SkillOptReviewWatchStatusWatching
	watch.StaleNotified = false
	if err := store.UpsertSkillOptReviewWatch(context.Background(), watch); err != nil {
		t.Fatalf("reset watch after stale notice returned error: %v", err)
	}
	fake.comments[67] = append(fake.comments[67], github.IssueComment{ID: 90, Body: body, Author: "gitmoot", CreatedAt: "2026-06-04T09:00:00Z"})
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("third pollSkillOptReviewWatches returned error: %v", err)
	}
	if len(fake.postedComments) != 1 {
		t.Fatalf("posted comments after remote stale marker = %+v", fake.postedComments)
	}
	watch, err = store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch after remote stale marker returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusStaleNotified || !watch.StaleNotified {
		t.Fatalf("watch after remote stale marker = %+v", watch)
	}

	fake.comments[67] = append(fake.comments[67], github.IssueComment{ID: 100, Body: "run_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n  - item_id: item-002\n    ranking:\n      - B > C > A > D\n", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"})
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("fourth pollSkillOptReviewWatches returned error: %v", err)
	}
	if len(fake.postedComments) != 2 || !strings.Contains(fake.postedComments[1].Body, skillOptReviewWatchSuccessMarker) {
		t.Fatalf("posted comments after feedback = %+v", fake.postedComments)
	}
	if len(fake.closedIssues) != 1 {
		t.Fatalf("closed issues after feedback = %+v", fake.closedIssues)
	}
	watch, err = store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch after feedback returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusClosed {
		t.Fatalf("watch after stale feedback import = %+v", watch)
	}
}

func TestSkillOptReviewWatcherDoesNotStaleBeforeThreshold(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	setSkillOptReviewWatchStaleAfter(t, store, time.Now().UTC().Add(time.Hour))
	fake := &skillOptFakeGitHub{comments: map[int64][]github.IssueComment{67: {}}}
	var stdout bytes.Buffer

	if _, err := pollSkillOptReviewWatches(context.Background(), config.PathsForHome(home), store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}

	if len(fake.postedComments) != 0 || len(fake.closedIssues) != 0 {
		t.Fatalf("posted=%+v closed=%+v", fake.postedComments, fake.closedIssues)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusWatching || watch.StaleNotified {
		t.Fatalf("watch before stale threshold = %+v", watch)
	}
}

func TestSkillOptReviewWatcherStalesAfterUnrelatedComment(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	setSkillOptReviewWatchStaleAfter(t, store, time.Now().UTC().Add(-time.Minute))
	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			67: {
				{ID: 100, Body: "Can someone explain what item-002 means?", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"},
			},
		},
	}
	var stdout bytes.Buffer

	if _, err := pollSkillOptReviewWatches(context.Background(), config.PathsForHome(home), store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}

	if len(fake.postedComments) != 1 || !strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchStaleMarker) {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusStaleNotified || !watch.StaleNotified {
		t.Fatalf("watch after unrelated stale comment = %+v", watch)
	}
}

func TestSkillOptReviewWatcherImportsFeedbackInsteadOfStaleNotice(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	setSkillOptReviewWatchStaleAfter(t, store, time.Now().UTC().Add(-time.Minute))
	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			67: {
				{ID: 100, Body: "run_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n  - item_id: item-002\n    ranking:\n      - B > C > A > D\n", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"},
			},
		},
	}
	var stdout bytes.Buffer

	if _, err := pollSkillOptReviewWatches(context.Background(), config.PathsForHome(home), store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}

	if len(fake.postedComments) != 1 || !strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchSuccessMarker) {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	if strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchStaleMarker) {
		t.Fatalf("unexpected stale notice = %q", fake.postedComments[0].Body)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusClosed || watch.StaleNotified {
		t.Fatalf("watch after valid stale-aged feedback = %+v", watch)
	}
}

func TestSkillOptReviewWatcherKeepsImportedWhenTrainReviewLockBusy(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	release, _, err := acquireSkillOptTrainReviewLock(context.Background(), store, "watcher-train", "watcher-train-001")
	if err != nil {
		t.Fatalf("acquireSkillOptTrainReviewLock returned error: %v", err)
	}
	defer func() {
		_ = release(context.Background())
	}()
	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			67: {
				{ID: 100, Body: "run_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n  - item_id: item-002\n    ranking:\n      - B > C > A > D\n", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"},
			},
		},
	}
	var stdout bytes.Buffer
	if _, err := pollSkillOptReviewWatches(context.Background(), config.PathsForHome(home), store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("pollSkillOptReviewWatches returned error: %v", err)
	}
	if len(fake.postedComments) != 1 ||
		!strings.Contains(fake.postedComments[0].Body, "already active") ||
		!strings.Contains(fake.postedComments[0].Body, skillOptReviewWatchSuccessMarker) {
		t.Fatalf("posted comments = %+v", fake.postedComments)
	}
	if len(fake.closedIssues) != 0 {
		t.Fatalf("closed issues = %+v", fake.closedIssues)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusImported {
		t.Fatalf("watch status = %s, want imported while lock is active", watch.Status)
	}
	iteration, err := store.GetSkillOptTrainIteration(context.Background(), "watcher-train-001")
	if err != nil {
		t.Fatalf("GetSkillOptTrainIteration returned error: %v", err)
	}
	if iteration.State != skillopt.TrainStateReviewPublished {
		t.Fatalf("iteration state = %s, want review_published while lock is active", iteration.State)
	}
}

func TestSkillOptReviewWatcherRetriesImportedAckAndClose(t *testing.T) {
	home, store, blobStore := seedSkillOptReviewWatcherRun(t)
	defer store.Close()
	fake := &skillOptFakeGitHub{
		comments: map[int64][]github.IssueComment{
			67: {
				{ID: 100, Body: "run_id: watcher-review-001\nitems:\n  - item_id: item-001\n    ranking:\n      - C > A > D > B\n  - item_id: item-002\n    ranking:\n      - B > C > A > D\n", URL: "https://github.com/owner/previews/issues/67#issuecomment-100", Author: "alice", CreatedAt: "2026-06-04T10:00:00Z"},
			},
		},
		closeIssueErr: errors.New("temporary close failure"),
	}
	paths := config.PathsForHome(home)
	var stdout bytes.Buffer
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err == nil || !strings.Contains(err.Error(), "temporary close failure") {
		t.Fatalf("first poll error = %v, want close failure", err)
	}
	watch, err := store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusImported {
		t.Fatalf("watch status after failed close = %s, want imported", watch.Status)
	}
	if len(fake.postedComments) != 1 || len(fake.closedIssues) != 0 {
		t.Fatalf("posted=%+v closed=%+v", fake.postedComments, fake.closedIssues)
	}
	fake.comments[67] = append(fake.comments[67], github.IssueComment{ID: 101, Body: fake.postedComments[0].Body, Author: "gitmoot"})
	fake.closeIssueErr = nil
	if _, err := pollSkillOptReviewWatches(context.Background(), paths, store, blobStore, fake, &stdout, false, home); err != nil {
		t.Fatalf("second pollSkillOptReviewWatches returned error: %v", err)
	}
	watch, err = store.GetSkillOptReviewWatch(context.Background(), "owner/previews", 67)
	if err != nil {
		t.Fatalf("GetSkillOptReviewWatch after retry returned error: %v", err)
	}
	if watch.Status != db.SkillOptReviewWatchStatusClosed {
		t.Fatalf("watch status after retry = %s, want closed", watch.Status)
	}
	if len(fake.postedComments) != 1 {
		t.Fatalf("posted comments after retry = %+v, want no duplicate success comment", fake.postedComments)
	}
	if len(fake.closedIssues) != 1 {
		t.Fatalf("closed issues = %+v", fake.closedIssues)
	}
}

func TestSkillOptReviewWatcherCloseDecisionKeepsBlockedReviewOpen(t *testing.T) {
	if skillOptReviewWatchShouldCloseAfterContinuation(skillOptReviewWatchContinuation{Phase: skillopt.TrainStateReviewPublished}) {
		t.Fatal("review_published continuation should keep the review issue open")
	}
	if !skillOptReviewWatchShouldCloseAfterContinuation(skillOptReviewWatchContinuation{Phase: skillopt.TrainStateFeedbackSynced}) {
		t.Fatal("advanced continuation should close the review issue")
	}
	if skillOptReviewWatchShouldCloseAfterContinuation(skillOptReviewWatchContinuation{Phase: skillopt.TrainStateReviewPublished, Busy: true, Err: errSkillOptTrainReviewBusy}) {
		t.Fatal("busy continuation should keep the review issue open so the watcher can retry")
	}
}
