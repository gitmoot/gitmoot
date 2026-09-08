package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/gitmoot/gitmoot/internal/db"
)

// HeadBoundExclusionEventKind records that a consumer excluded a row from a
// HEAD-BOUND decision, and why.
const HeadBoundExclusionEventKind = "head_bound_exclusion"

// Stable reason strings. They are constants rather than formatted text because
// the recorder's at-most-once guarantee is keyed on the exact message; see
// RecordHeadBoundExclusion.
const (
	// HeadBoundExclusionSessionRow is the #2008 class: an externally driven
	// session review self-roots and legitimately carries no head, because a head
	// on a review verdict must be ENGINE-OBSERVED and never caller-asserted
	// (#1990). Excluding it from a head-bound decision is CORRECT.
	HeadBoundExclusionSessionRow = "session row, self-rooted, no engine-observed head"
	// HeadBoundExclusionNoHeadRecorded covers every other headless row. It says
	// strictly less, on purpose.
	HeadBoundExclusionNoHeadRecorded = "no engine-observed head recorded on the row"
)

// HeadBoundExclusion reports whether a row must be excluded from a decision
// keyed on a head, and the reason to state when it is.
//
// THE REASONS ARE DIFFERENT BECAUSE HEADLESS DOES NOT IMPLY SESSION, and that is
// measured rather than assumed. On this box 211 review rows carry no head: 41
// externally driven, 156 failed, 10 cancelled, and 4 ephemeral ask-delegation
// children with pull_request=0 (#1962, #1895). Reporting all 211 as self-rooted
// session rows would be an over-attribution committed BY the change meant to
// improve honesty - the same defect class as proof's reviewed_ref reporting a
// head its row never recorded.
//
// It takes the two STORED fields rather than a job, so the db layer can carry
// them raw and this package can stay the only place the rule lives. workflow
// depends on db and never the reverse, so a predicate db could call does not
// exist; a copy inside db would be a second definition of the same rule and
// would still need the same column.
func HeadBoundExclusion(externallyDriven bool, headSHA string) (string, bool) {
	if strings.TrimSpace(headSHA) != "" {
		return "", false
	}
	if externallyDriven {
		return HeadBoundExclusionSessionRow, true
	}
	return HeadBoundExclusionNoHeadRecorded, true
}

// RecordHeadBoundExclusion makes one consumer's exclusion legible on the row it
// excluded, without changing whether the row is excluded.
//
// THE MESSAGE MUST STAY STABLE. ClaimJobEvent is at-most-once on the EXACT
// (job_id, kind, message) triple, so a message carrying anything variable - the
// evaluated head, a timestamp, a count of siblings - defeats the guarantee and
// appends one row per call. advanceReviewingPullRequest runs on every daemon
// poll tick, so that is not a slow leak: it is the mechanism that grew
// job_events to over a million rows. Consumer name and reason only.
//
// IT MUST NOT WRITE A HEAD, AND THAT IS THE WHOLE TRAP OF THIS CAMPAIGN. Making
// an exclusion legible and making the row head-bound are opposite fixes that
// look identical from a distance: both make the row stop vanishing. A consumer
// that "fixes" its silence by recording a head on a session row has bound a
// caller-asserted head into engine-observed evidence, which is exactly what
// #1990 established must never happen. This function records an EVENT beside the
// row and touches no payload.
func RecordHeadBoundExclusion(ctx context.Context, store *db.Store, jobID, consumer, reason string) error {
	jobID = strings.TrimSpace(jobID)
	consumer = strings.TrimSpace(consumer)
	reason = strings.TrimSpace(reason)
	if store == nil || jobID == "" || consumer == "" || reason == "" {
		return nil
	}
	_, err := store.ClaimJobEvent(ctx, db.JobEvent{
		JobID:   jobID,
		Kind:    HeadBoundExclusionEventKind,
		Message: fmt.Sprintf("%s: excluded from a head-bound decision (%s)", consumer, reason),
	})
	return err
}
