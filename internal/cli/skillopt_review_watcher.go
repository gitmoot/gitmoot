package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jerryfane/gitmoot/internal/artifact"
	"github.com/jerryfane/gitmoot/internal/daemon"
	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/feedback"
	"github.com/jerryfane/gitmoot/internal/github"
)

const skillOptReviewWatchErrorMarker = "<!-- gitmoot:skillopt-review-watch-error -->"

func pollSkillOptReviewWatches(ctx context.Context, store *db.Store, blobStore artifact.Store, gh github.Client, stdout io.Writer, dryRun bool) (int, error) {
	if store == nil {
		return 0, fmt.Errorf("store is required")
	}
	if gh == nil {
		return 0, fmt.Errorf("github client is required")
	}
	watches, err := store.ListSkillOptReviewWatches(ctx, db.SkillOptReviewWatchStatusWatching)
	if err != nil {
		return 0, err
	}
	polled := 0
	var pollErr error
	for _, watch := range watches {
		if dryRun {
			writeLine(stdout, "skillopt review watch dry_run repo=%s issue=%d run=%s", watch.Repo, watch.IssueNumber, watch.RunID)
			polled++
			continue
		}
		if err := pollSkillOptReviewWatch(ctx, store, blobStore, gh, stdout, watch); err != nil {
			writeLine(stdout, "skillopt review watch %s#%d: %s", watch.Repo, watch.IssueNumber, err)
			pollErr = errors.Join(pollErr, err)
			continue
		}
		polled++
	}
	if len(watches) > 0 {
		writeLine(stdout, "polled %d skillopt review watches", polled)
	}
	return polled, pollErr
}

func pollSkillOptReviewWatch(ctx context.Context, store *db.Store, blobStore artifact.Store, gh github.Client, stdout io.Writer, watch db.SkillOptReviewWatch) error {
	repo, err := daemon.ParseRepository(watch.Repo)
	if err != nil {
		return fmt.Errorf("skillopt review watch %s#%d: %w", watch.Repo, watch.IssueNumber, err)
	}
	comments, err := gh.ListIssueComments(ctx, repo, watch.IssueNumber)
	if err != nil {
		return fmt.Errorf("skillopt review watch %s#%d: list comments: %w", watch.Repo, watch.IssueNumber, err)
	}
	sort.SliceStable(comments, func(i, j int) bool {
		return comments[i].ID < comments[j].ID
	})
	expected, err := skillOptReviewWatchExpectedItemIDs(watch)
	if err != nil {
		return fmt.Errorf("skillopt review watch %s#%d: %w", watch.Repo, watch.IssueNumber, err)
	}
	collector := feedback.GitHubCollector{BlobStore: blobStore, GitHub: gh}
	for _, comment := range comments {
		if comment.ID <= watch.LastSeenCommentID || isGitmootSkillOptReviewWatchComment(comment.Body) {
			continue
		}
		validation, err := feedback.ValidateGitHubReviewComment(comment.Body, watch.RunID, expected)
		if err != nil {
			if err := postSkillOptReviewWatchImportError(ctx, store, gh, repo, &watch, comment, err); err != nil {
				return err
			}
			continue
		}
		if !validation.Parseable {
			if err := store.UpsertSkillOptReviewWatch(ctx, watch); err != nil {
				return err
			}
			continue
		}
		result, err := collector.ImportComments(ctx, store, watch.RunID, []github.IssueComment{comment})
		if err != nil {
			if err := postSkillOptReviewWatchImportError(ctx, store, gh, repo, &watch, comment, err); err != nil {
				return err
			}
			continue
		}
		watch.Status = db.SkillOptReviewWatchStatusImported
		watch.LastSeenCommentID = comment.ID
		watch.LastImportErrorHash = ""
		if err := store.UpsertSkillOptReviewWatch(ctx, watch); err != nil {
			return err
		}
		writeLine(stdout, "imported %d skillopt review feedback events from %s#%d comment %d", result.Count(), watch.Repo, watch.IssueNumber, comment.ID)
		return nil
	}
	return store.UpsertSkillOptReviewWatch(ctx, watch)
}

func skillOptReviewWatchExpectedItemIDs(watch db.SkillOptReviewWatch) ([]string, error) {
	content := strings.TrimSpace(watch.ExpectedItemIDsJSON)
	if content == "" {
		return nil, nil
	}
	var itemIDs []string
	if err := json.Unmarshal([]byte(content), &itemIDs); err != nil {
		return nil, fmt.Errorf("decode expected item ids: %w", err)
	}
	return itemIDs, nil
}

func isGitmootSkillOptReviewWatchComment(body string) bool {
	body = strings.TrimSpace(body)
	return strings.Contains(body, skillOptReviewWatchErrorMarker) ||
		strings.Contains(body, "<!-- gitmoot:skillopt-feedback-packet -->") ||
		strings.HasPrefix(body, "# Gitmoot SkillOpt Feedback:")
}

func postSkillOptReviewWatchImportError(ctx context.Context, store *db.Store, gh github.Client, repo github.Repository, watch *db.SkillOptReviewWatch, comment github.IssueComment, importErr error) error {
	message := strings.TrimSpace(importErr.Error())
	if message == "" {
		message = "feedback comment could not be imported"
	}
	hash := skillOptReviewWatchImportErrorHash(watch.RunID, comment.ID, message)
	if hash != watch.LastImportErrorHash {
		body := skillOptReviewWatchImportErrorComment(watch.RunID, comment.ID, message)
		if _, err := gh.PostIssueComment(ctx, repo, watch.IssueNumber, body); err != nil {
			return fmt.Errorf("skillopt review watch %s#%d: post import error: %w", watch.Repo, watch.IssueNumber, err)
		}
		watch.LastImportErrorHash = hash
	}
	watch.Status = db.SkillOptReviewWatchStatusWatching
	return store.UpsertSkillOptReviewWatch(ctx, *watch)
}

func skillOptReviewWatchImportErrorHash(runID string, commentID int64, message string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(runID) + "\n" + fmt.Sprint(commentID) + "\n" + strings.TrimSpace(message)))
	return hex.EncodeToString(sum[:12])
}

func skillOptReviewWatchImportErrorComment(runID string, commentID int64, message string) string {
	var builder strings.Builder
	builder.WriteString(skillOptReviewWatchErrorMarker)
	builder.WriteString("\nGitmoot could not import the SkillOpt review feedback yet.\n\n")
	fmt.Fprintf(&builder, "- run_id: `%s`\n", strings.TrimSpace(runID))
	fmt.Fprintf(&builder, "- comment_id: `%d`\n", commentID)
	builder.WriteString("- error: ")
	builder.WriteString(strings.ReplaceAll(message, "\n", " "))
	builder.WriteString("\n\nPlease reply with a complete fenced `yaml` block for the expected `run_id` and all review `item_id` values.\n")
	return builder.String()
}
