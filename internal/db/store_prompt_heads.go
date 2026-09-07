package db

import (
	"context"
	"strings"
)

// The head a job was dispatched against lives in the payload JSON, not in a
// column: `jobs` has `repo` and `pull_request` but no `head_sha`. Both queries
// below therefore read `$.head_sha` through the guarded form this package
// already uses everywhere else - json_valid plus a json_type check - because
// modernc SQLite REJECTS malformed JSON where the legacy Go parser tolerated it
// by returning an empty payload (see ListDashboardJobSummaries).
//
// They exist for #1819's prompt-head guard, which has to answer "was this sha
// ever a recorded head of this pull request" for a sha that git may no longer be
// able to reach. A recorded head is an APPEND-ONLY fact that survives a force
// push, which is exactly the case ancestry cannot answer.
//
// SCOPE WARNING, because the natural instinct is to share this with the #1561
// re-sync gate and that would be a defect. This answers a question about
// HISTORY - may a prompt cite this sha - and the re-sync gate answers a question
// about WHAT GETS REVIEWED, where accepting a non-descendant target is the
// failure mode. The same force push makes the correct answers opposite. Keep
// this prompt-scoped.
//
// A recorded head is also what was stored AT DISPATCH, and resync can rewrite
// payload.head_sha in place, so this reads a set of historical values rather
// than a claim about what any reviewer actually read.

const recordedPullRequestHeadsSQL = `SELECT DISTINCT lower(trim(json_extract(payload, '$.head_sha')))
	FROM jobs
	WHERE repo = ? AND pull_request = ?
	  AND json_valid(payload) AND json_type(payload, '$.head_sha') = 'text'
	  AND trim(json_extract(payload, '$.head_sha')) <> ''`

// RecordedPullRequestHeads returns every distinct head SHA any job has recorded
// for one pull request, lowercased.
func (s *Store) RecordedPullRequestHeads(ctx context.Context, repo string, pullRequest int) ([]string, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" || pullRequest <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, recordedPullRequestHeadsSQL, repo, pullRequest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var heads []string
	for rows.Next() {
		var head string
		if err := rows.Scan(&head); err != nil {
			return nil, err
		}
		if head != "" {
			heads = append(heads, head)
		}
	}
	return heads, rows.Err()
}

const pullRequestsForRecordedHeadSQL = `SELECT DISTINCT pull_request
	FROM jobs
	WHERE repo = ? AND pull_request > 0
	  AND json_valid(payload) AND json_type(payload, '$.head_sha') = 'text'
	  AND lower(trim(json_extract(payload, '$.head_sha'))) LIKE ? || '%'
	ORDER BY pull_request`

// PullRequestsForRecordedHead returns the pull requests that have recorded a
// head starting with prefix. It takes a PREFIX because prompts cite abbreviated
// SHAs, and it is used only to turn a refusal into a diagnosis: "that sha is
// #1810's head" is actionable where "unknown commit referenced" is a shrug.
func (s *Store) PullRequestsForRecordedHead(ctx context.Context, repo string, prefix string) ([]int, error) {
	repo = strings.TrimSpace(repo)
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if repo == "" || prefix == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, pullRequestsForRecordedHeadSQL, repo, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pullRequests []int
	for rows.Next() {
		var pullRequest int
		if err := rows.Scan(&pullRequest); err != nil {
			return nil, err
		}
		pullRequests = append(pullRequests, pullRequest)
	}
	return pullRequests, rows.Err()
}
